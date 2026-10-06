// Package syncretention bounds the incremental-sync tombstone tables
// (ADR-0025 WP6) with batched deletes:
//
//   - `chat_sync_tombstones` in every tenant schema `p_<id>` that has one
//     (elitea-main tenant migration 0144: conversation and message deletions
//     and lost-access markers), and
//   - `centry.notification_tombstones` (elitea-main shared migration 0144).
//
// # The window is a floor, not a preference
//
// elitea-main answers 410 `sync_cursor_expired` for a cursor older than
// changesync.TombstoneRetention — the longest offline_retention_days a
// deployment may set (90) plus seven days. A tombstone swept earlier than that
// would be missed by a cursor elitea-main still serves, so a client would keep
// a deleted conversation forever. MinimumRetentionDays is therefore a copy of
// that number (this is a separate Go module and cannot import it), and a
// configured window below it is RAISED to it with a warning: unlike the audit
// sweeper, refusing to start would leave the tables unbounded, and raising
// only keeps rows longer.
//
// Correctness does not depend on this sweeper running: tombstones that are
// kept longer are merely returned to clients again, who drop ids they do not
// hold. It honours the maintenance gate like the audit sweeper, since it
// writes to every tenant schema.
package syncretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MinimumRetentionDays mirrors elitea-main's changesync.TombstoneRetention
// (platformconfig.MaxOfflineRetentionDays + 7). Keep the two equal; the
// package test pins this value.
const MinimumRetentionDays = 97

// Pass bounds, applied when a Config field is zero.
const (
	DefaultInterval          = time.Hour
	DefaultBatchSize         = 1000
	DefaultMaxBatchesPerPass = 50
)

// tenantTablesSQL lists the tenant schemas that carry the tombstone table. The
// `^p_[0-9]+$` match is the tenant naming every other scan in the platform
// uses; a schema that never ran tenant 0144 simply is not listed.
const tenantTablesSQL = `
	SELECT namespace.nspname
	  FROM pg_class AS relation
	  JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
	 WHERE relation.relname = 'chat_sync_tombstones'
	   AND relation.relkind = 'r'
	   AND namespace.nspname ~ '^p_[0-9]+$'
	 ORDER BY namespace.nspname`

// deleteBatchSQL removes at most $2 rows older than $1 from one table, the
// oldest first, skipping rows a concurrent sweeper holds. %s is a quoted,
// catalogue-derived table name, never input.
const deleteBatchSQL = `
	WITH doomed AS (
	    SELECT id FROM %s
	     WHERE deleted_at < $1
	     ORDER BY deleted_at
	     LIMIT $2
	       FOR UPDATE SKIP LOCKED
	)
	DELETE FROM %s AS victim USING doomed WHERE victim.id = doomed.id`

// Store is the database seam (*pgxpool.Pool satisfies it).
type Store interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Gate reports an active maintenance window
// (maintenance.(*Switch).Active).
type Gate func(ctx context.Context) bool

// Config tunes the sweeper.
type Config struct {
	RetentionDays     int
	Interval          time.Duration
	BatchSize         int
	MaxBatchesPerPass int
}

// Stats is one pass's result.
type Stats struct {
	Suppressed bool
	// Deleted counts rows removed per table (schema-qualified name).
	Deleted map[string]int64
	// Drained is false when any table hit MaxBatchesPerPass with rows left.
	Drained bool
	Cutoff  time.Time
}

// Sweeper deletes expired sync tombstones on a timer.
type Sweeper struct {
	store  Store
	gate   Gate
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

// New builds a sweeper. A window below MinimumRetentionDays (including 0) is
// raised to it; raised reports that it was.
func New(store Store, gate Gate, cfg Config, logger *slog.Logger) (sweeper *Sweeper, raised bool, err error) {
	if store == nil {
		return nil, false, errors.New("sync retention sweep needs a store")
	}
	if gate == nil {
		return nil, false, errors.New("sync retention sweep needs a maintenance gate")
	}
	if cfg.RetentionDays < MinimumRetentionDays {
		raised = cfg.RetentionDays != 0
		cfg.RetentionDays = MinimumRetentionDays
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
	return &Sweeper{store: store, gate: gate, cfg: cfg, logger: logger, now: time.Now}, raised, nil
}

// Window is the enforced retention window.
func (s *Sweeper) Window() time.Duration {
	return time.Duration(s.cfg.RetentionDays) * 24 * time.Hour
}

// Sweep runs one pass.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{Deleted: map[string]int64{}, Drained: true}
	if s.gate(ctx) {
		stats.Suppressed = true
		return stats, nil
	}
	stats.Cutoff = s.now().UTC().Add(-s.Window())

	tables, err := s.tables(ctx)
	if err != nil {
		return stats, err
	}
	for _, table := range tables {
		quoted := table.Sanitize()
		statement := fmt.Sprintf(deleteBatchSQL, quoted, quoted)
		drained := false
		for batch := 0; batch < s.cfg.MaxBatchesPerPass; batch++ {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			tag, err := s.store.Exec(ctx, statement, stats.Cutoff, s.cfg.BatchSize)
			if err != nil {
				return stats, errors.Join(errors.New("syncretention: "+quoted), err)
			}
			stats.Deleted[quoted] += tag.RowsAffected()
			if tag.RowsAffected() < int64(s.cfg.BatchSize) {
				drained = true
				break
			}
		}
		stats.Drained = stats.Drained && drained
	}
	return stats, nil
}

func (s *Sweeper) tables(ctx context.Context) ([]pgx.Identifier, error) {
	rows, err := s.store.Query(ctx, tenantTablesSQL)
	if err != nil {
		return nil, errors.Join(errors.New("syncretention: list tenant tombstone tables"), err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, errors.Join(errors.New("syncretention: list tenant tombstone tables"), err)
	}
	tables := make([]pgx.Identifier, 0, len(schemas)+1)
	for _, schema := range schemas {
		tables = append(tables, pgx.Identifier{schema, "chat_sync_tombstones"})
	}
	var notifications bool
	if err := s.store.QueryRow(ctx,
		`SELECT to_regclass('centry.notification_tombstones') IS NOT NULL`).Scan(&notifications); err != nil {
		return nil, errors.Join(errors.New("syncretention: probe notification tombstones"), err)
	}
	if notifications {
		tables = append(tables, pgx.Identifier{"centry", "notification_tombstones"})
	}
	return tables, nil
}

// Run sweeps once now and then every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("syncretention: sweeper started",
		"window_days", s.cfg.RetentionDays, "interval", s.cfg.Interval,
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
		s.logger.Error("syncretention: sweep pass failed", "err", err, "window_days", s.cfg.RetentionDays)
		return
	}
	if stats.Suppressed {
		s.logger.Info("syncretention: maintenance mode is active; not sweeping")
		return
	}
	var total int64
	for _, count := range stats.Deleted {
		total += count
	}
	s.logger.Info("syncretention: sweep pass complete",
		"window_days", s.cfg.RetentionDays, "cutoff", stats.Cutoff.Format(time.RFC3339),
		"tables", len(stats.Deleted), "deleted", total, "drained", stats.Drained)
	if !stats.Drained {
		s.logger.Warn("syncretention: a table hit the batch ceiling with rows still expired",
			"max_batches_per_pass", s.cfg.MaxBatchesPerPass, "batch_size", s.cfg.BatchSize)
	}
}
