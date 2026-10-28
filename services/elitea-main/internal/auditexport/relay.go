package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of *pgxpool.Pool the relay uses.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Relay defaults.
const (
	DefaultInterval      = 5 * time.Second
	DefaultBatchSize     = 200
	DefaultExportTimeout = 30 * time.Second
	// DefaultSettleDelay is how long after its timestamp a row is exported.
	DefaultSettleDelay = 30 * time.Second
	maxBatchSize       = 5000

	// StreamAdminAudit is the cursor name of centry.audit_events.
	StreamAdminAudit = "admin_audit"
	// StreamSecurityAudit names the security audit outbox in metrics.
	StreamSecurityAudit = "security_audit"
)

// AuditEventsConfig configures AuditEventsRelay.
type AuditEventsConfig struct {
	Sink Sink
	// FromBeginning exports the whole existing table the first time the relay
	// runs. The default starts at the newest row, so enabling export does not
	// replay history into the SIEM.
	FromBeginning bool
	Interval      time.Duration
	BatchSize     int
	Timeout       time.Duration
	// SettleDelay is how long after its timestamp a row waits before export.
	// Row ids are assigned at insert but committed later and by several
	// replicas, so a row with a lower id can become visible after a higher
	// one. The cursor is an id, so an early read could pass it for good; the
	// delay covers the recorder's flush interval and commit time.
	SettleDelay time.Duration
	Logger      *slog.Logger
}

// AuditEventsRelay tails centry.audit_events by an id cursor kept in
// elitea_runtime.audit_export_cursors and sends new rows to the sink. The
// cursor advances in the same transaction as the read, after the sink
// accepted the batch, so a crash or a sink failure re-sends and never skips.
// A transaction-scoped advisory lock keeps one replica working at a time, so
// the order holds.
type AuditEventsRelay struct {
	db  DB
	cfg AuditEventsConfig
}

// NewAuditEventsRelay validates the configuration.
func NewAuditEventsRelay(db DB, cfg AuditEventsConfig) (*AuditEventsRelay, error) {
	if db == nil {
		return nil, errors.New("audit events relay needs a database")
	}
	if cfg.Sink == nil {
		return nil, errors.New("audit events relay needs a sink")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.BatchSize > maxBatchSize {
		return nil, fmt.Errorf("audit events relay batch size must be at most %d", maxBatchSize)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultExportTimeout
	}
	if cfg.SettleDelay < 0 {
		cfg.SettleDelay = 0
	} else if cfg.SettleDelay == 0 {
		cfg.SettleDelay = DefaultSettleDelay
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &AuditEventsRelay{db: db, cfg: cfg}, nil
}

// Run relays until ctx ends. A failure is logged and retried on the next tick.
func (r *AuditEventsRelay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Warn("auditexport: audit events relay pass failed; rows stay pending", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Drain exports batches until one comes back short or fails. It returns the
// number of rows exported.
func (r *AuditEventsRelay) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := r.ExportBatch(ctx)
		total += n
		if err != nil {
			return total, err
		}
		if n < r.cfg.BatchSize {
			return total, nil
		}
	}
}

const ensureCursorSQL = `
INSERT INTO elitea_runtime.audit_export_cursors (stream, last_id)
SELECT $1::text, CASE WHEN $2::boolean THEN 0::bigint ELSE coalesce((SELECT max(id) FROM centry.audit_events), 0) END
ON CONFLICT (stream) DO NOTHING`

const readCursorSQL = `SELECT last_id FROM elitea_runtime.audit_export_cursors WHERE stream = $1 FOR UPDATE`

const selectRowsSQL = `
SELECT id, timestamp, user_id, user_email, project_id, event_type, action,
       http_method, http_route, status_code, duration_ms, is_error,
       entity_type, entity_id, entity_name, trace_id, span_id, parent_span_id,
       e.timestamp <= clock_timestamp() - make_interval(secs => $3::float8) AS settled
  FROM centry.audit_events e
 WHERE e.id > $1
 ORDER BY id
 LIMIT $2`

const advanceCursorSQL = `
UPDATE elitea_runtime.audit_export_cursors
   SET last_id = $2, updated_at = clock_timestamp()
 WHERE stream = $1`

// ExportBatch exports one batch and returns the rows it exported.
func (r *AuditEventsRelay) ExportBatch(ctx context.Context) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit events export: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if ok, err := tryStageLock(ctx, tx, StreamAdminAudit); err != nil || !ok {
		return 0, err
	}
	if _, err := tx.Exec(ctx, ensureCursorSQL, StreamAdminAudit, r.cfg.FromBeginning); err != nil {
		return 0, fmt.Errorf("audit events export: ensure cursor: %w", err)
	}
	var last int64
	if err := tx.QueryRow(ctx, readCursorSQL, StreamAdminAudit).Scan(&last); err != nil {
		return 0, fmt.Errorf("audit events export: read cursor: %w", err)
	}
	rows, err := tx.Query(ctx, selectRowsSQL, last, r.cfg.BatchSize, r.cfg.SettleDelay.Seconds())
	if err != nil {
		return 0, fmt.Errorf("audit events export: %w", err)
	}
	events, newest, err := scanAuditRows(rows)
	if err != nil {
		return 0, fmt.Errorf("audit events export: %w", err)
	}
	if len(events) == 0 {
		// Commit: the first pass creates the cursor at the table's end, and a
		// rollback would move that starting point on every idle pass.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("audit events export: commit: %w", err)
		}
		return 0, nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	err = r.cfg.Sink.Send(sendCtx, events)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("audit events export: sink %s: %w", r.cfg.Sink.Name(), err)
	}
	if _, err := tx.Exec(ctx, advanceCursorSQL, StreamAdminAudit, newest); err != nil {
		return 0, fmt.Errorf("audit events export: advance cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("audit events export: commit: %w", err)
	}
	return len(events), nil
}

// scanAuditRows reads rows up to the first one that has not settled.
func scanAuditRows(rows pgx.Rows) ([]Event, int64, error) {
	defer rows.Close()
	var (
		events []Event
		newest int64
	)
	for rows.Next() {
		var (
			row                                              AuditRow
			userID, projectID, entityID                      *int32
			statusCode                                       *int16
			userEmail, method, route, entityType, entityName *string
			traceID, spanID, parentSpanID                    *string
			settled                                          bool
		)
		if err := rows.Scan(&row.ID, &row.Timestamp, &userID, &userEmail, &projectID, &row.EventType, &row.Action,
			&method, &route, &statusCode, &row.DurationMS, &row.IsError,
			&entityType, &entityID, &entityName, &traceID, &spanID, &parentSpanID, &settled); err != nil {
			return nil, 0, err
		}
		if !settled {
			break
		}
		row.UserID, row.ProjectID, row.EntityID = widen(userID), widen(projectID), widen(entityID)
		if statusCode != nil {
			v := int32(*statusCode)
			row.StatusCode = &v
		}
		row.UserEmail, row.HTTPMethod, row.HTTPRoute = deref(userEmail), deref(method), deref(route)
		row.EntityType, row.EntityName = deref(entityType), deref(entityName)
		row.TraceID, row.SpanID, row.ParentSpanID = deref(traceID), deref(spanID), deref(parentSpanID)
		events = append(events, FromAuditRow(row))
		newest = row.ID
	}
	return events, newest, rows.Err()
}

func widen(v *int32) *int64 {
	if v == nil {
		return nil
	}
	w := int64(*v)
	return &w
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tryStageLock takes the stream's transaction-scoped advisory lock. false
// means another replica holds it; this pass skips.
func tryStageLock(ctx context.Context, tx pgx.Tx, stream string) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey(stream)).Scan(&ok); err != nil {
		return false, fmt.Errorf("audit export %s: advisory lock: %w", stream, err)
	}
	return ok, nil
}

func lockKey(stream string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("elitea-platform:auditexport:" + stream))
	return int64(h.Sum64()) // PostgreSQL advisory locks use the bit pattern.
}

// DeadLetterRecorder stores dead letters in
// elitea_runtime.audit_export_dead_letters.
type DeadLetterRecorder struct{ DB DB }

const insertDeadLetterSQL = `
INSERT INTO elitea_runtime.audit_export_dead_letters
    (source_ref, event_id, sink, reason, attempts, event)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (source_ref, sink) DO UPDATE
   SET attempts = EXCLUDED.attempts, reason = EXCLUDED.reason, dead_lettered_at = clock_timestamp()`

// RecordDeadLetters implements DeadLetterStore.
func (d DeadLetterRecorder) RecordDeadLetters(ctx context.Context, sink string, events []Event, reason string, attempts int) error {
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := d.DB.Exec(ctx, insertDeadLetterSQL, e.Ref(), e.EventID, sink, reason, attempts, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// Monitor reports each stream's backlog and lag.
type Monitor struct {
	DB      DB
	Metrics *Metrics
	Logger  *slog.Logger
	// Interval defaults to 15s.
	Interval time.Duration
	// Security reports the security audit stream too. Off with the
	// governance flag, when the outbox stays empty.
	Security bool
}

const securityBacklogSQL = `
SELECT count(*), coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)::float8
  FROM elitea_runtime.security_audit_outbox
 WHERE exported_at IS NULL`

// auditBacklogSQL counts nothing before the relay has made its cursor, so the
// first scrape never counts the whole table.
const auditBacklogSQL = `
SELECT count(*), coalesce(extract(epoch FROM clock_timestamp() - min(e.timestamp)), 0)::float8
  FROM centry.audit_events e
 WHERE e.id > (SELECT last_id FROM elitea_runtime.audit_export_cursors WHERE stream = $1)`

// Observe reads both streams once.
func (m Monitor) Observe(ctx context.Context) error {
	var errs []error
	if m.Security {
		var n int64
		var lag float64
		if err := m.DB.QueryRow(ctx, securityBacklogSQL).Scan(&n, &lag); err != nil {
			errs = append(errs, fmt.Errorf("security audit backlog: %w", err))
		} else {
			m.Metrics.SetStream(StreamSecurityAudit, n, lag)
		}
	}
	var n int64
	var lag float64
	if err := m.DB.QueryRow(ctx, auditBacklogSQL, StreamAdminAudit).Scan(&n, &lag); err != nil {
		errs = append(errs, fmt.Errorf("audit events backlog: %w", err))
	} else {
		m.Metrics.SetStream(StreamAdminAudit, n, lag)
	}
	return errors.Join(errs...)
}

// Run observes until ctx ends.
func (m Monitor) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		obsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := m.Observe(obsCtx); err != nil && ctx.Err() == nil {
			logger.Warn("auditexport: backlog observation failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
