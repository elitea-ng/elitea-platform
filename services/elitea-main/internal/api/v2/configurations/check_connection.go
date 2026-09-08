// check_connection.go implements the real POST /check_connection and
// POST /check_connections handlers (#319). Previously both were unconditional
// stubs that reported "Connection successful" for every payload, including a
// credential the provider would reject outright — a false positive, not a
// missing feature: the user saved the credential believing it worked and only
// discovered otherwise at their first chat message.
//
// The check restores the save-time feedback legacy's LiteLLM
// health/test_connection call gave (see commit 071774fb), but the actual
// provider round trip is delegated to services/elitea-llm-gateway rather than
// dialled directly from here: the gateway is the component with an
// SSRF-safe egress allowlist for a tenant-authored api_base (issue #13), and
// elitea-main must not reimplement that guard to reach the same provider a
// second, unguarded way.
//
// Only the credential TYPES the gateway can actually validate today report a
// real success/failure (see checkableConnectionTypes below); every other
// KNOWN type — including the ones this same stub used to fake success for —
// gets the honest "checking connection is not supported yet" message legacy's
// own registry fallback already used for a type with no working check
// (legacy/plugins/configurations/models/pd/registry.py). No code path in this
// file can report success without the checker actually running.
package configurations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// checkableConnectionTypes is the set of configuration `type` values this
// handler can actually validate against the real provider, via the gateway's
// /llm/v1/check_connection endpoint (elitea-llm-gateway/internal/llmproxy/checkconnection.go).
//
// It now covers all six ai_credentials provider types legacy's LiteLLM check
// covered: amazon_bedrock (AWS SigV4) and vertex_ai (a Google service-account
// token exchange) joined the set when the gateway grew their probes
// (elitea-llm-gateway/internal/llmproxy/checkconnection_cloud.go). This map
// must stay identical to the gateway's checkConnectionProviders: a type here
// that the gateway cannot probe answers "unsupported_type", which reads to the
// user as a failed credential rather than a missing feature.
//
// Toolkit credential types (github, jira, confluence, ...) use a wholly
// different check (legacy's applications_configuration_check_connection over
// the SDK toolkit surface, itself never implemented in Go) and are out of this
// issue's scope (#319 is specifically the LiteLLM/ai_credentials path — see
// the issue body).
const (
	// maxBatchConnectionChecks bounds how many items of one POST
	// /check_connections body reach the gateway. The web app sends a
	// project's whole credential list unpaginated, so the cap sits far above
	// realistic use; an item above it reports "could not verify" instead of
	// failing the request.
	maxBatchConnectionChecks = 200

	// batchConnectionCheckWorkers is how many items are checked at the same
	// time. It keeps a legitimate list inside the budget without turning one
	// request into a burst at the provider.
	batchConnectionCheckWorkers = 6
)

// batchConnectionCheckBudget bounds the whole request. One item costs up to
// 12 s at the checker's own client timeout. A sequential run of a long list
// could therefore hold a worker for hours. It is a var so a test can shorten it.
var batchConnectionCheckBudget = 30 * time.Second

// checkableConnectionTypes must hold exactly the types the gateway carries a
// probe AND a lister for (checkConnectionProviders and providerModelListers in
// services/elitea-llm-gateway/internal/llmproxy). A type here that the gateway
// does not serve answers "unsupported_type", which an operator reads as a
// failed credential rather than a missing feature.
//
// `anthropic` is deliberately absent: no read-only probe is written for it, and
// the catalogue advertises has_test_connection = false for it, so no button is
// offered. Add all three together, or none.
var checkableConnectionTypes = map[string]struct{}{
	"open_ai":        {},
	"azure_open_ai":  {},
	"open_ai_azure":  {},
	"ai_dial":        {},
	"ollama":         {},
	"vllm":           {},
	"amazon_bedrock": {},
	"vertex_ai":      {},
}

// connectionCheckNotSupportedMessage matches, word for word, the message
// legacy's own registry produced for a registered type with no working check
// function (models/pd/registry.py: "Checking connection is not supported yet
// for configuration type {self.type}") — this is not a new "unsupported"
// concept, it is the same honest answer legacy already gave for any type it
// had not wired up either.
func connectionCheckNotSupportedMessage(configType string) string {
	return fmt.Sprintf("Checking connection is not supported yet for configuration type %s", configType)
}

// ConnectionCheckResult is the outcome of testing one credential payload.
type ConnectionCheckResult struct {
	Success bool
	// Message is always safe to return to the browser: it never carries a raw
	// provider response body or internal error detail (those are logged
	// separately via slog, never returned — repository convention for typed,
	// safe API errors).
	Message string
	// Reason is the machine-readable verdict of a TOOLKIT probe
	// (toolkit_check.go's closed four-value vocabulary). It is empty for the
	// gateway/LLM path, whose own reason vocabulary is already collapsed into
	// Message by connectionCheckMessageFor — a caller reads Message there, as
	// it always did, and switches on Reason only where one is present.
	Reason string
}

// ConnectionChecker performs the real, minimal provider round trip for one
// checkable credential type. Implementations MUST NOT report Success without
// actually exercising the provider round trip — every caller of Check in this
// package (single and batch) surfaces the result directly to the browser,
// exactly as it comes back.
type ConnectionChecker interface {
	Check(ctx context.Context, configType string, data map[string]any) (ConnectionCheckResult, error)
}

// checkConnectionRequestBody is the elitea-main -> gateway wire payload
// (mirrors elitea-llm-gateway/internal/llmproxy.checkConnectionRequest).
type checkConnectionRequestBody struct {
	Type       string `json:"type"`
	APIBase    string `json:"api_base"`
	APIKey     string `json:"api_key"`
	APIVersion string `json:"api_version"`

	// amazon_bedrock and vertex_ai carry no api_base and no api_key: they
	// authenticate with these fields instead (the same ones
	// application/configurations/litellm_normalizer.go stores for them, and
	// the same ones the gateway's account package builds their bifrost key
	// configs from). Without them on the wire the gateway can only answer
	// "this credential is missing ...", so a valid Bedrock or Vertex
	// credential would test as broken.
	AWSAccessKeyID     string `json:"aws_access_key_id,omitempty"`
	AWSSecretAccessKey string `json:"aws_secret_access_key,omitempty"`
	AWSSessionToken    string `json:"aws_session_token,omitempty"`
	AWSRegionName      string `json:"aws_region_name,omitempty"`

	VertexProject  string `json:"vertex_project,omitempty"`
	VertexLocation string `json:"vertex_location,omitempty"`
	// VertexCredentials is forwarded as the caller sent it, not coerced to a
	// string: the Google service-account document arrives as an escaped JSON
	// STRING from one screen and as a nested JSON OBJECT from another, and the
	// gateway accepts both shapes (its jsonTextField).
	VertexCredentials any `json:"vertex_credentials,omitempty"`
}

// checkConnectionResponseBody is the gateway's reply (mirrors
// elitea-llm-gateway/internal/llmproxy.checkConnectionResponse).
type checkConnectionResponseBody struct {
	Success bool   `json:"success"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
}

// connectionCheckMessages maps the gateway's safe reason vocabulary to the
// user-facing message. Never derived from provider response text.
var connectionCheckMessages = map[string]string{
	"missing_api_base":   "This credential is missing its endpoint (api_base).",
	"egress_not_allowed": "This endpoint is not permitted by the platform's configuration.",
	"unauthorized":       "The provider rejected the credential.",
	"unreachable":        "Could not reach the provider.",
	"upstream_error":     "The provider returned an unexpected response.",
	"unsupported_type":   "Checking connection is not supported yet for this configuration type.",
}

func connectionCheckMessageFor(reason, detail string) string {
	if msg, ok := connectionCheckMessages[reason]; ok {
		return msg
	}
	if detail != "" {
		return detail
	}
	return "Could not verify the connection."
}

// GatewayConnectionChecker calls elitea-llm-gateway's
// POST /llm/v1/check_connection over the same mTLS-internal transport the
// /llm streaming proxy uses (internal/llmproxy.NewMTLSTransport), signing the
// request with the identical identity-header scheme
// (internal/llmproxy.SignIdentityHeaders) so the gateway's verifySignature
// accepts it. It never dials the provider itself — see the package doc above.
type GatewayConnectionChecker struct {
	httpClient     *http.Client
	baseURL        string
	identitySecret []byte
	// projectID is threaded through Check by the caller (the projectID URL
	// path parameter), not resolved from ambient request context: the
	// /configurations mount does not populate *middleware.ProjectContext (it
	// is not proxying an end user's /llm request; the project id here comes
	// from the check_connection/{projectID}/{configType} path itself).
}

// NewGatewayConnectionChecker builds a checker that reaches gatewayBaseURL
// (e.g. "https://elitea-llm-gateway-svc:8443") over transport. When transport
// is nil, llmproxy.NewMTLSTransport(certFile, keyFile, caFile, serverName) is
// used — the identical construction the /llm reverse proxy uses.
func NewGatewayConnectionChecker(gatewayBaseURL string, transport http.RoundTripper, identitySecret string) *GatewayConnectionChecker {
	return &GatewayConnectionChecker{
		httpClient:     &http.Client{Transport: transport, Timeout: 12 * time.Second},
		baseURL:        strings.TrimRight(gatewayBaseURL, "/"),
		identitySecret: []byte(identitySecret),
	}
}

// checkerProjectIDKey is an unexported context key so Check can read the
// caller-resolved project id without widening the ConnectionChecker
// interface (which must stay generic — the fake used in tests has no notion
// of project id at all).
type checkerProjectIDKey struct{}

// WithConnectionCheckProjectID returns a context carrying projectID for
// GatewayConnectionChecker.Check to forward as the signed identity header.
func WithConnectionCheckProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, checkerProjectIDKey{}, projectID)
}

func connectionCheckProjectIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(checkerProjectIDKey{}).(string)
	return v
}

// Check implements ConnectionChecker by forwarding the (already unsecreted)
// payload to the gateway and translating its safe reason vocabulary into a
// browser-facing message. A transport-level failure (gateway unreachable,
// timeout, malformed response) is returned as an error — the caller must NOT
// map that to success; see CheckConnection/BatchCheckConnections below.
func (c *GatewayConnectionChecker) Check(ctx context.Context, configType string, data map[string]any) (ConnectionCheckResult, error) {
	body := checkConnectionRequestBody{
		Type:       configType,
		APIBase:    strVal(data, "api_base"),
		APIKey:     firstStrVal(data, "api_key", "api_token"),
		APIVersion: strVal(data, "api_version"),

		AWSAccessKeyID:     strVal(data, "aws_access_key_id"),
		AWSSecretAccessKey: strVal(data, "aws_secret_access_key"),
		AWSSessionToken:    strVal(data, "aws_session_token"),
		AWSRegionName:      strVal(data, "aws_region_name"),

		VertexProject:     strVal(data, "vertex_project"),
		VertexLocation:    strVal(data, "vertex_location"),
		VertexCredentials: data["vertex_credentials"],
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ConnectionCheckResult{}, fmt.Errorf("check connection: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/llm/v1/check_connection", strings.NewReader(string(raw)))
	if err != nil {
		return ConnectionCheckResult{}, fmt.Errorf("check connection: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No execution id: a connection check is authored by an operator in the
	// admin UI, not made from a runtime execution. The empty value signs v1,
	// byte for byte what this call signed before.
	llmproxy.SignIdentityHeaders(req.Header, c.identitySecret, connectionCheckProjectIDFrom(ctx), "", "", "")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ConnectionCheckResult{}, fmt.Errorf("check connection: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ConnectionCheckResult{}, fmt.Errorf("check connection: gateway responded with status %d", resp.StatusCode)
	}

	var out checkConnectionResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ConnectionCheckResult{}, fmt.Errorf("check connection: decode gateway response: %w", err)
	}

	if out.Success {
		return ConnectionCheckResult{Success: true, Message: "Connection successful"}, nil
	}
	return ConnectionCheckResult{Success: false, Message: connectionCheckMessageFor(out.Reason, out.Detail)}, nil
}

// CheckConnection validates a not-yet-saved credential payload against the
// real provider (#319). Unlike the rest of this package's routes, the
// external contract intentionally mirrors legacy's
// (configurations/api/v2/check_connection.py) byte for byte: HTTP 200 with
// {"success":true} only on a proven round trip, HTTP 404 for a wholly unknown
// type, HTTP 400 with {"success":false,"message":...} for every other
// failure — the browser (apps/elitea-ui useCreateConfiguration.onTestConnection)
// keys its success/failure toast off the HTTP status, not a body field.
func (h *Handler) CheckConnection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configType := chi.URLParam(r, "configType")

	var data map[string]any
	if !decodeBoundedJSON(w, r, &data) {
		return
	}
	if data == nil {
		data = map[string]any{}
	}

	if _, known := h.catalog.EntryByType(configType); !known {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": fmt.Sprintf("Unknown configuration type: %s", configType),
		})
		return
	}

	if err := validateNotSelfReferential(data, selfLLMOrigins()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}

	if _, checkable := checkableConnectionTypes[configType]; !checkable {
		// A TOOLKIT credential is checked by this build's own probe, not by the
		// gateway (toolkit_check.go). Before this branch existed every one of
		// them answered "not supported yet", so the credential card's attention
		// indicator could not light for a token the provider refuses.
		if outcome, probed := h.checkToolkitConnection(r.Context(), configType, data); probed {
			writeJSON(w, toolkitCheckStatus(outcome), map[string]any{
				"success": outcome.Success(),
				"message": outcome.Message,
				"reason":  outcome.Reason,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": connectionCheckNotSupportedMessage(configType),
			"reason":  ToolkitCheckReasonUnsupportedType,
		})
		return
	}

	if h.connectionChecker == nil {
		slog.ErrorContext(r.Context(), "check_connection: no connection checker configured", "type", configType)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Connection checking is not available right now.",
		})
		return
	}

	ctx := WithConnectionCheckProjectID(r.Context(), projectID)
	result, err := h.connectionChecker.Check(ctx, configType, data)
	if err != nil {
		// A transport-level failure must never be reported as success, and the
		// real cause is logged server-side only — never echoed to the browser
		// (typed/safe API error boundary).
		slog.ErrorContext(r.Context(), "check_connection: checker call failed", "type", configType, "project_id", projectID, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Could not verify the connection right now. Please try again.",
		})
		return
	}

	if result.Success {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": result.Message})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": result.Message})
}

// BatchCheckConnections validates multiple not-yet-saved credential payloads
// in one request (#319). The external contract mirrors legacy's
// (configurations/api/v2/check_connections.py): always HTTP 200, one
// {"id","success","message"?,"unsupported"?} object per input item.
// "unsupported" is set ONLY for a type this Go build's catalogue has never
// heard of at all — matching legacy's own distinction between "not a
// registered type" and "a registered type with no working check yet" (the
// latter gets the same not-supported MESSAGE the single-check path uses, no
// unsupported flag), so existing UI handling of both fields keeps working.
func (h *Handler) BatchCheckConnections(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var items []map[string]any
	if !decodeBoundedJSON(w, r, &items) {
		return
	}

	// The body is bounded at 1 MiB only, and one minimal item is ~50 bytes.
	// A single request could therefore ask for ~20,000 provider round trips.
	// Bound the work three ways: an item cap, one deadline for the whole request, and a
	// worker pool so a legitimate list finishes inside that deadline. The
	// server sets no WriteTimeout on purpose (cmd/elitea-main/http_server.go),
	// so nothing else cuts this handler short.
	ctx, cancel := context.WithTimeout(r.Context(), batchConnectionCheckBudget)
	defer cancel()

	// The contract is always HTTP 200 with one object per input item, in input
	// order. An over-cap list therefore degrades per row; it does not fail the
	// whole page. The web app marks EVERY credential invalid when this request
	// fails, so a 400 would paint a healthy project all red.
	results := make([]map[string]any, len(items))
	checked := items
	if len(checked) > maxBatchConnectionChecks {
		checked = items[:maxBatchConnectionChecks]
		slog.WarnContext(ctx, "check_connections: item count above the cap",
			"project_id", projectID, "items", len(items), "cap", maxBatchConnectionChecks)
		for index := maxBatchConnectionChecks; index < len(items); index++ {
			results[index] = connectionCheckUnavailableResult(items[index]["id"])
		}
	}

	positions := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < batchConnectionCheckWorkers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// Each worker writes its own index, so the slice needs no lock and
			// the input order survives.
			for index := range positions {
				results[index] = h.checkBatchItem(ctx, projectID, checked[index])
			}
		}()
	}
	for index := range checked {
		positions <- index
	}
	close(positions)
	workers.Wait()

	writeJSON(w, http.StatusOK, results)
}

// checkBatchItem produces the one result object for one input item.
func (h *Handler) checkBatchItem(ctx context.Context, projectID string, item map[string]any) map[string]any {
	id := item["id"]
	configType := strVal(item, "type")
	data, _ := item["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}

	if _, known := h.catalog.EntryByType(configType); !known {
		return map[string]any{"id": id, "success": false, "unsupported": true}
	}

	if err := validateNotSelfReferential(data, selfLLMOrigins()); err != nil {
		return map[string]any{"id": id, "success": false, "message": err.Error()}
	}

	if _, checkable := checkableConnectionTypes[configType]; !checkable {
		if outcome, probed := h.checkToolkitConnection(ctx, configType, data); probed {
			return map[string]any{
				"id": id, "success": outcome.Success(), "message": outcome.Message, "reason": outcome.Reason,
			}
		}
		return map[string]any{
			"id": id, "success": false, "message": connectionCheckNotSupportedMessage(configType),
			"reason": ToolkitCheckReasonUnsupportedType,
		}
	}

	if h.connectionChecker == nil {
		slog.ErrorContext(ctx, "check_connections: no connection checker configured", "type", configType)
		return map[string]any{
			"id": id, "success": false, "message": "Connection checking is not available right now.",
		}
	}

	// Read the deadline here rather than rely on Check returning an error, so
	// an expired budget costs no syscall for each remaining item.
	if ctx.Err() != nil {
		return connectionCheckUnavailableResult(id)
	}

	result, err := h.connectionChecker.Check(WithConnectionCheckProjectID(ctx, projectID), configType, data)
	if err != nil {
		// A transport-level failure must never be reported as success.
		slog.ErrorContext(ctx, "check_connections: checker call failed",
			"type", configType, "project_id", projectID, "id", id, "err", err)
		return connectionCheckUnavailableResult(id)
	}
	return map[string]any{"id": id, "success": result.Success, "message": result.Message}
}

// connectionCheckUnavailableResult is the one row shape for "this item was not
// checked": a transport failure, an expired budget, or a position above the
// item cap. It never reports success.
func connectionCheckUnavailableResult(id any) map[string]any {
	return map[string]any{
		"id": id, "success": false, "message": "Could not verify the connection right now. Please try again.",
	}
}

// gatewayConnectionCheckerFromEnv builds a GatewayConnectionChecker from the
// same LLM_GATEWAY_URL / mTLS / GATEWAY_IDENTITY_SECRET environment
// configuration the /llm reverse proxy uses
// (cmd/elitea-main/main.go llmproxy.Config), so operators configure the
// gateway connection once, not twice. Returns nil when LLM_GATEWAY_URL is
// unset — callers must treat a nil checker as "not available", never
// substitute a stub that reports success.
func NewGatewayConnectionCheckerFromConfig(gatewayURL, clientCertFile, clientKeyFile, caFile, identitySecret string) (*GatewayConnectionChecker, error) {
	if gatewayURL == "" {
		return nil, nil
	}
	transport, err := llmproxy.NewMTLSTransport(clientCertFile, clientKeyFile, caFile, "")
	if err != nil {
		return nil, fmt.Errorf("check connection: build mTLS transport: %w", err)
	}
	return NewGatewayConnectionChecker(gatewayURL, transport, identitySecret), nil
}

// checkToolkitConnection runs this build's own toolkit probe for a type the
// gateway path cannot check.
//
// `probed` is false for a type that is not a toolkit family this build carries
// a probe for, and the caller then falls through to the "not supported yet"
// answer it always gave — the honest one, and the same one legacy's registry
// produced for a registered type with no working check.
func (h *Handler) checkToolkitConnection(ctx context.Context, configType string, data map[string]any) (ToolkitCheckOutcome, bool) {
	if h == nil || h.toolkitChecker == nil || !IsToolkitCheckableType(configType) {
		return ToolkitCheckOutcome{}, false
	}
	return h.toolkitChecker.CheckToolkit(ctx, configType, data), true
}

// toolkitCheckStatus maps an outcome onto the status the single-check routes
// answer with. The contract is the LLM path's, byte for byte, because the same
// browser control renders both: 200 only for a proven round trip, 400 for every
// refusal, with the machine-readable reason beside the message.
func toolkitCheckStatus(outcome ToolkitCheckOutcome) int {
	if outcome.Success() {
		return http.StatusOK
	}
	return http.StatusBadRequest
}
