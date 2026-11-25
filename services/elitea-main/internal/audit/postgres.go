package audit

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertSQL writes one row. Column order is the table's own
// (services/elitea-main/internal/infra/db/migrations/001_initial.sql:149).
//
// The columns this service does not produce — tool_name, model_name,
// input_tokens, output_tokens, llm_cost, token_source, cost_source — are left
// out of the statement entirely rather than bound to NULL, so adding one later
// is a change to this list and not a silent behaviour change somewhere else.
const insertSQL = `
INSERT INTO centry.audit_events
    (timestamp, user_id, user_email, project_id, event_type, action,
     http_method, http_route, status_code, duration_ms, is_error,
     entity_type, entity_id, entity_name, trace_id, span_id, parent_span_id,
     acting_roles, elevation_id, credential_kind, central_override,
     reason_code, reason, reason_missing, elevation_flagged)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
        $18, $19::text::uuid, $20, $21, $22, $23, $24, $25)`

// Column widths from the same migration. A value longer than its column makes
// PostgreSQL refuse the INSERT with 22001, which would lose the row — and,
// batched, every row batched with it. Legacy truncated for the same reason
// (`str(action)[:512]` in audit_processor.py `_build_event`).
const (
	maxEventType    = 32
	maxAction       = 512
	maxHTTPMethod   = 10
	maxHTTPRoute    = 512
	maxUserEmail    = 256
	maxEntityType   = 32
	maxEntityName   = 256
	maxTraceID      = 32
	maxSpanID       = 16
	maxParentSpanID = 16
	// reason_code and reason are text columns; the bounds keep one caller's
	// reason from growing a row without limit (R3.8 validates its own).
	maxReasonCode = 128
	maxReason     = 2048
)

// Tuning. These are the shape of the back pressure, so they are named.
const (
	// defaultQueueSize bounds the memory an audit backlog may consume. At the
	// policy in internal/api/middleware/audit.go — administrative and
	// security-relevant mutations only — a busy platform produces single-digit
	// events per second, so 2048 is minutes of backlog, not milliseconds.
	defaultQueueSize = 2048
	// defaultBatchSize caps one round trip. Larger batches amortise better but
	// widen the window in which a crash loses unwritten rows.
	defaultBatchSize = 64
	// defaultFlushInterval bounds how long a lone event waits for company. It
	// also bounds how stale the Audit Trail page can be, which is why it is
	// sub-second: an admin who suspends a user and reloads the trail should see
	// the row.
	defaultFlushInterval = 250 * time.Millisecond
	// defaultWriteTimeout bounds one batch write. A hung pool must not wedge
	// the worker forever; the events are dropped, loudly, and the worker
	// carries on.
	defaultWriteTimeout = 5 * time.Second
	// dropReportInterval rate-limits the "events were dropped" line. Drops are
	// never silent, but a per-drop log during an outage would itself be a
	// denial of service against the log pipeline.
	dropReportInterval = 30 * time.Second
)

// PostgresRecorder persists events to `centry.audit_events` off the request
// path.
//
// # Failure policy: never fail the request, never vanish silently
//
// Both halves are chosen, and they are in tension. The chosen resolution:
//
//   - Record never blocks and never returns an error. It performs one
//     non-blocking channel send. An audit write may not fail, slow, or change
//     the outcome of the request it describes — an audit trail that can take
//     the product down is worse than no audit trail, and an audit trail whose
//     write is on the request's critical path also changes the very latency it
//     records.
//   - When the queue is full, or when a batch write fails, the affected events
//     are DROPPED — and every drop is counted and reported at WARN/ERROR with
//     the count and the cause, rate-limited to one line per 30s. Dropped is a
//     visible state, not a silent one. Dropped() exposes the running total for
//     a health check or a metric.
//
// The rejected alternative was blocking with a short deadline. It reintroduces
// exactly the coupling the first bullet forbids: an audit table that is slow
// (vacuum, lock, failover) would then add its latency to every administrative
// write, and the request would be recorded as slower than it was.
//
// What is NOT dropped-and-forgotten is the fact of the drop. A deployment that
// loses audit records will say so in its logs, on a bounded cadence, with a
// count — which is what makes "the trail is complete" a checkable claim rather
// than an assumption.
type PostgresRecorder struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	queue chan item
	done  chan struct{}

	batchSize     int
	flushInterval time.Duration
	writeTimeout  time.Duration

	dropped    atomic.Int64
	reported   atomic.Int64
	lastReport atomic.Int64 // unix nanos

	closeOnce sync.Once
}

// item is either an event to write or a flush barrier. The barrier is what
// makes the recorder testable without sleeping: Flush returns only after every
// event enqueued before it has been written or dropped.
type item struct {
	event   *Event
	barrier chan struct{}
}

// NewPostgresRecorder starts the background writer. It returns nil when pool is
// nil, and a nil *PostgresRecorder is a working no-op Recorder — so a
// composition without a database silently records nothing rather than panicking
// on the first request.
func NewPostgresRecorder(pool *pgxpool.Pool, logger *slog.Logger) *PostgresRecorder {
	if pool == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &PostgresRecorder{
		pool:          pool,
		log:           logger,
		queue:         make(chan item, defaultQueueSize),
		done:          make(chan struct{}),
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		writeTimeout:  defaultWriteTimeout,
	}
	go r.run()
	return r
}

// Record enqueues an event. It never blocks and never fails; see the type doc.
//
// The ctx argument is part of the Recorder interface and is deliberately NOT
// used to cancel the write: the request's context is cancelled the moment the
// response is finished, which is precisely when this event becomes writable. An
// audit row that disappears because its request completed would be a trail with
// a hole in it exactly where the successful requests are.
func (r *PostgresRecorder) Record(_ context.Context, event Event) {
	if r == nil {
		return
	}
	select {
	case r.queue <- item{event: &event}:
	default:
		r.dropped.Add(1)
		r.reportDrops("audit queue full", nil)
	}
}

// Flush blocks until everything enqueued before the call has been written or
// dropped. For tests and for shutdown; not for the request path.
func (r *PostgresRecorder) Flush(ctx context.Context) error {
	if r == nil {
		return nil
	}
	barrier := make(chan struct{})
	select {
	case r.queue <- item{barrier: barrier}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return errors.New("audit recorder is closed")
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drains the queue and stops the worker. Safe to call more than once.
func (r *PostgresRecorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	err := r.Flush(ctx)
	r.closeOnce.Do(func() { close(r.done) })
	return err
}

// Dropped is the running total of events this recorder could not persist.
// Non-zero means the trail has holes; the logs say when and why.
func (r *PostgresRecorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

func (r *PostgresRecorder) run() {
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	pending := make([]Event, 0, r.batchSize)
	flush := func() {
		r.write(pending)
		pending = pending[:0]
	}

	for {
		select {
		case <-r.done:
			flush()
			return
		case queued := <-r.queue:
			// A barrier is released only AFTER everything enqueued before it
			// has been written or dropped, which is what makes Flush a real
			// synchronisation point and not a sleep with extra steps.
			if queued.barrier != nil {
				flush()
				close(queued.barrier)
				continue
			}
			pending = append(pending, *queued.event)
			if len(pending) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			if len(pending) > 0 {
				flush()
			}
		}
	}
}

// write persists one batch. Every failure path counts the lost events and
// reports them; none returns an error to a caller, because there is no caller
// left that could act on one.
func (r *PostgresRecorder) write(events []Event) {
	if len(events) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.writeTimeout)
	defer cancel()

	batch := &pgx.Batch{}
	for i := range events {
		e := normalize(events[i])
		batch.Queue(insertSQL,
			e.Timestamp, e.UserID, nullable(e.UserEmail), e.ProjectID,
			e.EventType, e.Action, nullable(e.HTTPMethod), nullable(e.HTTPRoute),
			e.StatusCode, e.DurationMS, e.IsError,
			nullable(e.EntityType), e.EntityID, nullable(e.EntityName),
			nullable(e.TraceID), nullable(e.SpanID), nullable(e.ParentSpanID),
			nullableRoles(e.ActingRoles), nullable(e.ElevationID), nullable(e.CredentialKind),
			e.CentralOverride, nullable(e.ReasonCode), nullable(e.Reason), e.ReasonMissing, e.ElevationFlagged)
	}

	results := r.pool.SendBatch(ctx, batch)
	var failed int64
	for range events {
		if _, err := results.Exec(); err != nil {
			failed++
			// One representative cause, not one line per row: a batch fails
			// for the same reason across its members.
			if failed == 1 {
				r.dropped.Add(int64(len(events)))
				r.reportDrops("audit batch write failed", err)
			}
		}
	}
	if err := results.Close(); err != nil && failed == 0 {
		r.dropped.Add(int64(len(events)))
		r.reportDrops("audit batch close failed", err)
	}
}

// reportDrops emits at most one line per dropReportInterval, carrying the total
// since the last line, so a sustained outage costs a bounded number of logs
// while never becoming silent.
func (r *PostgresRecorder) reportDrops(reason string, cause error) {
	now := time.Now().UnixNano()
	last := r.lastReport.Load()
	if now-last < int64(dropReportInterval) && last != 0 {
		return
	}
	if !r.lastReport.CompareAndSwap(last, now) {
		return
	}
	total := r.dropped.Load()
	since := total - r.reported.Swap(total)
	attrs := []any{slog.String("reason", reason), slog.Int64("dropped", since), slog.Int64("dropped_total", total)}
	if cause != nil {
		attrs = append(attrs, slog.String("error", cause.Error()))
		r.log.Error("audit events dropped", attrs...)
		return
	}
	r.log.Warn("audit events dropped", attrs...)
}

// normalize clamps every string to its column width and fills the two NOT NULL
// columns that have no database default.
func normalize(e Event) Event {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.EventType == "" {
		e.EventType = "api"
	}
	if e.Action == "" {
		e.Action = "(unnamed)"
	}
	e.EventType = clamp(e.EventType, maxEventType)
	e.Action = clamp(e.Action, maxAction)
	e.HTTPMethod = clamp(e.HTTPMethod, maxHTTPMethod)
	e.HTTPRoute = clamp(e.HTTPRoute, maxHTTPRoute)
	e.UserEmail = clamp(e.UserEmail, maxUserEmail)
	e.EntityType = clamp(e.EntityType, maxEntityType)
	e.EntityName = clamp(e.EntityName, maxEntityName)
	e.TraceID = clamp(e.TraceID, maxTraceID)
	e.SpanID = clamp(e.SpanID, maxSpanID)
	e.ParentSpanID = clamp(e.ParentSpanID, maxParentSpanID)
	// A kind outside the vocabulary is not written: NULL, never a guess.
	if !KnownCredentialKind(e.CredentialKind) {
		e.CredentialKind = ""
	}
	// An elevation id that is not a UUID would fail the ::uuid cast and lose
	// the row and its batch; it is not recorded instead.
	if !uuidPattern.MatchString(e.ElevationID) {
		e.ElevationID = ""
	}
	e.ReasonCode = clamp(e.ReasonCode, maxReasonCode)
	e.Reason = clamp(e.Reason, maxReason)
	return e
}

// uuidPattern is the text form of a UUID PostgreSQL's uuid input accepts in
// the hyphenated form.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// nullableRoles maps nil to NULL and keeps an empty set as {}.
func nullableRoles(roles []string) any {
	if roles == nil {
		return nil
	}
	return roles
}

// clamp truncates by RUNE, not by byte: cutting a multi-byte character in half
// produces invalid UTF-8, which PostgreSQL rejects outright — turning an
// over-long value into the lost row the width limit was meant to prevent.
func clamp(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && len(string(runes)) > limit {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// nullable maps "" to a SQL NULL. An empty string and "no value" are different
// facts, and the readers treat NULL as "not recorded" (formatDuration's dash,
// the heatmap's excluded rows).
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
