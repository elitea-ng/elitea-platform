package microsoftauth

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// EntraHost is the only identity-provider origin the new Microsoft types accept
// (ADR-0057 decision 10): https, port 443. Sovereign login hosts
// (login.microsoftonline.us, login.partner.microsoftonline.cn) are refused.
const EntraHost = "https://login.microsoftonline.com"

const entraHostname = "login.microsoftonline.com"

// tenantPattern is a tenant GUID, a verified domain, or one of the alias
// tenants (common, organizations, consumers).
var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,99}$`)

// NormalizeEntraAuthority accepts https://login.microsoftonline.com/<tenant>
// (optionally with a trailing slash or /v2.0) and nothing else: not another
// host, not http, not a port, not user info, query or fragment, not a longer
// path. The client secret and a stored login are posted to this authority's
// token endpoint, so an authority a user could point elsewhere would hand them
// to that host. It returns the authority without a trailing slash.
func NormalizeEntraAuthority(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		!strings.EqualFold(parsed.Hostname(), entraHostname) ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return "", ErrNotEntraAuthority
	}
	segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/"), "/")
	if len(segments) == 0 || len(segments) > 2 || !tenantPattern.MatchString(segments[0]) ||
		(len(segments) == 2 && segments[1] != "v2.0") {
		return "", ErrNotEntraAuthority
	}
	return EntraHost + "/" + strings.Join(segments, "/"), nil
}

// EntraOnly reports whether this type accepts an Entra authority only.
func (d Descriptor) EntraOnly() bool { return d.entraOnly }

// TokenEndpoint is the token endpoint of an authority that passed
// NormalizeEntraAuthority: {authority}/oauth2/v2.0/token. It is always computed,
// never read from a discovery document a user could influence.
func TokenEndpoint(authority string) string {
	return strings.TrimSuffix(strings.TrimRight(authority, "/"), "/v2.0") + "/oauth2/v2.0/token"
}

// RequireEntraEndpoint refuses an endpoint that is not on login.microsoftonline.com
// over https (port 443).
func RequireEntraEndpoint(value any) error {
	raw, _ := value.(string)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		!strings.EqualFold(parsed.Hostname(), entraHostname) ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return ErrNotEntraAuthority
	}
	return nil
}

// DataverseError is a refusal of a Dataverse environment URL.
type DataverseError string

func (e DataverseError) Error() string { return string(e) }

// ErrDataverseURL is the one refusal for an environment URL that is not an
// https://<org>.<region>.dynamics.com origin.
const ErrDataverseURL DataverseError = "the Dataverse URL must look like https://<org>.crm.dynamics.com"

// NormalizeDataverseURL validates a Dataverse environment URL and returns its
// origin. Pinned to https, port 443, no path, no user info, no query, and a host
// that ends in .dynamics.com with at least one label before it. Sovereign
// clouds (.dynamics.cn, .microsoftdynamics.us) are refused, as upstream does;
// adding them is a later decision with its own ADR.
func NormalizeDataverseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", ErrDataverseURL
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") ||
		!strings.HasSuffix(host, ".dynamics.com") || len(host) == len(".dynamics.com") ||
		strings.Contains(host, "..") || strings.HasPrefix(host, ".") {
		return "", ErrDataverseURL
	}
	return "https://" + host, nil
}

// ProbeResult classifies one identity read.
type ProbeResult int

const (
	// ProbeOK: the token is accepted.
	ProbeOK ProbeResult = iota
	// ProbeUnauthorized: the token is missing, invalid or expired; sign in again.
	ProbeUnauthorized
	// ProbeForbidden: the token is valid but lacks a permission.
	ProbeForbidden
	// ProbeFailed: throttling, a server error or anything else; says nothing
	// about the token.
	ProbeFailed
)

// ClassifyProbe maps the HTTP status of an identity read to a ProbeResult.
func ClassifyProbe(status int) ProbeResult {
	switch {
	case status == http.StatusOK:
		return ProbeOK
	case status == http.StatusUnauthorized:
		return ProbeUnauthorized
	case status == http.StatusForbidden:
		return ProbeForbidden
	default:
		return ProbeFailed
	}
}

// ProbeRequest builds the identity read for a credential and a token: Graph
// GET /v1.0/me, or Dataverse WhoAmI on the credential's own validated org. It
// reads who the token is, never content. ProbeNone descriptors have none.
func (d Descriptor) ProbeRequest(ctx context.Context, data map[string]any, token string) (*http.Request, error) {
	var target string
	switch d.probe {
	case ProbeGraphMe:
		target = "https://" + GraphHost + "/v1.0/me?$select=id"
	case ProbeWhoAmI:
		org, err := NormalizeDataverseURL(text(data, FieldURL))
		if err != nil {
			return nil, err
		}
		target = org + "/api/data/v9.2/WhoAmI"
	default:
		return nil, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if d.probe == ProbeWhoAmI {
		request.Header.Set("OData-MaxVersion", "4.0")
		request.Header.Set("OData-Version", "4.0")
	}
	return request, nil
}
