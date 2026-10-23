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
	// ExpectedReplicas is N, the replica count that shares the fleet-wide
	// degraded allowance: the FRESH_NEAR remaining budget and the FRESH_SAFE
	// degraded cap are both divided by it (§8.5, default 1, PerReplicaShare).
	ExpectedReplicas int
	// DegradedCapNano is the FLEET-wide degraded-window overspend cap in
	// nano-USD (§8.5). Each replica admits PerReplicaShare(cap, ExpectedReplicas)
	// of it (human decision, DECISIONS.md 2026-10-22). It comes from
	// LLM_BUDGET_NATS_DEGRADED_CAP_USD; 0 makes Decide use 10% of HardLimitNano.
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
	// JoinedDuringOutage reports this replica has not completed a NATS budget
	// read or increment in its own process lifetime. The capped degraded
	// states then give it no share and answer 503 (human decision, DECISIONS.md
	// 2026-10-22). Set by GovernanceStore per request.
	JoinedDuringOutage bool
	// DegradedReservationNano is what one admitted capped degraded request
	// reserves against the replica's share until it is billed
	// (LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD). 0 means
	// DefaultDegradedReservationNano. The reservation is never larger than the
	// share. Used by GovernanceStore, not by Decide.
	DegradedReservationNano int64
}

// DefaultDegradedReservationNano is the reservation per admitted degraded
// request when none is configured: 0.10 USD.
const DefaultDegradedReservationNano int64 = NanoUSD / 10

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
	// Capped reports the decision came from a state where this replica may
	// spend only its share of a fleet allowance (FRESH_SAFE, FRESH_NEAR).
	Capped bool
	// ReplicaShareNano is that share in nano-USD (valid when Capped). The
	// caller reserves against it before it dispatches the request.
	ReplicaShareNano int64
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
		return cappedDecision(StateDownPGFreshNear,
			PerReplicaShare(limit-accumulated, p.ExpectedReplicas),
			replicaDegradedNano, reqCostNano, p)

	default:
		// FRESH_SAFE: allow, bounded by this replica's share of the FLEET
		// degraded overspend cap, so a long outage across replicas cannot run
		// unbounded (§8.5).
		//
		// Design §8.5 mandates a 10% default cap when the operator has not
		// configured an explicit DegradedCapNano (i.e. DegradedCapNano == 0).
		// Derive it as max(1, limit/10): integer division of limit<10 nanoUSD
		// floors to 0, which once disabled the cap entirely (Fix round-3 #11).
		//
		// The cap is the bound for the WHOLE FLEET, not for one replica (human
		// decision, DECISIONS.md 2026-10-22). Each replica counts its degraded
		// spend in process and cannot see the others, so each one admits only
		// PerReplicaShare(cap, ExpectedReplicas). Before this decision every
		// replica admitted the full cap, and N replicas overspent by N x cap.
		fleetCap := p.DegradedCapNano
		if fleetCap <= 0 {
			fleetCap = max(1, limit/10)
		}
		return cappedDecision(StateDownPGFreshSafe,
			PerReplicaShare(fleetCap, p.ExpectedReplicas),
			replicaDegradedNano, reqCostNano, p)
	}
}

// cappedDecision is the shared tail of FRESH_SAFE and FRESH_NEAR: the two
// states where this replica may spend only its share of a fleet allowance.
//
// A replica that has not completed a NATS budget read or increment in its own
// process lifetime (Params.JoinedDuringOutage) gets NO share and answers 503.
// N in the share counts the replicas that were live when the outage began; a
// pod that starts during the outage was not one of them, and a fresh share
// for it would let each restart add one more share to the fleet total. 503
// and not 402: the budget is not known to be exhausted, the replica cannot
// tell.
//
// The Decision carries the share so the caller can reserve against it
// (GovernanceStore.CheckBudget, DegradedCounters.TryReserve).
func cappedDecision(state State, share, spentNano, reqCostNano int64, p Params) Decision {
	if p.JoinedDuringOutage {
		return Decision{Verdict: Block503, State: state, Degraded: true, Capped: true}
	}
	d := Decision{State: state, Degraded: true, Capped: true, ReplicaShareNano: share}
	if overShare(spentNano, reqCostNano, share) {
		d.Verdict = Block402
		return d
	}
	d.Verdict = Allow
	return d
}

// PerReplicaShare is the part of a fleet-wide degraded allowance that ONE
// replica may spend while NATS is down: floor(total / replicas).
//
// The rounding is FLOOR on purpose. replicas x floor(total/replicas) <= total
// for every input, so the fleet can never spend more than total. Ceiling would
// let the fleet exceed total by up to replicas-1 nano-USD.
//
// The minimum share is 0. When total < replicas the share is 0, and the
// replica admits no degraded request for that scope (402, overShare) until
// NATS returns.
// Any positive minimum m would let replicas x m exceed total. A share of 0 is
// the conservative result: such a cap is below one nano-USD per replica.
//
// replicas < 1 is read as 1 so the function never divides by zero. The boot
// check (LLM_BUDGET_EXPECTED_REPLICAS, cmd/elitea-llm-gateway) refuses such a
// value before it reaches here. A negative total yields a share of 0.
func PerReplicaShare(total int64, replicas int) int64 {
	if total <= 0 {
		return 0
	}
	n := int64(replicas)
	if n < 1 {
		n = 1
	}
	return total / n
}

// overShare reports that a degraded request must be refused.
//
//   - share <= 0: the replica has no share at all.
//   - reqCostNano == 0 (the cost is not known, which is every admission
//     check): refuse once spent >= share. A replica exactly AT its share has
//     nothing left, so "spent+0 > share" would admit one more request.
//   - reqCostNano > 0: refuse when spent + reqCostNano > share; a request that
//     fits exactly is admitted.
//
// spentNano is billed + held (DegradedCounters.Get). The reservation that
// actually bounds concurrent admissions is taken after this test, atomically,
// by DegradedCounters.TryReserve.
func overShare(spentNano, reqCostNano, shareNano int64) bool {
	if shareNano <= 0 {
		return true
	}
	if reqCostNano <= 0 {
		return spentNano >= shareNano
	}
	return spentNano+reqCostNano > shareNano
}
