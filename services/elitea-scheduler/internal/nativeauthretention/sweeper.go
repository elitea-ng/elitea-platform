// Package nativeauthretention bounds the native authorization tables
// (elitea-main shared migration 0141, ADR-0025 WP2) with batched deletes.
//
// Correctness never depends on this sweeper: every read in elitea-main checks
// expiry itself (an expired access token is refused, an idle family is revoked
// at its next refresh). What the sweeper adds is that the tables stop growing,
// and that an idle device stops being listed as active and stops holding its
// auth_core__token anchor.
//
// One pass runs five steps, each in batches of BatchSize up to
// MaxBatchesPerPass:
//
//  1. access tokens past expires_at + 1h are deleted;
//  2. authorization requests past expires_at + 1h are deleted;
//  3. consumed refresh tokens older than their family's idle TTL are deleted
//     (a consumed token older than that can no longer be presented within any
//     re-delivery window, and it outlived the family's reuse horizon);
//  4. live families past their idle TTL or absolute cap are revoked
//     (`expired`): the anchor and its ADR-0018 binding are deleted and the
//     access tokens dropped, exactly as elitea-main's revokeFamily does;
//  5. families revoked more than RevokedRetention ago are deleted (their
//     refresh and access tokens cascade).
package nativeauthretention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Defaults.
const (
	DefaultInterval          = time.Hour
	DefaultBatchSize         = 1000
	DefaultMaxBatchesPerPass = 50
	// RevokedRetention keeps a revoked device listed (as revoked) this long.
	RevokedRetention = 90 * 24 * time.Hour
	// expiryGrace keeps an expired access token or authorization row a little
	// longer than its expiry so a request racing the boundary reads a row.
	expiryGrace = time.Hour
)

// Store is the database seam (a *pgxpool.Pool satisfies it).
type Store interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config tunes the sweeper.
type Config struct {
	Interval          time.Duration
	BatchSize         int
	MaxBatchesPerPass int
}

type step struct {
	name string
	sql  string
	// cutoff computes the step's $1 from now.
	cutoff func(now time.Time) time.Time
}

// Each statement takes $1 (a timestamp cutoff, or now) and $2 (the batch size),
// affects at most $2 rows, and answers how many it affected.
var steps = []step{
	{
		name: "access_tokens",
		sql: `
			WITH doomed AS (
			    SELECT token_hash FROM elitea_auth.native_access_tokens
			    WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
			)
			, gone AS (
			    DELETE FROM elitea_auth.native_access_tokens AS victim
			    USING doomed WHERE victim.token_hash = doomed.token_hash RETURNING 1
			)
			SELECT count(*) FROM gone`,
		cutoff: func(now time.Time) time.Time { return now.Add(-expiryGrace) },
	},
	{
		name: "authorizations",
		sql: `
			WITH doomed AS (
			    SELECT id FROM elitea_auth.native_authorizations
			    WHERE expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED
			)
			, gone AS (
			    DELETE FROM elitea_auth.native_authorizations AS victim
			    USING doomed WHERE victim.id = doomed.id RETURNING 1
			)
			SELECT count(*) FROM gone`,
		cutoff: func(now time.Time) time.Time { return now.Add(-expiryGrace) },
	},
	{
		name: "consumed_refresh_tokens",
		sql: `
			WITH doomed AS (
			    SELECT rt.token_hash
			    FROM elitea_auth.native_refresh_tokens AS rt
			    JOIN elitea_auth.native_sessions AS s ON s.id = rt.session_id
			    WHERE rt.consumed_at IS NOT NULL
			      AND rt.consumed_at < $1::timestamptz - make_interval(secs => s.idle_timeout_seconds)
			    LIMIT $2
			    FOR UPDATE OF rt SKIP LOCKED
			)
			, gone AS (
			    DELETE FROM elitea_auth.native_refresh_tokens AS victim
			    USING doomed WHERE victim.token_hash = doomed.token_hash RETURNING 1
			)
			SELECT count(*) FROM gone`,
		cutoff: func(now time.Time) time.Time { return now },
	},
	{
		name: "idle_families",
		sql: `
			WITH expired AS (
			    SELECT id, token_id FROM elitea_auth.native_sessions
			    WHERE revoked_at IS NULL
			      AND (last_refreshed_at + make_interval(secs => idle_timeout_seconds) <= $1
			           OR (expires_at IS NOT NULL AND expires_at <= $1))
			    ORDER BY last_refreshed_at
			    LIMIT $2
			    FOR UPDATE SKIP LOCKED
			), marked AS (
			    UPDATE elitea_auth.native_sessions AS s
			    SET revoked_at = $1, revoke_reason = 'expired', token_id = NULL
			    FROM expired WHERE s.id = expired.id
			    RETURNING s.id, expired.token_id AS anchor
			), unbound AS (
			    DELETE FROM elitea_identity.token_project_binding AS binding
			    USING marked WHERE binding.token_id = marked.anchor
			), anchors AS (
			    DELETE FROM public.auth_core__token AS token
			    USING marked WHERE token.id = marked.anchor
			), access AS (
			    DELETE FROM elitea_auth.native_access_tokens AS access_token
			    USING marked WHERE access_token.session_id = marked.id
			), unsealed AS (
			    UPDATE elitea_auth.native_refresh_tokens AS rt
			    SET successor_sealed = NULL
			    FROM marked WHERE rt.session_id = marked.id AND rt.successor_sealed IS NOT NULL
			)
			SELECT count(*) FROM marked`,
		cutoff: func(now time.Time) time.Time { return now },
	},
	{
		name: "revoked_families",
		sql: `
			WITH doomed AS (
			    SELECT id FROM elitea_auth.native_sessions
			    WHERE revoked_at IS NOT NULL AND revoked_at < $1
			    ORDER BY revoked_at LIMIT $2 FOR UPDATE SKIP LOCKED
			)
			, gone AS (
			    DELETE FROM elitea_auth.native_sessions AS victim
			    USING doomed WHERE victim.id = doomed.id RETURNING 1
			)
			SELECT count(*) FROM gone`,
		cutoff: func(now time.Time) time.Time { return now.Add(-RevokedRetention) },
	},
}

// Sweeper runs the passes.
type Sweeper struct {
	store  Store
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

// New builds a sweeper.
func New(store Store, cfg Config, logger *slog.Logger) (*Sweeper, error) {
	if store == nil {
		return nil, errors.New("native auth retention sweep needs a store")
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
	return &Sweeper{store: store, cfg: cfg, logger: logger, now: time.Now}, nil
}

// Stats is one pass's result: rows affected per step, by step name.
type Stats map[string]int64

// Sweep runs one pass and reports rows affected per step (families revoked for
// idle_families, rows deleted for the others).
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{}
	now := s.now().UTC()
	for _, current := range steps {
		cutoff := current.cutoff(now)
		for batch := 0; batch < s.cfg.MaxBatchesPerPass; batch++ {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			var affected int64
			if err := s.store.QueryRow(ctx, current.sql, cutoff, s.cfg.BatchSize).Scan(&affected); err != nil {
				return stats, errors.Join(errors.New("nativeauthretention: "+current.name), err)
			}
			stats[current.name] += affected
			if affected < int64(s.cfg.BatchSize) {
				break
			}
		}
	}
	return stats, nil
}

// Run sweeps once now and then every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("nativeauthretention: sweeper started", "interval", s.cfg.Interval)
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
		s.logger.Error("nativeauthretention: sweep pass failed", "err", err)
		return
	}
	attributes := make([]any, 0, 2*len(stats))
	for name, count := range stats {
		attributes = append(attributes, name, count)
	}
	s.logger.Info("nativeauthretention: sweep pass complete", attributes...)
}
