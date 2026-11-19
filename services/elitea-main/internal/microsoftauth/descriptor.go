// Package microsoftauth describes the Microsoft delegated credential types
// (ADR-0057): Outlook, Teams, Microsoft Graph and Dataverse, plus SharePoint's
// delegated mode as the fifth.
//
// One immutable Descriptor per type is the only place a Microsoft type is
// spelled out. The delegated connection check
// (api/v2/configurations/check_connection_oauth.go) reads it, and so will the
// credential broker; nothing else hard-codes a Microsoft type. Descriptors are
// assembled in this package's init-free table (no plugin loading) and a
// Descriptor has no exported fields, so a caller cannot change what another
// caller sees.
//
// This package builds nothing that holds a token. It decides which authority a
// credential signs in at, which scopes it asks for, which audience a token is
// for and which identity read proves a token works. It never logs a value it
// was given.
package microsoftauth

import (
	"errors"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth/scopes"
)

// Credential type ids.
const (
	Outlook    = "outlook"
	Teams      = "teams"
	MSGraph    = "ms_graph"
	Dataverse  = "dataverse"
	SharePoint = "sharepoint"
)

// Field names a credential carries.
const (
	FieldDiscoveryEndpoint = "oauth_discovery_endpoint"
	FieldTenantID          = "tenant_id"
	FieldURL               = "url"
	FieldScopes            = "scopes"
	FieldAutoRefresh       = "auto_refresh_token"
	FieldAppID             = "oauth_app_id"
	FieldMailbox           = "mailbox"
)

// GraphHost is the only host a Graph token is ever sent to.
const GraphHost = "graph.microsoft.com"

// ScopeModel says how a type's scopes are chosen.
type ScopeModel int

const (
	// ScopesFixedList is a checkbox list (Outlook, Teams).
	ScopesFixedList ScopeModel = iota
	// ScopesFreeText is free text validated as Graph permissions (Microsoft Graph).
	ScopesFreeText
	// ScopesOrgImpersonation is the single scope derived from the org URL (Dataverse).
	ScopesOrgImpersonation
	// ScopesAsStored passes the credential's own list through, as SharePoint
	// always has.
	ScopesAsStored
)

// Probe says which identity read proves a token works.
type Probe int

const (
	// ProbeGraphMe is GET https://graph.microsoft.com/v1.0/me.
	ProbeGraphMe Probe = iota
	// ProbeWhoAmI is the Dataverse WhoAmI function on the credential's org.
	ProbeWhoAmI
	// ProbeNone means the check only ever asks for sign-in. SharePoint's probe
	// (GET /sites/{host}) lives with its toolkit, as it does today.
	ProbeNone
)

// Audience is the resource a token is issued for.
type Audience int

const (
	// AudienceGraph is Microsoft Graph.
	AudienceGraph Audience = iota
	// AudienceOrg is the Dataverse environment named by the credential's url.
	AudienceOrg
)

// Descriptor is one Microsoft delegated credential type.
type Descriptor struct {
	configType string
	label      string
	audience   Audience
	scopeModel ScopeModel
	probe      Probe
	// entraOnly refuses an authority that is not login.microsoftonline.com.
	// SharePoint keeps the check it always had (the egress guard and the
	// discovery document), so its saved credentials behave as before.
	entraOnly bool
	// delegatedAlways is false for SharePoint, whose credential is delegated
	// only when it names a discovery endpoint (without one it is app-only).
	delegatedAlways bool
	// tenantAuthority builds the authority from tenant_id (Dataverse).
	tenantAuthority bool
	governedAdapter string
	resourceName    string
	scopeList       scopes.List
	// defaultApp is true where a credential may name an admin-managed OAuth app
	// (oauth_app_id). SharePoint gains it with the registry (ADR-0057 decision 12).
	defaultApp bool
}

var descriptors = []Descriptor{
	{
		configType: Outlook, label: "Outlook", audience: AudienceGraph, scopeModel: ScopesFixedList,
		probe: ProbeGraphMe, entraOnly: true, delegatedAlways: true, governedAdapter: "outlook",
		resourceName: "Outlook", scopeList: scopes.Outlook, defaultApp: true,
	},
	{
		configType: Teams, label: "Teams", audience: AudienceGraph, scopeModel: ScopesFixedList,
		probe: ProbeGraphMe, entraOnly: true, delegatedAlways: true, governedAdapter: "teams",
		resourceName: "Teams", scopeList: scopes.Teams, defaultApp: true,
	},
	{
		configType: MSGraph, label: "Microsoft Graph", audience: AudienceGraph, scopeModel: ScopesFreeText,
		probe: ProbeGraphMe, entraOnly: true, delegatedAlways: true, governedAdapter: "ms_graph",
		resourceName: "Microsoft Graph", defaultApp: true,
	},
	{
		configType: Dataverse, label: "Dataverse (OneCRM)", audience: AudienceOrg, scopeModel: ScopesOrgImpersonation,
		probe: ProbeWhoAmI, entraOnly: true, delegatedAlways: true, tenantAuthority: true,
		governedAdapter: "dataverse", resourceName: "Dataverse", defaultApp: true,
	},
	{
		configType: SharePoint, label: "SharePoint", audience: AudienceGraph, scopeModel: ScopesAsStored,
		probe: ProbeNone, governedAdapter: "sharepoint", resourceName: "SharePoint",
	},
}

// For returns the descriptor of a credential type.
func For(configType string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.configType == configType {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

// workerWithheldFields are the credential fields a worker never receives for a
// delegated Microsoft credential (ADR-0057 decision 7): the worker holds an
// access token only. It calls no token endpoint, so it has no use for the
// client secret or a stored refresh token.
var workerWithheldFields = []string{"client_secret", "refresh_token"}

// WithholdFromWorker removes from data (a credential's fields on their way to
// a worker claim or a frozen run input) every field that would let the worker
// mint or refresh a Microsoft token. It reports whether it removed any. It
// acts only on the types whose delegated mode is the only mode (Outlook,
// Teams, Microsoft Graph, Dataverse): SharePoint's worker config still reads
// its client id and secret, and its behaviour is unchanged.
func WithholdFromWorker(configType string, data map[string]any) bool {
	descriptor, ok := For(configType)
	if !ok || !descriptor.delegatedAlways {
		return false
	}
	removed := false
	for _, field := range workerWithheldFields {
		if _, present := data[field]; present {
			delete(data, field)
			removed = true
		}
	}
	return removed
}

// Types lists the descriptor-driven credential types, in a stable order.
func Types() []string {
	types := make([]string, len(descriptors))
	for i, descriptor := range descriptors {
		types[i] = descriptor.configType
	}
	return types
}

// Type is the credential type id.
func (d Descriptor) Type() string { return d.configType }

// DefaultAppSupported reports whether a credential of this type may name an
// admin-managed OAuth app (oauth_app_id) instead of its own client.
func (d Descriptor) DefaultAppSupported() bool { return d.defaultApp }

// Label is the name the credential picker shows.
func (d Descriptor) Label() string { return d.label }

// ResourceName names the resource in a sign-in request.
func (d Descriptor) ResourceName() string { return d.resourceName }

// Audience says whose token this is.
func (d Descriptor) Audience() Audience { return d.audience }

// ScopeModel says how the scopes are chosen.
func (d Descriptor) ScopeModel() ScopeModel { return d.scopeModel }

// Probe says which identity read proves a token.
func (d Descriptor) Probe() Probe { return d.probe }

// GovernedAdapter is the credentialadapters name a governed deployment uses.
func (d Descriptor) GovernedAdapter() string { return d.governedAdapter }

// ScopeList is the fixed list of an Outlook or Teams descriptor (zero for the others).
func (d Descriptor) ScopeList() scopes.List { return d.scopeList }

// Errors a descriptor reports about a credential's own fields. Each message is
// safe to show to the person who typed the credential.
var (
	// ErrAuthorityMissing: the credential names no authority (Custom app with
	// no discovery endpoint).
	ErrAuthorityMissing = errors.New("this credential needs an OAuth discovery endpoint " +
		"(https://login.microsoftonline.com/{tenant_id})")
	// ErrNotEntraAuthority: the endpoint is not https://login.microsoftonline.com/<tenant>.
	ErrNotEntraAuthority = errors.New("the OAuth discovery endpoint must be " +
		"https://login.microsoftonline.com/<tenant_id>")
	// ErrInvalidTenant: tenant_id is not a tenant GUID or a domain name.
	ErrInvalidTenant = errors.New("tenant_id must be a tenant GUID or domain name")
	// ErrScopes wraps a refused scope list.
	ErrScopes = errors.New("invalid scopes")
)

// IsDelegated reports whether this credential signs in per user. The four new
// types always do; SharePoint does only when it names a discovery endpoint.
func (d Descriptor) IsDelegated(data map[string]any) bool {
	return d.delegatedAlways || text(data, FieldDiscoveryEndpoint) != ""
}

// Authority is the identity provider the credential signs in at.
//
// Dataverse builds it from tenant_id. The others take the discovery endpoint;
// those that are entraOnly accept nothing but https://login.microsoftonline.com.
func (d Descriptor) Authority(data map[string]any) (string, error) {
	if d.tenantAuthority {
		tenant := text(data, FieldTenantID)
		if !tenantPattern.MatchString(tenant) {
			return "", ErrInvalidTenant
		}
		return EntraHost + "/" + tenant, nil
	}
	endpoint := text(data, FieldDiscoveryEndpoint)
	if endpoint == "" {
		return "", ErrAuthorityMissing
	}
	if d.entraOnly {
		return NormalizeEntraAuthority(endpoint)
	}
	return endpoint, nil
}

// AudienceURL is the resource a token for this credential is for: Graph, or the
// validated Dataverse org.
func (d Descriptor) AudienceURL(data map[string]any) (string, error) {
	if d.audience == AudienceOrg {
		return NormalizeDataverseURL(text(data, FieldURL))
	}
	return "https://" + GraphHost, nil
}

// Scopes is the normalized scope selection of the credential: the same value a
// save stores and a sign-in asks for. dropped names what a fixed list did not
// offer.
func (d Descriptor) Scopes(data map[string]any) (selected, dropped []string, err error) {
	switch d.scopeModel {
	case ScopesFixedList:
		selected, dropped = d.scopeList.Normalize(data[FieldScopes])
		return selected, dropped, nil
	case ScopesFreeText:
		selected, err = scopes.NormalizeMSGraph(data[FieldScopes])
		if err != nil {
			return nil, nil, errors.Join(ErrScopes, err)
		}
		return selected, nil, nil
	case ScopesOrgImpersonation:
		org, err := NormalizeDataverseURL(text(data, FieldURL))
		if err != nil {
			return nil, nil, err
		}
		return []string{scopes.DataverseScope(org)}, nil, nil
	default:
		// SharePoint: whatever the credential lists, as it always has.
		return asStrings(data[FieldScopes]), nil, nil
	}
}

// SignInScopes is what the sign-in asks Microsoft for: Scopes plus
// offline_access unless token refresh is off (ADR-0057 decision 3).
func (d Descriptor) SignInScopes(data map[string]any) ([]string, []string, error) {
	selected, dropped, err := d.Scopes(data)
	if err != nil {
		return nil, nil, err
	}
	var autoRefresh *bool
	if value, ok := data[FieldAutoRefresh].(bool); ok {
		autoRefresh = &value
	}
	if d.scopeModel == ScopesAsStored {
		// SharePoint's sign-in scopes are the web client's business today.
		return selected, dropped, nil
	}
	return scopes.SignIn(selected, autoRefresh), dropped, nil
}

func text(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

func asStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
