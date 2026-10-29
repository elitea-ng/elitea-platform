package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the slice of *pgxpool.Pool the relay uses. Acquire gives the relay a
// connection of its own for the session advisory lock that keeps one replica
// relaying at a time.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// Relay defaults.
const (
	DefaultInterval      = 5 * time.Second
	DefaultBatchSize     = 200
	DefaultExportTimeout = 30 * time.Second
	maxBatchSize         = 5000

	// StreamAdminAudit is the cursor stream of centry.audit_events.
	StreamAdminAudit = "admin_audit"
	// StreamSecurityAudit names the security audit outbox in metrics.
	StreamSecurityAudit = "security_audit"
	// StreamRunSteps is the cursor stream of elitea_runtime.run_steps
	// (ADR-0042 decision 1, #1499).
	StreamRunSteps = "run_steps"
)

// ErrCursorMoved reports that a sink's position changed under the relay
// between its read and its compare-and-set: another replica relayed the same
// rows. The rows were delivered (at least once); the pass stops.
var ErrCursorMoved = errors.New("audit export cursor moved under the relay")

var sinkNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// AuditEventsConfig configures AuditEventsRelay.
type AuditEventsConfig struct {
	// Sinks each get their own persisted position. Names must be unique and
	// match ^[a-z0-9_]{1,64}$.
	Sinks []Sink
	// FromBeginning exports the whole existing table the first time a sink
	// runs. The default starts at the end of the table, so enabling export
	// (or adding a sink) does not replay history into the SIEM.
	FromBeginning bool
	Interval      time.Duration
	BatchSize     int
	Timeout       time.Duration
	Logger        *slog.Logger
}

// AuditEventsRelay tails centry.audit_events and sends new rows to each sink.
//
// # Order and completeness
//
// Rows are read in (export_txid, id) order, where export_txid is the
// inserting transaction (0196), and only rows whose transaction is older than
// pg_snapshot_xmin of the reading statement's snapshot: every transaction
// below that bound has ended, so the set of such rows is final and no row
// can later appear behind a position the relay has passed. An id cursor
// alone cannot promise that: ids are taken at insert and become visible at
// commit, on several replicas, so a lower id can commit after a higher one
// was exported. The price is latency: a long-running write transaction
// anywhere in the cluster holds the bound back, and export waits for it (the
// lag alert shows it). Rows from before 0196 have export_txid 0 and are read
// first, by id.
//
// # Positions
//
// Each sink has a row in elitea_runtime.audit_export_cursors with its own
// position, so a healthy sink receives each row once while another sink is
// down, and the failing sink resumes from where it stopped. A position moves
// past a row only after the sink settled it (accepted, or dead-lettered by
// the pipeline), so a crash or an outage re-sends and never skips.
//
// # No transaction across the network
//
// A pass reads the position and rows with plain statements, sends, and then
// advances with a compare-and-set (UPDATE ... WHERE last_txid = old AND
// last_id = old). Nothing is locked while the receiver is being called. One
// replica relays at a time because it holds a session-level advisory lock on
// a dedicated connection; if that connection is lost, another replica may
// take over mid-pass, and the compare-and-set then refuses the stale
// replica's advance (its rows were delivered twice, which at-least-once
// allows).
type AuditEventsRelay struct {
	db   DB
	cfg  AuditEventsConfig
	spec streamSpec

	// pass serialises Drain within the process; the advisory lock does so
	// across replicas.
	pass     sync.Mutex
	mu       sync.Mutex
	lockConn *pgx.Conn
}

// streamSpec is what differs between the tables a relay tails: the stream
// name of its cursors and metrics, and the statements that read it in
// (export_txid, id) order.
type streamSpec struct {
	stream string
	// newestSQL selects the newest (export_txid, id) of the table, for a
	// new sink's start position.
	newestSQL string
	// legacySQL reads rows from before the table had export_txid ($1 id,
	// $2 limit); empty when every row has one.
	legacySQL string
	// rowsSQL reads rows after a position whose transaction ended ($1 txid,
	// $2 id, $3 limit).
	rowsSQL string
	scan    func(pgx.Rows) ([]Event, []position, error)
}

var adminAuditSpec = streamSpec{
	stream: StreamAdminAudit,
	newestSQL: `SELECT export_txid, id FROM centry.audit_events
     WHERE export_txid <> 0
     ORDER BY export_txid DESC, id DESC
     LIMIT 1`,
	legacySQL: selectLegacyRowsSQL,
	rowsSQL:   selectRowsSQL,
	scan:      scanAuditRows,
}

// NewAuditEventsRelay validates the configuration and tails
// centry.audit_events.
func NewAuditEventsRelay(db DB, cfg AuditEventsConfig) (*AuditEventsRelay, error) {
	return newTableRelay(db, cfg, adminAuditSpec)
}

// NewRunStepsRelay is the same relay over elitea_runtime.run_steps: each
// step record goes to every sink as a run_step event, with its own position
// per sink (stream run_steps), in commit-safe order.
func NewRunStepsRelay(db DB, cfg AuditEventsConfig) (*AuditEventsRelay, error) {
	return newTableRelay(db, cfg, runStepsSpec)
}

func newTableRelay(db DB, cfg AuditEventsConfig, spec streamSpec) (*AuditEventsRelay, error) {
	if db == nil {
		return nil, errors.New("audit events relay needs a database")
	}
	if len(cfg.Sinks) == 0 {
		return nil, errors.New("audit events relay needs a sink")
	}
	seen := map[string]bool{}
	for _, sink := range cfg.Sinks {
		if sink == nil {
			return nil, errors.New("audit events relay: nil sink")
		}
		name := sink.Name()
		if !sinkNameRe.MatchString(name) {
			return nil, fmt.Errorf("audit events relay: sink name %q must match %s", name, sinkNameRe)
		}
		if seen[name] {
			return nil, fmt.Errorf("audit events relay: sink %q configured twice", name)
		}
		seen[name] = true
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &AuditEventsRelay{db: db, cfg: cfg, spec: spec}, nil
}

// Run relays until ctx ends. A failure is logged and retried on the next tick.
func (r *AuditEventsRelay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	defer r.Close()
	for {
		if _, err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Warn("auditexport: audit events relay pass failed; rows stay pending", "stream", r.spec.stream, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Close releases the relay lock, so another replica can take over.
func (r *AuditEventsRelay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLockLocked()
}

func (r *AuditEventsRelay) dropLockLocked() {
	if r.lockConn != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// Unlock first: closing the session releases the lock too, but only
		// once the server has noticed the backend exit, so a replica asking
		// right after would still find it held. Best effort; the close below
		// is the backstop.
		_, _ = r.lockConn.Exec(closeCtx, `SELECT pg_advisory_unlock($1)`, lockKey(r.spec.stream))
		_ = r.lockConn.Close(closeCtx) // ends the session, which releases the lock
		cancel()
		r.lockConn = nil
	}
}

// holdLock reports whether this relay holds the relay lock, taking it when
// free. false with no error means another replica holds it.
func (r *AuditEventsRelay) holdLock(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lockConn != nil {
		if err := r.lockConn.Ping(ctx); err == nil {
			return true, nil
		}
		// The session is gone, or unusable: so is the lock.
		r.dropLockLocked()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("audit events export: acquire lock connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey(r.spec.stream)).Scan(&ok); err != nil {
		conn.Release()
		return false, fmt.Errorf("audit events export: advisory lock: %w", err)
	}
	if !ok {
		conn.Release() // holds no lock: safe to return to the pool
		return false, nil
	}
	// Take the connection out of the pool: it must never be handed to other
	// work while it holds the lock.
	r.lockConn = conn.Hijack()
	return true, nil
}

// position is a place in the export order: export_txid, then id.
type position struct{ txid, id int64 }

// ensureCursorsSQL gives a new sink its start position. Without
// FromBeginning that is the newest row this statement can see, or the
// snapshot's xmin when that is later. xmin alone is not enough: any
// transaction open elsewhere in the cluster holds it below rows that are
// already committed, and those rows would then be replayed as new. A row
// still in flight at this instant whose transaction is older than the newest
// visible row counts as history and is not exported.
func ensureCursorsSQL(newest string) string {
	return `
WITH bound AS (
    SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint AS xmin
), newest AS (
    ` + newest + `
), start AS (
    SELECT CASE WHEN n.export_txid >= b.xmin THEN n.export_txid ELSE b.xmin END AS txid,
           CASE WHEN n.export_txid >= b.xmin THEN n.id ELSE 0 END AS id
      FROM bound AS b LEFT JOIN newest AS n ON true
)
INSERT INTO elitea_runtime.audit_export_cursors (stream, sink, last_txid, last_id)
SELECT $1::text, s,
       CASE WHEN $3::boolean THEN 0::bigint ELSE start.txid END,
       CASE WHEN $3::boolean THEN 0::bigint ELSE start.id END
  FROM unnest($2::text[]) AS s CROSS JOIN start
ON CONFLICT (stream, sink) DO NOTHING`
}

// pruneCursorsSQL drops the positions of sinks no longer configured, so a
// removed sink does not hold the retention sweep back forever.
const pruneCursorsSQL = `
DELETE FROM elitea_runtime.audit_export_cursors
 WHERE stream = $1 AND NOT (sink = ANY($2::text[]))`

const readPositionSQL = `
SELECT last_txid, last_id FROM elitea_runtime.audit_export_cursors WHERE stream = $1 AND sink = $2`

const auditColumns = `e.id, e.timestamp, e.user_id, e.user_email, e.project_id, e.event_type, e.action,
       e.http_method, e.http_route, e.status_code, e.duration_ms, e.is_error,
       e.entity_type, e.entity_id, e.entity_name, e.trace_id, e.span_id, e.parent_span_id,
       e.partition_key, e.partition_seq, e.prev_hash, e.hash`

// selectLegacyRowsSQL reads rows inserted before 0196 (export_txid 0). That
// set is closed: the migration's lock waited for every inserting transaction.
const selectLegacyRowsSQL = `
SELECT e.export_txid, ` + auditColumns + `
  FROM centry.audit_events e
 WHERE e.export_txid = 0 AND e.id > $1
 ORDER BY e.id
 LIMIT $2`

// selectRowsSQL reads rows after a position whose inserting transaction has
// ended: it is below the oldest transaction still running when this
// statement's snapshot was taken.
const selectRowsSQL = `
SELECT e.export_txid, ` + auditColumns + `
  FROM centry.audit_events e
 WHERE e.export_txid <> 0
   AND (e.export_txid, e.id) > ($1::bigint, $2::bigint)
   AND e.export_txid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint
 ORDER BY e.export_txid, e.id
 LIMIT $3`

const advanceCursorSQL = `
UPDATE elitea_runtime.audit_export_cursors
   SET last_txid = $3, last_id = $4, updated_at = clock_timestamp()
 WHERE stream = $1 AND sink = $2 AND last_txid = $5 AND last_id = $6`

// Drain relays every sink until its batch comes back short or fails. It
// returns the number of events settled, summed over the sinks. A failing sink
// does not stop the others.
func (r *AuditEventsRelay) Drain(ctx context.Context) (int, error) {
	if !r.pass.TryLock() {
		return 0, nil // a pass of this relay is already running
	}
	defer r.pass.Unlock()
	ok, err := r.holdLock(ctx)
	if err != nil || !ok {
		return 0, err
	}
	names := make([]string, len(r.cfg.Sinks))
	for i, sink := range r.cfg.Sinks {
		names[i] = sink.Name()
	}
	if _, err := r.db.Exec(ctx, ensureCursorsSQL(r.spec.newestSQL), r.spec.stream, names, r.cfg.FromBeginning); err != nil {
		return 0, fmt.Errorf("audit events export: ensure cursors: %w", err)
	}
	if tag, err := r.db.Exec(ctx, pruneCursorsSQL, r.spec.stream, names); err != nil {
		return 0, fmt.Errorf("audit events export: prune cursors: %w", err)
	} else if tag.RowsAffected() > 0 {
		r.cfg.Logger.Info("auditexport: dropped the positions of sinks no longer configured", "rows", tag.RowsAffected())
	}
	total := 0
	var errs []error
	for _, sink := range r.cfg.Sinks {
		for {
			n, full, err := r.exportSinkBatch(ctx, sink)
			total += n
			if err != nil {
				errs = append(errs, fmt.Errorf("sink %s: %w", sink.Name(), err))
				break
			}
			if !full || ctx.Err() != nil {
				break
			}
		}
	}
	return total, errors.Join(errs...)
}

// exportSinkBatch sends one batch to one sink from its own position. full
// reports a whole batch was read and settled, so more may follow.
func (r *AuditEventsRelay) exportSinkBatch(ctx context.Context, sink Sink) (int, bool, error) {
	var from position
	if err := r.db.QueryRow(ctx, readPositionSQL, r.spec.stream, sink.Name()).Scan(&from.txid, &from.id); err != nil {
		return 0, false, fmt.Errorf("read position: %w", err)
	}
	events, positions, err := r.readAfter(ctx, from)
	if err != nil {
		return 0, false, err
	}
	if len(events) == 0 {
		return 0, false, nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	sendErr := sink.Send(sendCtx, events)
	cancel()
	settled := settledCount(sendErr, len(events))
	if settled > 0 {
		to := positions[settled-1]
		tag, err := r.db.Exec(ctx, advanceCursorSQL, r.spec.stream, sink.Name(), to.txid, to.id, from.txid, from.id)
		if err != nil {
			return 0, false, errors.Join(sendErr, fmt.Errorf("advance position: %w", err))
		}
		if tag.RowsAffected() != 1 {
			return settled, false, errors.Join(sendErr, ErrCursorMoved)
		}
	}
	if sendErr != nil {
		return settled, false, sendErr
	}
	return settled, len(events) == r.cfg.BatchSize, nil
}

// readAfter reads the next batch after a position: rows from before
// export_txid existed first (centry.audit_events before 0196), then rows
// whose inserting transaction has ended.
func (r *AuditEventsRelay) readAfter(ctx context.Context, from position) ([]Event, []position, error) {
	if from.txid == 0 && r.spec.legacySQL != "" {
		rows, err := r.db.Query(ctx, r.spec.legacySQL, from.id, r.cfg.BatchSize)
		if err != nil {
			return nil, nil, fmt.Errorf("read rows: %w", err)
		}
		events, positions, err := r.spec.scan(rows)
		if err != nil || len(events) > 0 {
			return events, positions, err
		}
	}
	rows, err := r.db.Query(ctx, r.spec.rowsSQL, from.txid, from.id, r.cfg.BatchSize)
	if err != nil {
		return nil, nil, fmt.Errorf("read rows: %w", err)
	}
	return r.spec.scan(rows)
}

func scanAuditRows(rows pgx.Rows) ([]Event, []position, error) {
	defer rows.Close()
	var (
		events    []Event
		positions []position
	)
	for rows.Next() {
		var (
			txid                                             int64
			row                                              AuditRow
			userID, projectID, entityID                      *int32
			statusCode                                       *int16
			userEmail, method, route, entityType, entityName *string
			traceID, spanID, parentSpanID                    *string
		)
		if err := rows.Scan(&txid, &row.ID, &row.Timestamp, &userID, &userEmail, &projectID, &row.EventType, &row.Action,
			&method, &route, &statusCode, &row.DurationMS, &row.IsError,
			&entityType, &entityID, &entityName, &traceID, &spanID, &parentSpanID,
			&row.PartitionKey, &row.PartitionSeq, &row.PrevHash, &row.Hash); err != nil {
			return nil, nil, err
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
		positions = append(positions, position{txid: txid, id: row.ID})
	}
	return events, positions, rows.Err()
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
	// Security reports the security audit stream too. elitea-main sets it
	// on every install: the relay runs whatever the governance flag (#1499).
	Security bool
}

const securityBacklogSQL = `
SELECT count(*), coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)::float8
  FROM elitea_runtime.security_audit_outbox
 WHERE exported_at IS NULL`

// slowestPositionSQL is the position of the sink furthest behind. No row
// means the relay has not made its positions yet, and nothing is counted, so
// the first scrape never counts the whole table.
const slowestPositionSQL = `
SELECT last_txid, last_id FROM elitea_runtime.audit_export_cursors
 WHERE stream = $1 ORDER BY last_txid, last_id LIMIT 1`

// The backlog is counted in two statements so each uses an index: rows after
// the position (the partial export-order index), and rows from before 0196
// (the primary key), which only count while the position is still among them.
const auditBacklogSQL = `
SELECT count(*), min(e.timestamp)
  FROM centry.audit_events e
 WHERE e.export_txid <> 0 AND (e.export_txid, e.id) > ($1::bigint, $2::bigint)`

const auditLegacyBacklogSQL = `
SELECT count(*), min(e.timestamp)
  FROM centry.audit_events e
 WHERE e.export_txid = 0 AND e.id > $1`

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
	if n, lag, err := m.auditBacklog(ctx); err != nil {
		errs = append(errs, fmt.Errorf("audit events backlog: %w", err))
	} else {
		m.Metrics.SetStream(StreamAdminAudit, n, lag)
	}
	if n, lag, err := m.runStepsBacklog(ctx); err != nil {
		errs = append(errs, fmt.Errorf("run steps backlog: %w", err))
	} else {
		m.Metrics.SetStream(StreamRunSteps, n, lag)
	}
	return errors.Join(errs...)
}

func (m Monitor) auditBacklog(ctx context.Context) (int64, float64, error) {
	var at position
	err := m.DB.QueryRow(ctx, slowestPositionSQL, StreamAdminAudit).Scan(&at.txid, &at.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var (
		total  int64
		oldest *time.Time
	)
	count := func(sql string, args ...any) error {
		var n int64
		var first *time.Time
		if err := m.DB.QueryRow(ctx, sql, args...).Scan(&n, &first); err != nil {
			return err
		}
		total += n
		if first != nil && (oldest == nil || first.Before(*oldest)) {
			oldest = first
		}
		return nil
	}
	if at.txid == 0 {
		if err := count(auditLegacyBacklogSQL, at.id); err != nil {
			return 0, 0, err
		}
	}
	if err := count(auditBacklogSQL, at.txid, at.id); err != nil {
		return 0, 0, err
	}
	lag := 0.0
	if oldest != nil {
		lag = max(0, time.Since(*oldest).Seconds())
	}
	return total, lag, nil
}

const runStepsBacklogSQL = `
SELECT count(*), min(s.recorded_at)
  FROM elitea_runtime.run_steps s
 WHERE (s.export_txid, s.id) > ($1::bigint, $2::bigint)`

// runStepsBacklog counts the step records the slowest run_steps sink has
// not passed. No position yet means nothing is counted.
func (m Monitor) runStepsBacklog(ctx context.Context) (int64, float64, error) {
	var at position
	err := m.DB.QueryRow(ctx, slowestPositionSQL, StreamRunSteps).Scan(&at.txid, &at.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var (
		n      int64
		oldest *time.Time
	)
	if err := m.DB.QueryRow(ctx, runStepsBacklogSQL, at.txid, at.id).Scan(&n, &oldest); err != nil {
		return 0, 0, err
	}
	lag := 0.0
	if oldest != nil {
		lag = max(0, time.Since(*oldest).Seconds())
	}
	return n, lag, nil
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
