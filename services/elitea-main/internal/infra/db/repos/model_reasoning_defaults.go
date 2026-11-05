package repos

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

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
// A read that fails is not cached: the catalogue keeps the last rows it read
// (or none) and the next catalogue read tries again. A database before 0225
// has no table, which is a definite answer, so it is cached as no rows. The
// catalogue then serves each model without a family default, which is the
// behaviour before ADR-0038. It never fails the read.
//
// The read runs once at a time (singleflight) on a context detached from the
// caller, so a caller that gives up does not cancel the read every other
// caller waits for, and no lock is held across the query. While a refresh
// runs, callers get the last good rules at once.

const modelReasoningDefaultsTTL = time.Minute

// modelReasoningDefaultsReadTimeout bounds the detached read.
const modelReasoningDefaultsReadTimeout = 5 * time.Second

const listModelReasoningDefaultsSQL = `SELECT name_pattern, reasoning_control, tool_steps, off_with_tools
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

	flight singleflight.Group

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
	rules, loaded, fresh := r.rules, r.loaded, r.loaded && r.now().Sub(r.loadedAt) < modelReasoningDefaultsTTL
	r.mu.Unlock()
	if fresh {
		return rules
	}
	result := r.flight.DoChan("defaults", func() (any, error) {
		return r.refresh(ctx)
	})
	if loaded {
		// Stale: serve the last good rules; the refresh lands for later reads.
		return rules
	}
	select {
	case done := <-result:
		if done.Err != nil {
			return nil
		}
		fetched, _ := done.Val.([]configurationapp.ModelReasoningDefault)
		return fetched
	case <-ctx.Done():
		return nil
	}
}

// refresh reads the table on a context detached from the caller and caches a
// successful read only.
func (r *modelReasoningDefaultsReader) refresh(caller context.Context) ([]configurationapp.ModelReasoningDefault, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(caller), modelReasoningDefaultsReadTimeout)
	defer cancel()
	rules, err := r.read(ctx)
	if err != nil && isUndefinedTable(err) {
		rules, err = []configurationapp.ModelReasoningDefault{}, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "reading the model reasoning defaults failed; the catalogue keeps the last defaults it read",
			"error", err)
		return nil, err
	}
	r.mu.Lock()
	r.rules, r.loadedAt, r.loaded = rules, r.now(), true
	r.mu.Unlock()
	return rules, nil
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
		var toolSteps, offWithTools bool
		if err := rows.Scan(&pattern, &control, &toolSteps, &offWithTools); err != nil {
			return nil, err
		}
		rule, err := configurationapp.CompileModelReasoningDefault(pattern, control, toolSteps, offWithTools)
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
