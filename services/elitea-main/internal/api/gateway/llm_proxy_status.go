package gateway

// llm_proxy_status.go — `GET /api/v2/admin/gateway/status`, the admin-side
// read of what the LLM gateway is actually enforcing.
//
// ## Why this route exists
//
// The gateway serves `GET /governance/status`, and that route was built to
// answer the one question the authoring surface cannot:
// *is the rule I saved in force?* The authoring table says what was written;
// the gateway's snapshot says what loaded. The three differences between them —
// a row that was REJECTED, a row that parsed but is INERT, and a snapshot that
// is STALE because refreshes are failing — are invisible from the table, and
// each one means an operator believes a ceiling is applied when it is not.
//
// Until now nothing could reach it. elitea-main proxies only `/llm` to the
// gateway's ClusterIP Service, so the admin SPA had no path to this route at
// all; `services/elitea-llm-gateway/DECISIONS.md` records the
// gap and defers "a proxy for it" as a separate decision. This file is that
// decision: a read-only proxy on the admin API, reusing the identical gateway
// transport the `/llm` reverse proxy and the #319 connection checker already
// use, so an operator configures the gateway hop once rather than three times.
//
// ## The body is passed through, not re-declared
//
// `Status` returns the gateway's own JSON verbatim. Re-declaring
// `governanceStatusBody` here would be a second specification of a contract the
// gateway owns — free to drift, and drifting silently, which is precisely the
// failure this route exists to expose in the governance corpus. The one thing
// this file adds is `reachable`, which the gateway cannot report about itself.
//
// ## Authorisation and safety
//
// Mounted inside the existing `/gateway` group under
// `RequireCentralPermissions("configuration.governance")` — the same boundary
// the governance CRUD it explains already sits behind. The upstream body
// carries row ids, names, types and refusal reasons only, never secret
// material or a tenant's prompt, which is what makes it safe to surface to an
// admin at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// maxStatusBody bounds how much of the gateway's answer is read. The real body
// is a few kilobytes of counters and refusal reasons; the cap exists so a
// misconfigured target that returns something enormous cannot be streamed into
// admin memory.
const maxStatusBody = 1 << 20

// statusTimeout bounds the upstream call. The gateway answers this route from
// an in-memory snapshot with no database or NATS call on the path, so a slow
// answer means the hop itself is unhealthy — which is a result worth returning
// promptly rather than waiting out.
const statusTimeout = 5 * time.Second

// ErrNoGatewayStatusSource reports that no gateway address was configured, so
// no status can be read. It is distinct from an unreachable gateway: one is a
// deployment that never wired the hop, the other is a hop that is down, and an
// operator needs to tell those apart.
var ErrNoGatewayStatusSource = errors.New("gateway status source not configured")

// StatusReader reads the gateway's enforcement status. It is an interface so
// tests can substitute a fake without a live gateway — the seam every other
// outbound client in this service uses.
type StatusReader interface {
	Status(ctx context.Context) (json.RawMessage, error)
}

// GatewayStatusClient reads the gateway's enforcement status through the
// GovernanceStatus RPC (llmproxy.Gateway, gRPC over mutual TLS).
//
// No identity is sent, deliberately: the gateway verifies no identity on this
// call, unlike the /llm dialect calls. Signing it would imply an
// authorization the upstream does not perform. The call is admitted by the
// gateway's client-certificate allowlist alone; the gateway's HTTP listener
// serves only health and metrics, so nothing else reaches this status.
type GatewayStatusClient struct {
	gateway llmproxy.Gateway
}

// NewGatewayStatusClient builds a reader over gateway. It returns nil when
// gateway is nil: a deployment without the gateway is supported, and the
// handler reports that posture rather than failing to compose.
func NewGatewayStatusClient(gateway llmproxy.Gateway) *GatewayStatusClient {
	if gateway == nil {
		return nil
	}
	return &GatewayStatusClient{gateway: gateway}
}

// Status fetches and returns the gateway's status body verbatim.
func (c *GatewayStatusClient) Status(ctx context.Context) (json.RawMessage, error) {
	if c == nil || c.gateway == nil {
		return nil, ErrNoGatewayStatusSource
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	resp, err := c.gateway.GovernanceStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway status: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway status: gateway responded with status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return nil, fmt.Errorf("gateway status: read response: %w", err)
	}
	// Validated rather than passed through blind: an operator screen that
	// renders whatever arrived would show a proxy error page as though it were
	// the gateway's own report.
	if !json.Valid(raw) {
		return nil, errors.New("gateway status: gateway returned a non-JSON body")
	}
	return json.RawMessage(raw), nil
}

// statusResponse is what this route returns.
//
// `Reachable` is the field the upstream cannot supply about itself, and it is
// the one the screen leads with: every other number in `Gateway` describes a
// snapshot that may be minutes old or absent entirely, and reading them without
// knowing whether the hop answered is how a stale report gets read as a live
// one.
type statusResponse struct {
	Reachable bool            `json:"reachable"`
	Gateway   json.RawMessage `json:"gateway,omitempty"`
	// Error is the reason the gateway could not be read, when it could not be.
	// It is the transport's own sentence: "not configured" and "connection
	// refused" call for different actions, and a single "unavailable" would
	// discard the distinction.
	Error string `json:"error,omitempty"`
}

// Status serves GET /gateway/status.
//
// A gateway that cannot be reached is reported with HTTP 200 and
// `reachable: false`, not with a 5xx. The route succeeded — it asked, and the
// answer is that the hop is down — and an admin screen that received a 502
// could not tell that apart from its own request failing.
func (h *LLMProxyHandler) Status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), statusTimeout)
	defer cancel()

	if h == nil || h.status == nil {
		writeJSON(w, http.StatusOK, statusResponse{
			Reachable: false,
			Error:     "this deployment has no LLM gateway address configured, so no enforcement status can be read.",
		})
		return
	}

	body, err := h.status.Status(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, statusResponse{Reachable: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Reachable: true, Gateway: body})
}
