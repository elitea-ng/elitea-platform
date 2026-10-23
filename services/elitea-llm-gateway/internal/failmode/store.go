package failmode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// snapshotSQL point-reads the applicable budget scope from the durable tier
// (design §8.5). It LEFT JOINs the accumulator for the current period onto the
// project's budget config so a project with a config but no spend this period
// still returns its limit/fail-mode with a zero, fresh accumulated cost.
//
// accumulated_cost is USD NUMERIC in the durable tier; it is scaled to nano-USD
// in SQL ((accumulated_cost * NanoUSD)::bigint) so the money path stays exact
// and the FSM compares like-for-like int64 nano-USD. The snapshot age is
// computed as EXTRACT(EPOCH FROM now() - last_updated) seconds.
//
// $1 project_id, $2 scope, $3 scope_id, $4 period_start (unix seconds).
const snapshotSQL = `SELECT
		pb.is_unlimited,
		COALESCE((pb.hard_limit_usd * $5::numeric)::bigint, 0)      AS hard_limit_nano,
		COALESCE((acc.accumulated_cost * $5::numeric)::bigint, 0)   AS accumulated_nano,
		COALESCE(pb.soft_alert_pct, ga.threshold_pct, ` + defaultSoftAlertPctSQL + `) AS soft_alert_pct,
		pb.nats_fail_mode,
		acc.id IS NOT NULL                                          AS acc_found,
		COALESCE(EXTRACT(EPOCH FROM (now() - acc.last_updated)), 0) AS age_seconds,
		NOT COALESCE(ga.alerts_enabled, true)                       AS soft_alerts_disabled
	FROM gateway.project_budget pb
	LEFT JOIN gateway.llm_budget_accumulators acc
		ON acc.scope = $2 AND acc.scope_id = $3
		AND acc.period_start = to_timestamp($4)
	LEFT JOIN ` + globalAlertConfigSQL + ` ga ON true
	WHERE pb.project_id = $1`

// defaultSoftAlertPctSQL is the last-resort threshold, used when neither the
// project nor the persisted global config names one. It matches
// fsm.Decide's own 0-or-out-of-range fallback, so the two cannot disagree.
const defaultSoftAlertPctSQL = `80`

// globalAlertConfigSQL is the platform soft-alert config (issue #322): the one
// global gateway.governance_config row that PUT /admin/gateway/budget-alerts
// writes. It is joined into every snapshot read rather than cached in-process,
// because the whole defect being fixed is a value that diverged per replica.
// The lookup is a single-row unique-key probe on a table the gateway already
// opens a pool against, in the same round trip as the budget read.
//
// It is uncorrelated, so it is a plain subquery join, not LATERAL. Both fields
// are read as NULL-able: a deployment whose migration seeded no row, or an
// operator who saved only one of the two keys, falls back to the shipped
// defaults rather than to a zero that would read as "alerts off, threshold 0".
//
// BOTH CASTS ARE GUARDED, and the guard is the difference between one bad row
// and a platform outage. This subquery runs inside the snapshot read on EVERY
// /llm call, and a bare `(data->>'threshold_pct')::smallint` over a value that
// is not a number raises 22P02 — which fails the snapshot, which fails closed,
// which 503s every request for every project until somebody finds the row. The
// authoring API validates 1..100, but this JSONB column is reachable by direct
// SQL and by any future writer, so the hot path does not depend on that
// validation. A value that fails the guard reads as NULL and falls back to the
// shipped default, exactly as a missing key does.
//
// The `{1,3}` length bound is part of the guard, not decoration: '99999'
// matches `^[0-9]+$` and then overflows smallint, raising 22003 from the same
// place.
const globalAlertConfigSQL = `(
		SELECT CASE WHEN data->>'enabled' IN ('true','false')
		            THEN (data->>'enabled')::boolean END       AS alerts_enabled,
		       CASE WHEN data->>'threshold_pct' ~ '^[0-9]{1,3}$'
		             AND (data->>'threshold_pct')::int BETWEEN 1 AND 100
		            THEN (data->>'threshold_pct')::smallint END AS threshold_pct
		FROM gateway.governance_config
		WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'
		  AND enabled
	)`

// userSnapshotSQL is the per-member counterpart of snapshotSQL (issue #321).
// It answers the same question against gateway.user_budget, so the FSM's whole
// tiered-hybrid state table — including the acc_found / age_seconds durable
// tier — applies unchanged to a member cap.
//
// Three columns are derived rather than stored, because gateway.user_budget
// carries neither:
//
//   - is_unlimited: (hard_limit_usd IS NULL OR NOT enabled), the same rule the
//     elitea-main member read derives it with (budgets/user_budgets.go
//     userLimitJoin). Deriving it in both places from the same expression is
//     what keeps the API's `enforced` flag and the gateway's admission from
//     disagreeing about whether a member cap bites.
//   - nats_fail_mode: taken from the OWNING PROJECT's row. A member cap sits
//     inside a project, so it must degrade the way that project degrades; a
//     member cap that fails closed inside a fail_open project would block calls
//     the project's own policy says to admit.
//   - soft_alert_pct: the member's own value, then the global default.
//
// $1 project_id, $2 scope, $3 scope_id, $4 period_start (unix seconds),
// $5 nano-USD scale, $6 user_id.
const userSnapshotSQL = `SELECT
		(ub.hard_limit_usd IS NULL OR NOT ub.enabled)                AS is_unlimited,
		COALESCE((ub.hard_limit_usd * $5::numeric)::bigint, 0)       AS hard_limit_nano,
		COALESCE((acc.accumulated_cost * $5::numeric)::bigint, 0)    AS accumulated_nano,
		COALESCE(ub.soft_alert_pct, ga.threshold_pct, ` + defaultSoftAlertPctSQL + `) AS soft_alert_pct,
		pb.nats_fail_mode,
		acc.id IS NOT NULL                                           AS acc_found,
		COALESCE(EXTRACT(EPOCH FROM (now() - acc.last_updated)), 0)  AS age_seconds,
		NOT COALESCE(ga.alerts_enabled, true)                        AS soft_alerts_disabled
	FROM gateway.user_budget ub
	LEFT JOIN gateway.project_budget pb
		ON pb.project_id = ub.project_id
	LEFT JOIN gateway.llm_budget_accumulators acc
		ON acc.scope = $2 AND acc.scope_id = $3
		AND acc.period_start = to_timestamp($4)
	LEFT JOIN ` + globalAlertConfigSQL + ` ga ON true
	WHERE ub.project_id = $1 AND ub.user_id = $6`

// outageUpsertSQL is the delta-accumulating outage-window UPSERT (design §8.5).
// It writes outage_mode=true so the write-back consumer (§8.6) skips the row
// (its guard is NOT (outage_mode AND NOT reconciled)); the outage and healthy
// deltas for the same period then accumulate on the same row without either
// writer clobbering the other. A fresh INSERT sets reconciled=false.
//
// The nano-USD delta is converted to USD NUMERIC exactly in SQL, mirroring the
// write-back consumer's single conversion point.
//
// $1 project_id, $2 org_id, $3 scope, $4 scope_id, $5 period_start,
// $6 period_end, $7 delta_nano, $8 NanoUSD divisor.
var outageUpsertSQL = fmt.Sprintf(`INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / %d, true, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + (EXCLUDED.accumulated_cost),
		outage_mode = true,
		reconciled = false,
		reconciliation_in_progress = false,
		last_updated = now()`, NanoUSD)

// outageDedupSQL claims the billing event id for the outage-window write
// (issue #515). It is the SAME statement, against the SAME table, that the
// scheduler's write-back consumer uses (budgetwriteback.dedupSQL): whichever
// writer inserts the id first owns that event's money, and the other one adds
// nothing.
//
// This gate is what makes the outage-window write exactly-once against the
// consumer, and the recovery sweep is why it has to be. The outage branch is
// entered on a SINGLE failed counter increment, and the write-behind publish
// that follows it runs on a healthy connection and usually succeeds. So the
// same request can reach the accumulator twice: once directly here, and once
// through the delta the consumer later applies. Until issue #515 nothing
// exposed that, because the outage flag it set was never cleared and the
// consumer's guard deferred the delta for the rest of the period. Clearing the
// flag without this gate would turn a wedged row into a double-counted one,
// which is worse.
//
// pgx.ErrNoRows from the RETURNING clause means the consumer already applied
// this event. The caller then writes nothing at all — not the accumulator, not
// the usage ledger, and not the outage flag. There is no outage to record for a
// request whose money is already durable.
const outageDedupSQL = `INSERT INTO gateway.processed_event_ids (event_id) VALUES ($1)
	ON CONFLICT DO NOTHING RETURNING event_id`

// ErrNoBudgetRow is returned by ReadSnapshot when the scope has no budget
// config row at all. The caller treats a scope with no budget config as
// unlimited (there is nothing to enforce).
var ErrNoBudgetRow = errors.New("failmode: no budget row")

// The two budget scopes the gateway admits and bills against. They are the
// `scope` column of gateway.llm_budget_accumulators, and they must stay spelled
// exactly as elitea-main's budgets API spells them (internal/api/v2/budgets:
// budgetScopeProject / budgetScopeUser) — that API reads the accumulator rows
// this gateway writes, and a different spelling would report zero spend against
// a real limit.
const (
	// ScopeProject bills the whole project. Its scope_id is the numeric project
	// id as text.
	ScopeProject = "project"
	// ScopeUser bills one member within one project (issue #321). Its scope_id
	// is "{project_id}:{user_id}" — see UserScopeID.
	ScopeUser = "user"
)

// UserScopeID builds the accumulator scope_id for a member within a project.
// The "{project_id}:{user_id}" shape is fixed by elitea-main's member reads
// (budgets/user_budgets.go userBudgetPageSQL joins on
// `$1::text || ':' || member.id::text`), which have been asking for these rows
// since #246 and reading spend_available=false because nothing wrote them.
func UserScopeID(projectID, userID int) string {
	return strconv.Itoa(projectID) + ":" + strconv.Itoa(userID)
}

// UserIDFromScopeID recovers the member id from a user-scope scope_id. It
// reports false for anything that is not "{project_id}:{user_id}" with a
// positive member id, so a malformed key becomes an error rather than a lookup
// of member 0.
func UserIDFromScopeID(scopeID string) (int, bool) {
	head, rest, found := strings.Cut(scopeID, ":")
	if !found {
		return 0, false
	}
	// Both halves are validated even though only the member id is returned. The
	// caller already knows the project; checking it here is what makes this
	// function a test of "is this a well-formed member key" rather than "does
	// this string end in a number", so a truncated or mis-joined key is caught
	// instead of resolving to a real member of the wrong project.
	if projectID, err := strconv.Atoi(head); err != nil || projectID <= 0 {
		return 0, false
	}
	userID, err := strconv.Atoi(rest)
	if err != nil || userID <= 0 {
		return 0, false
	}
	return userID, true
}

// Store reads budget snapshots and persists outage-window deltas against the
// durable accumulator tier. It is the FSM's Postgres side.
type Store struct {
	db DB
}

// NewStore builds a Store over the given DB seam.
func NewStore(db DB) *Store { return &Store{db: db} }

// ReadSnapshot point-reads the applicable budget scope for a project/period.
// It runs off a transaction (a single hot-path read must not hold a tx) and is
// intended to be wrapped in the caller's OpTimeout-bounded context.
//
// The scope selects the limit table (issue #321): ScopeProject reads
// gateway.project_budget, ScopeUser reads gateway.user_budget for the member
// named in scopeID. Everything downstream — the accumulator join, the FSM, the
// write-back and the outage recovery — is already keyed by (scope, scope_id)
// and needs no per-scope branch.
func (s *Store) ReadSnapshot(ctx context.Context, projectID int, scope, scopeID string, periodStartUnix int64) (Snapshot, error) {
	var row Row
	switch scope {
	case ScopeUser:
		userID, ok := UserIDFromScopeID(scopeID)
		if !ok {
			// A user-scoped read whose scope_id is not "{project}:{member}"
			// cannot name a row. Reporting "no budget row" (⇒ unlimited) would
			// silently admit a capped member, so this is an error.
			return Snapshot{}, fmt.Errorf("failmode: malformed user scope_id %q", scopeID)
		}
		row = s.db.QueryRow(ctx, userSnapshotSQL, projectID, scope, scopeID, periodStartUnix, NanoUSD, userID)
	default:
		row = s.db.QueryRow(ctx, snapshotSQL, projectID, scope, scopeID, periodStartUnix, NanoUSD)
	}

	var (
		isUnlimited   bool
		hardLimitNano int64
		accumNano     int64
		softAlertPct  int
		natsFailMode  *string
		accFound      bool
		ageSeconds    float64
		alertsOff     bool
	)
	if err := row.Scan(&isUnlimited, &hardLimitNano, &accumNano, &softAlertPct, &natsFailMode, &accFound, &ageSeconds, &alertsOff); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, ErrNoBudgetRow
		}
		return Snapshot{}, fmt.Errorf("failmode: read snapshot: %w", err)
	}

	snap := Snapshot{
		IsUnlimited:        isUnlimited,
		HardLimitNano:      hardLimitNano,
		AccumulatedNano:    accumNano,
		SoftAlertPct:       softAlertPct,
		Found:              accFound,
		SoftAlertsDisabled: alertsOff,
	}
	// A missing accumulator row (no spend yet this period) is a fresh zero
	// snapshot; age is only meaningful when the row exists.
	if accFound {
		snap.Age = durationFromSeconds(ageSeconds)
	}
	// Populate the per-project fail-mode override so the caller can apply it
	// without a second round-trip. An empty / NULL column means "inherit baseline".
	if natsFailMode != nil {
		snap.NatsFailMode = FailMode(*natsFailMode)
	}
	return snap, nil
}

// PerProjectFailMode reads only the per-project nats_fail_mode override for a
// project, returning "" when unset (inherit the baseline). It is a thin read
// used where the caller has not already loaded a full snapshot.
func (s *Store) PerProjectFailMode(ctx context.Context, projectID int) (string, error) {
	row := s.db.QueryRow(ctx,
		`SELECT nats_fail_mode FROM gateway.project_budget WHERE project_id = $1`, projectID)
	var mode *string
	if err := row.Scan(&mode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoBudgetRow
		}
		return "", fmt.Errorf("failmode: read fail-mode: %w", err)
	}
	if mode == nil {
		return "", nil
	}
	return *mode, nil
}

// PersistOutageDelta records a billed delta directly to the durable accumulator
// as an outage-window row (outage_mode=true) — the §8.5 "outage-window
// persistence" step. It runs off the response path (the caller spawns it in a
// short-lived goroutine) so a slow Postgres does not stall the /llm stream.
//
// It opens its own transaction and claims the event id in that transaction
// (outageDedupSQL), so this write and the write-back consumer are exactly-once
// against each other on the same event: whichever gets there first adds the
// money, and the other adds none. A commit failure is returned for the caller
// to log; the in-process degraded counter remains the authoritative per-replica
// bound while degraded.
func (s *Store) PersistOutageDelta(ctx context.Context, d OutageDelta) error {
	if err := d.validate(); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Claim the event id before anything is written. A conflict means the
	// write-back consumer already applied this delta, so there is nothing to
	// add and no outage to flag; the deferred rollback discards the (empty)
	// transaction.
	var claimed string
	switch err := tx.QueryRow(ctx, outageDedupSQL, d.EventID).Scan(&claimed); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("failmode: claim outage event id: %w", err)
	}

	if _, err := tx.ExecAffected(ctx, outageUpsertSQL,
		d.ProjectID, d.OrgID, d.Scope, d.ScopeID,
		d.PeriodStart, d.PeriodEnd, d.DeltaNanoUSD,
	); err != nil {
		return fmt.Errorf("failmode: persist outage delta: %w", err)
	}
	// The usage ledger row rides the SAME transaction as the accumulator write
	// (issue #320). While NATS is down the write-behind delta cannot be
	// published, so the scheduler's consumer — the ledger's normal writer —
	// never sees this request. Without this branch every request billed during
	// an outage would be money the accumulator has and the Usage page cannot
	// account for, and the gap would be invisible: the daily chart would simply
	// be short, with no field saying so.
	if d.Usage != nil {
		if _, err := tx.ExecAffected(ctx, usageEventInsertSQL,
			usageEventArgs(d)...,
		); err != nil {
			return fmt.Errorf("failmode: persist outage usage event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failmode: commit outage delta: %w", err)
	}
	return nil
}

// healthyUpsertSQL is the write-back consumer's guarded accumulator UPSERT
// (services/elitea-scheduler/internal/budgetwriteback/store.go upsertSQL),
// byte-for-byte in effect: it writes outage_mode=false on a fresh row and adds
// to an existing row only while that row is NOT outage-owned. Zero rows
// affected means the guard blocked it.
//
// $1 project_id, $2 org_id, $3 scope, $4 scope_id, $5 period_start,
// $6 period_end, $7 delta_nano.
var healthyUpsertSQL = fmt.Sprintf(`INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / %d, false, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + EXCLUDED.accumulated_cost,
		last_updated = now()
	WHERE NOT (acc.outage_mode AND NOT acc.reconciled)`, NanoUSD)

// PersistHealthyDelta durably writes one billed delta that is ALREADY on the
// authoritative NATS counter but whose write-behind publish failed
// ([human decision] 2026-10-16, option A; DECISIONS.md "Healthy write").
//
// It is what the write-back consumer would have done with the delta, in one
// transaction: claim the event id, append the usage-ledger row, and apply the
// GUARDED healthy UPSERT, which leaves outage_mode=false. It does NOT mark the
// row outage-owned. The outage write did, and while NATS was healthy that
// made the consumer defer every other replica's delta for the scope until the
// next sweep; under load those deferrals exhausted MaxDeliver and were lost.
//
// Only when the guard blocks (the row is ALREADY outage-owned by a real
// outage) does it fall back to outageUpsertSQL, which adds to that row and
// leaves it outage-owned: the recovery pass then replays it as before, and no
// new deferral starts. The event-id claim keeps the write exactly-once against
// a lost-ack redelivery of the same delta to the consumer, in both branches.
func (s *Store) PersistHealthyDelta(ctx context.Context, d OutageDelta) error {
	if err := d.validate(); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var claimed string
	switch err := tx.QueryRow(ctx, outageDedupSQL, d.EventID).Scan(&claimed); {
	case errors.Is(err, pgx.ErrNoRows):
		// The consumer already applied this event: nothing to add.
		return nil
	case err != nil:
		return fmt.Errorf("failmode: claim healthy event id: %w", err)
	}
	affected, err := tx.ExecAffected(ctx, healthyUpsertSQL,
		d.ProjectID, d.OrgID, d.Scope, d.ScopeID,
		d.PeriodStart, d.PeriodEnd, d.DeltaNanoUSD,
	)
	if err != nil {
		return fmt.Errorf("failmode: persist healthy delta: %w", err)
	}
	if affected == 0 {
		if _, err := tx.ExecAffected(ctx, outageUpsertSQL,
			d.ProjectID, d.OrgID, d.Scope, d.ScopeID,
			d.PeriodStart, d.PeriodEnd, d.DeltaNanoUSD,
		); err != nil {
			return fmt.Errorf("failmode: persist delta onto an outage-owned row: %w", err)
		}
	}
	if d.Usage != nil {
		if _, err := tx.ExecAffected(ctx, usageEventInsertSQL,
			usageEventArgs(d)...,
		); err != nil {
			return fmt.Errorf("failmode: persist healthy usage event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failmode: commit healthy delta: %w", err)
	}
	return nil
}

// usageEventInsertSQL appends one billed request to the per-request usage
// ledger (issue #320). It is written here and, on the healthy path, by the
// scheduler's write-back consumer; both use this same shape.
//
// ON CONFLICT (event_id) DO NOTHING is what lets the two writers overlap. The
// event_id is the billing event's own id, so a delta that is both persisted
// here during an outage AND later redelivered to the consumer produces one
// ledger row, and the outage-window direct write is at-least-once by design.
//
// The nano-USD → USD conversion is the same exact NUMERIC division the
// accumulator UPSERT uses. This row is a REPORT of the same money the
// accumulator holds, never a second source of it: no budget decision reads it.
var usageEventInsertSQL = fmt.Sprintf(`INSERT INTO gateway.llm_usage_events
		(event_id, project_id, user_id, provider, model,
		 prompt_tokens, completion_tokens, cost_usd, period_start, period_end,
		 occurred_at, execution_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric / %d,
		to_timestamp($9), to_timestamp($10), to_timestamp($11), $12)
	ON CONFLICT (event_id) DO NOTHING`, NanoUSD)

// nullableExecutionID renders an absent execution id as SQL NULL. "This request
// came from no execution" and "it came from an execution with an empty id" are
// different claims, and only NULL makes the first one.
func nullableExecutionID(value string) any {
	if value == "" {
		return nil
	}
	return requestlog.Truncate(value, usageExecutionIDBytes)
}

// The usage-ledger column widths (shared migrations 0084 and 0101). The ledger
// row rides in the SAME transaction as the money, and Postgres refuses an
// over-long VARCHAR rather than trimming it, so an odd provider or model name
// would fail the accumulator write with it ([human decision] 2026-10-16,
// "Isolate, then drop": remove the known trigger). The values are trimmed to
// fit instead; the ledger is a report, the accumulator is the money.
const (
	usageProviderBytes    = 64
	usageModelBytes       = 128
	usageExecutionIDBytes = 128
)

// usageEventArgs is usageEventInsertSQL's argument list for d, with every
// bounded text column trimmed to its width.
func usageEventArgs(d OutageDelta) []any {
	return []any{
		d.EventID, d.ProjectID, d.Usage.UserID,
		requestlog.Truncate(d.Usage.Provider, usageProviderBytes),
		requestlog.Truncate(d.Usage.Model, usageModelBytes),
		d.Usage.PromptTokens, d.Usage.CompletionTokens,
		d.DeltaNanoUSD, d.PeriodStart, d.PeriodEnd, d.Usage.OccurredAtUnix,
		nullableExecutionID(d.Usage.ExecutionID),
	}
}

// UsageDimensions are the reporting dimensions of one billed request: who made
// it, against which provider and model, and how many tokens it consumed
// (issue #320).
//
// They are deliberately NOT part of the budget decision. The accumulator holds
// money per (scope, scope_id, period) and that is what admission reads; these
// fields exist so the Usage page can draw the per-day series and the per-model
// table that the LiteLLM tag ledger used to supply.
type UsageDimensions struct {
	// UserID is the member the call is attributed to, nil when the caller
	// carried no resolvable member id. Nil rather than 0: "no member" and
	// "member 0" are different claims and the per-member views must be able to
	// tell them apart.
	UserID *int
	// Provider and Model are the resolved upstream, as billed.
	Provider string
	Model    string
	// PromptTokens / CompletionTokens are the provider-REPORTED counts. A
	// response that carries no usage field yields zeros, which is what the
	// billing path already assumes; no count is ever estimated here (#79).
	PromptTokens     int64
	CompletionTokens int64
	// ExecutionID is the runtime execution the call was made from, empty when
	// the caller is not one. It is what gives the ledger an AGENT dimension:
	// the id resolves to an agent at READ time, against
	// elitea_runtime.execution_jobs, so the ledger never has to carry an agent
	// id — and never has to choose between execution_jobs' two project columns.
	ExecutionID string
	// OccurredAtUnix is when the GATEWAY billed the request, not when a writer
	// stored the row.
	//
	// It has to travel, because the two are not the same instant and the
	// difference lands on a date. The write-back consumer runs behind the
	// stream, and an outage-deferred group is redelivered for as long as the
	// accumulator row stays outage-owned. A `now()` column default would put a
	// request billed at 23:59 on the last of the month into the NEXT month's
	// day bucket, while its money went to the previous period — the per-day
	// chart would then show a day the period does not contain. Issue #214 is
	// this platform's record of that class.
	OccurredAtUnix int64
}

// OutageDelta is a billed increment persisted to the durable tier while NATS is
// down (§8.5). It mirrors the write-behind BudgetDelta's key fields so the same
// accumulator row is targeted; the write-back guard keeps the two writers off
// each other's rows.
type OutageDelta struct {
	ProjectID int
	OrgID     *int
	Scope     string
	ScopeID   string
	// EventID is the billing event id. It is required: it is the key this write
	// claims in gateway.processed_event_ids so the write-back consumer cannot
	// apply the same money a second time (issue #515), and it is the usage
	// ledger's primary key when Usage is set.
	EventID      string
	PeriodStart  int64
	PeriodEnd    int64
	DeltaNanoUSD int64
	// Usage carries the reporting dimensions for the ledger row written in the
	// same transaction (issue #320). Nil means "do not append to the ledger" —
	// which is correct for the member-scope delta of a request whose dimensions
	// the project-scope delta already recorded. Recording both would double the
	// token counts and the request count on every per-model row.
	Usage *UsageDimensions
}

func (d OutageDelta) validate() error {
	switch {
	case d.ProjectID < 1:
		return fmt.Errorf("failmode: non-positive project_id %d", d.ProjectID)
	case d.Usage != nil && d.EventID == "":
		return errors.New("failmode: usage dimensions without event_id")
	case d.Scope == "":
		return errors.New("failmode: empty scope")
	case d.ScopeID == "":
		return errors.New("failmode: empty scope_id")
	case d.EventID == "":
		// Without an id this write cannot be deduplicated against the write-back
		// consumer, and the same request would be billed twice as soon as the
		// recovery sweep hands the row back (issue #515).
		return errors.New("failmode: empty event_id")
	case d.PeriodStart <= 0:
		return fmt.Errorf("failmode: non-positive period_start %d", d.PeriodStart)
	case d.PeriodEnd <= d.PeriodStart:
		return fmt.Errorf("failmode: period_end %d not after period_start %d", d.PeriodEnd, d.PeriodStart)
	}
	return nil
}

// durationFromSeconds converts a fractional-seconds age to a time.Duration,
// guarding against a NaN/Inf from a NULL last_updated slipping through.
func durationFromSeconds(sec float64) time.Duration {
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}
