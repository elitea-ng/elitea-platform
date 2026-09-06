package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// checkconnection.go implements POST /llm/v1/check_connection (#319): a real,
// minimal round trip to the named provider using a credential the caller has
// NOT yet saved (elitea-main's "Test connection" button tests a payload
// before Create/Update persists it, restoring the save-time feedback legacy's
// LiteLLM health/test_connection gave — 071774fb). Because the credential
// under test has no p_{projectID}.configuration row, it cannot be resolved
// through account.EliteaAccount.GetKeysForProvider, and there is no saved
// "model" field to complete against, so this endpoint deliberately does not
// go through bifrost/core at all: it makes its own minimal, read-only,
// provider-native request (list models / list deployments / list local
// models) directly, over a purpose-built SSRF-safe client that mirrors
// bifrost's own protections (account/egress.go) rather than inheriting them.
//
// The wire contract here is INTERNAL — elitea-main is the only caller, and it
// translates checkConnectionResponse into the browser-facing, legacy-parity
// {"success":...,"message":...} shape its own handler already promises.
// Nothing in this contract or its logs ever carries the raw provider
// response body: only a small, fixed reason vocabulary crosses the wire, and
// full detail (status code, error text) is logged server-side only.

// EgressPolicy is the subset of *account.EliteaAccount CheckConnection needs.
// It is a narrow interface, defined here rather than imported from the
// account package, so llmproxy does not gain a dependency on account's
// Postgres/vault plumbing merely to reuse its allowlist decision.
type EgressPolicy interface {
	// EgressAllows reports whether apiBase may be dialled under the
	// operator's GATEWAY_EGRESS_ALLOWLIST — the identical decision
	// GetKeysForProvider applies to every persisted credential (issue #13).
	EgressAllows(apiBase string) bool
	// EgressPrivateNetworkAllowed reports whether the merged allowlist
	// EXPLICITLY names a private host or CIDR. It is the SAME question, read
	// from the SAME gate, that account.GetConfigForProvider asks before it
	// sets NetworkConfig.AllowPrivateNetwork for the self-hosted provider
	// classes.
	//
	// It used to be "is an allowlist armed at all"
	// (account.EgressAllowlistConfigured). The egress-governance change made
	// those two different questions: a name-only entry arms the allowlist and
	// grants NO private reachability, because a name cannot declare where it
	// resolves (egresslib, "Why a NAME allowlist"). The probe then reported
	// success for a destination every chat turn refused in the dialer, and it
	// wrote status_ok for it. One predicate, one gate instance, both
	// directions: the probe must never pass a destination the request path
	// refuses, and it must never refuse one the request path dials.
	//
	// The gate instance matters as much as the predicate. The account merges
	// the environment floor with the authored `egress_allowlist` governance
	// rows, so a decision taken from the environment alone would drift from
	// the dialer the moment an admin authors a row.
	EgressPrivateNetworkAllowed() bool
}

// checkConnectionProbeTimeout bounds the single provider round trip a check
// performs, end to end (DNS, dial, TLS, response). The UI's "Test connection"
// button is a synchronous click; an unreachable host must fail promptly
// rather than hang the request.
const checkConnectionProbeTimeout = 8 * time.Second

// checkConnectionMaxBody bounds the request body: a handful of short string
// fields, never a bulk payload.
const checkConnectionMaxBody = 8 << 10 // 8 KiB

// checkConnectionRequest is the elitea-main → gateway wire contract. Type
// selects the probe (see checkConnectionProviders); the remaining fields are
// the raw, already-unsecreted credential values elitea-main read from the
// user's not-yet-saved form payload.
type checkConnectionRequest struct {
	Type       string `json:"type"`
	APIBase    string `json:"api_base"`
	APIKey     string `json:"api_key"`
	APIVersion string `json:"api_version"`

	// amazon_bedrock carries no api_base at all: its endpoint is DERIVED from
	// the region, exactly as account/credentials.go's schemas.BedrockKeyConfig
	// is built from these same three fields (issue #454).
	AWSAccessKeyID     string `json:"aws_access_key_id"`
	AWSSecretAccessKey string `json:"aws_secret_access_key"`
	AWSSessionToken    string `json:"aws_session_token"`
	AWSRegionName      string `json:"aws_region_name"`

	// vertex_ai likewise derives its endpoint from the location, and
	// authenticates with the Google service-account document rather than an
	// api_key — the same three fields schemas.VertexKeyConfig is built from
	// (issue #453). VertexCredentials is a jsonTextField because the platform
	// stores that document as an escaped JSON STRING on one screen and as a
	// nested JSON OBJECT on another (account/credentials.go's jsonText).
	VertexProject     string        `json:"vertex_project"`
	VertexLocation    string        `json:"vertex_location"`
	VertexCredentials jsonTextField `json:"vertex_credentials"`
}

// jsonTextField decodes a field that arrives either as a JSON string or as a
// nested JSON object, yielding the document text either way. It mirrors
// account/credentials.go's jsonText: a plain `string` field makes the object
// form fail to decode, and a failed decode would refuse a credential that is
// in fact perfectly valid.
type jsonTextField string

func (t *jsonTextField) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*t = ""
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		*t = jsonTextField(s)
		return nil
	}
	*t = jsonTextField(raw)
	return nil
}

// checkConnectionResponse is the gateway → elitea-main wire contract. Reason
// is a small, fixed vocabulary (checkConnectionReason* below); Detail adds a
// human-readable but still provider-body-free hint. Success is true only when
// the probe actually round-tripped to the provider and got back a successful
// response — there is no code path that reports Success without calling the
// probe.
type checkConnectionResponse struct {
	Success bool   `json:"success"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Reason vocabulary for checkConnectionResponse. elitea-main maps each of
// these to its own user-facing message; the gateway itself never emits raw
// provider response text over this contract.
const (
	checkConnectionReasonOK          = "ok"
	checkConnectionReasonUnsupported = "unsupported_type"
	checkConnectionReasonMissingBase = "missing_api_base"
	// checkConnectionReasonMissingFields covers a credential class whose
	// endpoint is NOT an api_base (amazon_bedrock, vertex_ai): the field that
	// is missing or malformed is named in Detail instead, because
	// elitea-main's message table keys off "missing_api_base" with an
	// api_base-specific sentence that would be wrong here. An unmapped reason
	// falls through to Detail in elitea-main's connectionCheckMessageFor, so
	// the user sees the accurate field list — and Detail never carries a
	// VALUE, only a field name.
	checkConnectionReasonMissingFields = "missing_credential_fields"
	checkConnectionReasonEgress        = "egress_not_allowed"
	checkConnectionReasonUnauth        = "unauthorized"
	checkConnectionReasonUnreachable   = "unreachable"
	checkConnectionReasonUpstream      = "upstream_error"
)

// defaultAzureAPIVersion is used when the caller supplies no api_version for
// an Azure-shaped credential (azure_open_ai, ai_dial — both map to
// schemas.Azure in account/credentials.go's providerConfigTypes). It is a
// long-stable, generally-available Azure OpenAI API version whose sole use
// here is listing deployments, not completions, so newer API surface changes
// do not affect it.
const defaultAzureAPIVersion = "2023-05-15"

// checkConnectionProvider describes one credential type's minimal, real probe.
type checkConnectionProvider struct {
	// dialTargets returns EVERY base URL this provider's probe will contact,
	// so CheckConnection can put all of them through the operator's egress
	// allowlist before any one of them is dialled. It is the single
	// enforcement point: a probe must not reach a host that did not come back
	// from here.
	//
	// Most types dial exactly the tenant's api_base
	// (checkConnectionAPIBaseTargets). amazon_bedrock and vertex_ai carry no
	// api_base at all — their endpoint is derived from the region/location —
	// and vertex_ai contacts TWO hosts (Google's OAuth token endpoint and the
	// Vertex API host), which is precisely why this returns a slice rather
	// than a single string.
	dialTargets func(req checkConnectionRequest) ([]string, error)
	probe       func(ctx context.Context, client *http.Client, req checkConnectionRequest) error
}

// checkConnectionAllowsPrivateNetwork reports whether this credential may
// probe a private address, and it answers the question the SAME way the
// request path answers it.
//
// THE DEFECT THIS REPLACES. The decision used to be a static per-TYPE flag on
// checkConnectionProvider, set for `ollama` alone. The request path does not
// decide by type: account.ProviderForCredential reads the api_base as well,
// because the platform's `open_ai` credential is also the generic
// OpenAI-compatible credential — with a non-OpenAI origin it is dispatched
// through bifrost's vLLM provider, and GetConfigForProvider then carves out
// the private network for it. So an `open_ai` credential naming a private
// vLLM host served completions and FAILED its own Test connection with
// `no permitted address`. The button said the credential was broken while the
// credential worked.
//
// One predicate, read from the one table (providerConfigTypes, through
// ProviderForCredential), is the whole fix: a second copy keyed on the type
// alone is what drifted.
//
// A private destination is still refused unless the operator's merged
// allowlist EXPLICITLY names a private host or CIDR — that half of the gate is
// applied by probeAllowsPrivateNetwork, and every target has already passed
// EgressAllows before this is consulted.
func checkConnectionAllowsPrivateNetwork(req checkConnectionRequest) bool {
	provider, ok := account.ProviderForCredential(req.Type, req.APIBase)
	if !ok {
		return false
	}
	// The self-hosted provider classes, and only them — the identical switch
	// account.GetConfigForProvider applies to AllowPrivateNetwork.
	return provider == schemas.VLLM || provider == schemas.Ollama
}

// probeAllowsPrivateNetwork is the WHOLE private-network decision for a probe,
// and it is the request path's decision, not a second one.
//
// account.GetConfigForProvider builds NetworkConfig.AllowPrivateNetwork from
// two facts: the credential resolves to a self-hosted provider class, AND the
// merged egress allowlist explicitly names a private destination. This reads
// the same two facts, the second one from the same gate instance through
// EgressPolicy, so a probe cannot pass a destination the dialer refuses.
//
// Both callers (CheckConnection and ListProviderModels) go through here. Two
// copies of one predicate is exactly how the earlier per-type flag drifted.
//
// A nil policy answers false: a handler composed without WithEgressPolicy
// refuses the request before this is reached, and this must not be the one
// place that would have opened the private network for it.
func (h *Handler) probeAllowsPrivateNetwork(req checkConnectionRequest) bool {
	if h == nil || h.egressPolicy == nil {
		return false
	}
	return checkConnectionAllowsPrivateNetwork(req) && h.egressPolicy.EgressPrivateNetworkAllowed()
}

// checkConnectionFieldError is a pre-dial refusal: the payload itself cannot
// name a legitimate destination (no api_base, no region, a malformed region,
// a credential document that is not a Google service account). It carries the
// reason/detail pair verbatim, so a field problem is never misreported as an
// "unreachable provider".
type checkConnectionFieldError struct {
	reason string
	detail string
}

func (e *checkConnectionFieldError) Error() string {
	if e.detail == "" {
		return e.reason
	}
	return e.reason + ": " + e.detail
}

// checkConnectionAPIBaseTargets is the dialTargets function for every
// credential class whose endpoint IS the tenant-authored api_base. It keeps
// the original contract: an empty api_base is refused before any dial.
func checkConnectionAPIBaseTargets(req checkConnectionRequest) ([]string, error) {
	if strings.TrimSpace(req.APIBase) == "" {
		return nil, &checkConnectionFieldError{reason: checkConnectionReasonMissingBase}
	}
	return []string{req.APIBase}, nil
}

// checkConnectionProviders holds the probe for each credential type this
// gateway can actually test. It started as the six legacy's LiteLLM check
// covered; amazon_bedrock and vertex_ai (AWS SigV4 and a Google
// service-account token exchange rather than a bearer/api-key header) live in
// checkconnection_cloud.go and reuse the credential shapes
// account/credentials.go already stores for them.
//
// It does NOT cover every type providerConfigTypes can build a key for.
// `anthropic` has no entry: no read-only probe is written for it yet, and
// elitea-main's catalogue advertises has_test_connection = false for it, so the
// screen offers no button rather than a button that answers "unsupported".
// Adding one here means adding the type to elitea-main's
// checkableConnectionTypes and flipping that flag in the same change.
var checkConnectionProviders = map[string]checkConnectionProvider{
	// OpenAI's own credential type: GET /models is OpenAI's documented,
	// canonical way to validate a key (no billed completion).
	"open_ai": {dialTargets: checkConnectionAPIBaseTargets, probe: probeOpenAICompatibleModels},
	// azure_open_ai and ai_dial both map to schemas.Azure in
	// account/credentials.go's providerConfigTypes — AI DIAL is explicitly an
	// Azure-OpenAI-API-compatible proxy, so both list deployments the same
	// way.
	"azure_open_ai": {dialTargets: checkConnectionAPIBaseTargets, probe: probeAzureDeployments},
	"ai_dial":       {dialTargets: checkConnectionAPIBaseTargets, probe: probeAzureDeployments},
	// open_ai_azure is the third name providerConfigTypes maps to schemas.Azure
	// — it is an ALIAS of azure_open_ai, kept because the legacy integration
	// path wrote rows under it. The same credential shape must get the same
	// probe, or the alias tests as broken while it serves completions.
	"open_ai_azure": {dialTargets: checkConnectionAPIBaseTargets, probe: probeAzureDeployments},
	// Ollama is self-hosted: its api_base routinely names a private address.
	// The carve-out is no longer declared here — checkConnectionAllowsPrivateNetwork
	// derives it from the provider this credential resolves to, exactly as the
	// request path does.
	"ollama": {dialTargets: checkConnectionAPIBaseTargets, probe: probeOllamaTags},
	// vLLM serves the OpenAI-compatible surface, so it is probed with GET
	// {api_base}/models — with the customary api_base of
	// http://host:8000/v1 that is /v1/models. It is NOT probed with Ollama's
	// /api/tags: vLLM does not serve that path and answers 404, which reads to
	// an operator as a rejected credential.
	"vllm": {dialTargets: checkConnectionAPIBaseTargets, probe: probeOpenAICompatibleModels},
	// amazon_bedrock: SigV4-signed GET bedrock.{region}.amazonaws.com/foundation-models.
	"amazon_bedrock": {dialTargets: checkConnectionBedrockTargets, probe: probeBedrockFoundationModels},
	// vertex_ai: service-account token exchange, then GET
	// {location}-aiplatform.googleapis.com/v1beta1/publishers/google/models.
	"vertex_ai": {dialTargets: checkConnectionVertexTargets, probe: probeVertexPublisherModels},
}

// CheckConnection performs a real, minimal round trip to the named provider
// using the supplied (not-yet-persisted) credential fields, and reports
// whether it actually succeeded. It never dials a host the operator's egress
// allowlist forbids (issue #13), and it never returns the provider's raw
// response body to the caller — see the package doc above.
func (h *Handler) CheckConnection(w http.ResponseWriter, r *http.Request) {
	if !verifySignature(r.Header, h.identitySecret) {
		writeError(w, http.StatusForbidden, "permission_error", "invalid identity signature", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, checkConnectionMaxBody)
	var req checkConnectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	provider, ok := checkConnectionProviders[req.Type]
	if !ok {
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonUnsupported,
		})
		return
	}
	// Resolve every host the probe will contact BEFORE anything is dialled.
	// A payload that cannot name a legitimate destination at all (no
	// api_base, no aws_region_name, a credential document that is not a
	// service account) is refused here, with the offending FIELD named and no
	// value echoed.
	targets, err := provider.dialTargets(req)
	if err != nil {
		var fe *checkConnectionFieldError
		if errors.As(err, &fe) {
			writeJSON(w, http.StatusOK, checkConnectionResponse{
				Success: false, Reason: fe.reason, Detail: fe.detail,
			})
			return
		}
		h.logger.WarnContext(r.Context(), "check_connection: could not resolve a probe target",
			"type", req.Type, "err", err)
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonUnreachable,
			Detail: "could not reach the provider",
		})
		return
	}

	// Fail closed: no policy wired (a Handler built without WithEgressPolicy)
	// refuses every request rather than silently skipping the gate. An empty
	// target list is refused for the same reason — a probe with no gated
	// destination is a probe with no gate.
	if h.egressPolicy == nil || len(targets) == 0 {
		h.logger.WarnContext(r.Context(), "check_connection: no egress policy or no gated target",
			"type", req.Type)
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonEgress,
		})
		return
	}
	// EVERY host the probe touches goes through the allowlist, not just the
	// first: vertex_ai contacts Google's token endpoint as well as the Vertex
	// API host, and a gate on one of two hosts is not a gate.
	for _, target := range targets {
		if !h.egressPolicy.EgressAllows(target) {
			h.logger.WarnContext(r.Context(), "check_connection: probe host is not on the egress allowlist",
				"type", req.Type)
			writeJSON(w, http.StatusOK, checkConnectionResponse{
				Success: false, Reason: checkConnectionReasonEgress,
			})
			return
		}
	}

	allowPrivate := h.probeAllowsPrivateNetwork(req)
	client := newCheckConnectionProbeClient(allowPrivate)

	ctx, cancel := context.WithTimeout(r.Context(), checkConnectionProbeTimeout)
	defer cancel()

	if err := provider.probe(ctx, client, req); err != nil {
		reason, detail := classifyCheckConnectionProbeError(err)
		h.logger.WarnContext(r.Context(), "check_connection: provider probe failed",
			"type", req.Type, "reason", reason, "err", err)
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: reason, Detail: detail,
		})
		return
	}

	writeJSON(w, http.StatusOK, checkConnectionResponse{Success: true, Reason: checkConnectionReasonOK})
}

// checkConnectionProbeError carries the provider's HTTP status when the
// request round-tripped, so classifyCheckConnectionProbeError can tell
// "credential rejected" apart from "could not reach the provider" without
// ever inspecting (or forwarding) the response body.
type checkConnectionProbeError struct {
	status int // 0 when the request never received a response
	err    error
}

func (e *checkConnectionProbeError) Error() string { return e.err.Error() }
func (e *checkConnectionProbeError) Unwrap() error { return e.err }

func classifyCheckConnectionProbeError(err error) (reason, detail string) {
	var pe *checkConnectionProbeError
	if errors.As(err, &pe) {
		switch {
		case pe.status == http.StatusUnauthorized || pe.status == http.StatusForbidden:
			return checkConnectionReasonUnauth, "the provider rejected the credential"
		case pe.status >= 500:
			return checkConnectionReasonUpstream, "the provider returned a server error"
		case pe.status > 0:
			return checkConnectionReasonUpstream, fmt.Sprintf("the provider returned an unexpected response (status %d)", pe.status)
		}
	}
	return checkConnectionReasonUnreachable, "could not reach the provider"
}

// probeOpenAICompatibleModels validates an OpenAI credential with GET
// {api_base}/models — OpenAI's own documented way to check a key without a
// billed completion.
func probeOpenAICompatibleModels(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	headers := map[string]string{}
	if req.APIKey != "" {
		headers["Authorization"] = "Bearer " + req.APIKey
	}
	return checkConnectionProbeGET(ctx, client, checkConnectionJoinURL(req.APIBase, "/models"), headers)
}

// probeAzureDeployments validates an Azure-OpenAI-shaped credential
// (azure_open_ai, ai_dial) with GET
// {api_base}/openai/deployments?api-version=..., authenticated with the
// api-key header Azure's control-plane API expects (not Bearer).
func probeAzureDeployments(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	apiVersion := req.APIVersion
	if apiVersion == "" {
		apiVersion = defaultAzureAPIVersion
	}
	target := checkConnectionJoinURL(req.APIBase, "/openai/deployments") + "?api-version=" + url.QueryEscape(apiVersion)
	headers := map[string]string{}
	if req.APIKey != "" {
		headers["api-key"] = req.APIKey
	}
	return checkConnectionProbeGET(ctx, client, target, headers)
}

// probeOllamaTags validates a self-hosted Ollama credential with GET
// {api_base}/api/tags — Ollama's native "list local models" call. Ollama
// credentials carry no api_key (the elitea-main catalog schema for "ollama"
// has only api_base), so no Authorization header is sent.
func probeOllamaTags(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	return checkConnectionProbeGET(ctx, client, checkConnectionJoinURL(req.APIBase, "/api/tags"), nil)
}

func checkConnectionJoinURL(base, path string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + path
}

// checkConnectionProbeGET performs the actual minimal, read-only provider
// call. A non-2xx response is reported as a *checkConnectionProbeError
// carrying the status (never the body); a transport-level failure (DNS,
// dial, TLS, timeout) is reported with status 0.
func checkConnectionProbeGET(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &checkConnectionProbeError{err: fmt.Errorf("check_connection: api_base does not yield a valid http(s) url")}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	for k, v := range headers {
		if v != "" {
			httpReq.Header.Set(k, v)
		}
	}
	return checkConnectionProbeDo(client, httpReq)
}

// checkConnectionProbeDo sends an already-built probe request and turns the
// outcome into the file's error vocabulary. It is split out of
// checkConnectionProbeGET because the amazon_bedrock probe must build and
// SIGN its request before it is sent (a header added after signing that the
// signature covers would invalidate it), and it must still classify the
// answer exactly like every other checker.
func checkConnectionProbeDo(client *http.Client, httpReq *http.Request) error {
	resp, err := client.Do(httpReq)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &checkConnectionProbeError{
			status: resp.StatusCode,
			err:    fmt.Errorf("check_connection: provider returned status %d", resp.StatusCode),
		}
	}
	return nil
}

// newCheckConnectionProbeClient builds the http.Client CheckConnection uses
// for its single real provider round trip.
//
// This endpoint bypasses bifrost/core entirely (see the package doc above:
// no persisted credential, no bifrost Account, no saved model to complete
// against), so it does NOT inherit bifrost's own SSRF-safe dialer the way
// every other provider call in this gateway does — it has to reimplement the
// equivalent address-level protection itself, or a tenant-authored api_base
// could steer this endpoint at an internal address the name-only egress
// allowlist alone cannot catch by hostname (e.g. a hostname that legitimately
// resolves to both a public and a private address).
//
// The DialContext hook resolves the target host ITSELF and validates the
// resolved address before connecting, then dials that exact address — not
// the original addr, which would let the standard dialer re-resolve the name
// a second time. Validating a first resolution and then dialing by name again
// is exactly the check-then-dial DNS-rebinding race account/egress.go's own
// doc comment warns about; dialing the validated IP directly closes it.
//
// allowPrivate comes from probeAllowsPrivateNetwork ONLY. It is true for a
// self-hosted credential class AND only when the merged allowlist explicitly
// names a private host or CIDR — mirroring GetConfigForProvider's
// NetworkConfig.AllowPrivateNetwork exactly.
func newCheckConnectionProbeClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var dialIP net.IP
			for _, candidate := range ips {
				if !allowPrivate && isDisallowedCheckConnectionAddress(candidate.IP) {
					continue
				}
				dialIP = candidate.IP
				break
			}
			if dialIP == nil {
				return nil, fmt.Errorf("check_connection: no permitted address for %q", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
		},
		MaxIdleConns:      1,
		IdleConnTimeout:   5 * time.Second,
		DisableKeepAlives: true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   checkConnectionProbeTimeout,
		// Never follow a redirect: doing so would dial a second, unvalidated
		// host through net/http's own connection logic, bypassing the
		// DialContext guard above for that hop. Treating the redirect itself
		// as the (non-2xx) final response is enough to prove or disprove the
		// credential.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// isDisallowedCheckConnectionAddress reports whether ip is a loopback,
// private (RFC 1918/RFC 4193), link-local (including the
// 169.254.169.254/fd00:ec2::254 cloud metadata addresses, which are
// link-local unicast), unspecified, or multicast address — the standard SSRF
// destination denylist, mirroring what bifrost/core's own dialer refuses for
// every other provider call in this gateway.
func isDisallowedCheckConnectionAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
