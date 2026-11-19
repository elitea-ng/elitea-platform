package microsoftauth

import (
	"context"
	"errors"
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
	switch status {
	case http.StatusOK:
		return ProbeOK
	case http.StatusUnauthorized:
		return ProbeUnauthorized
	case http.StatusForbidden:
		return ProbeForbidden
	default:
		return ProbeFailed
	}
}

// ProbeRequest builds the check read for a credential and a token, exactly as
// the SDK's own checks make it: Outlook reads one mail folder
// (/me/mailFolders?$top=1, or the shared mailbox's), Teams the joined teams
// (/me/joinedTeams?$top=1), Microsoft Graph /me, Dataverse WhoAmI on the
// credential's own validated org. The read proves the token AND the permission
// the toolkit needs; it asks for one item at most and nothing of it is kept.
// ProbeNone descriptors have none.
func (d Descriptor) ProbeRequest(ctx context.Context, data map[string]any, token string) (*http.Request, error) {
	var target string
	switch d.probe {
	case ProbeGraphMe:
		path, err := d.graphProbePath(data)
		if err != nil {
			return nil, err
		}
		target = "https://" + GraphHost + path
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

// mailboxPattern is what a shared mailbox address may look like. The value goes
// into a Graph URL path, so nothing that could change the path is accepted.
var mailboxPattern = regexp.MustCompile(`^[A-Za-z0-9._%+'-]+@[A-Za-z0-9.-]+$`)

// ErrMailbox refuses a mailbox that is not an address.
var ErrMailbox = errors.New("the mailbox must be an email address")

func (d Descriptor) graphProbePath(data map[string]any) (string, error) {
	switch d.configType {
	case Outlook:
		mailbox := text(data, FieldMailbox)
		if mailbox == "" {
			return "/v1.0/me/mailFolders?$top=1", nil
		}
		if !mailboxPattern.MatchString(mailbox) {
			return "", ErrMailbox
		}
		return "/v1.0/users/" + url.PathEscape(mailbox) + "/mailFolders?$top=1", nil
	case Teams:
		return "/v1.0/me/joinedTeams?$top=1", nil
	default:
		return "/v1.0/me?$select=id,displayName", nil
	}
}

// ForbiddenMessage is what a 403 on the check read means: the token is good and
// lacks the permission the toolkit needs, and the message names it (the SDK's
// own wording).
func (d Descriptor) ForbiddenMessage(data map[string]any) string {
	switch d.configType {
	case Outlook:
		if mailbox := text(data, FieldMailbox); mailbox != "" {
			return "Access to mailbox '" + mailbox + "' is forbidden - you need Full Access to it " +
				"and the Mail.Read.Shared scope. Tick it in the credential scopes and sign in again."
		}
		return "Access forbidden - the token lacks the Microsoft Graph permission Mail.Read. " +
			"Tick it in the credential scopes and sign in again."
	case Teams:
		return "Access forbidden - the token lacks the Microsoft Graph permission Team.ReadBasic.All. " +
			"Tick it in the credential scopes and sign in again."
	case MSGraph:
		return "Access forbidden - the token lacks the Microsoft Graph permission User.Read. " +
			"Add it to the credential scopes and sign in again."
	case Dataverse:
		return "Your Dataverse user has no permission for this environment."
	default:
		return "The signed-in account was refused."
	}
}

// NotFoundMessage is what a 404 on the check read means, where it means more than
// a failure (a mailbox that is not one the person can open); empty otherwise.
func (d Descriptor) NotFoundMessage(data map[string]any) string {
	if mailbox := text(data, FieldMailbox); d.configType == Outlook && mailbox != "" {
		return "'" + mailbox + "' is not a mailbox you can open. A distribution list or group has no " +
			"mailbox of its own (its mail arrives in your inbox): leave the mailbox field empty."
	}
	return ""
}
