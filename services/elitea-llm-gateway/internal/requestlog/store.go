package requestlog

// The Postgres sink.
//
// One multi-row INSERT per batch, and a bounded DELETE for the prune. Neither
// statement is built from caller data: every value is a bind parameter, and the
// only thing interpolated into the statement text is the placeholder list,
// whose length comes from the batch size.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is the slice of *pgxpool.Pool this package uses, so the statements can
// be exercised without a live database.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store writes records into gateway.llm_request_logs.
type Store struct {
	db execer
	// now is the clock the schema latch reads. Tests replace it.
	now func() time.Time
	// legacyUntil is the unix-nano time before which WriteBatch uses the
	// pre-0139 column list. Zero means "use the full list". See WriteBatch.
	legacyUntil atomic.Int64
}

// NewStore returns a sink over the pool. A nil pool returns nil, which New()
// turns into a no-op recorder.
func NewStore(db execer) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db, now: time.Now}
}

// insertColumns is the column list, declared once so the placeholder count and
// the argument order cannot drift from it.
//
// The last three columns come from shared migration 0139. legacyInsertColumns
// is the same list without them, for a database that has not run 0139 yet.
const insertColumns = `(occurred_at, project_id, user_id, route, method, status,
	duration_ms, provider, model, streaming, error_code, prompt_tokens, completion_tokens,
	execution_id, credential_owner, cache_read_tokens, cache_write_tokens)`

const legacyInsertColumns = `(occurred_at, project_id, user_id, route, method, status,
	duration_ms, provider, model, streaming, error_code, prompt_tokens, completion_tokens,
	execution_id)`

// columnsPerRow MUST equal the number of columns in insertColumns, and
// legacyColumnsPerRow the number in legacyInsertColumns. A mismatch would bind
// each row's values into the wrong columns — which for this table means a
// status landing in duration_ms and every latency reading as nonsense.
const (
	columnsPerRow       = 17
	legacyColumnsPerRow = 14
)

// legacyColumnsRetry is how long WriteBatch keeps the pre-0139 column list
// after the database refused the full one. It expires, so the gateway heals by
// itself when elitea-migrate lands, at the cost of one refused statement per
// interval.
const legacyColumnsRetry = 5 * time.Minute

// WriteBatch inserts every record in one statement.
//
// A gateway pod can roll out before elitea-migrate applies 0139. Postgres then
// refuses the full column list with 42703 (undefined_column). Without a
// fallback EVERY batch fails, and the log loses all traffic rather than three
// columns. So a 42703 latches the pre-0139 list for legacyColumnsRetry, and the
// same batch is written again without the three new columns. This is the rule
// internal/cost/schema_probe.go applies to the price catalog.
func (s *Store) WriteBatch(ctx context.Context, records []Record) error {
	if s == nil || len(records) == 0 {
		return nil
	}
	if !s.legacyColumns() {
		err := s.writeBatch(ctx, records, false)
		if err == nil || !isUndefinedColumn(err) {
			return err
		}
		s.legacyUntil.Store(s.now().Add(legacyColumnsRetry).UnixNano())
	}
	return s.writeBatch(ctx, records, true)
}

// legacyColumns reports whether the pre-0139 column list is latched.
func (s *Store) legacyColumns() bool {
	until := s.legacyUntil.Load()
	return until != 0 && s.now().UnixNano() < until
}

// isUndefinedColumn reports Postgres 42703, the code a missing column raises.
func isUndefinedColumn(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703"
}

func (s *Store) writeBatch(ctx context.Context, records []Record, legacy bool) error {
	columns, perRow := insertColumns, columnsPerRow
	if legacy {
		columns, perRow = legacyInsertColumns, legacyColumnsPerRow
	}

	placeholders := make([]string, 0, len(records))
	args := make([]any, 0, len(records)*perRow)
	for index, record := range records {
		base := index * perRow
		slots := make([]string, perRow)
		for column := range slots {
			slots[column] = "$" + strconv.Itoa(base+column+1)
		}
		placeholders = append(placeholders, "("+strings.Join(slots, ",")+")")

		args = append(args,
			record.OccurredAt,
			nullableID(record.ProjectID),
			nullableID(record.UserID),
			truncate(record.Route, 128),
			truncate(record.Method, 8),
			record.Status,
			record.Duration.Milliseconds(),
			truncate(record.Provider, 64),
			truncate(record.Model, 128),
			record.Streaming,
			truncate(record.ErrorCode, 64),
			record.PromptToks,
			record.OutputToks,
			// NULL rather than '' for a request that came from no execution.
			// The agent breakdown counts rows it can attribute, and an empty
			// string is a value that groups — it would become a nameless agent
			// carrying every un-attributed request in the project.
			nullableExecutionID(record.ExecutionID),
		)
		if !legacy {
			args = append(args,
				truncate(record.CredentialOwner, 16),
				nonNegative(record.CacheReadToks),
				nonNegative(record.CacheWriteToks),
			)
		}
	}

	statement := fmt.Sprintf(
		`INSERT INTO gateway.llm_request_logs %s VALUES %s`,
		columns, strings.Join(placeholders, ","))
	if _, err := s.db.Exec(ctx, statement, args...); err != nil {
		return fmt.Errorf("write request log batch: %w", err)
	}
	return nil
}

// Prune deletes expired rows.
//
// Bounded by `ctid IN (SELECT … LIMIT)` rather than an open-ended DELETE: on a
// table that has grown for a month, one unbounded statement takes a long lock
// and a large amount of WAL. The prune runs hourly, so removing a bounded slice
// each time keeps up with any rate that does not already exceed the write path.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	if s == nil {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM gateway.llm_request_logs
		 WHERE ctid IN (
		       SELECT ctid FROM gateway.llm_request_logs
		        WHERE occurred_at < $1
		        LIMIT 10000)`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune request logs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// nullableID turns the header string into an integer or NULL.
//
// A value that is not an integer becomes NULL rather than failing the write:
// the identity headers are validated on the request path, so a malformed one
// here means a request that was already refused, and losing the whole batch
// over it would discard the log of the refusal too.
func nullableID(raw string) any {
	if raw == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return nil
	}
	return parsed
}

// truncate bounds a value to its column width.
//
// The columns are VARCHAR(n) and Postgres REFUSES an over-long value rather
// than trimming it, so a single long model name would fail the whole batch —
// taking every other record in it. Trimming here keeps one odd value from
// costing the log a second of traffic.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// nullableExecutionID renders an absent execution id as SQL NULL.
//
// The column is nullable for the same reason project_id and user_id are: "this
// request came from no execution" is a different claim from "it came from an
// execution whose id is the empty string", and only NULL says the first one.
func nullableExecutionID(value string) any {
	if value == "" {
		return nil
	}
	return truncate(value, 128)
}

// nonNegative clamps a token count to zero. A negative count is a provider or
// decode fault, and it must not subtract from a sum on the analytics page.
func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
