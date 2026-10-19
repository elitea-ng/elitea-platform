package workspaceruntime

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The egress modes of a workspace run (ADR-0034 decision 9, "Host allowlist:
// the egress proxy"). The worker parses the mode strictly: a mode it does
// not know refuses the turn.
const (
	// EgressModeNone: the project's network is off. No interface but
	// loopback, no DNS.
	EgressModeNone = "none"
	// EgressModeDirect: the network is on with no host allowlist. The public
	// internet minus the deny ranges, DNS to the resolver only.
	EgressModeDirect = "direct"
	// EgressModeProxy: the network is on with a host allowlist. The sandbox
	// reaches only the egress proxy, with a run token that carries the
	// allowlist; no direct egress and no DNS.
	EgressModeProxy = "proxy"
	// EgressModeUnavailable: the project restricts the network to a host
	// allowlist, but this deployment cannot enforce one (no egress proxy, or
	// no run-token signing key). The worker refuses the turn with
	// workspace_run.egress_proxy_unavailable before any tool runs; the
	// network is never silently widened to the public internet.
	EgressModeUnavailable = "unavailable"
)

// Why allowlist mode is unavailable. Fixed strings: the worker names the
// reason in the turn's failure, and Main logs it.
const (
	EgressUnavailableProxyNotEnabled = "egress_proxy_not_enabled"
	EgressUnavailableNoSigningKey    = "egress_token_signing_key_not_configured"
	EgressUnavailableMintFailed      = "egress_token_mint_failed"
)

// RuntimeContextEgress is the `egress` object of the `workspace` field. Every
// key is always present (empty when the mode has no value for it). Token is a
// credential: it is never logged, and the worker hands it to the sandbox
// only inside the proxy URL of the command environment.
type RuntimeContextEgress struct {
	Mode string `json:"mode"`
	// ProxyURL is the proxy without credentials, `http://host:port`; the
	// worker adds `run:<token>@`. Empty unless Mode is proxy.
	ProxyURL string `json:"proxy_url"`
	Token    string `json:"token"`
	// Reason is one of the EgressUnavailable* values when Mode is
	// unavailable, and empty otherwise.
	Reason string `json:"reason"`
}

// String never prints the token, so formatting the context cannot leak it.
func (e RuntimeContextEgress) String() string {
	token := ""
	if e.Token != "" {
		token = "[REDACTED]"
	}
	return fmt.Sprintf("{mode:%s proxy_url:%s token:%s reason:%s}", e.Mode, e.ProxyURL, token, e.Reason)
}

// GoString is String, for %#v.
func (e RuntimeContextEgress) GoString() string { return e.String() }

// EgressTokenMinter signs a run token (egresstoken.Minter).
type EgressTokenMinter interface {
	Mint(executionID string, projectID int64, allowlist []string, ttl time.Duration) (string, error)
}

// Egress is how the deployment enforces a host allowlist. The zero value is
// "no egress proxy": an allowlist turn then gets mode unavailable.
type Egress struct {
	// ProxyURL is the egress proxy as the sandbox reaches it,
	// `http://host:port` (ELITEA_EGRESS_PROXY_URL). Empty: the proxy is not
	// enabled on this deployment.
	ProxyURL string
	// Minter signs run tokens; nil when no signing key is configured.
	Minter EgressTokenMinter
	// DefaultTurnWall is the token lifetime for a policy that leaves the
	// turn's wall time to the worker's deployment default (zero
	// TurnWallSeconds). Zero means DefaultEgressTurnWall.
	DefaultTurnWall time.Duration
}

// EgressTokenGrace is added to the turn's wall time. The token is minted at
// claim time, and the worker starts the turn's wall clock only after it has
// fetched and hydrated the workspace, so a token of exactly the wall time
// would expire before the turn does. The grace equals the supervisor's
// sweep grace (sandbox::workspace_run::SWEEP_GRACE_SECONDS): the sandbox
// cannot outlive it either.
const EgressTokenGrace = 2 * time.Minute

// DefaultEgressTurnWall is the token lifetime base when neither the policy
// nor the configuration names a turn wall time: the worker's default turn
// wall time (config.rs WorkspaceRunsConfig, 3600 s).
const DefaultEgressTurnWall = time.Hour

// maxEgressTTL is the verifier's longest token (runtoken.MaxLifetime).
const maxEgressTTL = 24 * time.Hour

// ErrEgressMisconfigured reports an Egress that cannot serve proxy mode as
// configured. The composition root refuses to start with it.
var ErrEgressMisconfigured = errors.New("workspaceruntime: the egress proxy configuration is invalid")

// Validate checks the static configuration. A zero Egress is valid.
func (e Egress) Validate() error {
	if e.DefaultTurnWall < 0 || e.DefaultTurnWall > maxEgressTTL {
		return errors.Join(ErrEgressMisconfigured, errors.New("the default turn wall time is outside 0..24h"))
	}
	if e.ProxyURL == "" {
		return nil
	}
	if err := ValidateProxyURL(e.ProxyURL); err != nil {
		return errors.Join(ErrEgressMisconfigured, err)
	}
	return nil
}

// ValidateProxyURL accepts exactly `http://host:port`: the worker adds the
// credentials, so the URL carries none, and it has no path, query or
// fragment.
func ValidateProxyURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("the proxy URL does not parse")
	}
	if parsed.Scheme != "http" {
		return errors.New("the proxy URL scheme must be http (the sandbox tunnels TLS through CONNECT)")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.Opaque != "" {
		return errors.New("the proxy URL must be http://host:port, without credentials, path or query")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" {
		return errors.New("the proxy URL must name a host and a port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("the proxy URL port is invalid")
	}
	if strings.ContainsAny(host, " \t\r\n@/\\") {
		return errors.New("the proxy URL host is invalid")
	}
	return nil
}

// canonicalProxyURL is the URL as served: `http://host:port`.
func canonicalProxyURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return "http://" + parsed.Host
}

// egressFor decides the egress of one turn. It returns a token only in
// proxy mode, and never widens an allowlist to direct egress. The second
// result is the mint failure, for the caller's log (it names no token
// material).
func (e Egress) egressFor(claim TurnClaim, policy Policy, allowlist []string) (RuntimeContextEgress, error) {
	switch {
	case !policy.Network:
		return RuntimeContextEgress{Mode: EgressModeNone}, nil
	case len(allowlist) == 0:
		return RuntimeContextEgress{Mode: EgressModeDirect}, nil
	case e.ProxyURL == "":
		return RuntimeContextEgress{Mode: EgressModeUnavailable, Reason: EgressUnavailableProxyNotEnabled}, nil
	case e.Minter == nil:
		return RuntimeContextEgress{Mode: EgressModeUnavailable, Reason: EgressUnavailableNoSigningKey}, nil
	}
	token, err := e.Minter.Mint(claim.ExecutionID, claim.ProjectID, allowlist, e.tokenTTL(policy))
	if err != nil {
		return RuntimeContextEgress{Mode: EgressModeUnavailable, Reason: EgressUnavailableMintFailed}, err
	}
	return RuntimeContextEgress{Mode: EgressModeProxy, ProxyURL: canonicalProxyURL(e.ProxyURL), Token: token}, nil
}

// tokenTTL is the turn's wall time plus EgressTokenGrace, at most 24 h.
func (e Egress) tokenTTL(policy Policy) time.Duration {
	wall := e.DefaultTurnWall
	if wall <= 0 {
		wall = DefaultEgressTurnWall
	}
	if policy.TurnWallSeconds > 0 && policy.TurnWallSeconds < uint64(maxEgressTTL/time.Second) {
		wall = time.Duration(policy.TurnWallSeconds) * time.Second
	}
	return min(wall+EgressTokenGrace, maxEgressTTL)
}
