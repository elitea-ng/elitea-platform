// Package budgetwriteback implements the JetStream write-behind consumer
// (design §8.6): the durable pull consumer "budget-writeback" that drains the
// GATEWAY_BUDGET_DELTAS stream and lazily persists budget deltas into
// gateway.llm_budget_accumulators, keeping the Postgres durable tier
// seconds-fresh for the tiered-hybrid fallback (§8.5).
//
// Placement (design §8.6): the consumer lives in elitea-scheduler, which
// already has a pgxpool and a running loop. A durable pull consumer gives
// natural multi-consumer safety; a single active consumer is sufficient at the
// scale-1 baseline.
//
// This package owns ONLY the write-back drain path. The recovery-reconciliation
// goroutine (§8.5) and the outage-window direct-write path stay in the gateway;
// the two writers touch DISJOINT accumulator rows (write-back:
// NOT (outage_mode AND NOT reconciled); reconciliation: outage_mode AND NOT
// reconciled). The disjointness is enforced here by the ON CONFLICT DO UPDATE
// WHERE guard (see consumer.go) and is formally exercised by BF0.4b's
// integration test.
package budgetwriteback

import (
	"errors"
	"fmt"
)

const (
	// DeltasStream is the write-behind stream the consumer drains (§8.6). It is
	// created by the nats-bootstrap chart / the gateway's NATS client; this
	// consumer only binds a durable pull consumer to it.
	DeltasStream = "GATEWAY_BUDGET_DELTAS"

	// DeltaSubject is the write-behind delta subject (§8.6). Must match the
	// subject the gateway publishes on (gateway internal/infra/nats DeltaSubject).
	DeltaSubject = "gateway.budget.delta"

	// DurableName is the durable pull-consumer name (§8.6). A stable durable
	// name lets a restarted scheduler resume from the last ACK'd position.
	DurableName = "budget-writeback"
)

// nanoUSDPerUSD is the nano-USD → USD divisor. Budget counters are int64
// nano-USD (NanoUSD = 1e9); the durable accumulator column is USD NUMERIC. The
// conversion is done in SQL (CAST($ AS numeric) / this) to keep it exact —
// never via float64, which would reintroduce rounding on the money path.
const nanoUSDPerUSD = 1000000000

// BudgetDelta is the write-behind delta record carried on the
// GATEWAY_BUDGET_DELTAS stream (design §8.6). The design's §8.6 example payload
// is the minimal {scope, scope_id, period, delta_nano_usd, event_id}; this is
// its concrete superset. The extra fields (project_id, org_id, period_end) are
// REQUIRED because gateway.llm_budget_accumulators declares project_id and
// period_end NOT NULL — a fresh accumulator INSERT for a new period cannot be
// satisfied by the minimal payload alone. The gateway's GovernanceStore Update*
// path (BF0.4 s4, not yet built) MUST publish this exact JSON shape.
//
// `period` in the §8.6 example is this struct's PeriodStart: the period-boundary
// unix timestamp that, with (scope, scope_id), forms the accumulator's unique
// upsert key.
type BudgetDelta struct {
	// EventID is the delta's unique id. It becomes the JetStream Nats-Msg-Id on
	// publish (publish-side dedup) AND the processed_event_ids primary key
	// (consumer-side dedup) — together giving effective exactly-once (§8.6).
	EventID string `json:"event_id"`

	// Scope ∈ {project, team, customer, global}. ScopeID is the id within the
	// scope (numeric project id, uuid, etc.). Together with PeriodStart they key
	// the accumulator row.
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`

	// ProjectID is the owning project (accumulator project_id, NOT NULL). For
	// scope="project" it equals ScopeID as an int.
	ProjectID int `json:"project_id"`
	// OrgID is the optional owning org (accumulator org_id, nullable).
	OrgID *int `json:"org_id,omitempty"`

	// PeriodStart / PeriodEnd are the budget period bounds, unix seconds
	// (accumulator period_start / period_end, both NOT NULL).
	PeriodStart int64 `json:"period_start"`
	PeriodEnd   int64 `json:"period_end"`

	// DeltaNanoUSD is the billed increment in int64 nano-USD. May be negative
	// for a correction. Coalesced (summed) per key before the accumulator UPSERT.
	DeltaNanoUSD int64 `json:"delta_nano_usd"`

	// Usage carries the request's reporting dimensions (issue #320). It is
	// nil for a delta that has none, and the consumer then appends nothing to
	// the usage ledger.
	//
	// It is NOT coalesced with the rest of the group. The accumulator is summed
	// per (scope, scope_id, period) because money adds up; the ledger keeps one
	// row per request, because a per-model table cannot be reconstructed from a
	// sum. So a group of ten deltas produces ONE accumulator UPSERT and up to
	// ten ledger rows.
	Usage *UsageDimensions `json:"usage,omitempty"`
}

// UsageDimensions are the reporting dimensions of one billed request: the
// member it is attributed to, the provider and model it was billed against, and
// the provider-reported token counts (issue #320).
//
// The JSON keys MUST match the gateway's usageDimsPayload exactly
// (services/elitea-llm-gateway/internal/governance/store.go).
//
// These fields never reach gateway.llm_budget_accumulators. Budget admission
// reads money per (scope, scope_id, period) and nothing else; this struct
// exists so the Usage page can draw the per-day series and the per-model table
// that the LiteLLM tag ledger used to supply and that nothing has supplied
// since.
type UsageDimensions struct {
	// UserID is nil when the call carried no resolvable member (a service
	// account, a token-authenticated integration). Nil, not 0: the per-member
	// views must be able to tell "no member" from "member 0".
	UserID *int `json:"user_id,omitempty"`
	// Provider and Model are the resolved upstream, as billed.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// PromptTokens / CompletionTokens are provider-REPORTED. A response with no
	// usage field yields zeros; nothing here is estimated (#79).
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	// OccurredAtUnix is when the GATEWAY billed the request, not when this
	// consumer stored the row. The consumer runs behind the stream, and an
	// outage-deferred group is redelivered until the accumulator row stops
	// being outage-owned, so the two instants can fall on different DATES and
	// even in different billing periods. The per-day series buckets on this
	// value, so it must be the gateway's.
	OccurredAtUnix int64 `json:"occurred_at"`
	// ExecutionID is the runtime execution the call was made from, empty when
	// the caller is not one — which is most callers. It is what gives the
	// ledger an AGENT dimension.
	//
	// It is stored RAW and resolved to an agent at READ time, against
	// elitea_runtime.execution_jobs. Resolving it HERE would mean this consumer
	// choosing between execution_jobs' resource_project_id and
	// projection_project_id, which can differ — and baking that choice into a
	// table whose value is having exactly one project column.
	//
	// omitempty on both sides: a gateway that emits this key can talk to a
	// consumer that predates it, and vice versa. An absent key leaves the
	// column NULL, which is what "we do not know" already means there
	// (shared migration 0101).
	ExecutionID string `json:"execution_id,omitempty"`
	// Operation, Units and Unit mark a METERED TOOL row (#1238): a web search,
	// a page fetch or a host query billed through the same budget path as a
	// model call. Operation is "tool" for such a row and absent for a model
	// call, which the ledger then records as "llm" (its column default).
	// Provider and Model hold the vendor and the operation ("tavily",
	// "web_search"), so the per-(provider, model) Usage table lists tool spend
	// beside model spend with no query change.
	//
	// Units is a pointer because zero is a real count for a tool row (a free
	// count-only host search bills 0 credits) and absent is what a model row
	// means. omitempty on all three: an older consumer ignores the keys and
	// writes the row as an "llm" row with zero tokens and the right cost.
	Operation string `json:"operation,omitempty"`
	Units     *int64 `json:"units,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

// deltaKey is the coalescing / upsert key: one accumulator row per
// (scope, scope_id, period_start) — matching the table's unique constraint.
type deltaKey struct {
	scope       string
	scopeID     string
	periodStart int64
}

// key returns the coalescing key for a delta.
func (d BudgetDelta) key() deltaKey {
	return deltaKey{scope: d.Scope, scopeID: d.ScopeID, periodStart: d.PeriodStart}
}

// validate rejects a delta that cannot be durably persisted. A malformed delta
// is a poison message: the consumer Term()s it rather than redelivering forever
// (design §8.6 at-least-once redelivery is for transient failures, not poison).
func (d BudgetDelta) validate() error {
	switch {
	case d.EventID == "":
		return errors.New("empty event_id")
	case d.Scope == "":
		return errors.New("empty scope")
	case d.ScopeID == "":
		return errors.New("empty scope_id")
	case d.ProjectID < 1:
		// project_id is NOT NULL on the accumulator; a non-positive value is a
		// sentinel/bug, never a real project.
		return fmt.Errorf("non-positive project_id %d", d.ProjectID)
	case d.PeriodStart <= 0:
		return fmt.Errorf("non-positive period_start %d", d.PeriodStart)
	case d.PeriodEnd <= d.PeriodStart:
		return fmt.Errorf("period_end %d not after period_start %d", d.PeriodEnd, d.PeriodStart)
	}
	return nil
}
