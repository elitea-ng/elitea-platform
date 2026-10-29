// Package auditretention bounds the growth of `centry.audit_events`.
//
// Issue #615 gave that table its first product writer — an audit middleware
// that emits for administrative and security-relevant requests. It shipped
// deliberately WITHOUT a retention sweeper and said so. This package is that
// follow-up (issue #619).
//
// # What it does, and the one thing it must not do
//
// It DELETES rows whose `timestamp` is older than a window, in bounded batches,
// on a timer. It never inserts, never updates, and never reads a row's content.
// The audit table has exactly one writer — the middleware in elitea-main — and
// a sweeper that also rewrote rows would be a second author of the trail's
// meaning, which is the failure this package is written to avoid.
//
// Deletion is HARD, not an archive. An archive needs a destination, a format
// and a restore path, and this platform has decided none of the three; a
// sweeper that wrote rows to a second table would only move the unbounded
// growth, and one that wrote them to object storage would invent a retention
// policy for the artifact store on the way past. When an archive destination is
// chosen, this package is where it plugs in.
//
// # Why it lives in elitea-scheduler and not in elitea-main
//
// elitea-main's scheduling kernel owns PRODUCT schedule occurrences: work with
// a ledger, a lease epoch and a fence, because a missed or doubled occurrence
// is visible to a user. A retention sweep has none of those properties. It is
// idempotent, it carries no cursor (the cutoff is recomputed from the clock on
// every pass), and a pass that is skipped costs nothing but a later pass. It is
// the same shape as this daemon's two other independent workers — price sync
// and budget write-back — and it is started the same way: its own loop, from
// cmd/elitea-scheduler, with no `centry.schedule` row anywhere near it. See
// services/elitea-scheduler/RETIREMENT.md: the "two clocks" prohibition is
// about `centry.schedule` jobs that elitea-main also registers, and this is not
// one.
//
// # How a pass is bounded
//
// Three limits, and each answers a different way a DELETE goes wrong:
//
//   - BatchSize bounds ONE statement, so no single DELETE takes a lock on an
//     unbounded number of rows. `ix_audit_events_timestamp` is the index the
//     inner SELECT uses.
//   - MaxBatchesPerPass bounds ONE pass, so the first pass after a
//     long-unbounded deployment upgrades cannot spend hours inside the
//     database. The remainder is removed by the next pass, and the log says the
//     pass did not drain.
//   - Interval bounds how often a pass starts.
//
// `FOR UPDATE SKIP LOCKED` in the inner select is what makes a second replica
// harmless rather than merely unlikely: two sweepers select disjoint row sets
// instead of blocking on each other. That is also why there is no distributed
// lock here: a duplicated DELETE removes rows that were going to be removed
// anyway.
//
// # The floor
//
// A window shorter than MinimumRetentionDays is REFUSED, not clamped, and the
// sweeper does not start. `AUDIT_RETENTION_DAYS=1` is far more likely to be a
// typo than a policy, and the cost of acting on the typo is a destroyed audit
// trail that no later configuration change brings back. Refusing is loud and
// reversible; clamping silently enforces a window the operator did not ask for.
//
// # Retention never outruns the SIEM export
//
// When elitea-main exports audit events to a SIEM (ADR-0042 decision 3), each
// export sink keeps its position in elitea_runtime.audit_export_cursors
// (stream 'admin_audit'). A pass reads the slowest sink's position first and
// deletes only rows at or before it, so an expired row that has not yet
// reached every receiver — during a long SIEM outage, say — stays until it
// has. The table check and the read are separate statements: PostgreSQL
// rejects a statement that names a missing table even under a false WHERE,
// and the table exists only once migration 0196 ran. No row for the stream
// means the export is off and nothing is held. A read failure fails the pass:
// a destructive pass with an unknown export position deletes nothing.
//
// Switching the export off leaves its positions behind, and they keep holding
// rows back; the pass logs a warning once the position is older than the
// window. Delete the stream's rows to release them:
// DELETE FROM elitea_runtime.audit_export_cursors WHERE stream = 'admin_audit'.
//
// # Where the window lives, and why it is not an admin setting
//
// Issue #619 asked for a decision between a `centry.platform_config` key, which
// the admin Configuration page would write, and deployment configuration. It is
// DEPLOYMENT CONFIGURATION: `AUDIT_RETENTION_DAYS` and the three pass bounds,
// set in deploy/helm/elitea/values.yaml's `scheduler.env` map and in
// deploy/docker-compose.yml. Three reasons, in the order they weigh:
//
//   - A retention window is a data-DESTRUCTION control. As an admin field it
//     needs its own permission, a floor the form enforces, and an audit event
//     recording the change — and that last one is a row in the very table the
//     change is about to shorten the life of. None of that is built, and half
//     of it is worse than none.
//   - Only this process acts on the value. An admin field is read by
//     elitea-main, so publishing it would put the number in two processes with
//     nothing comparing them. That is the divergence the admin Configuration
//     page already documents for `ai_project_id`, which is read independently
//     by elitea-main and by the LLM gateway.
//   - The value is not hidden. It is in the operator's own values file, the
//     sweeper states it in its start-up line, and every pass logs the window
//     and the cutoff it enforced.
//
// So the admin Audit Trail page gains nothing here and no platform-settings key
// is added. If the window later becomes an operator-editable setting, this
// package reads it from `centry.platform_config` the way
// internal/maintenance reads the maintenance switch, the env value
// becomes the default for a deployment that has never set it, and the admin
// field owes the permission, the floor and the change record listed above.
package auditretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-scheduler/internal/retentionpolicy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MinimumRetentionDays is the shortest window this sweeper acts on. See the
// package doc: below it the sweeper refuses to start rather than clamping.
const MinimumRetentionDays = 7

// Default bounds for a pass. Applied when a caller leaves the field at zero, so
// a partly filled Config cannot produce a sweeper that deletes everything in
// one statement or spins with a batch size of nothing.
const (
	DefaultInterval          = time.Hour
	DefaultBatchSize         = 1000
	DefaultMaxBatchesPerPass = 50
)

// ErrDisabled reports a window of zero or less: the operator has switched the
// sweep off. It is a distinct error because the caller must log it differently
// from a misconfiguration — the table is then unbounded, which is a state worth
// naming in the log rather than passing over in silence.
var ErrDisabled = errors.New("audit retention sweep is disabled")

// ErrWindowTooShort reports a positive window below MinimumRetentionDays.
var ErrWindowTooShort = errors.New("audit retention window is shorter than the floor")

// deleteBatchSQL removes at most $2 rows older than $1.
//
// The inner SELECT is ordered by `timestamp`, so a batch is always the OLDEST
// rows. That is what makes a pass which hits its batch ceiling still make
// progress from the correct end of the table. `FOR UPDATE SKIP LOCKED` lets a
// second sweeper take a disjoint set instead of waiting.
//
// The table name is a literal, not a parameter: this package sweeps exactly one
// table, and a configurable table name would be a way to point a bulk DELETE at
// a different one.
const deleteBatchSQL = `
	WITH doomed AS (
	    SELECT id
	      FROM centry.audit_events
	     WHERE timestamp < $1
	     ORDER BY timestamp
	     LIMIT $2
	       FOR UPDATE SKIP LOCKED
	)
	DELETE FROM centry.audit_events AS victim
	 USING doomed
	 WHERE victim.id = doomed.id`

// deleteBatchHeldSQL is deleteBatchSQL for a deployment that exports audit
// events: only rows at or before the slowest export position ($3, $4 in
// (export_txid, id) order) may go.
const deleteBatchHeldSQL = `
	WITH doomed AS (
	    SELECT id
	      FROM centry.audit_events
	     WHERE timestamp < $1
	       AND (export_txid, id) <= ($3::bigint, $4::bigint)
	     ORDER BY timestamp
	     LIMIT $2
	       FOR UPDATE SKIP LOCKED
	)
	DELETE FROM centry.audit_events AS victim
	 USING doomed
	 WHERE victim.id = doomed.id`

// exportCursorTableSQL and exportPositionSQL are two statements on purpose:
// see the package doc.
const (
	exportCursorTableSQL = `SELECT to_regclass('elitea_runtime.audit_export_cursors') IS NOT NULL`
	exportPositionSQL    = `
	SELECT last_txid, last_id, updated_at
	  FROM elitea_runtime.audit_export_cursors
	 WHERE stream = 'admin_audit'
	 ORDER BY last_txid, last_id
	 LIMIT 1`
)

// Store is the slice of *pgxpool.Pool this package uses. It deletes rows, and
// reads only the SIEM export's position.
type Store interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ExportHold is the SIEM export position a pass did not delete past.
type ExportHold struct {
	TxID, ID  int64
	UpdatedAt time.Time
}

// Gate reports whether the platform is in a maintenance window. It is supplied
// by the caller — maintenance.(*Switch).Active — rather than
// re-implemented here, because the `centry.platform_config` section and key it
// reads are a database contract with no compiler behind them, and a second copy
// of those strings is a second thing that can drift.
type Gate func(ctx context.Context) bool

// Config is the sweeper's shape. RetentionDays is the operator's knob; the rest
// bound one pass.
type Config struct {
	RetentionDays     int
	Interval          time.Duration
	BatchSize         int
	MaxBatchesPerPass int
}

// Stats is what one pass did. Returned so a test asserts the numbers rather
// than the absence of an error, and logged so an operator sees the same ones.
type Stats struct {
	// Suppressed is true when a maintenance window stopped the pass before it
	// deleted anything.
	Suppressed bool
	// Deleted counts rows actually removed, summed over the pass's batches.
	Deleted int64
	// Batches counts DELETE statements issued.
	Batches int
	// Drained is true when the pass reached the end of the eligible rows — a
	// batch came back short. False means the pass hit MaxBatchesPerPass with
	// work still to do.
	Drained bool
	// Cutoff is the instant the pass compared against, kept so the log line and
	// a test agree on which window was enforced.
	Cutoff time.Time
	// WindowDays is the window the pass enforced: the administrator's saved
	// retention policy when there is one, else RetentionDays.
	WindowDays int
	// KeptForever is true when the pass deleted nothing because the policy
	// (or a disabled deployment value) keeps the rows forever.
	KeptForever bool
	// HeldForExport is the SIEM export position the pass stopped at; nil when
	// the export is off.
	HeldForExport *ExportHold
}

// Sweeper deletes expired audit rows on a timer.
type Sweeper struct {
	store  Store
	gate   Gate
	cfg    Config
	policy retentionpolicy.Reader
	logger *slog.Logger
	now    func() time.Time
}

// NewWithPolicy is New for a sweeper that follows the administrator's
// retention policy for `admin_audit` (ADR-0042 decision 4) once it is saved,
// and RetentionDays until then. RetentionDays of zero is then not a refusal:
// the sweeper runs and deletes nothing until a policy sets a maximum.
func NewWithPolicy(store Store, gate Gate, cfg Config, policy retentionpolicy.Reader, logger *slog.Logger) (*Sweeper, error) {
	if policy == nil {
		return New(store, gate, cfg, logger)
	}
	envDays := cfg.RetentionDays
	if envDays <= 0 {
		cfg.RetentionDays = MinimumRetentionDays // passes New's checks; never used as a window
	}
	s, err := New(store, gate, cfg, logger)
	if err != nil {
		return nil, err
	}
	s.cfg.RetentionDays = envDays
	s.policy = policy
	return s, nil
}

// exportHold reads the slowest SIEM export position, or nil when the export
// is off (no cursor table, or no row for the stream).
func (s *Sweeper) exportHold(ctx context.Context) (*ExportHold, error) {
	var exists bool
	if err := s.store.QueryRow(ctx, exportCursorTableSQL).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var hold ExportHold
	err := s.store.QueryRow(ctx, exportPositionSQL).Scan(&hold.TxID, &hold.ID, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &hold, nil
}

// windowDays resolves the window of one pass. ok is false when the pass must
// delete nothing.
func (s *Sweeper) windowDays(ctx context.Context) (days int, ok bool, err error) {
	days = s.cfg.RetentionDays
	if s.policy != nil {
		decision, err := s.policy(ctx)
		if err != nil {
			// A destructive pass with an unknown policy deletes nothing.
			return 0, false, err
		}
		if decision.Set {
			if decision.MaxAgeDays == nil {
				return 0, false, nil
			}
			days = *decision.MaxAgeDays
		}
	}
	if days <= 0 {
		return 0, false, nil
	}
	if days < MinimumRetentionDays {
		return 0, false, fmt.Errorf("%w: %d days, floor is %d", ErrWindowTooShort, days, MinimumRetentionDays)
	}
	return days, true, nil
}

// New builds a Sweeper, or explains why there will not be one.
//
// The gate is REQUIRED, not optional. An optional gate is how a sweeper comes
// to be composed without one and then deletes straight through a maintenance
// window — the dead-wiring shape this repository has met often enough to stop
// offering the option.
func New(store Store, gate Gate, cfg Config, logger *slog.Logger) (*Sweeper, error) {
	if store == nil {
		return nil, errors.New("audit retention sweep needs a store")
	}
	if gate == nil {
		return nil, errors.New("audit retention sweep needs a maintenance gate")
	}
	if cfg.RetentionDays <= 0 {
		return nil, ErrDisabled
	}
	if cfg.RetentionDays < MinimumRetentionDays {
		return nil, fmt.Errorf("%w: %d days, floor is %d",
			ErrWindowTooShort, cfg.RetentionDays, MinimumRetentionDays)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxBatchesPerPass <= 0 {
		cfg.MaxBatchesPerPass = DefaultMaxBatchesPerPass
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweeper{store: store, gate: gate, cfg: cfg, logger: logger, now: time.Now}, nil
}

// Window is the retention window this sweeper enforces.
func (s *Sweeper) Window() time.Duration {
	return time.Duration(s.cfg.RetentionDays) * 24 * time.Hour
}

// runOnce executes one pass under its own timeout, so a pass that meets a
// wedged database cannot hold the loop past the next tick.
func (s *Sweeper) runOnce(ctx context.Context) (string, error) {
	passCtx, cancel := context.WithTimeout(ctx, s.passTimeout())
	defer cancel()

	started := s.now()
	stats, err := s.Sweep(passCtx)
	elapsed := s.now().Sub(started)

	if err != nil {
		// Reported, not swallowed, and the pass is not retried before the next
		// tick: a failing DELETE retried immediately turns one bad pass into a
		// busy loop against a database that is already unwell.
		s.logger.Error("auditretention: sweep pass failed",
			"err", err,
			"window_days", stats.WindowDays,
			"deleted_before_failure", stats.Deleted,
			"batches", stats.Batches)
		return routines.OutcomeError, err
	}
	if stats.Suppressed {
		s.logger.Info("auditretention: maintenance mode is active; not sweeping",
			"window_days", s.cfg.RetentionDays)
		return routines.OutcomeMaintenance, nil
	}
	if stats.KeptForever {
		s.logger.Info("auditretention: the retention policy keeps audit events forever; not sweeping")
		return routines.OutcomeOK, nil
	}

	// One line per pass, always — this is the only place an operator sees that
	// the bound is enforced and by how much. A line emitted only when rows were
	// deleted would make "the sweeper is running and there is nothing to
	// delete" and "the sweeper never started" identical in the log.
	s.logger.Info("auditretention: sweep pass complete",
		"window_days", stats.WindowDays,
		"cutoff", stats.Cutoff.Format(time.RFC3339),
		"deleted", stats.Deleted,
		"batches", stats.Batches,
		"drained", stats.Drained,
		"held_for_export", stats.HeldForExport != nil,
		"duration_ms", elapsed.Milliseconds())

	if hold := stats.HeldForExport; hold != nil && hold.UpdatedAt.Before(stats.Cutoff) {
		// Expired rows the SIEM export has not delivered are being kept. During
		// an outage that is the point; if the export was switched off, its
		// positions must be deleted to release them.
		s.logger.Warn("auditretention: the SIEM export position has not moved within the retention window; expired rows after it are kept",
			"export_position_updated_at", hold.UpdatedAt.Format(time.RFC3339),
			"export_txid", hold.TxID, "export_id", hold.ID,
			"hint", "if audit export was switched off: DELETE FROM elitea_runtime.audit_export_cursors WHERE stream = 'admin_audit'")
	}

	if !stats.Drained {
		// The pass hit its ceiling with rows still eligible. Benign once — the
		// backlog of a deployment that has never swept — and the signal to
		// watch if it repeats: the issue's own next step is monthly RANGE
		// partitioning on `timestamp`, which is not to be done pre-emptively.
		s.logger.Warn("auditretention: pass ended at its batch ceiling with rows still expired",
			"deleted", stats.Deleted,
			"max_batches_per_pass", s.cfg.MaxBatchesPerPass,
			"batch_size", s.cfg.BatchSize)
	}
	return routines.OutcomeOK, nil
}

// Pass runs one pass and reports its outcome word; the routine registry loop
// (libs/go/routines) calls it on the operator-configured schedule.
func (s *Sweeper) Pass(ctx context.Context) (string, error) { return s.runOnce(ctx) }

// passTimeout keeps a pass inside its own tick: half the interval, so a hung
// pass has ended before the next one starts, with a floor for the very short
// intervals only a test sets.
func (s *Sweeper) passTimeout() time.Duration {
	half := s.cfg.Interval / 2
	if half < time.Minute {
		return time.Minute
	}
	return half
}

// Sweep runs one pass and reports what it did.
//
// Exported so a test drives a pass directly, with a clock it chose, instead of
// waiting on a ticker.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{}

	// Asked ONCE per pass. A pass
	// is bounded to MaxBatchesPerPass batches, so it cannot straddle a window
	// for long, and asking per batch would put a point read in front of every
	// DELETE for a window that opens a few times a year.
	if s.gate(ctx) {
		stats.Suppressed = true
		return stats, nil
	}

	days, ok, err := s.windowDays(ctx)
	if err != nil {
		return stats, err
	}
	if !ok {
		stats.KeptForever = true
		return stats, nil
	}
	stats.WindowDays = days
	stats.Cutoff = s.now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	hold, err := s.exportHold(ctx)
	if err != nil {
		return stats, fmt.Errorf("read the audit export position: %w", err)
	}
	stats.HeldForExport = hold
	statement, args := deleteBatchSQL, []any{stats.Cutoff, s.cfg.BatchSize}
	if hold != nil {
		statement, args = deleteBatchHeldSQL, []any{stats.Cutoff, s.cfg.BatchSize, hold.TxID, hold.ID}
	}

	for stats.Batches < s.cfg.MaxBatchesPerPass {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		tag, err := s.store.Exec(ctx, statement, args...)
		if err != nil {
			return stats, err
		}
		removed := tag.RowsAffected()
		stats.Batches++
		stats.Deleted += removed
		if removed < int64(s.cfg.BatchSize) {
			// A short batch means the eligible rows are gone. This is the only
			// exit that may claim the pass drained the table.
			stats.Drained = true
			return stats, nil
		}
	}
	return stats, nil
}
