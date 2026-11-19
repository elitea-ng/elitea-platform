package microsoftapps

import (
	"regexp"
	"sort"
	"strings"
)

// The permission model of a registry app (ADR-0057 decision 3): per credential
// type, which delegated Microsoft Graph permissions the app may ask for (its
// scope ceiling), and what the ceiling means when an administrator leaves it
// empty.
//
// The offered lists of Outlook and Teams are the fixed lists of the SDK's
// microsoft_graph_scopes.py (the credential form's checkboxes). Microsoft Graph
// and SharePoint take free text. Dataverse has no per-permission ceiling: its one
// scope is the environment's user_impersonation, derived from the credential's
// own org URL, so the app's ceiling for it is only "served or not".

// Permission is one delegated permission the Entra app registration must list.
type Permission struct {
	Name string `json:"name"`
	// AdminConsent reports a permission Microsoft grants only after an Entra
	// administrator consents (the checklist prints the consent URL for it).
	AdminConsent bool `json:"admin_consent"`
}

// Resource is one API an Entra app registration adds permissions of.
type Resource struct {
	// API is the name the Entra portal shows under "Request API permissions".
	API         string       `json:"api"`
	Permissions []Permission `json:"permissions"`
}

var outlookPermissions = []string{
	"Mail.Read", "Mail.ReadWrite", "Mail.Send", "Mail.Read.Shared", "Mail.ReadWrite.Shared", "Mail.Send.Shared",
}

var teamsPermissions = []string{
	"User.ReadBasic.All", "Chat.Read", "ChatMessage.Send", "Chat.Create", "Team.ReadBasic.All",
	"Channel.ReadBasic.All", "ChannelMessage.Send", "ChannelMessage.Read.All",
}

var (
	outlookDefaults    = []string{"Mail.Read"}
	teamsDefaults      = []string{"User.ReadBasic.All", "Chat.Read", "Team.ReadBasic.All", "Channel.ReadBasic.All"}
	msGraphDefaults    = []string{"User.Read"}
	sharePointDefaults = []string{"Sites.Read.All"}
)

// adminConsentPermissions are the offered permissions that need Entra admin
// consent (the SDK: ChannelMessage.Read.All, "off by default").
var adminConsentPermissions = map[string]bool{"ChannelMessage.Read.All": true}

// permissionPattern is a Graph permission name: Mail.Read, Sites.ReadWrite.All.
var permissionPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)+$`)

// offered returns the fixed list of a type, or nil for a free-text type.
func offered(credentialType string) []string {
	switch credentialType {
	case TypeOutlook:
		return outlookPermissions
	case TypeTeams:
		return teamsPermissions
	}
	return nil
}

// Offered is the list an administrator picks the ceiling from: the fixed list of
// Outlook or Teams, and nil for the free-text types.
func Offered(credentialType string) []string {
	return append([]string(nil), offered(credentialType)...)
}

// Defaults is the least-privilege ceiling of a type with no ceiling saved.
func Defaults(credentialType string) []string {
	switch credentialType {
	case TypeOutlook:
		return append([]string(nil), outlookDefaults...)
	case TypeTeams:
		return append([]string(nil), teamsDefaults...)
	case TypeMSGraph:
		return append([]string(nil), msGraphDefaults...)
	case TypeSharePoint:
		return append([]string(nil), sharePointDefaults...)
	}
	return nil
}

// NormalizeCeiling validates one type's ceiling and returns it in canonical form:
// the fixed list's own casing and order for Outlook and Teams; for Microsoft
// Graph and SharePoint, Graph permission names (a bare name or the
// https://graph.microsoft.com/ form), de-duplicated case-insensitively with the
// first spelling winning, and never `.default`, which would ask for everything
// the app was granted and so has no ceiling. Dataverse takes no list.
func NormalizeCeiling(credentialType string, names []string) ([]string, error) {
	if credentialType == TypeDataverse {
		if len(names) > 0 {
			return nil, invalid("scope_ceiling", "Dataverse has no permission ceiling: its one scope is the environment's user_impersonation")
		}
		return nil, nil
	}
	if len(names) > 64 {
		return nil, invalid("scope_ceiling", "a ceiling lists at most 64 permissions")
	}
	if fixed := offered(credentialType); fixed != nil {
		canonical := map[string]string{}
		for _, name := range fixed {
			canonical[strings.ToLower(name)] = name
		}
		picked := map[string]bool{}
		for _, raw := range names {
			name, ok := canonical[strings.ToLower(stripGraphPrefix(strings.TrimSpace(raw)))]
			if !ok {
				return nil, invalid("scope_ceiling", "%q is not a permission the %s credential offers", raw, credentialType)
			}
			picked[name] = true
		}
		var out []string
		for _, name := range fixed {
			if picked[name] {
				out = append(out, name)
			}
		}
		return out, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range names {
		name := stripGraphPrefix(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !permissionPattern.MatchString(name) || strings.EqualFold(name, ".default") {
			return nil, invalid("scope_ceiling", "%q is not a Microsoft Graph delegated permission (use names like Mail.Read)", raw)
		}
		if key := strings.ToLower(name); !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out, nil
}

const graphPrefix = "https://graph.microsoft.com/"

func stripGraphPrefix(text string) string {
	if len(text) >= len(graphPrefix) && strings.EqualFold(text[:len(graphPrefix)], graphPrefix) {
		return text[len(graphPrefix):]
	}
	return text
}

// Ceiling is the permissions the app may ask for on a credential of type t: the
// saved ceiling, or the type's least-privilege default when none is saved.
func (a App) Ceiling(t string) []string {
	if saved := a.ScopeCeiling[t]; len(saved) > 0 {
		return append([]string(nil), saved...)
	}
	return Defaults(t)
}

// EffectiveScopes is a credential's scopes under a Default app: what it ticked, cut
// to the app's ceiling for the type. A credential that ticked nothing asks for the
// type's defaults, which the ceiling then limits like any other request. removed
// names what the ceiling cut, so the form can show it.
func (a App) EffectiveScopes(credentialType string, requested []string) (effective, removed []string) {
	if len(requested) == 0 {
		requested = Defaults(credentialType)
	}
	return Intersect(a.Ceiling(credentialType), requested)
}

// Intersect keeps the requested names the ceiling allows (case-insensitive, the
// ceiling's spelling wins), in the ceiling's order. An empty request yields an
// empty answer: the defaults of a type are EffectiveScopes' business, because only
// it knows the type. removed names what the ceiling cut.
func Intersect(ceiling, requested []string) (effective, removed []string) {
	allowed := map[string]string{}
	for _, name := range ceiling {
		allowed[strings.ToLower(name)] = name
	}
	want := map[string]bool{}
	for _, raw := range requested {
		name := stripGraphPrefix(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if canonical, ok := allowed[strings.ToLower(name)]; ok {
			want[canonical] = true
		} else {
			removed = append(removed, name)
		}
	}
	for _, name := range ceiling {
		if want[name] {
			effective = append(effective, name)
		}
	}
	return effective, removed
}

// RequiredPermissions is what the Entra app registration must list for the
// types the app serves and the ceilings it holds (the checklist's step 3).
// OIDC `offline_access` is added when the app refreshes tokens, and `openid` and
// `profile` always (a sign-in without them yields no identity). Dataverse is a
// separate API ("Dynamics CRM") with one permission, user_impersonation.
func (a App) RequiredPermissions() []Resource {
	graph := map[string]bool{"openid": false, "profile": false}
	if a.AutoRefreshToken {
		graph["offline_access"] = false
	}
	for _, t := range a.CredentialTypes {
		if t == TypeDataverse {
			continue
		}
		for _, name := range a.Ceiling(t) {
			graph[name] = adminConsentPermissions[name]
		}
	}
	names := make([]string, 0, len(graph))
	for name := range graph {
		names = append(names, name)
	}
	sortPermissionNames(names)
	resources := []Resource{{API: "Microsoft Graph"}}
	for _, name := range names {
		resources[0].Permissions = append(resources[0].Permissions, Permission{Name: name, AdminConsent: graph[name]})
	}
	if a.Serves(TypeDataverse) {
		resources = append(resources, Resource{
			API:         "Dynamics CRM",
			Permissions: []Permission{{Name: "user_impersonation"}},
		})
	}
	return resources
}

func sortPermissionNames(names []string) {
	rank := func(name string) int {
		switch name {
		case "openid":
			return 0
		case "profile":
			return 1
		case "offline_access":
			return 2
		}
		return 3
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
}
