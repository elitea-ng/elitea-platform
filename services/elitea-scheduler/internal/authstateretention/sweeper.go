// Package authstateretention bounds the browser sign-in state tables of
// elitea-main with batched deletes:
//
//   - elitea_auth.form_sessions, form_login_transactions and
//     browser_attempt_windows (shared migration 0145): the Form graph's
//     sessions, one-time login transactions and attempt windows;
//   - elitea_auth.browser_sessions (shared migration 0117): the OIDC and
//     SAML planes' sessions. Its store has a DeleteExpired that nothing ever
//     called, so until this sweeper the table grew without bound.
//
// Correctness never depends on this sweeper. Every read in elitea-main
// filters on the expiry column, so an expired row is already absent to it.
// What the sweeper adds is that the tables stop growing.
//
// It honours the maintenance gate like the audit and sync sweepers: an
// operator who froze writes for maintenance gets no deletes from here either.
package authstateretention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Defaults.
const (
	DefaultInterval          = 5 * time.Minute
	DefaultBatchSize         = 1000
	DefaultMaxBatchesPerPass = 50
	// expiryGrace keeps an expired Form row an hour past its expiry, so a
	// request racing the boundary and an operator reading a just-failed
	// sign-in still find it.
	expiryGrace = time.Hour
	// BrowserSessionGrace keeps an expired OIDC or SAML session a day, so a
	// support question about a sign-out ("why was I signed out?") still has
	// the row (0117's header).
	BrowserSessionGrace = 24 * time.Hour
)

// Store is the database seam (a *pgxpool.Pool satisfies it).
type Store interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Gate reports an active maintenance window (maintenance.(*Switch).Active).
type Gate func(ctx context.Context) bool

// Config tunes the sweeper.
type Config struct {
	Interval          time.Duration
	BatchSize         int
	MaxBatchesPerPass int
}

type step struct {
	// name is the table's qualified name; to_regclass reads it.
	name string
	sql  string
	// grace is how long past its expiry a row is kept.
	grace time.Duration
}

// Each statement takes $1 (the cutoff) and $2 (the batch size), deletes at
// most $2 rows, and answers how many it deleted. SKIP LOCKED leaves a row that
// a sign-in holds right now for the next pass.
var steps = []step{
	{
		name: "elitea_auth.form_sessions",
		sql: `
			WITH doomed AS (
			    SELECT id FROM elitea_auth.form_sessions
			    WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM elitea_auth.form_sessions AS victim
			    USING doomed WHERE victim.id = doomed.id RETURNING 1
			)
			SELECT count(*) FROM gone`,
		grace: expiryGrace,
	},
	{
		name: "elitea_auth.form_login_transactions",
		sql: `
			WITH doomed AS (
			    SELECT id FROM elitea_auth.form_login_transactions
			    WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM elitea_auth.form_login_transactions AS victim
			    USING doomed WHERE victim.id = doomed.id RETURNING 1
			)
			SELECT count(*) FROM gone`,
		grace: expiryGrace,
	},
	{
		name: "elitea_auth.browser_attempt_windows",
		sql: `
			WITH doomed AS (
			    SELECT key FROM elitea_auth.browser_attempt_windows
			    WHERE window_ends_at < $1 ORDER BY window_ends_at LIMIT $2 FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM elitea_auth.browser_attempt_windows AS victim
			    USING doomed WHERE victim.key = doomed.key RETURNING 1
			)
			SELECT count(*) FROM gone`,
		grace: expiryGrace,
	},
	{
		name: "elitea_auth.browser_sessions",
		sql: `
			WITH doomed AS (
			    SELECT id FROM elitea_auth.browser_sessions
			    WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
			), gone AS (
			    DELETE FROM elitea_auth.browser_sessions AS victim
			    USING doomed WHERE victim.id = doomed.id RETURNING 1
			)
			SELECT count(*) FROM gone`,
		grace: BrowserSessionGrace,
	},
}

// Sweeper runs the passes.
type Sweeper struct {
	store  Store
	gate   Gate
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

// New builds a sweeper.
func New(store Store, gate Gate, cfg Config, logger *slog.Logger) (*Sweeper, error) {
	if store == nil {
		return nil, errors.New("auth state retention sweep needs a store")
	}
	if gate == nil {
		return nil, errors.New("auth state retention sweep needs a maintenance gate")
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

// Stats is one pass's result.
type Stats struct {
	// Deleted is rows deleted per table.
	Deleted map[string]int64
	// Absent names tables this database does not have yet (elitea-main has
	// not applied the migration that creates them). They are skipped.
	Absent []string
	// Drained is false when a table hit MaxBatchesPerPass with rows left.
	Drained bool
	// Suppressed is true when maintenance mode stopped the pass.
	Suppressed bool
}

// Sweep runs one pass.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{Deleted: map[string]int64{}, Drained: true}
	if s.gate(ctx) {
		stats.Suppressed = true
		return stats, nil
	}
	now := s.now().UTC()
	for _, current := range steps {
		var present bool
		if err := s.store.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, current.name).Scan(&present); err != nil {
			return stats, errors.Join(errors.New("authstateretention: probe "+current.name), err)
		}
		if !present {
			stats.Absent = append(stats.Absent, current.name)
			continue
		}
		cutoff := now.Add(-current.grace)
		drained := false
		for batch := 0; batch < s.cfg.MaxBatchesPerPass; batch++ {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			var deleted int64
			if err := s.store.QueryRow(ctx, current.sql, cutoff, s.cfg.BatchSize).Scan(&deleted); err != nil {
				return stats, errors.Join(errors.New("authstateretention: "+current.name), err)
			}
			stats.Deleted[current.name] += deleted
			if deleted < int64(s.cfg.BatchSize) {
				drained = true
				break
			}
		}
		stats.Drained = stats.Drained && drained
	}
	return stats, nil
}

// Run sweeps once now and then every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("authstateretention: sweeper started", "interval", s.cfg.Interval,
		"batch_size", s.cfg.BatchSize, "max_batches_per_pass", s.cfg.MaxBatchesPerPass)
	s.runOnce(ctx)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Sweeper) runOnce(ctx context.Context) {
	timeout := s.cfg.Interval / 2
	if timeout < time.Minute {
		timeout = time.Minute
	}
	passCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stats, err := s.Sweep(passCtx)
	if err != nil {
		s.logger.Error("authstateretention: sweep pass failed", "err", err)
		return
	}
	if stats.Suppressed {
		s.logger.Info("authstateretention: maintenance mode is active; not sweeping")
		return
	}
	attributes := make([]any, 0, 2*len(stats.Deleted)+4)
	for name, count := range stats.Deleted {
		attributes = append(attributes, name, count)
	}
	attributes = append(attributes, "drained", stats.Drained)
	if len(stats.Absent) > 0 {
		// Not an error: the scheduler can start before elitea-main migrates.
		attributes = append(attributes, "absent_tables", stats.Absent)
	}
	s.logger.Info("authstateretention: sweep pass complete", attributes...)
	if !stats.Drained {
		s.logger.Warn("authstateretention: a table hit the batch ceiling with rows still expired",
			"max_batches_per_pass", s.cfg.MaxBatchesPerPass, "batch_size", s.cfg.BatchSize)
	}
}
