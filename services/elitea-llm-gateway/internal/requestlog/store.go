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
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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
	// meteredUntil is the unix-nano time before which a batch holding a
	// metered tool row is written WITHOUT the shared migration 0174 columns.
	// Zero means "write them". See WriteBatch.
	meteredUntil atomic.Int64
	// reasoningUntil is the unix-nano time before which the full column list
	// is written WITHOUT reasoning_tokens (shared migration 0225). Zero
	// means "write it". See execWithReasoning.
	reasoningUntil atomic.Int64
	// verdictsUntil is the unix-nano time before which the guardrail
	// verdicts are not written, because the database has no
	// gateway.guardrail_verdicts table yet (shared 0214). See writeVerdicts.
	verdictsUntil atomic.Int64
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
// credential_owner and the two cache columns come from shared migration 0139,
// and reasoning_tokens from shared migration 0225. pre0225InsertColumns is the
// list without reasoning_tokens, for a database that has not run 0225 yet;
// legacyInsertColumns is the list without all four, for one that has not run
// 0139 yet.
const insertColumnsUpTo0139 = `(occurred_at, project_id, user_id, route, method, status,
	duration_ms, provider, model, streaming, error_code, prompt_tokens, completion_tokens,
	execution_id, credential_owner, cache_read_tokens, cache_write_tokens`

const insertColumns = insertColumnsUpTo0139 + `, reasoning_tokens)`

const pre0225InsertColumns = insertColumnsUpTo0139 + `)`

const legacyInsertColumns = `(occurred_at, project_id, user_id, route, method, status,
	duration_ms, provider, model, streaming, error_code, prompt_tokens, completion_tokens,
	execution_id)`

// reasoningColumn is the shared migration 0225 column (ADR-0038 decision 5),
// the last of insertColumns.
const reasoningColumn = `reasoning_tokens`

// meteredColumns are the shared migration 0174 columns, appended to
// insertColumns for a batch that holds a metered tool row (#1238).
const meteredColumns = `operation, units, unit, cost_usd`

// meteredColumnsPerRow is columnsPerRow plus the four meteredColumns. A
// statement of another shape binds its own count plus the same four.
const meteredColumnsPerRow = columnsPerRow + 4

// operationLLM and operationTool are the two values of the operation column.
const (
	operationLLM  = "llm"
	operationTool = "tool"
)

// columnsPerRow MUST equal the number of columns in insertColumns, and
// legacyColumnsPerRow the number in legacyInsertColumns. A mismatch would bind
// each row's values into the wrong columns — which for this table means a
// status landing in duration_ms and every latency reading as nonsense.
const (
	columnsPerRow        = 18
	pre0225ColumnsPerRow = 17
	legacyColumnsPerRow  = 14
)

// columnShape is the column list a statement binds: the full list, the list
// before 0225, or the list before 0139.
type columnShape int

const (
	shapeFull columnShape = iota
	shapePre0225
	shapeLegacy
)

// columns is the shape's column list and its count.
func (shape columnShape) columns() (string, int) {
	switch shape {
	case shapePre0225:
		return pre0225InsertColumns, pre0225ColumnsPerRow
	case shapeLegacy:
		return legacyInsertColumns, legacyColumnsPerRow
	default:
		return insertColumns, columnsPerRow
	}
}

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
//
// A batch that holds a metered tool row (#1238) is written with the shared
// migration 0174 columns too. A database without them refuses with 42703; that
// latches a separate interval, and the batch takes the path above instead, so
// the tool rows keep their provider, model and status and lose only the four
// metered columns. A batch with no tool row never names those columns, so the
// skew of 0174 costs model traffic nothing.
func (s *Store) WriteBatch(ctx context.Context, records []Record) error {
	if s == nil || len(records) == 0 {
		return nil
	}
	if hasMetered(records) && !s.legacyColumns() && !s.latched(&s.meteredUntil) {
		err := s.writeMeteredBatch(ctx, records)
		if err == nil {
			s.writeVerdictsAfterLog(ctx, records)
			return nil
		}
		if !isUndefinedColumn(err) {
			return err
		}
		s.meteredUntil.Store(s.now().Add(legacyColumnsRetry).UnixNano())
	}
	if !s.legacyColumns() {
		err := s.writeBatch(ctx, records, false)
		if err == nil {
			s.writeVerdictsAfterLog(ctx, records)
			return nil
		}
		if !isUndefinedColumn(err) {
			return err
		}
		s.legacyUntil.Store(s.now().Add(legacyColumnsRetry).UnixNano())
	}
	if err := s.writeBatch(ctx, records, true); err != nil {
		return err
	}
	s.writeVerdictsAfterLog(ctx, records)
	return nil
}

// legacyColumns reports whether the pre-0139 column list is latched.
func (s *Store) legacyColumns() bool {
	return s.latched(&s.legacyUntil)
}

// latched reports whether a latch is set and not yet expired.
func (s *Store) latched(until *atomic.Int64) bool {
	value := until.Load()
	return value != 0 && s.now().UnixNano() < value
}

// hasMetered reports whether a batch holds a metered tool row.
func hasMetered(records []Record) bool {
	for _, record := range records {
		if record.Metered != nil {
			return true
		}
	}
	return false
}

// writeMeteredBatch writes every record with the full column list plus the
// 0174 columns. A model row gets operation "llm" and NULL units, unit and
// cost; the column default would say the same, but one statement binds one
// shape.
func (s *Store) writeMeteredBatch(ctx context.Context, records []Record) error {
	return s.execWithReasoning(ctx, func(shape columnShape) (string, []any) {
		columns, perRow := shape.columns()
		perRow += meteredColumnsPerRow - columnsPerRow
		columns = strings.TrimSuffix(columns, ")") + ", " + meteredColumns + ")"
		placeholders := make([]string, 0, len(records))
		args := make([]any, 0, len(records)*perRow)
		for index, record := range records {
			placeholders = append(placeholders, rowPlaceholders(index*perRow, perRow))
			args = append(args, recordArgs(record, shape)...)
			if record.Metered == nil {
				args = append(args, operationLLM, nil, nil, nil)
				continue
			}
			args = append(args, operationTool, record.Metered.Units,
				truncate(record.Metered.Unit, 16), nanoNumeric(record.Metered.CostNanoUSD))
		}
		return fmt.Sprintf(`INSERT INTO gateway.llm_request_logs %s VALUES %s`,
			columns, strings.Join(placeholders, ",")), args
	})
}

// rowPlaceholders is one row's "($n,...)" group, numbered from base+1.
func rowPlaceholders(base, perRow int) string {
	slots := make([]string, perRow)
	for column := range slots {
		slots[column] = "$" + strconv.Itoa(base+column+1)
	}
	return "(" + strings.Join(slots, ",") + ")"
}

// execWithReasoning runs a statement built with the reasoning_tokens column
// while the database has it (shared migration 0225), else without it.
//
// A gateway pod can roll out before elitea-migrate applies 0225. Postgres then
// refuses the column with 42703, and the error names the column. Only THAT
// refusal latches reasoningUntil and writes the same batch again without the
// column, so a refusal of a 0139 or 0174 column still reaches the latch that
// owns it. The count is enrichment: losing it for one retry interval loses
// no row.
func (s *Store) execWithReasoning(ctx context.Context, build func(shape columnShape) (string, []any)) error {
	shape := shapeFull
	if s.latched(&s.reasoningUntil) {
		shape = shapePre0225
	}
	statement, args := build(shape)
	_, err := s.db.Exec(ctx, statement, args...)
	if err != nil && shape == shapeFull && isMissingColumn(err, reasoningColumn) {
		s.reasoningUntil.Store(s.now().Add(legacyColumnsRetry).UnixNano())
		statement, args = build(shapePre0225)
		_, err = s.db.Exec(ctx, statement, args...)
	}
	if err != nil {
		return fmt.Errorf("write request log batch: %w", err)
	}
	return nil
}

// isMissingColumn reports a 42703 whose message names column.
func isMissingColumn(err error, column string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703" &&
		strings.Contains(pgErr.Message, `"`+column+`"`)
}

// nanoNumeric is a nano-USD amount as an exact NUMERIC in USD: the integer
// with nine fractional digits, no float on the way.
func nanoNumeric(nano int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(nano), Exp: -9, Valid: true}
}

// isUndefinedColumn reports Postgres 42703, the code a missing column raises.
func isUndefinedColumn(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703"
}

func (s *Store) writeBatch(ctx context.Context, records []Record, legacy bool) error {
	build := func(shape columnShape) (string, []any) {
		columns, perRow := shape.columns()
		placeholders := make([]string, 0, len(records))
		args := make([]any, 0, len(records)*perRow)
		for index, record := range records {
			placeholders = append(placeholders, rowPlaceholders(index*perRow, perRow))
			args = append(args, recordArgs(record, shape)...)
		}
		return fmt.Sprintf(`INSERT INTO gateway.llm_request_logs %s VALUES %s`,
			columns, strings.Join(placeholders, ",")), args
	}
	if legacy {
		statement, args := build(shapeLegacy)
		if _, err := s.db.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("write request log batch: %w", err)
		}
		return nil
	}
	return s.execWithReasoning(ctx, build)
}

// rowQuerier is the optional read half of the pool, used only for the
// retention policy.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RetentionPolicy reads the platform row of `gateway_request_log` from
// elitea_runtime.retention_policy (elitea-main shared 0195). A database
// without the table, a missing row or a null maximum is ok=false.
func (s *Store) RetentionPolicy(ctx context.Context) (time.Duration, bool, error) {
	if s == nil {
		return 0, false, nil
	}
	q, ok := s.db.(rowQuerier)
	if !ok {
		return 0, false, nil
	}
	// Two statements: a query naming a table that does not exist fails at
	// parse time, whatever its WHERE says.
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.retention_policy') IS NOT NULL`).Scan(&present); err != nil {
		return 0, false, fmt.Errorf("read request log retention policy: %w", err)
	}
	if !present {
		return 0, false, nil
	}
	var days *int
	err := q.QueryRow(ctx, `
		SELECT max_age_days FROM elitea_runtime.retention_policy
		 WHERE scope = 'platform' AND data_class = 'gateway_request_log'`).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read request log retention policy: %w", err)
	}
	if days == nil || *days < 1 {
		return 0, false, nil
	}
	return time.Duration(*days) * 24 * time.Hour, true, nil
}

// Prune deletes expired rows.
//
// Each DELETE is bounded by `ctid IN (SELECT … LIMIT)` rather than open-ended:
// on a table that has grown for a month, one unbounded statement takes a long
// lock and a large amount of WAL. One prune runs the bounded DELETE again and
// again until no expired row is left, within the per-prune caps of
// pruneBatches, so the table cannot outgrow the retention window at any write
// rate the caps allow.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	if s == nil {
		return 0, nil
	}
	removed, err := s.pruneBatches(ctx, `
		DELETE FROM gateway.llm_request_logs
		 WHERE ctid IN (
		       SELECT ctid FROM gateway.llm_request_logs
		        WHERE occurred_at < $1
		        LIMIT $2)`, olderThan)
	if err != nil {
		return removed, fmt.Errorf("prune request logs: %w", err)
	}
	verdicts, err := s.pruneVerdicts(ctx, olderThan)
	return removed + verdicts, err
}

// Bounds of one prune of one table.
const (
	// pruneBatchRows is the LIMIT of each DELETE statement.
	pruneBatchRows = 10000
	// pruneMaxBatches caps the statements of one prune of one table (one
	// million rows per hour). A backlog larger than that clears over the next
	// prunes.
	pruneMaxBatches = 100
	// pruneMaxDuration caps the time one prune of one table runs, so the
	// hourly prune never holds the writer goroutine for long.
	pruneMaxDuration = 10 * time.Second
)

// pruneBatches runs statement (a DELETE with olderThan as $1 and the row
// bound pruneBatchRows as $2) until a statement removes fewer rows than that,
// until pruneMaxBatches statements ran, until pruneMaxDuration passed, or
// until ctx ends. It returns the rows removed and the first error.
func (s *Store) pruneBatches(ctx context.Context, statement string, olderThan time.Time) (int64, error) {
	started := s.now()
	var removed int64
	for batch := 0; batch < pruneMaxBatches; batch++ {
		if ctx.Err() != nil {
			return removed, nil
		}
		tag, err := s.db.Exec(ctx, statement, olderThan, pruneBatchRows)
		if err != nil {
			return removed, err
		}
		n := tag.RowsAffected()
		removed += n
		if n < pruneBatchRows || s.now().Sub(started) >= pruneMaxDuration {
			return removed, nil
		}
	}
	return removed, nil
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
func truncate(value string, limit int) string { return Truncate(value, limit) }

// Truncate bounds value to at most limit BYTES, cut on a UTF-8 boundary. A
// VARCHAR(n) bound is in characters, so a byte-bounded value always fits, and
// the rune boundary keeps the result valid UTF-8 (Postgres refuses a broken
// sequence as surely as an over-long one). The usage ledger writers in
// internal/failmode reuse it for the same columns.
func Truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
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

// recordArgs is one record's bind values for the shape's column list.
func recordArgs(record Record, shape columnShape) []any {
	args := make([]any, 0, columnsPerRow)
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
	if shape != shapeLegacy {
		args = append(args,
			truncate(record.CredentialOwner, 16),
			nonNegative(record.CacheReadToks),
			nonNegative(record.CacheWriteToks),
		)
	}
	if shape == shapeFull {
		args = append(args, nonNegative(record.ReasoningToks))
	}
	return args
}
