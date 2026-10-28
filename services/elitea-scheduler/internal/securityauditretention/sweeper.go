// Package securityauditretention bounds the growth of the security audit
// OUTBOX (elitea_runtime.security_audit_outbox, elitea-main shared 0183,
// issue #1114).
//
// It deletes only outbox rows that are exported and older than the window,
// and only when the durable store (elitea_identity.security_audit_events)
// holds the same seq and hash. A row that is not yet stored or not yet
// exported is never deleted, whatever its age. The durable store is never
// deleted from: it is append-only, and its trigger refuses a DELETE.
//
// THE DEFAULT IS NO DELETION. SECURITY_AUDIT_OUTBOX_RETENTION_DAYS unset or
// 0 leaves the sweeper off (gate D7: retention is an operator parameter).
//
// The shape follows internal/auditretention: bounded batches, FOR UPDATE
// SKIP LOCKED (a second replica takes a disjoint set), a maintenance gate,
// one log line per pass.
package securityauditretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-scheduler/internal/retentionpolicy"
	"github.com/jackc/pgx/v5/pgconn"
)

// Environment variables. Unset means the default.
const (
	EnvRetentionDays = "SECURITY_AUDIT_OUTBOX_RETENTION_DAYS"
	EnvInterval      = "SECURITY_AUDIT_OUTBOX_RETENTION_INTERVAL"
	EnvBatchSize     = "SECURITY_AUDIT_OUTBOX_RETENTION_BATCH_SIZE"
	EnvMaxBatches    = "SECURITY_AUDIT_OUTBOX_RETENTION_MAX_BATCHES"
)

// MinimumRetentionDays is the shortest window the sweeper acts on. A shorter
// positive window is refused, not clamped.
const MinimumRetentionDays = 1

// Pass bounds applied when a field is zero.
const (
	DefaultInterval          = time.Hour
	DefaultBatchSize         = 1000
	DefaultMaxBatchesPerPass = 50
)

// ErrDisabled is a window of zero: the default, no deletion.
var ErrDisabled = errors.New("security audit outbox retention is disabled")

// ErrInvalidConfig is a malformed or refused value.
var ErrInvalidConfig = errors.New("security audit outbox retention configuration is invalid")

// deleteBatchSQL deletes at most $2 exported rows exported before $1. The
// EXISTS is the second guard: the durable copy must match by seq, chain
// position and hash. The table names are literals on purpose.
const deleteBatchSQL = `
	WITH doomed AS (
	    SELECT o.seq
	      FROM elitea_runtime.security_audit_outbox o
	     WHERE o.exported_at IS NOT NULL
	       AND o.exported_at < $1
	       AND EXISTS (
	           SELECT 1 FROM elitea_identity.security_audit_events d
	            WHERE d.seq = o.seq
	              AND d.partition_key = o.partition_key
	              AND d.partition_seq = o.partition_seq
	              AND d.hash = o.hash)
	     ORDER BY o.exported_at
	     LIMIT $2
	       FOR UPDATE OF o SKIP LOCKED
	)
	DELETE FROM elitea_runtime.security_audit_outbox AS victim
	 USING doomed
	 WHERE victim.seq = doomed.seq`

// Store is the slice of *pgxpool.Pool the sweeper uses.
type Store interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Gate reports whether a maintenance window is active.
type Gate func(ctx context.Context) bool

// Config is the sweeper's shape.
type Config struct {
	RetentionDays     int
	Interval          time.Duration
	BatchSize         int
	MaxBatchesPerPass int
}

// ConfigFromEnv reads the four variables through lookup. Unset values take
// the defaults; RetentionDays defaults to 0 (off).
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("%w: no environment lookup", ErrInvalidConfig)
	}
	var cfg Config
	var err error
	if cfg.RetentionDays, err = intEnv(lookup, EnvRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.BatchSize, err = intEnv(lookup, EnvBatchSize); err != nil {
		return Config{}, err
	}
	if cfg.MaxBatchesPerPass, err = intEnv(lookup, EnvMaxBatches); err != nil {
		return Config{}, err
	}
	if raw, ok := lookup(EnvInterval); ok && strings.TrimSpace(raw) != "" {
		interval, parseErr := time.ParseDuration(strings.TrimSpace(raw))
		if parseErr != nil || interval < time.Minute {
			return Config{}, fmt.Errorf("%w: %s must be a duration of at least 1m", ErrInvalidConfig, EnvInterval)
		}
		cfg.Interval = interval
	}
	return cfg, nil
}

func intEnv(lookup func(string) (string, bool), name string) (int, error) {
	raw, ok := lookup(name)
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%w: %s must be a non-negative integer", ErrInvalidConfig, name)
	}
	return value, nil
}

// Stats is what one pass did.
type Stats struct {
	Suppressed bool
	Deleted    int64
	Batches    int
	Drained    bool
	Cutoff     time.Time
	// WindowDays is the window the pass enforced; KeptForever means the
	// policy (or the default) deletes nothing.
	WindowDays  int
	KeptForever bool
}

// Sweeper deletes exported, expired outbox rows on a timer.
type Sweeper struct {
	store  Store
	gate   Gate
	cfg    Config
	policy retentionpolicy.Reader
	logger *slog.Logger
	now    func() time.Time
}

// NewWithPolicy is New for a sweeper that follows the administrator's
// retention policy for `security_audit` (ADR-0042 decision 4) once it is
// saved, and the deployment window until then. A deployment window of zero is
// then not ErrDisabled: the sweeper runs and deletes nothing until a policy
// sets a maximum. It still deletes only exported rows the durable store holds.
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

// windowDays resolves the window of one pass; ok false deletes nothing.
func (s *Sweeper) windowDays(ctx context.Context) (days int, ok bool, err error) {
	days = s.cfg.RetentionDays
	if s.policy != nil {
		decision, err := s.policy(ctx)
		if err != nil {
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
		return 0, false, fmt.Errorf("%w: %d days, floor is %d", ErrInvalidConfig, days, MinimumRetentionDays)
	}
	return days, true, nil
}

// New builds a Sweeper. ErrDisabled is the default (window 0).
func New(store Store, gate Gate, cfg Config, logger *slog.Logger) (*Sweeper, error) {
	if store == nil {
		return nil, errors.New("security audit outbox retention needs a store")
	}
	if gate == nil {
		return nil, errors.New("security audit outbox retention needs a maintenance gate")
	}
	if cfg.RetentionDays <= 0 {
		return nil, ErrDisabled
	}
	if cfg.RetentionDays < MinimumRetentionDays {
		return nil, fmt.Errorf("%w: %d days, floor is %d", ErrInvalidConfig, cfg.RetentionDays, MinimumRetentionDays)
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

// Interval is the configured pass interval.
func (s *Sweeper) Interval() time.Duration { return s.cfg.Interval }

// Window is the retention window.
func (s *Sweeper) Window() time.Duration {
	return time.Duration(s.cfg.RetentionDays) * 24 * time.Hour
}

// Pass runs one pass for the routine registry loop.
func (s *Sweeper) Pass(ctx context.Context) (string, error) {
	passCtx, cancel := context.WithTimeout(ctx, max(s.cfg.Interval/2, time.Minute))
	defer cancel()
	stats, err := s.Sweep(passCtx)
	if err != nil {
		s.logger.Error("securityauditretention: sweep pass failed",
			"err", err, "window_days", s.cfg.RetentionDays, "deleted_before_failure", stats.Deleted)
		return routines.OutcomeError, err
	}
	if stats.Suppressed {
		s.logger.Info("securityauditretention: maintenance mode is active; not sweeping")
		return routines.OutcomeMaintenance, nil
	}
	if stats.KeptForever {
		s.logger.Info("securityauditretention: the retention policy keeps the outbox forever; not sweeping")
		return routines.OutcomeOK, nil
	}
	s.logger.Info("securityauditretention: sweep pass complete",
		"window_days", stats.WindowDays,
		"cutoff", stats.Cutoff.Format(time.RFC3339),
		"deleted", stats.Deleted,
		"batches", stats.Batches,
		"drained", stats.Drained)
	return routines.OutcomeOK, nil
}

// Sweep runs one pass and reports what it did.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{}
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
			stats.Drained = true
			return stats, nil
		}
	}
	return stats, nil
}
