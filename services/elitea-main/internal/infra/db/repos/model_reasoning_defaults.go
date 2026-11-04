package repos

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// model_reasoning_defaults.go — the family defaults of the reasoning control
// (ADR-0038 decision 3, shared migration 0225).
//
// The table is platform data that an operator changes with SQL, and every
// catalogue read needs it, so the rows are cached for
// modelReasoningDefaultsTTL. A change reaches the catalogue within that
// interval.
//
// The defaults are an enrichment of the catalogue, never a condition of it.
// A database before 0225 has no table; a read that fails keeps the last rows
// it read, or none. The catalogue then serves each model without a family
// default, which is the behaviour before ADR-0038. It never fails the read.

const modelReasoningDefaultsTTL = time.Minute

const listModelReasoningDefaultsSQL = `SELECT name_pattern, reasoning_control, tool_steps
FROM elitea_runtime.model_reasoning_defaults
ORDER BY priority, name_pattern
LIMIT 256`

// modelReasoningDefaultsSource reads the compiled defaults in priority order.
type modelReasoningDefaultsSource interface {
	Load(context.Context) []configurationapp.ModelReasoningDefault
}

type modelReasoningDefaultsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (sqlRows, error)
}

type modelReasoningDefaultsReader struct {
	db  modelReasoningDefaultsQuerier
	now func() time.Time

	mu       sync.Mutex
	rules    []configurationapp.ModelReasoningDefault
	loadedAt time.Time
	loaded   bool
}

func newModelReasoningDefaultsReader(pool *pgxpool.Pool) *modelReasoningDefaultsReader {
	if pool == nil {
		return nil
	}
	return &modelReasoningDefaultsReader{db: pgxExecutor{queryer: pool}, now: time.Now}
}

// Load returns the cached defaults, refreshed when they are older than the
// TTL. It never returns an error: see the file comment.
func (r *modelReasoningDefaultsReader) Load(ctx context.Context) []configurationapp.ModelReasoningDefault {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && r.now().Sub(r.loadedAt) < modelReasoningDefaultsTTL {
		return r.rules
	}
	rules, err := r.read(ctx)
	if err != nil {
		if ctx.Err() == nil && !isUndefinedTable(err) {
			slog.WarnContext(ctx, "reading the model reasoning defaults failed; the catalogue keeps the last defaults it read",
				"error", err)
		}
		// Retry after the TTL, not on every catalogue read.
		r.loadedAt, r.loaded = r.now(), true
		return r.rules
	}
	r.rules, r.loadedAt, r.loaded = rules, r.now(), true
	return r.rules
}

func (r *modelReasoningDefaultsReader) read(ctx context.Context) ([]configurationapp.ModelReasoningDefault, error) {
	rows, err := r.db.Query(ctx, listModelReasoningDefaultsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []configurationapp.ModelReasoningDefault{}
	for rows.Next() {
		var pattern, control string
		var toolSteps bool
		if err := rows.Scan(&pattern, &control, &toolSteps); err != nil {
			return nil, err
		}
		rule, err := configurationapp.CompileModelReasoningDefault(pattern, control, toolSteps)
		if err != nil {
			// One row an operator wrote in a syntax RE2 does not take must
			// not remove every other default.
			slog.WarnContext(ctx, "skipping a model reasoning default that does not compile",
				"name_pattern", pattern, "error", err)
			continue
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// isUndefinedTable reports Postgres 42P01: the database has not run 0225.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
