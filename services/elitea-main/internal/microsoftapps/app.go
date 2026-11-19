// Package microsoftapps is the Default app registry of the Microsoft delegated
// credentials (ADR-0057 decisions 4, 5 and 11; epic 2254 story S1b).
//
// An administrator registers an Entra application once. A credential of type
// outlook, teams, ms_graph, dataverse or sharepoint then names only the app's id
// (`oauth_app_id`, "Default app"), and Main resolves the client at every use:
// client id, secret, authority, scope ceiling and refresh setting come from the
// registry, so an admin change needs no user action.
//
// THE CLIENT SECRET NEVER LEAVES MAIN. It is sealed in Postgres (AES-256-GCM,
// HKDF label KeyLabel, AAD-bound to the row), the admin API only ever says
// whether one is set and when it expires, and the one reader of the plaintext is
// Resolver.Resolve, which hands it to the code exchange and the refresh inside
// this process in a Secret that redacts itself in every format, log and JSON
// encoding.
//
// NOTHING SHIPS ENABLED. An empty registry means a credential form offers only
// Custom app; turning Default apps on later needs the Entra side and one row,
// created in the admin portal or seeded from Helm, with no release, migration or
// feature flag involved.
package microsoftapps

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Credential types a registry app can serve (ADR-0057 decision 2).
const (
	TypeOutlook    = "outlook"
	TypeTeams      = "teams"
	TypeMSGraph    = "ms_graph"
	TypeDataverse  = "dataverse"
	TypeSharePoint = "sharepoint"
)

// CredentialTypes are the five types, in display order.
func CredentialTypes() []string {
	return []string{TypeOutlook, TypeTeams, TypeMSGraph, TypeDataverse, TypeSharePoint}
}

// ValidType reports whether t is one of the five.
func ValidType(t string) bool {
	for _, known := range CredentialTypes() {
		if t == known {
			return true
		}
	}
	return false
}

// State is where an app is in its life.
type State string

const (
	// StateDraft has no secret yet.
	StateDraft State = "draft"
	// StateActive is usable.
	StateActive State = "active"
	// StateDisabled was switched off by an administrator.
	StateDisabled State = "disabled"
	// StateNeedsAttention was set by the platform: a secret that does not open,
	// a failed Test. StateReason carries the stable code.
	StateNeedsAttention State = "needs_attention"
)

// ProjectScope says which projects may use an app.
type ProjectScope string

const (
	// ScopeAll serves every project.
	ScopeAll ProjectScope = "all"
	// ScopeSelected serves the projects in App.ProjectIDs only.
	ScopeSelected ProjectScope = "selected"
)

// Stable reasons a state carries.
const (
	ReasonSecretUnreadable = "secret_unreadable"
)

// Bounds shared by validation, the migration's CHECKs and the form.
const (
	MaxNameLength        = 80
	MaxDescriptionLength = 300
	MaxProjects          = 500
	maxSecretLength      = 512
	// maxTenantLength is the migration's CHECK on the authority's tenant segment.
	maxTenantLength = 253
)

// EntraLoginHost is Entra's public login host. The registry pins it: a sovereign
// login host or any other host is refused in code (ADR-0057 decision 10), so the
// client secret goes only to login.microsoftonline.com.
const EntraLoginHost = "login.microsoftonline.com"

// App is one registry row as the admin sees it. It never holds the secret.
type App struct {
	ID      string
	SeedKey string
	// SeedHash is the keyed digest of the Helm entry last applied to the row (see
	// Store.Seed); empty for a row the console created.
	SeedHash    string
	Name        string
	Description string
	ClientID    string
	// Authority is https://login.microsoftonline.com/{tenant}.
	Authority string
	// SecretSet reports that a sealed secret exists. The value is never read
	// back.
	SecretSet       bool
	SecretExpiresAt *time.Time
	CredentialTypes []string
	// ScopeCeiling is, per type, the Graph permissions the app may ask for. A
	// type with no entry has its least-privilege default (see Ceiling).
	ScopeCeiling     map[string][]string
	AutoRefreshToken bool
	ProjectScope     ProjectScope
	ProjectIDs       []int64
	State            State
	StateReason      string
	CreatedBy        *int
	UpdatedBy        *int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Revision         int
}

// Serves reports whether the app lists credential type t.
func (a App) Serves(t string) bool {
	for _, served := range a.CredentialTypes {
		if served == t {
			return true
		}
	}
	return false
}

// ServesProject reports whether the app's project scope admits projectID.
func (a App) ServesProject(projectID int64) bool {
	if a.ProjectScope == ScopeAll {
		return true
	}
	for _, id := range a.ProjectIDs {
		if id == projectID {
			return true
		}
	}
	return false
}

// Input is a create or update. A nil Secret keeps the sealed one (update) or
// leaves the app a draft (create); a pointer to "" clears it.
type Input struct {
	Name             string
	Description      string
	ClientID         string
	Authority        string
	CredentialTypes  []string
	ScopeCeiling     map[string][]string
	AutoRefreshToken bool
	ProjectScope     ProjectScope
	ProjectIDs       []int64
	SecretExpiresAt  *time.Time
	Secret           *string
}

// ValidationError names the field that failed, so the form can point at it.
type ValidationError struct {
	Field  string
	Reason string
}

func (e ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, format string, args ...any) error {
	return ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

var (
	guidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	seedPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
)

// NormalizeClientID lower-cases and checks an Entra application (client) id.
func NormalizeClientID(raw string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !guidPattern.MatchString(id) {
		return "", invalid("client_id", "the client id must be the application (client) ID GUID from the Entra app registration")
	}
	return id, nil
}

// Authority is a parsed Entra authority.
type Authority struct {
	// URL is the canonical form, https://login.microsoftonline.com/{tenant}.
	URL string
	// Tenant is a GUID, a verified domain or an alias.
	Tenant string
}

// MultiTenant reports an alias that lets any organization's account sign in.
func (a Authority) MultiTenant() bool { return a.Tenant == "organizations" || a.Tenant == "common" }

// ParseAuthority accepts only https://login.microsoftonline.com/{tenant}[/v2.0]
// where tenant is a GUID, a verified domain, `organizations` or `common`. Every
// other host, scheme, port, userinfo, query, fragment or extra path is refused.
func ParseAuthority(raw string) (Authority, error) {
	text := strings.TrimSpace(raw)
	parsed, err := url.Parse(text)
	if text == "" || err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery ||
		!strings.EqualFold(parsed.Hostname(), EntraLoginHost) ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return Authority{}, invalid("authority", "the authority must be https://%s/{tenant}: only Microsoft Entra's public login host is accepted", EntraLoginHost)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 2 && strings.EqualFold(segments[1], "v2.0") {
		segments = segments[:1]
	}
	if len(segments) != 1 || segments[0] == "" {
		return Authority{}, invalid("authority", "the authority must name one tenant: a tenant ID, a verified domain, organizations or common")
	}
	tenant := strings.ToLower(segments[0])
	if len(tenant) > maxTenantLength || (!guidPattern.MatchString(tenant) && !domainPattern.MatchString(tenant) && tenant != "organizations" && tenant != "common") {
		return Authority{}, invalid("authority", "the tenant must be a tenant ID (GUID), a verified domain, organizations or common")
	}
	return Authority{URL: "https://" + EntraLoginHost + "/" + tenant, Tenant: tenant}, nil
}

// Normalize validates an input against the rules of the registry and returns it
// in canonical form. It does not read the secret except to bound it.
func (in Input) Normalize() (Input, error) {
	out := in
	out.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(out.Name); n < 1 || n > MaxNameLength {
		return Input{}, invalid("name", "the name must be 1 to %d characters", MaxNameLength)
	}
	out.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(out.Description) > MaxDescriptionLength {
		return Input{}, invalid("description", "the description is limited to %d characters", MaxDescriptionLength)
	}
	clientID, err := NormalizeClientID(in.ClientID)
	if err != nil {
		return Input{}, err
	}
	out.ClientID = clientID
	authority, err := ParseAuthority(in.Authority)
	if err != nil {
		return Input{}, err
	}
	out.Authority = authority.URL

	out.CredentialTypes = nil
	seen := map[string]bool{}
	for _, t := range in.CredentialTypes {
		t = strings.TrimSpace(t)
		if !ValidType(t) {
			return Input{}, invalid("credential_types", "%q is not a Microsoft credential type (%s)", t, strings.Join(CredentialTypes(), ", "))
		}
		if !seen[t] {
			seen[t] = true
			out.CredentialTypes = append(out.CredentialTypes, t)
		}
	}
	if len(out.CredentialTypes) == 0 {
		return Input{}, invalid("credential_types", "choose at least one credential type")
	}
	sortTypes(out.CredentialTypes)

	out.ScopeCeiling = map[string][]string{}
	for t, names := range in.ScopeCeiling {
		if !seen[t] {
			return Input{}, invalid("scope_ceiling", "a ceiling is set for %q, which the app does not serve", t)
		}
		normalized, err := NormalizeCeiling(t, names)
		if err != nil {
			return Input{}, err
		}
		if len(normalized) > 0 {
			out.ScopeCeiling[t] = normalized
		}
	}

	switch in.ProjectScope {
	case ScopeAll, ScopeSelected:
	case "":
		out.ProjectScope = ScopeAll
	default:
		return Input{}, invalid("project_scope", "the project scope must be all or selected")
	}
	out.ProjectIDs = nil
	if out.ProjectScope == ScopeSelected {
		ids := map[int64]bool{}
		for _, id := range in.ProjectIDs {
			if id <= 0 {
				return Input{}, invalid("project_ids", "a project id must be positive")
			}
			if !ids[id] {
				ids[id] = true
				out.ProjectIDs = append(out.ProjectIDs, id)
			}
		}
		if len(out.ProjectIDs) == 0 {
			return Input{}, invalid("project_ids", "choose at least one project, or serve every project")
		}
		if len(out.ProjectIDs) > MaxProjects {
			return Input{}, invalid("project_ids", "an app serves at most %d projects; serve every project instead", MaxProjects)
		}
		sort.Slice(out.ProjectIDs, func(i, j int) bool { return out.ProjectIDs[i] < out.ProjectIDs[j] })
	} else if len(in.ProjectIDs) > 0 {
		return Input{}, invalid("project_ids", "projects are listed only when the scope is selected")
	}

	if in.Secret != nil {
		if strings.ContainsAny(*in.Secret, "\r\n\x00") || len(*in.Secret) > maxSecretLength {
			return Input{}, invalid("secret", "the client secret is not a valid secret value")
		}
		trimmed := strings.TrimSpace(*in.Secret)
		if trimmed == "" && *in.Secret != "" {
			// Only an explicit empty string clears a secret. A blank value is a
			// paste mistake, and treating it as a clear would take the app out of
			// service.
			return Input{}, invalid("secret", "the client secret is blank; leave it out to keep the stored one")
		}
		out.Secret = &trimmed
	}
	return out, nil
}

// NormalizeID returns the lower-case canonical form of an app id, and whether id is
// one. Only the hyphenated GUID is accepted: the database cast and a lenient parser
// disagree about other spellings (urn:uuid:, braces), and a disagreement here would
// answer 503 where 404 is true.
func NormalizeID(id string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(id))
	return lower, guidPattern.MatchString(lower)
}

// ParseExpiry reads an administrator-entered secret expiry: an RFC 3339 time, or a
// date (2027-06-30), which means the END of that day in UTC. A secret that "expires
// on 30 June" is valid on 30 June; reading the date as its midnight would call it
// expired on its last valid day.
func ParseExpiry(text string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed, nil
	}
	date, err := time.Parse("2006-01-02", text)
	if err != nil {
		return time.Time{}, invalid("secret_expires_at", "use a date such as 2027-06-30")
	}
	return date.Add(24*time.Hour - time.Second), nil
}

// ValidSeedKey reports whether key may name a Helm-managed row.
func ValidSeedKey(key string) bool { return seedPattern.MatchString(key) }

func sortTypes(types []string) {
	order := map[string]int{}
	for i, t := range CredentialTypes() {
		order[t] = i
	}
	sort.Slice(types, func(i, j int) bool { return order[types[i]] < order[types[j]] })
}

// Errors the store answers.
var (
	ErrNotFound = errors.New("microsoftapps: no such app")
	ErrConflict = errors.New("microsoftapps: an app with this name already exists")
	ErrNoPool   = errors.New("microsoftapps: the database is not configured")
	// ErrCustodyIntegrity means a sealed secret did not open under its row.
	ErrCustodyIntegrity = errors.New("microsoftapps: the sealed secret does not match its record")
)
