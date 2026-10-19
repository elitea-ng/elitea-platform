package budgetwriteback

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// dedupSQL records the delta's event_id in the SAME transaction as the
// accumulator UPSERT (§8.6). ON CONFLICT DO NOTHING RETURNING event_id: a
// returned row means the event is new (apply it); pgx.ErrNoRows means it was
// already applied by an earlier (committed) delivery and MUST be skipped so the
// UPSERT is not double-applied.
const dedupSQL = `INSERT INTO gateway.processed_event_ids (event_id) VALUES ($1)
	ON CONFLICT DO NOTHING RETURNING event_id`

// upsertSQL is the guarded delta-UPSERT into the durable accumulator. It adds
// the coalesced nano-USD delta (converted to USD NUMERIC exactly, in SQL) to
// the row for (scope, scope_id, period_start).
//
// The WHERE guard on DO UPDATE enforces the §8.5/§8.6 disjointness invariant:
// the write-back consumer MUST NOT touch a row still in the un-reconciled outage
// state (outage_mode=true AND reconciled=false) — those rows are owned
// exclusively by the gateway's recovery-reconciliation goroutine until it resets
// outage_mode=false. When the guard fails the DO UPDATE matches no row and
// RowsAffected()==0, which the consumer reads as "deferred": it rolls the whole
// transaction back (so the dedup rows are NOT persisted) and redelivers later.
// A fresh INSERT (new period) writes outage_mode=false and always affects 1 row.
//
// The $7::numeric / nanoUSDPerUSD is the ONE nano-USD → USD conversion point,
// done in Postgres NUMERIC (never float64) so the money path stays exact.
var upsertSQL = fmt.Sprintf(`INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / %d, false, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + EXCLUDED.accumulated_cost,
		last_updated = now()
	WHERE NOT (acc.outage_mode AND NOT acc.reconciled)`, nanoUSDPerUSD)

// usageEventSQL appends one billed request to the per-request usage ledger
// (issue #320). Same shape as the gateway's outage-window insert
// (failmode/store.go usageEventInsertSQL); both are idempotent on event_id.
//
// The nano-USD → USD conversion is the same exact NUMERIC division the
// accumulator UPSERT uses. cost_usd here and accumulated_cost there are the
// same money seen two ways — a sum of this column over a period equals that
// period's accumulator, and summing BOTH would double-count. No budget decision
// reads this table.
var usageEventSQL = fmt.Sprintf(`INSERT INTO gateway.llm_usage_events
		(event_id, project_id, user_id, provider, model,
		 prompt_tokens, completion_tokens, cost_usd, period_start, period_end,
		 occurred_at, execution_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric / %d,
		to_timestamp($9), to_timestamp($10), to_timestamp($11), $12)
	ON CONFLICT (event_id) DO NOTHING`, nanoUSDPerUSD)

// nullableExecutionID renders an absent execution id as SQL NULL. "This request
// came from no execution" and "it came from an execution whose id is the empty
// string" are different claims, and the agent breakdown groups on this column —
// so an empty string would become a nameless agent collecting every
// unattributable request in the project.
func nullableExecutionID(value string) any {
	if value == "" {
		return nil
	}
	return truncateUTF8(value, usageExecutionIDBytes)
}

// The usage-ledger column widths (shared migrations 0084 and 0101). The ledger
// row rides in the SAME transaction as the money, and Postgres refuses an
// over-long VARCHAR rather than trimming it, so one odd provider or model name
// used to fail the accumulator write with it, on every delivery ([human
// decision] 2026-10-16, "Isolate, then drop": remove the known trigger). The
// gateway's ledger writer trims to the same widths
// (services/elitea-llm-gateway/internal/failmode usageEventArgs).
const (
	usageProviderBytes    = 64
	usageModelBytes       = 128
	usageExecutionIDBytes = 128
)

// truncateUTF8 bounds value to at most limit BYTES, cut on a UTF-8 boundary,
// so it fits a VARCHAR(limit) and stays valid UTF-8.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// RetentionWindow is how long a usage-ledger row is kept (issue #320).
//
// gateway.llm_usage_events is the first table in this schema whose size follows
// call volume rather than the number of projects or periods, so it needs an
// answer to "how big does this get" that is written down and executed, not
// deferred. 400 days keeps a full year of month-over-month comparison plus a
// month of slack for a late reconciliation, and bounds the table at
// (calls per day × 400).
//
// It is a compiled constant rather than an environment variable on purpose: an
// operator cannot set it to something that never prunes, and there is no new
// setting for a values file to drift from.
const RetentionWindow = 400 * 24 * time.Hour

// pruneUsageEventsSQL deletes ledger rows past the retention window. It is
// bounded per pass (LIMIT via a sub-select) so a first run against a long-lived
// deployment cannot hold a lock over millions of rows; the loop calls it again
// on the next tick until it stops deleting.
//
// It touches gateway.llm_usage_events ONLY. The accumulator rows the deleted
// events contributed to are NOT adjusted, and must not be: the accumulator is
// the money, the ledger is the report, and rewriting a past period's spend to
// match a pruned report would corrupt the durable enforcement tier.
const pruneUsageEventsSQL = `DELETE FROM gateway.llm_usage_events
	WHERE event_id IN (
		SELECT event_id FROM gateway.llm_usage_events
		WHERE occurred_at < now() - make_interval(secs => $1)
		LIMIT $2
	)`

// pruneBatchSize bounds one prune pass.
const pruneBatchSize = 5000

// applyOutcome is the result of persisting one coalesced key-group.
type applyOutcome int

const (
	// outcomeApplied: the transaction committed. Either the summed delta was
	// applied to the accumulator, or every event in the group was already
	// applied (a no-op commit). Either way the messages are safe to ACK.
	outcomeApplied applyOutcome = iota
	// outcomeDeferred: the target accumulator row is in the un-reconciled outage
	// state, so the UPSERT matched no row. The transaction was rolled back
	// (nothing persisted, including dedup rows); the messages MUST be redelivered
	// later, after the gateway's recovery goroutine clears outage_mode.
	outcomeDeferred
)

// Store applies coalesced budget deltas to the durable accumulator tier.
type Store struct {
	db DB
}

// NewStore builds a Store over the given DB seam.
func NewStore(db DB) *Store { return &Store{db: db} }

// PruneUsageEvents deletes up to pruneBatchSize usage-ledger rows older than
// RetentionWindow and reports how many it removed. A non-zero return means
// there may be more, so the caller can run it again.
//
// It is deliberately NOT part of Apply's transaction: a delete of old reporting
// rows must never be able to roll back, retry or defer a billing write.
func (s *Store) PruneUsageEvents(ctx context.Context) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// make_interval(secs => …) rather than a cast of RetentionWindow.String():
	// Go renders a Duration as "9600h0m0s", which Postgres does not accept as
	// an interval literal, and the failure would only appear at runtime.
	deleted, err := tx.ExecAffected(ctx, pruneUsageEventsSQL,
		RetentionWindow.Seconds(), pruneBatchSize)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deleted, nil
}

// Apply persists one coalesced key-group in a single transaction (design §8.6):
//
//  1. For each delta, INSERT its event_id into processed_event_ids
//     (ON CONFLICT DO NOTHING RETURNING). Only events whose id is newly inserted
//     contribute to the summed delta — an already-applied event contributes 0,
//     so a partial redelivery of a coalesced group is never double-counted.
//  2. If at least one event is new, run the guarded delta-UPSERT for the summed
//     nano-USD delta. RowsAffected==0 ⇒ the row is outage-owned ⇒ roll back and
//     report deferred.
//  3. Commit only after the UPSERT (and dedup rows) are staged in the same
//     transaction; a crash before commit leaves neither, so redelivery re-runs
//     both idempotently.
//
// All deltas in group MUST share the same (scope, scope_id, period_start) key
// (the caller coalesces by key). group must be non-empty and pre-validated.
func (s *Store) Apply(ctx context.Context, group []BudgetDelta) (applyOutcome, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return outcomeApplied, err
	}
	// Rollback is a no-op after a successful Commit; safe to always defer.
	defer func() { _ = tx.Rollback(ctx) }()

	var sumNano int64
	var newEvents int
	for _, d := range group {
		var gotID string
		err := tx.QueryRow(ctx, dedupSQL, d.EventID).Scan(&gotID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Already applied by an earlier committed delivery — contributes 0.
			continue
		case err != nil:
			return outcomeApplied, err
		}
		newEvents++
		sumNano += d.DeltaNanoUSD

		// The usage-ledger row for this event (issue #320), written inside the
		// SAME dedup gate as the money. Putting it here rather than beside the
		// UPSERT is what stops it double-counting: an event whose id was already
		// in processed_event_ids `continue`s above, so a redelivery cannot add a
		// second row for a request that is already in the ledger.
		//
		// The insert also carries its own ON CONFLICT DO NOTHING, because the
		// gateway's outage-window path writes the same row under the same id
		// while NATS is down. Either writer may get there first; neither can
		// duplicate the other.
		if d.Usage == nil {
			continue
		}
		if _, err := tx.ExecAffected(ctx, usageEventSQL,
			d.EventID, d.ProjectID, d.Usage.UserID,
			truncateUTF8(d.Usage.Provider, usageProviderBytes),
			truncateUTF8(d.Usage.Model, usageModelBytes),
			d.Usage.PromptTokens, d.Usage.CompletionTokens,
			d.DeltaNanoUSD, d.PeriodStart, d.PeriodEnd, d.Usage.OccurredAtUnix,
			nullableExecutionID(d.Usage.ExecutionID),
		); err != nil {
			return outcomeApplied, err
		}
	}

	if newEvents == 0 {
		// Every event already applied. Nothing to UPSERT; commit the (empty)
		// transaction and ACK — a benign no-op redelivery.
		if err := tx.Commit(ctx); err != nil {
			return outcomeApplied, err
		}
		return outcomeApplied, nil
	}

	head := group[0]
	affected, err := tx.ExecAffected(ctx, upsertSQL,
		head.ProjectID, head.OrgID, head.Scope, head.ScopeID,
		head.PeriodStart, head.PeriodEnd, sumNano,
	)
	if err != nil {
		return outcomeApplied, err
	}
	if affected == 0 {
		// Outage-owned row: the guard blocked the DO UPDATE. Roll back so the
		// dedup rows staged above are discarded, then signal deferral so the
		// caller redelivers after recovery clears the outage flag.
		_ = tx.Rollback(ctx)
		return outcomeDeferred, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return outcomeApplied, err
	}
	return outcomeApplied, nil
}
