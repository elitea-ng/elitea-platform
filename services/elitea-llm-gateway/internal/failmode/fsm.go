// Package failmode implements the tiered-hybrid NATS-failure FSM (design §8.5).
//
// When NATS JetStream is unreachable the governance store can neither increment
// nor read the authoritative budget counter. A naive global fail-open risks
// uncontrolled provider overspend (real money); a naive global fail-closed turns
// a NATS blip into a total /llm outage. The resolved policy is a tiered-hybrid
// FSM that uses the Postgres snapshot (gateway.llm_budget_accumulators) as a
// bounded fallback tier, gated by a per-replica in-process overspend cap.
//
// This package owns three concerns, each behind a narrow, offline-testable seam:
//
//   - Decide (fsm.go): the pure §8.5 state table. Given the breaker state, a PG
//     snapshot, the resolved fail-mode, and the per-replica degraded counter, it
//     returns allow / 402 / 503 with the FSM state that produced it. No I/O.
//   - Snapshot / outage-window persistence (store.go): the point-read of the
//     applicable accumulator row and the delta-accumulating outage UPSERT
//     (outage_mode=true), behind a pgx seam mirroring the account/budgetwriteback
//     packages.
//   - Recovery reconciliation (recovery.go): the breaker→CLOSED one-shot that
//     replays outage spend onto the recovered NATS counter (crash-safe,
//     idempotent) and resets the per-replica degraded counter.
//
// It is deliberately NOT wired into the request path here — that is the
// GovernanceStore's job (BF0.4 s4). This package provides the enforcement
// primitives s4 composes.
package failmode

import "time"

// NanoUSD is the nano-USD scale factor: 1 USD = 1e9 nano-USD. Budget counters
// are int64 nano-USD; hard limits are authored in USD and scaled by this for
// counter comparison (design §8, "int64 nano-USD (NanoUSD = 1e9)").
const NanoUSD int64 = 1_000_000_000

// State is a §8.5 FSM state — the reason a Decision was reached.
type State int

const (
	// StateNATSHealthy: the authoritative NATS counter was read successfully.
	StateNATSHealthy State = iota
	// StateDownPGFreshSafe: NATS down, snapshot fresh, spend < soft-alert pct.
	StateDownPGFreshSafe
	// StateDownPGFreshNear: NATS down, snapshot fresh, spend in [soft, 100%).
	StateDownPGFreshNear
	// StateDownPGFreshOver: NATS down, snapshot fresh, spend ≥ limit → 402.
	StateDownPGFreshOver
	// StateDownPGStale: NATS down, snapshot too old (or PG down) → 503.
	StateDownPGStale
	// StateForcedClosed: NATS down beyond the max-duration ceiling → 503.
	StateForcedClosed
)

// String renders the FSM state name used in the design's §8.5 table and in the
// degraded-mode alarm / metric labels.
func (s State) String() string {
	switch s {
	case StateNATSHealthy:
		return "NATS_HEALTHY"
	case StateDownPGFreshSafe:
		return "NATS_DOWN_PG_FRESH_SAFE"
	case StateDownPGFreshNear:
		return "NATS_DOWN_PG_FRESH_NEAR"
	case StateDownPGFreshOver:
		return "NATS_DOWN_PG_FRESH_OVER"
	case StateDownPGStale:
		return "NATS_DOWN_PG_STALE"
	case StateForcedClosed:
		return "NATS_DOWN_FORCED_CLOSED"
	default:
		return "UNKNOWN"
	}
}

// Verdict is the enforcement outcome the governance PreLLMHook applies.
type Verdict int

const (
	// Allow: let the request proceed.
	Allow Verdict = iota
	// Block402: budget exhausted — HTTP 402 (budget_exceeded / insufficient_quota).
	Block402
	// Block503: infrastructure failure (stale/down snapshot, forced-closed) —
	// HTTP 503 (api_error), NOT a budget decision.
	Block503
)

// FailMode is the resolved per-request NATS-failure policy (§8.5). It is the
// per-project override when present, else the platform baseline.
type FailMode string

const (
	// ModeTieredHybrid is the default policy (the full §8.5 table).
	ModeTieredHybrid FailMode = "tiered_hybrid"
	// ModeFailOpen allows all traffic while NATS is down (short of FORCED_CLOSED).
	ModeFailOpen FailMode = "fail_open"
	// ModeFailClosed blocks (402) all traffic while NATS is down.
	ModeFailClosed FailMode = "fail_closed"
)

// ResolveFailMode picks the effective policy: a valid per-project override wins,
// otherwise the platform baseline. An unrecognised value falls back to the
// baseline so a bad row cannot silently disable enforcement.
func ResolveFailMode(perProject string, baseline FailMode) FailMode {
	switch FailMode(perProject) {
	case ModeTieredHybrid, ModeFailOpen, ModeFailClosed:
		return FailMode(perProject)
	default:
		return baseline
	}
}

// Snapshot is the point-read of the applicable budget scope from the Postgres
// durable tier (gateway.project_budget + gateway.llm_budget_accumulators),
// captured at the moment the FSM is consulted.
type Snapshot struct {
	// IsUnlimited: the project has no budget cap. Always allowed while NATS is
	// down (except FORCED_CLOSED) — there is no threshold to protect (§8.5).
	IsUnlimited bool
	// HardLimitNano is the budget ceiling in nano-USD (0 when IsUnlimited).
	HardLimitNano int64
	// AccumulatedNano is the durable-tier spend for the current period in
	// nano-USD (healthy-path + already-reconciled outage spend).
	AccumulatedNano int64
	// SoftAlertPct is the soft-alert threshold percent (1..100); the boundary
	// between FRESH_SAFE and FRESH_NEAR. 0 is treated as the 80% default.
	SoftAlertPct int
	// Age is how old this snapshot is (now − last_updated). ≥ freshness window
	// ⇒ NATS_DOWN_PG_STALE (§8.5). A zero LastUpdated (row never written) yields
	// a very large Age via the caller, so it is treated as stale.
	Age time.Duration
	// Found reports whether an accumulator row existed. A missing row for a
	// budgeted project means no spend yet this period (Accumulated=0, fresh).
	Found bool
	// NatsFailMode is the per-project nats_fail_mode override read from
	// gateway.project_budget. Empty string means "inherit the platform baseline".
	// The governance layer calls ResolveFailMode(snap.NatsFailMode, baseline)
	// before invoking Decide so the per-project policy is always honoured.
	NatsFailMode FailMode
	// SoftAlertsDisabled carries the platform-wide soft-alert switch an operator
	// sets through PUT /admin/gateway/budget-alerts, read from the global
	// gateway.governance_config row by the snapshot query (issue #322).
	//
	// The field is NEGATIVE on purpose. A Snapshot built without it — every unit
	// test, every fake store, any future caller — must keep emitting alerts,
	// because that is the behaviour of a deployment that has never touched the
	// switch. A positive AlertsEnabled would make the zero value "alerts off",
	// so forgetting to set it would silently stop the one signal that tells an
	// operator a project is approaching its ceiling.
	SoftAlertsDisabled bool
}

// Params is the resolved configuration the FSM decision needs (from config plus
// the per-request snapshot). All durations/limits are pre-resolved so Decide is
// pure and trivially testable.
type Params struct {
	// Mode is the effective fail-mode (post ResolveFailMode).
	Mode FailMode
	// PGFreshness is the max snapshot age still trusted (§8.5, default 5m).
	PGFreshness time.Duration
	// ExpectedReplicas is N for the FRESH_NEAR per-replica cap (§8.5, default 1).
	ExpectedReplicas int
	// DegradedCapNano is the per-replica degraded-window overspend cap in
	// nano-USD (§8.5). Resolved from LLM_BUDGET_NATS_DEGRADED_CAP_USD or, when 0,
	// 10% of HardLimitNano by the caller.
	DegradedCapNano int64
	// DegradedMaxDuration is the continuous-outage ceiling: once the NATS
	// circuit breaker has been open longer than this the GovernanceStore sets
	// OutageExceededMax=true before calling Decide, forcing FORCED_CLOSED (§8.5,
	// LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN, default 10m). 0 disables the
	// ceiling (tests and configs that do not set the duration).
	DegradedMaxDuration time.Duration
	// OutageExceededMax reports the breaker has been open longer than
	// DegradedMaxDuration — forces closed (§8.5). Set by GovernanceStore per
	// request; never set by external callers of Decide directly.
	OutageExceededMax bool
}

// Decision is the FSM output: the enforcement verdict plus the state and the
// per-request billable nano amount the caller must add to the in-process
// degraded counter (0 unless the request is allowed in a degraded state).
type Decision struct {
	// Verdict is the enforcement outcome (Allow / Block402 / Block503).
	Verdict Verdict
	// State is the FSM state that produced the verdict (for alarms/metrics).
	State State
	// Degraded reports the decision was made in a NATS-down fallback state (any
	// state other than NATS_HEALTHY). The caller emits the degraded alarm and,
	// on an allowed degraded request, persists an outage-window delta row.
	Degraded bool
	// SoftThresholdNear is set by the NATS_HEALTHY path when the authoritative
	// counter has met or exceeded the soft-alert threshold (default 80% of
	// HardLimitNano) but has NOT yet reached the hard limit. Used by the handler's
	// post-increment soft-alert detection (Fix round-3 #6) so the 80%-crossing
	// triggers an alert on the healthy path, not only when Block402 fires.
	SoftThresholdNear bool
	// SoftAlertsDisabled forwards Snapshot.SoftAlertsDisabled so the handler's
	// soft-alert path can honour the platform switch without a second read
	// (issue #322). It is carried on every Decision, including the blocking
	// ones, because the handler consults the POST-increment decision.
	SoftAlertsDisabled bool
	// Budgeted reports that the scope has a hard ceiling: a budget row, or an
	// authored default that applies to it. It is false for an unlimited scope.
	// Decide does not set it; GovernanceStore.CheckBudget does, from the
	// snapshot it read. The audio routes read it to refuse a model the catalog
	// cannot price, rather than bill it zero against a budget (audio.go).
	Budgeted bool
}

// natsHealthy is the sentinel the caller passes as authoritativeNano to signal
// the NATS read failed (breaker open / ErrUnavailable). A separate bool is
// clearer than overloading the counter, so Decide takes natsUp explicitly.

// Decide applies the §8.5 tiered-hybrid state table. It is pure: all inputs are
// resolved values.
//
//   - natsUp: the authoritative NATS counter read succeeded. When true,
//     authoritativeNano is the running total and the decision is authoritative.
//   - authoritativeNano: the NATS counter running total (valid only if natsUp).
//   - replicaDegradedNano: this replica's in-process overspend counter for the
//     scope this period (the amount billed while NATS has been down).
//   - snap: the Postgres snapshot (used only when !natsUp).
//   - reqCostNano: the estimated cost of the request being admitted (used to
//     test the per-replica NEAR cap and the degraded cap). 0 is acceptable for a
//     pure admission check that does not pre-estimate cost.
//
// The platform soft-alert switch (issue #322) is copied from the snapshot onto
// the returned Decision here rather than at each return site, so a new state
// branch cannot forget to carry it.
func Decide(natsUp bool, authoritativeNano int64, replicaDegradedNano int64, snap Snapshot, reqCostNano int64, p Params) Decision {
	decision := decide(natsUp, authoritativeNano, replicaDegradedNano, snap, reqCostNano, p)
	decision.SoftAlertsDisabled = snap.SoftAlertsDisabled
	return decision
}

// decide is the state table itself. It never sets SoftAlertsDisabled; Decide
// does, once.
func decide(natsUp bool, authoritativeNano int64, replicaDegradedNano int64, snap Snapshot, reqCostNano int64, p Params) Decision {
	// ── NATS_HEALTHY: authoritative decision ────────────────────────────────
	if natsUp {
		if !snap.IsUnlimited && snap.HardLimitNano > 0 && authoritativeNano >= snap.HardLimitNano {
			return Decision{Verdict: Block402, State: StateNATSHealthy}
		}
		// Fix round-3 #6: set SoftThresholdNear on the NATS_HEALTHY path when
		// the authoritative counter has reached the soft-alert zone (≥ soft%).
		// This lets the handler's post-increment logic fire the 80% alert on the
		// healthy path without requiring another CheckBudget round-trip for the
		// threshold value (the snapshot carries SoftAlertPct already).
		var softNear bool
		if !snap.IsUnlimited && snap.HardLimitNano > 0 {
			softPct := snap.SoftAlertPct
			if softPct <= 0 || softPct > 100 {
				softPct = 80
			}
			softThreshold := snap.HardLimitNano * int64(softPct) / 100
			softNear = authoritativeNano >= softThreshold
		}
		return Decision{Verdict: Allow, State: StateNATSHealthy, SoftThresholdNear: softNear}
	}

	// ── NATS down. FORCED_CLOSED overrides everything, incl. is_unlimited and
	// per-project fail_open (§8.5: "signals sustained infrastructure failure"). ─
	if p.OutageExceededMax {
		return Decision{Verdict: Block503, State: StateForcedClosed, Degraded: true}
	}

	// Explicit per-project / baseline overrides (short of FORCED_CLOSED).
	switch p.Mode {
	case ModeFailClosed:
		// Contractually capped tenants: block while the counter can't be trusted.
		// Use StateDownPGFreshOver (policy/budget block) — not StateDownPGStale,
		// which is an infra-staleness label used for 503 and would pollute alarms.
		return Decision{Verdict: Block402, State: StateDownPGFreshOver, Degraded: true}
	case ModeFailOpen:
		return Decision{Verdict: Allow, State: StateDownPGFreshSafe, Degraded: true}
	}

	// Unlimited projects have no threshold to protect (§8.5).
	if snap.IsUnlimited {
		return Decision{Verdict: Allow, State: StateDownPGFreshSafe, Degraded: true}
	}

	// ── tiered_hybrid: require a fresh snapshot, else 503. A missing row is a
	// fresh zero-spend snapshot (Age is meaningless when Found is false). ──────
	if snap.Found && snap.Age >= p.PGFreshness {
		return Decision{Verdict: Block503, State: StateDownPGStale, Degraded: true}
	}

	limit := snap.HardLimitNano
	if limit <= 0 {
		// Budgeted project with no positive limit is a config error; treating it
		// as a hard zero would 402 every degraded request. Fall back to allow in
		// the SAFE state — the healthy path will re-establish the real limit.
		return Decision{Verdict: Allow, State: StateDownPGFreshSafe, Degraded: true}
	}

	accumulated := snap.AccumulatedNano
	softPct := snap.SoftAlertPct
	if softPct <= 0 || softPct > 100 {
		softPct = 80
	}
	// Multiply before divide to preserve precision (limit/100*pct loses the
	// remainder fraction and yields 0 for limit<100 nanoUSD, forcing every
	// request into FRESH_NEAR regardless of actual spend).
	// Overflow guard: limit*100 fits int64 for limits up to ~9.2e16 nanoUSD
	// ($92 M USD). Budgets above that amount are not supported by this code path;
	// callers must enforce a reasonable ceiling on HardLimitNano.
	softThreshold := limit * int64(softPct) / 100

	switch {
	case accumulated >= limit:
		// FRESH_OVER: correct block regardless of NATS state.
		return Decision{Verdict: Block402, State: StateDownPGFreshOver, Degraded: true}

	case accumulated >= softThreshold:
		// FRESH_NEAR: allow up to the per-replica share of the remaining budget.
		n := int64(p.ExpectedReplicas)
		if n < 1 {
			n = 1
		}
		perReplicaCap := (limit - accumulated) / n
		if replicaDegradedNano+reqCostNano > perReplicaCap {
			return Decision{Verdict: Block402, State: StateDownPGFreshNear, Degraded: true}
		}
		return Decision{Verdict: Allow, State: StateDownPGFreshNear, Degraded: true}

	default:
		// FRESH_SAFE: allow, bounded by the per-replica degraded overspend cap so
		// a long outage across replicas cannot run unbounded (§8.5).
		//
		// Design §8.5 mandates a 10% default cap when the operator has not
		// configured an explicit DegradedCapNano (i.e. DegradedCapNano == 0).
		// Derive it as max(1, limit/10): integer division of limit<10 nanoUSD
		// floors to 0, which disables the cap entirely (every degraded request
		// would be admitted without bound). Using max(1, …) ensures that even a
		// 1-nanoUSD budget has a non-zero cap — a 1-nanoUSD degraded limit is
		// effectively a forced-closed policy, which is the correct conservative
		// behaviour for extremely small budgets (Fix round-3 #11).
		effectiveCap := p.DegradedCapNano
		if effectiveCap <= 0 {
			effectiveCap = max(1, limit/10)
		}
		if effectiveCap > 0 && replicaDegradedNano+reqCostNano > effectiveCap {
			return Decision{Verdict: Block402, State: StateDownPGFreshSafe, Degraded: true}
		}
		return Decision{Verdict: Allow, State: StateDownPGFreshSafe, Degraded: true}
	}
}
