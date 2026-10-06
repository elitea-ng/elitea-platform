package pricesync

import (
	"context"
	"errors"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5/pgconn"
)

// advisoryLockKey is the pg_try_advisory_xact_lock key that serialises price
// sync across scheduler replicas (design §8.8 multi-replica safety). Distinct
// from any app-level advisory lock.
const advisoryLockKey int64 = 0x4c4c4d5052494345 // "LLMPRICE" ascii, arbitrary-but-stable

// driftThreshold is the relative input-price disagreement (fraction) above which
// two sources are considered in conflict and a drift alarm is logged (§8.8).
const driftThreshold = 0.10

// Syncer composes ordered PriceSources, normalises their output to per-1M, merges
// them (first source with a model wins; later sources fill gaps), and UPSERTs the
// result into gateway.gateway_models under a Postgres advisory lock.
type Syncer struct {
	db      DB
	sources []PriceSource
	norm    Normalizer
	logger  *slog.Logger
}

// NewSyncer builds a Syncer. sources are in precedence order (first wins).
func NewSyncer(db DB, sources []PriceSource, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{db: db, sources: sources, logger: logger}
}

// Sync runs one sync pass:
//  1. Fetch every source (network) BEFORE taking the DB lock, so a partition
//     doesn't hold a connection+lock open; a per-source failure is fail-open
//     (WARN + skip, other sources still apply — §8.8 degradation).
//  2. Merge normalised prices in precedence order with a drift alarm.
//  3. In one transaction: take pg_try_advisory_xact_lock (bail if another
//     replica holds it), UPSERT all rows, commit (which releases the xact lock).
//
// It returns the number of rows OFFERED to the catalog, which since shared
// migration 0097 is an upper bound on the number written: the UPSERT's DO UPDATE
// skips a row an operator has priced by hand. A run where the lock is held by
// another replica returns (0, nil) — a benign no-op, not an error.
func (s *Syncer) Sync(ctx context.Context) (int, error) {
	merged, err := s.collect(ctx)
	if err != nil {
		return 0, err
	}
	if len(merged) == 0 {
		s.logger.Warn("pricesync: no prices resolved from any source; skipping upsert")
		return 0, nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	// Rollback is a no-op after a successful Commit; safe to always defer.
	defer func() { _ = tx.Rollback(ctx) }()

	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, advisoryLockKey).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		s.logger.Info("pricesync: advisory lock held by another replica; skipping this pass")
		return 0, nil
	}

	for _, m := range merged {
		if err := s.upsert(ctx, tx, m); err != nil {
			// The UPSERT names the four audio price columns that migration 0086
			// adds. On a database where 0086 has not been applied — a scheduler
			// that rolls out ahead of elitea-migrate — PostgreSQL answers 42703
			// and the whole transaction rolls back, so NO price is refreshed,
			// token prices included.
			//
			// This is deliberately NOT the gateway's behaviour, and the
			// asymmetry is the point. The gateway READS on every request, so it
			// degrades: it falls back to the pre-0086 statement and keeps
			// pricing tokens from the catalog (internal/cost/schema_probe.go).
			// The scheduler WRITES on a timer. Failing the pass leaves the
			// catalog at its previous values and the next tick retries, so the
			// safe outcome is already the default and a partial write is the
			// only thing that could do harm.
			//
			// What was missing was the diagnosis. Name the migration in the
			// error so an operator reads the cause instead of an opaque
			// "column does not exist" from a table they did not change.
			//
			// The message names the COLUMN Postgres refused, not one fixed
			// migration. There are now two that can produce 42703 here —
			// 0086_gateway_audio_prices.sql for the per-second and
			// per-character columns, and 0097_gateway_model_price_override.sql
			// for the price_overridden guard — and a scheduler rolled ahead of
			// either one fails identically. Naming only 0086, as this did, sends
			// an operator to verify a migration that is already applied while
			// the catalog silently stops updating.
			if isUndefinedColumn(err) {
				s.logger.Error("pricesync: the price catalog is missing a column this build writes; "+
					"apply the pending elitea-main shared migrations (0086_gateway_audio_prices.sql adds the "+
					"audio price columns, 0097_gateway_model_price_override.sql adds price_overridden). "+
					"No prices were refreshed this pass; the catalog keeps its previous values and the next "+
					"pass retries.",
					"model", m.ModelName, "provider", m.Provider,
					"missing_column", undefinedColumnName(err), "err", err)
			}
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	// len(merged) is the number of rows OFFERED, which since 0097 is not the
	// number written: a row an operator has priced by hand is skipped by the
	// DO UPDATE's guard. Postgres does not report that per statement without a
	// RETURNING clause per row, and nothing consumes this count except a log
	// line, so it is reported for what it is rather than made exact.
	s.logger.Info("pricesync: catalog pass complete", "models_offered", len(merged))
	return len(merged), nil
}

// collect fetches and normalises every source, then merges by precedence.
func (s *Syncer) collect(ctx context.Context) ([]NormalizedModelPrice, error) {
	if len(s.sources) == 0 {
		return nil, errors.New("pricesync: no sources configured")
	}

	// merged holds the winning row per key; order preserves first-seen for a
	// deterministic UPSERT order.
	merged := map[string]NormalizedModelPrice{}
	var order []string
	// seenInput tracks the winning input price per key for the drift alarm.
	seenInput := map[string]float64{}
	var anySucceeded bool

	for _, src := range s.sources {
		raws, err := src.Fetch(ctx)
		if err != nil {
			// Fail-open: log and skip this source; keep serving other sources.
			s.logger.Warn("pricesync: source fetch failed; skipping", "source", src.Name(), "err", err)
			continue
		}
		anySucceeded = true
		for _, raw := range raws {
			n, err := s.norm.Normalize(raw, src.Denomination(), src.Name())
			if err != nil {
				s.logger.Warn("pricesync: normalize failed; skipping model",
					"source", src.Name(), "provider", raw.Provider, "model", raw.ModelName, "err", err)
				continue
			}
			k := n.key()
			if _, exists := merged[k]; !exists {
				// First source with this model wins.
				merged[k] = n
				order = append(order, k)
				if n.InputCostPer1M != nil {
					seenInput[k] = *n.InputCostPer1M
				}
				continue
			}
			// Later source: does not override, but flag price drift for audit.
			if n.InputCostPer1M != nil {
				if prev, ok := seenInput[k]; ok && priceDrifts(prev, *n.InputCostPer1M) {
					s.logger.Warn("pricesync: price drift between sources",
						"provider", n.Provider, "model", n.ModelName,
						"winning_input_per_1m", prev, "other_source", src.Name(),
						"other_input_per_1m", *n.InputCostPer1M)
				}
			}
		}
	}

	if !anySucceeded {
		return nil, errors.New("pricesync: all sources failed to fetch")
	}

	out := make([]NormalizedModelPrice, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out, nil
}

// priceDrifts reports whether two prices disagree by more than driftThreshold
// (relative). Two exact zeros never drift; a zero-vs-nonzero always drifts.
func priceDrifts(a, b float64) bool {
	if a == b {
		return false
	}
	denom := math.Max(math.Abs(a), math.Abs(b))
	if denom == 0 {
		return false
	}
	return math.Abs(a-b)/denom > driftThreshold
}

// upsert writes one normalised row into gateway.gateway_models. The write is
// per-1M (matching the column denomination) and records source provenance.
//
// EVERY PRICE COLUMN MUST APPEAR IN BOTH HALVES of this statement — the INSERT
// column list and the ON CONFLICT DO UPDATE set list. The two halves serve
// different rows: the first runs for a model the catalog has never seen, the
// second for one it already holds. A column named in only one half is not a
// compile error and not a runtime error; it produces a row that is written once
// and then never refreshed again (or, the other way round, a column that stays
// NULL on every new model). Both failures look like a correct sync in the logs,
// because the upsert reports success either way. TestUpsertSQLCoversEveryPriceColumn
// pins the pairing.
//
// The DO UPDATE carries a WHERE (shared migration 0097). A row an operator has
// priced by hand — `price_overridden` — is left exactly as authored, and the
// upstream number is discarded for that pair rather than applied. Without the
// guard an authored price is correct only until the next tick of this worker
// and then reverts with nothing on any screen reporting that it did, which is
// the same "saves into a void" failure the admin surface exists to remove. The
// INSERT half is deliberately NOT guarded: a pair this catalog has never seen
// cannot be overridden, so a first sync still creates it normally.
func (s *Syncer) upsert(ctx context.Context, tx Tx, m NormalizedModelPrice) error {
	const q = `
		INSERT INTO gateway.gateway_models (
			provider, model_name,
			input_cost_per_1m_tokens, output_cost_per_1m_tokens,
			cache_creation_input_token_cost, cache_read_input_token_cost,
			input_cost_per_1m_tokens_above_128k,
			input_cost_per_1m_seconds, output_cost_per_1m_seconds,
			input_cost_per_1m_characters, output_cost_per_1m_characters,
			source, source_synced_at, last_sync_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), now(), now())
		ON CONFLICT (provider, model_name) DO UPDATE SET
			input_cost_per_1m_tokens            = EXCLUDED.input_cost_per_1m_tokens,
			output_cost_per_1m_tokens           = EXCLUDED.output_cost_per_1m_tokens,
			cache_creation_input_token_cost     = EXCLUDED.cache_creation_input_token_cost,
			cache_read_input_token_cost         = EXCLUDED.cache_read_input_token_cost,
			input_cost_per_1m_tokens_above_128k = EXCLUDED.input_cost_per_1m_tokens_above_128k,
			input_cost_per_1m_seconds           = EXCLUDED.input_cost_per_1m_seconds,
			output_cost_per_1m_seconds          = EXCLUDED.output_cost_per_1m_seconds,
			input_cost_per_1m_characters        = EXCLUDED.input_cost_per_1m_characters,
			output_cost_per_1m_characters       = EXCLUDED.output_cost_per_1m_characters,
			source                              = EXCLUDED.source,
			source_synced_at                    = EXCLUDED.source_synced_at,
			last_sync_at                        = EXCLUDED.last_sync_at,
			updated_at                          = now()
		WHERE NOT gateway_models.price_overridden`
	return tx.Exec(ctx, q,
		m.Provider, m.ModelName,
		m.InputCostPer1M, m.OutputCostPer1M,
		m.CacheCreationPer1M, m.CacheReadPer1M,
		m.InputCostAbove128k,
		m.InputCostPer1MSeconds, m.OutputCostPer1MSeconds,
		m.InputCostPer1MCharacters, m.OutputCostPer1MCharacters,
		m.Source,
	)
}

// undefinedColumnCode is the PostgreSQL SQLSTATE for "column does not exist".
// It is the one error that means "the database is older than this binary",
// rather than "this row is not there" or "the database is unreachable". The
// gateway's reader names the same constant for the same reason
// (elitea-llm-gateway/internal/cost/schema_probe.go).
const undefinedColumnCode = "42703"

// undefinedColumnName returns the column PostgreSQL refused, or "" when the
// error carries no column name. It exists so the 42703 diagnostic can name what
// is actually missing instead of guessing which migration is behind.
func undefinedColumnName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ColumnName
	}
	return ""
}

// isUndefinedColumn reports whether err is PostgreSQL 42703.
//
// It matches the SQLSTATE through pgconn.PgError rather than the message text:
// the message is localised by the server's lc_messages setting, so a string
// match silently stops working on a non-English database.
func isUndefinedColumn(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedColumnCode
}
