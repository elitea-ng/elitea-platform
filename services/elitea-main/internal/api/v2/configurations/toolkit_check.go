// toolkit_check.go implements the connection check for TOOLKIT credentials —
// the half of #319 that check_connection.go deliberately left out.
//
// # Why this file exists beside check_connection.go
//
// check_connection.go checks the eight `ai_credentials` provider types by
// delegating to the LLM gateway, which owns the SSRF-safe egress allowlist for
// a tenant-authored api_base. Every OTHER known type — github, gitlab,
// bitbucket, jira, confluence — answered the honest "Checking connection is
// not supported yet for configuration type github", because the gateway speaks
// to LLM providers and nothing else.
//
// The consequence was user visible and is recorded in the live lane's own
// header (apps/elitea-web/e2e/live/toolkits.indicators.spec.ts): the credential
// card's attention indicator could not light for ANY toolkit credential, right
// or wrong, so a token the provider refuses looked exactly like a token that
// works until the first tool run failed.
//
// # What a check is here, and what it is NOT
//
// ONE authenticated metadata GET per family, with a 5 s budget, against the
// endpoint the stored credential names. It reads the identity the credential
// belongs to and nothing else: no repository, no issue, no page. It writes
// nothing, anywhere — the same rule stored_check.go states at length for the
// LLM path, and for the same reason (status_ok records ADMISSION, not a
// provider verdict).
//
// # The four outcomes
//
// A check answers exactly one of:
//
//	ok               2xx — the provider recognised the credential
//	auth_failed      401/403 — the provider refused it
//	unreachable      no answer, a timeout, or an answer nobody can read
//	unsupported_type this build cannot check this type, or this auth method
//
// The MESSAGE for each is a fixed constant. No provider response body, no
// error text, and no part of the credential ever reaches it: a "test
// connection" control that echoes the upstream body is a way to read a secret
// back out of a project you can only write to.
//
// # Egress
//
// The base URL is tenant-authored (#13), so it is checked against an allowlist
// BEFORE the dial, with the same matching rules the DeepWiki and Inventory
// facades use (internal/providerhost/material.GitEgressPolicy — hostnames,
// case-insensitive, `*.` for direct subdomains only, a bare `*` to disable the
// control out loud). ELITEA_TOOLKIT_CHECK_ALLOWLIST replaces the default list;
// unset, the default is the five vendors' PUBLIC API hosts, so a SaaS
// deployment works out of the box and a self-hosted GitLab or Jira has to be
// named. An empty allowlist here does NOT mean "refuse everything" the way
// DeepWiki's does, because unlike a clone this call reaches a host the platform
// itself already ships as a toolkit default.
package configurations

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// ToolkitCheckAllowlistEnv names the variable that replaces the built-in host
// list. Spelled out as a literal because the env-drift gate and the chart are
// both searched by the string.
const ToolkitCheckAllowlistEnv = "ELITEA_TOOLKIT_CHECK_ALLOWLIST"

// defaultToolkitCheckAllowlist is the five vendors' public API hosts.
//
// It is a DEFAULT, not a floor: setting the variable replaces it entirely, so a
// deployment that must not reach github.com can say so. `*.atlassian.net`
// covers a Jira/Confluence Cloud tenant, which is a direct subdomain by
// construction.
const defaultToolkitCheckAllowlist = "api.github.com, github.com, gitlab.com, api.bitbucket.org, bitbucket.org, *.atlassian.net"

// toolkitCheckTimeout bounds ONE probe. It is short on purpose: the credential
// card checks a whole project's rows at once through the batch route, whose own
// budget (batchConnectionCheckBudget) has to cover all of them.
const toolkitCheckTimeout = 5 * time.Second

// The reason vocabulary. These four strings are the contract this file
// publishes; the handler puts them on the wire beside `success`.
const (
	ToolkitCheckReasonOK              = "ok"
	ToolkitCheckReasonAuthFailed      = "auth_failed"
	ToolkitCheckReasonUnreachable     = "unreachable"
	ToolkitCheckReasonUnsupportedType = "unsupported_type"
)

// The user-facing message for each reason. Fixed constants: see the package
// doc's "what a check is NOT".
const (
	toolkitCheckOKMessage          = "Connection successful"
	toolkitCheckAuthFailedMessage  = "Authentication failed. The provider rejected this credential."
	toolkitCheckUnreachableMessage = "Could not reach the provider with this credential."
	toolkitCheckEgressMessage      = "This provider endpoint is not permitted by the platform's configuration."
)

// ToolkitCheckOutcome is one probe's verdict.
//
// Reason is always one of the four constants above, so a caller switches on a
// closed set rather than parsing Message — which exists for the browser and may
// be reworded.
type ToolkitCheckOutcome struct {
	Reason  string
	Message string
}

// Success reports the one reason that means the provider recognised the
// credential.
func (o ToolkitCheckOutcome) Success() bool { return o.Reason == ToolkitCheckReasonOK }

// ToolkitConnectionChecker probes ONE toolkit credential.
//
// Implementations MUST NOT report ok without an actual round trip, and MUST NOT
// put provider text or credential material into Message.
type ToolkitConnectionChecker interface {
	CheckToolkit(ctx context.Context, configType string, data map[string]any) ToolkitCheckOutcome
}

// WithToolkitConnectionChecker replaces the checker built from the environment.
// It exists for tests, which point it at an httptest provider; production
// composition uses the default.
func WithToolkitConnectionChecker(checker ToolkitConnectionChecker) Option {
	return func(handler *Handler) { handler.toolkitChecker = checker }
}

// toolkitProbe describes one family's metadata GET.
//
// path is joined onto the credential's own base URL. authorize turns the stored
// data into the request headers; it returns false when the credential carries
// an auth method this probe cannot use (a GitHub App private key, say), which
// is reported as unsupported_type rather than as a refusal — the credential may
// be perfectly good.
type toolkitProbe struct {
	// baseURLFields are read in order; the first non-empty one wins.
	baseURLFields []string
	// defaultBaseURL is the vendor's public API host, used when the credential
	// names none. It matches the SDK toolkit's own prefill, so the allowlist
	// sees the host a real call would reach rather than an empty string.
	defaultBaseURL string
	// path is appended to the base URL. It reads an identity, never content.
	path      func(data map[string]any) string
	authorize func(header http.Header, data map[string]any) bool
}

// toolkitCheckProbes is the set of types this build can check.
//
// A type that is NOT here answers unsupported_type, which is the same honest
// answer connectionCheckNotSupportedMessage already gave. Adding a family means
// adding a probe that reads an IDENTITY: a call that lists repositories or
// issues would turn a scope problem into "your credential is broken".
var toolkitCheckProbes = map[string]toolkitProbe{
	"github": {
		baseURLFields:  []string{"base_url"},
		defaultBaseURL: "https://api.github.com",
		path:           func(map[string]any) string { return "/user" },
		authorize:      authorizeGitHub,
	},
	"gitlab": {
		baseURLFields:  []string{"url", "base_url"},
		defaultBaseURL: "https://gitlab.com",
		path:           func(map[string]any) string { return "/api/v4/user" },
		authorize:      authorizeGitLab,
	},
	"bitbucket": {
		baseURLFields:  []string{"url", "base_url"},
		defaultBaseURL: "https://api.bitbucket.org",
		path:           func(map[string]any) string { return "/2.0/user" },
		authorize:      authorizeBasicOrBearer("username", "password", "token", "api_key"),
	},
	"jira": {
		baseURLFields:  []string{"base_url", "url"},
		defaultBaseURL: "",
		path:           jiraMyselfPath,
		authorize:      authorizeBasicOrBearer("username", "api_key", "token", "api_key"),
	},
	"confluence": {
		baseURLFields:  []string{"base_url", "url"},
		defaultBaseURL: "",
		path:           func(map[string]any) string { return "/rest/api/user/current" },
		authorize:      authorizeBasicOrBearer("username", "api_key", "token", "api_key"),
	},
}

// IsToolkitCheckableType reports whether this build carries a probe for the
// type. The handlers ask before they refuse, so a toolkit type keeps the
// "not supported yet" answer it always had rather than becoming an error.
func IsToolkitCheckableType(configType string) bool {
	_, ok := toolkitCheckProbes[configType]
	return ok
}

// jiraMyselfPath honours the credential's own api_version, because a Jira
// Server deployment pinned to v2 answers 404 on the v3 path and a 404 is not a
// credential verdict.
func jiraMyselfPath(data map[string]any) string {
	version := strVal(data, "api_version")
	if version != "2" && version != "3" {
		version = "2"
	}
	return "/rest/api/" + version + "/myself"
}

// authorizeGitHub accepts a token or a username/password pair. A GitHub App
// (app_id + app_private_key) needs a signed JWT exchange, which is a different
// call with a different failure vocabulary — reported as unsupported rather
// than probed badly.
func authorizeGitHub(header http.Header, data map[string]any) bool {
	if token := firstStrVal(data, "access_token", "token", "api_key"); token != "" {
		header.Set("Authorization", "Bearer "+token)
		return true
	}
	return setBasicAuth(header, strVal(data, "username"), strVal(data, "password"))
}

// authorizeGitLab uses the header GitLab documents for a personal access
// token. A Bearer header works for an OAuth token only, and the field this
// platform stores is the PAT.
func authorizeGitLab(header http.Header, data map[string]any) bool {
	if token := firstStrVal(data, "private_token", "token", "api_key", "access_token"); token != "" {
		header.Set("PRIVATE-TOKEN", token)
		return true
	}
	return false
}

// authorizeBasicOrBearer builds the Atlassian/Bitbucket shape: a user name plus
// a token is HTTP basic (Cloud), a token alone is a bearer (Server/DC PAT).
func authorizeBasicOrBearer(userField, secretField string, bearerFields ...string) func(http.Header, map[string]any) bool {
	return func(header http.Header, data map[string]any) bool {
		user := strVal(data, userField)
		secret := strVal(data, secretField)
		if user != "" && secret != "" {
			return setBasicAuth(header, user, secret)
		}
		if token := firstStrVal(data, bearerFields...); token != "" {
			header.Set("Authorization", "Bearer "+token)
			return true
		}
		return false
	}
}

func setBasicAuth(header http.Header, user, secret string) bool {
	if user == "" || secret == "" {
		return false
	}
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+secret)))
	return true
}

// httpToolkitConnectionChecker is the production checker: an allowlist, a
// bounded client, and one request.
type httpToolkitConnectionChecker struct {
	client *http.Client
	policy material.GitEgressPolicy
}

// NewToolkitConnectionCheckerFromEnv builds the checker every deployment gets.
//
// It never returns nil: a checker with an empty allowlist still answers, and
// what it answers is the egress refusal, which names the variable an operator
// has to set. A nil checker would instead read as "this type cannot be
// checked", which is a different and wrong statement.
func NewToolkitConnectionCheckerFromEnv() ToolkitConnectionChecker {
	raw := strings.TrimSpace(os.Getenv(ToolkitCheckAllowlistEnv))
	if raw == "" {
		raw = defaultToolkitCheckAllowlist
	}
	return NewToolkitConnectionChecker(material.ParseGitEgress(raw, ToolkitCheckAllowlistEnv), nil)
}

// NewToolkitConnectionChecker builds a checker over an explicit policy and an
// optional transport (tests supply the httptest server's).
func NewToolkitConnectionChecker(policy material.GitEgressPolicy, transport http.RoundTripper) ToolkitConnectionChecker {
	return &httpToolkitConnectionChecker{
		client: &http.Client{
			Transport: transport,
			Timeout:   toolkitCheckTimeout,
			// A credential check follows no redirects. A provider that answers
			// 30x to an authenticated identity read is answering something this
			// probe cannot interpret, and following it would replay the
			// Authorization header at a host the allowlist never saw.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		policy: policy,
	}
}

// CheckToolkit performs the probe. It never returns an error: every failure is
// one of the four reasons, because every one of them is an answer the browser
// has to render.
func (c *httpToolkitConnectionChecker) CheckToolkit(ctx context.Context, configType string, data map[string]any) ToolkitCheckOutcome {
	probe, known := toolkitCheckProbes[configType]
	if !known {
		return ToolkitCheckOutcome{
			Reason:  ToolkitCheckReasonUnsupportedType,
			Message: connectionCheckNotSupportedMessage(configType),
		}
	}
	if data == nil {
		data = map[string]any{}
	}

	target, ok := c.resolveTarget(probe, data)
	if !ok {
		return ToolkitCheckOutcome{
			Reason:  ToolkitCheckReasonUnsupportedType,
			Message: "This credential does not name an endpoint to check.",
		}
	}
	if err := c.policy.Allow(target.Hostname()); err != nil {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckEgressMessage}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckUnreachableMessage}
	}
	request.Header.Set("Accept", "application/json")
	if !probe.authorize(request.Header, data) {
		// The credential is not necessarily wrong — this build simply has no
		// probe for the authentication method it carries.
		return ToolkitCheckOutcome{
			Reason:  ToolkitCheckReasonUnsupportedType,
			Message: "This credential uses an authentication method this platform cannot test yet.",
		}
	}

	response, err := c.client.Do(request)
	if err != nil {
		// The cause is deliberately dropped rather than logged with the
		// request: a transport error's text carries the full URL, which is
		// tenant data, and this function is called once per row of a list.
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckUnreachableMessage}
	}
	defer func() { _ = response.Body.Close() }()

	return outcomeForStatus(response.StatusCode)
}

// resolveTarget joins the credential's base URL and the probe's path.
//
// A base URL with a path of its own is kept (a Confluence Cloud base ends in
// /wiki), which is why the join is a string concatenation of trimmed parts
// rather than url.Parse of the path alone.
func (c *httpToolkitConnectionChecker) resolveTarget(probe toolkitProbe, data map[string]any) (*url.URL, bool) {
	base := strings.TrimSpace(firstStrVal(data, probe.baseURLFields...))
	if base == "" {
		base = probe.defaultBaseURL
	}
	if base == "" {
		return nil, false
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/") + probe.path(data))
	if err != nil || parsed.Host == "" {
		return nil, false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// A stored `file:` or `gopher:` base URL is tenant-authored input, and
		// a scheme nobody expected is not something to dial and find out.
		return nil, false
	}
	// A credential must not smuggle its own auth past the header this probe
	// sets, and a fragment or query on an identity read means nothing.
	parsed.User = nil
	parsed.Fragment = ""
	return parsed, true
}

// outcomeForStatus maps one HTTP status onto the closed reason vocabulary.
//
// 401 AND 403 are both auth_failed. A 403 from these five providers is a token
// that authenticated and may not do this, which for an identity read is the
// same operational answer as a token that did not authenticate: the credential
// as stored cannot be used. Splitting them would need a fifth reason nothing
// renders differently.
func outcomeForStatus(status int) ToolkitCheckOutcome {
	switch {
	case status >= 200 && status < 300:
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonOK, Message: toolkitCheckOKMessage}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonAuthFailed, Message: toolkitCheckAuthFailedMessage}
	default:
		return ToolkitCheckOutcome{
			Reason: ToolkitCheckReasonUnreachable,
			Message: fmt.Sprintf(
				"Could not verify the credential: the provider answered %d.", status),
		}
	}
}
