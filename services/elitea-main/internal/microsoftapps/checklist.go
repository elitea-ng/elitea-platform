package microsoftapps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CallbackPath is the one https callback an Entra app needs, for every client
// (web popup, desktop, mobile) and both app modes (ADR-0057 decision 6). It is
// the only redirect URI the checklist prints.
const CallbackPath = "/api/v2/microsoft_login:callback"

// SecretExpiryWarningDays is how long before its entered expiry the portal warns
// that the client secret is about to lapse (decision 11).
const SecretExpiryWarningDays = 30

// Everything an Entra administrator needs to register the app, generated for this
// deployment. The portal renders the steps; the facts live here so the portal, the
// docs and the tests read one source.

// RedirectURI is the Web redirect URI for a deployment origin
// (https://host[:port], no trailing slash).
func RedirectURI(origin string) string {
	return strings.TrimRight(origin, "/") + CallbackPath
}

// RedirectURIIssue reports why Entra would refuse a redirect URI, or "" when it
// would accept it: Entra allows http only for localhost.
func RedirectURIIssue(redirectURI string) string {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Host == "" {
		return "invalid"
	}
	switch parsed.Scheme {
	case "https":
		return ""
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return ""
		}
	}
	return "https_required"
}

// ExpiryState is the secret-expiry banner state.
type ExpiryState string

const (
	ExpiryNone     ExpiryState = "none"
	ExpiryOK       ExpiryState = "ok"
	ExpiryExpiring ExpiryState = "expiring"
	ExpiryExpired  ExpiryState = "expired"
)

// SecretExpiry classifies a secret's entered expiry against now.
func SecretExpiry(expires *time.Time, now time.Time) (ExpiryState, int) {
	if expires == nil {
		return ExpiryNone, 0
	}
	days := int(expires.Sub(now).Hours() / 24)
	switch {
	case !expires.After(now):
		return ExpiryExpired, days
	case days <= SecretExpiryWarningDays:
		return ExpiryExpiring, days
	}
	return ExpiryOK, days
}

// Checklist is the generated Entra setup for one app.
type Checklist struct {
	RedirectURI string `json:"redirect_uri"`
	// RedirectIssue is "https_required" when the deployment origin is plain http
	// and not localhost; Entra would refuse the URI.
	RedirectIssue string `json:"redirect_issue,omitempty"`
	// AccountTypes is "single" for a tenant authority and "multi" for the
	// organizations and common aliases.
	AccountTypes string `json:"account_types"`
	// Permissions are the delegated permissions to add, per API.
	Permissions []Resource `json:"permissions"`
	// AdminConsentURL is what an Entra administrator opens to grant the
	// permissions that need consent. For a multi-tenant app it carries the
	// placeholder {tenant-id}: each customer tenant's administrator opens it
	// with their own tenant.
	AdminConsentURL string `json:"admin_consent_url"`
	// NeedsAdminConsent reports that at least one listed permission requires it.
	NeedsAdminConsent bool   `json:"needs_admin_consent"`
	SecretExpiry      string `json:"secret_expiry"`
	SecretDaysLeft    int    `json:"secret_days_left"`
}

// BuildChecklist generates the checklist of an app for a deployment origin.
func BuildChecklist(app App, origin string, now time.Time) Checklist {
	redirect := RedirectURI(origin)
	resources := app.RequiredPermissions()
	list := Checklist{
		RedirectURI: redirect, RedirectIssue: RedirectURIIssue(redirect),
		AccountTypes: "single", Permissions: resources,
	}
	tenant := ""
	if authority, err := ParseAuthority(app.Authority); err == nil {
		tenant = authority.Tenant
		if authority.MultiTenant() {
			list.AccountTypes, tenant = "multi", "{tenant-id}"
		}
	}
	for _, resource := range resources {
		for _, permission := range resource.Permissions {
			list.NeedsAdminConsent = list.NeedsAdminConsent || permission.AdminConsent
		}
	}
	if tenant != "" {
		query := url.Values{"client_id": {app.ClientID}, "redirect_uri": {redirect}}.Encode()
		list.AdminConsentURL = fmt.Sprintf("https://%s/%s/adminconsent?%s", EntraLoginHost, tenant, query)
	}
	state, days := SecretExpiry(app.SecretExpiresAt, now)
	list.SecretExpiry, list.SecretDaysLeft = string(state), days
	return list
}

// DeploymentChecklist is the setup that does not depend on an app yet: what to
// do in Entra before the first client id exists. Permissions are the defaults of
// every type.
type DeploymentChecklist struct {
	RedirectURI   string `json:"redirect_uri"`
	RedirectIssue string `json:"redirect_issue,omitempty"`
	// Types lists, per credential type, the permissions an app of that type
	// can be given: the offered list (Outlook, Teams), the default, and
	// whether Microsoft Graph free text applies.
	Types []TypePermissions `json:"types"`
}

// TypePermissions is one credential type's permission facts for the portal.
type TypePermissions struct {
	Type string `json:"type"`
	// Offered is the fixed list an administrator picks the ceiling from; empty
	// for the free-text types.
	Offered []Permission `json:"offered"`
	// Default is the ceiling when none is saved.
	Default []string `json:"default"`
	// API names the Entra API the permissions belong to.
	API string `json:"api"`
}

// BuildDeploymentChecklist is the checklist for a deployment origin.
func BuildDeploymentChecklist(origin string) DeploymentChecklist {
	redirect := RedirectURI(origin)
	out := DeploymentChecklist{RedirectURI: redirect, RedirectIssue: RedirectURIIssue(redirect)}
	for _, t := range CredentialTypes() {
		entry := TypePermissions{Type: t, Default: Defaults(t), API: "Microsoft Graph", Offered: []Permission{}}
		for _, name := range offered(t) {
			entry.Offered = append(entry.Offered, Permission{Name: name, AdminConsent: adminConsentPermissions[name]})
		}
		if t == TypeDataverse {
			entry.API = "Dynamics CRM"
			entry.Default = []string{"user_impersonation"}
		}
		out.Types = append(out.Types, entry)
	}
	return out
}

// Check is one line of a Test result.
type Check struct {
	// ID is a stable key the portal translates.
	ID string `json:"id"`
	OK bool   `json:"ok"`
	// Warn marks a check that passed with a warning (a secret about to lapse).
	Warn bool `json:"warn,omitempty"`
	// Detail is a short code or the Entra error code, never free text from Entra.
	Detail string `json:"detail,omitempty"`
}

// TestResult is the outcome of Test. It cannot prove consent: a delegated flow
// cannot be tested without a person, so the first sign-in does that and reports
// Entra's error code back to the administrator.
type TestResult struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// HTTPDoer is the client a Test reads Entra's metadata with. It must be an
// egress-guarded client (infra/egress.Guard.RoundTripper).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

const metadataLimit = 1 << 20

// Test checks what can be checked without a person: the client id is a GUID, the
// authority publishes Entra metadata on Entra's login host, the redirect URI is one
// Entra accepts and matches the deployment host, a secret is set and not expired.
// requestOrigin is the origin the administrator is browsing from; deployment is the
// configured public origin (may be empty).
func Test(ctx context.Context, app App, doer HTTPDoer, requestOrigin, deployment string, now time.Time) TestResult {
	result := TestResult{OK: true}
	add := func(check Check) {
		if !check.OK {
			result.OK = false
		}
		result.Checks = append(result.Checks, check)
	}

	_, err := NormalizeClientID(app.ClientID)
	add(Check{ID: "client_id", OK: err == nil})

	add(authorityCheck(ctx, app, doer))

	origin := deployment
	if origin == "" {
		origin = requestOrigin
	}
	redirect := RedirectURI(origin)
	switch {
	case RedirectURIIssue(redirect) != "":
		add(Check{ID: "redirect_uri", Detail: RedirectURIIssue(redirect)})
	case deployment != "" && requestOrigin != "" && !strings.EqualFold(deployment, requestOrigin):
		// The administrator reached the portal on a host that is not the
		// deployment's public host: the sign-in would redirect to the
		// configured one, and the registered URI must be that one.
		add(Check{ID: "redirect_uri", Detail: "host_mismatch"})
	default:
		add(Check{ID: "redirect_uri", OK: true})
	}

	add(Check{ID: "secret", OK: app.SecretSet, Detail: map[bool]string{true: "", false: "not_set"}[app.SecretSet]})
	if app.SecretSet {
		switch state, _ := SecretExpiry(app.SecretExpiresAt, now); state {
		case ExpiryExpired:
			add(Check{ID: "secret_expiry", Detail: "expired"})
		case ExpiryExpiring:
			add(Check{ID: "secret_expiry", OK: true, Warn: true, Detail: "expiring"})
		}
	}
	return result
}

func authorityCheck(ctx context.Context, app App, doer HTTPDoer) Check {
	authority, err := ParseAuthority(app.Authority)
	if err != nil {
		return Check{ID: "authority", Detail: "invalid"}
	}
	if doer == nil {
		return Check{ID: "authority", Detail: "unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authority.URL+"/v2.0/.well-known/openid-configuration", nil)
	if err != nil {
		return Check{ID: "authority", Detail: "invalid"}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return Check{ID: "authority", Detail: "unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 400 is how Entra answers an unknown tenant.
		return Check{ID: "authority", Detail: fmt.Sprintf("http_%d", resp.StatusCode)}
	}
	var metadata struct {
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, metadataLimit)).Decode(&metadata); err != nil ||
		!entraEndpoint(metadata.Authorization) || !entraEndpoint(metadata.Token) {
		return Check{ID: "authority", Detail: "not_entra_metadata"}
	}
	return Check{ID: "authority", OK: true}
}

// entraEndpoint reports an https endpoint on Entra's login host. A metadata
// document that points the token endpoint anywhere else would send the client
// secret there, so it is not Entra metadata.
func entraEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), EntraLoginHost) &&
		(parsed.Port() == "" || parsed.Port() == "443")
}
