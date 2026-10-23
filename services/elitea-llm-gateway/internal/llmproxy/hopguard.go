package llmproxy

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
)

// The INBOUND half of hop-marker detection (issue #164, follow-up to #12).
//
// The outbound half lives in internal/account (GetConfigForProvider stamps
// hopmarker.Header on every provider request). This half recognises that
// header coming back in.
//
// # Why this is the loop detector and loopbreaker.go is not
//
// loopbreaker.go counts requests per (project_id, model). Issue #12 established
// that no rate threshold there can separate a routing loop from legitimate
// traffic, because both are bounded by the same per-replica provider worker
// pool. It remains, correctly named, as an amplification backstop. Hop
// detection asks a different question — "did this deployment send this exact
// request?" — and that question has an exact answer at any rate, including one
// request per hour.
//
// # Containment depth
//
// The canonical loop is a credential whose api_base names the platform's own
// /llm surface:
//
//	gateway → provider (= platform /llm) → edge → elitea-main → gateway
//
// The request that closes that circle carries the marker, so it is refused
// before dispatch. The loop is contained on the FIRST re-entry — one extra
// hop, not a growing fan-out.
//
// (Guard #1 in internal/account rejects a credential whose api_base matches a
// configured GATEWAY_SELF_LLM_ORIGINS entry. That guard is a string match on
// operator-enumerated origins, so it misses an alias, a CDN hostname, a tenant
// proxy that forwards to the platform, and every deployment that leaves
// GATEWAY_SELF_LLM_ORIGINS empty. Hop detection needs no enumeration: it
// recognises the platform's own traffic by what the traffic carries.)
//
// # What a forged or harvested marker can do
//
// Nothing but deny its own request. hopGuard reads one header, compares it in
// constant time and records NOTHING — no counter, no circuit, no map keyed by
// anything. Any upstream can harvest the marker off a request the gateway sent
// it, and any client can put that value on a request of its own; the result is
// a 400 for that request alone. No other request, project or tenant is
// reachable from it.
//
// This is exactly why the check must stay stateless. If a recognised marker
// opened the (project, model) circuit, or fed any shared counter, then a value
// every upstream already holds would become a cross-tenant denial of service —
// the guard would have created the attack it exists to contain.
//
// # Refusal shape
//
// 400 invalid_request_error / circular_routing_detected.
//
// HTTP 508 Loop Detected reads better in a log and was rejected for one
// reason: it is a 5xx, and the OpenAI-compatible SDKs on this path retry 5xx.
// A retried loop request re-enters the platform two more times before the
// client gives up, which is amplification of a request already known to be a
// loop. No SDK retries a 400. The condition is also genuinely a caller-side
// configuration fault — a credential api_base that points back at the platform
// — so invalid_request_error names it honestly.
//
// The message says what to fix without naming any internal address.

// hopRefusalMessage is the tenant-visible text of a hop-marker refusal. It
// describes the misconfiguration and nothing about the internal topology.
const hopRefusalMessage = "Circular routing detected: this request already passed through the Elitea LLM gateway. " +
	"A provider credential's api_base points back at this platform's own /llm surface. " +
	"Point it at the provider instead."

// hopRefusalCode is the machine-readable code of a hop-marker refusal.
const hopRefusalCode = "circular_routing_detected"

// WithHopMarker arms hop-marker detection with the deployment's marker
// (issue #164). A nil marker leaves detection UNARMED: the handler then admits
// a request carrying any marker value, exactly as it did before this existed.
//
// Unarmed is a legitimate posture for a deployment with no GATEWAY_HOP_SECRET,
// but it must never be a quiet one — main() states the mode at startup
// (logHopMarkerMode), for the reason logLoopBreakerMode exists.
//
// The composition root MUST wire this; TestMainWiring asserts the call.
func WithHopMarker(m *hopmarker.Marker) HandlerOption {
	return func(h *Handler) { h.hopMarker = m }
}

// HopGuard is the inbound hop-marker middleware. Mount it on the ROUTER ROOT
// (internal/api.NewRouterWithLog does), not inside individual handlers.
//
// Root mounting is what makes the guard complete, for the same reason the
// request log is mounted there: every route below is covered without being
// named, a route added later is covered without anyone remembering, and the
// paths that never reach a handler — NotFound, MethodNotAllowed, the realtime
// upgrade — are covered too. A loop does not agree to use only the routes
// somebody remembered to annotate.
//
// It runs before identity verification on purpose. A re-entering request may
// carry a perfectly valid signed identity — in the canonical loop elitea-main
// signs it, because as far as elitea-main is concerned it is an ordinary
// caller. Loop containment must not depend on the request being otherwise
// invalid.
func (h *Handler) HopGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.hopMarker.Matches(r.Header.Get(hopmarker.Header)) {
			// project_id is logged UNVERIFIED and is labelled as such: this
			// runs before signature verification, so it is the claim on the
			// request, not a checked fact. It is here because it is the field
			// an operator needs to find the offending credential.
			h.logger.Warn("hop marker: refusing a request that already transited this gateway — circular routing",
				"path", r.URL.Path,
				"claimed_project_id", r.Header.Get(headerProjectID))
			writeError(w, http.StatusBadRequest, "invalid_request_error", hopRefusalMessage, hopRefusalCode)
			return
		}
		next.ServeHTTP(w, r)
	})
}
