package main

// governance_status.go — GET /governance/status.
//
// An operator authors a governance definition in the admin UI and then has to
// answer one question: is the gateway enforcing it? Before this route the only
// answer was a log line on whichever pod happened to load the row, which is
// unavailable the moment that pod is replaced.
//
// The route reports what the gateway HOLDS, not what the table contains — the
// admin surface can already read the table. The difference between the two is
// the whole point: a row that was rejected, a row that is inert, and a snapshot
// that is stale because refreshes are failing are each invisible from the
// authoring side.
//
// It sits on the same internal listener as /metrics and /readyz. That listener
// is a ClusterIP Service behind mutual TLS and the edge proxies only /llm, so
// this is reachable from inside the cluster and not from a tenant. It carries
// no secret material: row ids, names, types and reasons only.

import (
	"encoding/json"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// governanceStatusBody is the response shape.
type governanceStatusBody struct {
	// Enabled is false when this gateway reads no definitions at all — no
	// database pool. Every other field is then empty, and an operator reading
	// "enabled": false knows the answer without interpreting a zero count.
	Enabled bool `json:"enabled"`
	// RateLimitsEnforceable is false when the gateway has no NATS counter. An
	// authored rate limit then loads and does nothing, and this is the only
	// place that says so.
	RateLimitsEnforceable bool `json:"rate_limits_enforceable"`
	// SharedProjectID is the public project whose `shared = true` credentials
	// and models this gateway reads IN ADDITION to the caller's own (issue
	// #316). Empty means the second scope is OFF.
	//
	// It is reported here for the same reason everything else on this route is:
	// it is something the gateway HOLDS that the authoring side cannot see. The
	// admin panel publishes a platform provider into the public project's
	// schema, and whether that credential ever resolves depends on this value
	// being set AND naming the same project. The two are configured from
	// different environment variables in different services, so the mismatch is
	// silent and total — every project gets a credential that resolves for
	// nobody, and every other signal on the admin screen still looks correct.
	//
	// A project ID is not secret: it is a small integer that appears in every
	// tenant URL, and this route is on the internal listener behind mutual TLS
	// in any case.
	SharedProjectID string             `json:"shared_project_id"`
	Store           policy.Status      `json:"store"`
	Definitions     policy.Diagnostics `json:"definitions"`
	RateLimiter     *rateLimiterCounts `json:"rate_limiter,omitempty"`
	// Egress is the MERGED allowlist the gateway is enforcing, tagged by
	// source: the GATEWAY_EGRESS_ALLOWLIST floor, the authored
	// `egress_allowlist` rows, and the union of the two (gap G6).
	//
	// It belongs here for the same reason as everything else on this route: it
	// is something the gateway HOLDS that the authoring side cannot see. The
	// admin list shows the rows in the table; only the gateway knows which of
	// them parsed, what the chart already contributed, and — the question an
	// operator actually has — whether a private destination is reachable at all.
	//
	// It carries no secret material. A host name is not a credential, and this
	// route is on the internal listener behind mutual TLS.
	//
	// nil when the gateway has no vault-backed account, which is the posture
	// with no database pool: there are then no credentials to govern.
	Egress *account.EgressReport `json:"egress,omitempty"`
}

// rateLimiterCounts reports how often the limiter refused and how often it
// could not enforce. Degraded is the number that matters: it is the count of
// requests admitted WITHOUT their authored ceiling applied, because the counter
// was unreachable. A non-zero value there means the limits are not being kept.
type rateLimiterCounts struct {
	Refused  int64 `json:"refused"`
	Degraded int64 `json:"degraded"`
}

// makeGovernanceStatusHandler builds the route. A nil store reports disabled
// rather than erroring: a gateway without a database is a supported posture,
// not a fault.
func makeGovernanceStatusHandler(
	store *policy.Store, limiter *policy.Limiter, sharedProjectID string, acct *account.EliteaAccount,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := governanceStatusBody{
			Enabled:               store != nil,
			RateLimitsEnforceable: limiter.Enabled(),
			SharedProjectID:       sharedProjectID,
		}
		if acct != nil {
			// Read it per request, not once at wiring: the authored half of the
			// list changes on the governance poll, and a value captured at
			// startup would report the floor for the life of the pod.
			rep := acct.EgressReport()
			body.Egress = &rep
		}
		if store != nil {
			body.Store = store.Status()
			body.Definitions = store.Current().Diagnostics()
		} else {
			body.Definitions = policy.Empty.Diagnostics()
		}
		if limiter.Enabled() {
			body.RateLimiter = &rateLimiterCounts{Refused: limiter.Refused(), Degraded: limiter.Degraded()}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
}
