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
// lock here, unlike the dispatch tick — a duplicated DELETE removes rows that
// were going to be removed anyway, while a duplicated `last_run` stamp is a
// claim that work ran.
//
// # The floor
//
// A window shorter than MinimumRetentionDays is REFUSED, not clamped, and the
// sweeper does not start. `AUDIT_RETENTION_DAYS=1` is far more likely to be a
// typo than a policy, and the cost of acting on the typo is a destroyed audit
// trail that no later configuration change brings back. Refusing is loud and
// reversible; clamping silently enforces a window the operator did not ask for.
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
// internal/scheduler/maintenance.go reads the maintenance switch, the env value
// becomes the default for a deployment that has never set it, and the admin
// field owes the permission, the floor and the change record listed above.
package auditretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

// Store is the slice of *pgxpool.Pool this package uses. One method: the
// sweeper writes and never reads.
type Store interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Gate reports whether the platform is in a maintenance window. It is supplied
// by the caller — scheduler.(*Scheduler).MaintenanceActive — rather than
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
}

// Sweeper deletes expired audit rows on a timer.
type Sweeper struct {
	store  Store
	gate   Gate
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
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

// Run blocks until ctx is cancelled, sweeping once immediately and then on each
// interval tick.
//
// The first pass is immediate rather than delayed by a whole interval: the
// deployment this matters most to is the one upgrading with a table that has
// never been swept, and making it wait an hour to start buys nothing. The pass
// is bounded either way.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("auditretention: sweeper started",
		"window_days", s.cfg.RetentionDays,
		"interval", s.cfg.Interval,
		"batch_size", s.cfg.BatchSize,
		"max_batches_per_pass", s.cfg.MaxBatchesPerPass)

	s.runOnce(ctx)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("auditretention: sweeper stopping")
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce executes one pass under its own timeout, so a pass that meets a
// wedged database cannot hold the loop past the next tick.
func (s *Sweeper) runOnce(ctx context.Context) {
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
			"window_days", s.cfg.RetentionDays,
			"deleted_before_failure", stats.Deleted,
			"batches", stats.Batches)
		return
	}
	if stats.Suppressed {
		s.logger.Info("auditretention: maintenance mode is active; not sweeping",
			"window_days", s.cfg.RetentionDays)
		return
	}

	// One line per pass, always — this is the only place an operator sees that
	// the bound is enforced and by how much. A line emitted only when rows were
	// deleted would make "the sweeper is running and there is nothing to
	// delete" and "the sweeper never started" identical in the log.
	s.logger.Info("auditretention: sweep pass complete",
		"window_days", s.cfg.RetentionDays,
		"cutoff", stats.Cutoff.Format(time.RFC3339),
		"deleted", stats.Deleted,
		"batches", stats.Batches,
		"drained", stats.Drained,
		"duration_ms", elapsed.Milliseconds())

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
}

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

	// Asked ONCE per pass, as the dispatch tick asks it once per tick. A pass
	// is bounded to MaxBatchesPerPass batches, so it cannot straddle a window
	// for long, and asking per batch would put a point read in front of every
	// DELETE for a window that opens a few times a year.
	if s.gate(ctx) {
		stats.Suppressed = true
		return stats, nil
	}

	stats.Cutoff = s.now().UTC().Add(-s.Window())

	for stats.Batches < s.cfg.MaxBatchesPerPass {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		tag, err := s.store.Exec(ctx, deleteBatchSQL, stats.Cutoff, s.cfg.BatchSize)
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
