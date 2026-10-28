package securityaudit

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Relay defaults.
const (
	DefaultRelayInterval  = time.Second
	DefaultRelayBatchSize = 500
	DefaultExportTimeout  = 30 * time.Second
	maxRelayBatchSize     = 10000
)

// TxStarter is the slice of *pgxpool.Pool the relay uses.
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RelayConfig configures the relay.
type RelayConfig struct {
	// BacklogAlertThreshold is the unexported row count above which the
	// alert metric is 1 (PolicyDocument.AuditBacklogAlertThreshold).
	// Required. Rows are never dropped, whatever the backlog.
	BacklogAlertThreshold int64
	// Exporter is optional. Without one a row is exported when it is in the
	// durable store.
	Exporter      Exporter
	Interval      time.Duration
	BatchSize     int
	ExportTimeout time.Duration
	Logger        *slog.Logger
	// Meter defaults to otel.Meter("elitea-main").
	Meter metric.Meter
}

// Relay copies outbox rows to the durable store and the exporter, in seq
// order, and reports the backlog.
//
// Each stage takes a transaction-scoped advisory lock first, so with several
// replicas one relay works a stage at a time and the order holds. Rows are
// picked FOR UPDATE SKIP LOCKED, so the relay never waits on a row another
// transaction holds.
type Relay struct {
	db        TxStarter
	exporter  Exporter
	threshold int64
	interval  time.Duration
	batch     int
	timeout   time.Duration
	logger    *slog.Logger

	backlogGauge metric.Int64Gauge
	alertGauge   metric.Int64Gauge
	errorCounter metric.Int64Counter
	alerting     bool
}

// NewRelay builds the relay. Start it with Run only while the governance flag
// is on.
func NewRelay(db TxStarter, cfg RelayConfig) (*Relay, error) {
	if db == nil {
		return nil, errors.New("security audit relay needs a database")
	}
	if cfg.BacklogAlertThreshold <= 0 {
		return nil, errors.New("security audit relay needs a positive backlog alert threshold")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultRelayInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultRelayBatchSize
	}
	if cfg.BatchSize > maxRelayBatchSize {
		return nil, fmt.Errorf("security audit relay batch size must be at most %d", maxRelayBatchSize)
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = DefaultExportTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Meter == nil {
		cfg.Meter = otel.Meter("elitea-main")
	}
	r := &Relay{
		db: db, exporter: cfg.Exporter, threshold: cfg.BacklogAlertThreshold,
		interval: cfg.Interval, batch: cfg.BatchSize, timeout: cfg.ExportTimeout, logger: cfg.Logger,
	}
	var err error
	if r.backlogGauge, err = cfg.Meter.Int64Gauge("security_audit.backlog",
		metric.WithDescription("Security audit outbox rows not yet exported. Rows are never dropped.")); err != nil {
		return nil, err
	}
	if r.alertGauge, err = cfg.Meter.Int64Gauge("security_audit.backlog_alert",
		metric.WithDescription("1 while the security audit backlog exceeds the policy threshold, else 0.")); err != nil {
		return nil, err
	}
	if r.errorCounter, err = cfg.Meter.Int64Counter("security_audit.relay.errors",
		metric.WithDescription("Security audit relay stage failures, by stage. The rows stay pending.")); err != nil {
		return nil, err
	}
	return r, nil
}

// Run relays until ctx ends. A failure is logged and retried on the next
// tick; the rows stay pending.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if _, err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("securityaudit: relay pass failed; rows stay pending", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DrainStats is what one Drain moved.
type DrainStats struct {
	Stored   int
	Exported int
	Backlog  int64
}

// Drain runs both stages until a batch comes back short or one fails, then
// reports the backlog. Exported so a test drives the relay without a ticker.
func (r *Relay) Drain(ctx context.Context) (DrainStats, error) {
	var stats DrainStats
	var stageErr error
	for {
		n, err := r.StoreBatch(ctx)
		stats.Stored += n
		if err != nil {
			r.errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "store")))
			stageErr = err
			break
		}
		if n < r.batch {
			break
		}
	}
	if r.exporter != nil && stageErr == nil {
		for {
			n, err := r.ExportBatch(ctx)
			stats.Exported += n
			if err != nil {
				r.errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "export")))
				stageErr = err
				break
			}
			if n < r.batch {
				break
			}
		}
	}
	backlog, err := r.Backlog(ctx)
	stats.Backlog = backlog
	if err != nil {
		return stats, errors.Join(stageErr, err)
	}
	r.report(ctx, backlog)
	return stats, stageErr
}

func (r *Relay) report(ctx context.Context, backlog int64) {
	r.backlogGauge.Record(ctx, backlog)
	alert := backlog > r.threshold
	var value int64
	if alert {
		value = 1
	}
	r.alertGauge.Record(ctx, value)
	if alert != r.alerting {
		if alert {
			r.logger.Error("securityaudit: backlog exceeds the alert threshold; no row is dropped",
				"backlog", backlog, "threshold", r.threshold)
		} else {
			r.logger.Info("securityaudit: backlog is below the alert threshold again",
				"backlog", backlog, "threshold", r.threshold)
		}
		r.alerting = alert
	}
}

// Alerting reports whether the last Drain saw the backlog above threshold.
func (r *Relay) Alerting() bool { return r.alerting }

const backlogSQL = `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE exported_at IS NULL`

// Backlog is the number of rows not yet exported.
func (r *Relay) Backlog(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRow(ctx, backlogSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("security audit backlog: %w", err)
	}
	return n, nil
}

// storeBatchSQL copies the oldest unstored rows to the durable store and
// marks them stored (and exported when $2, no exporter). ON CONFLICT keeps a
// copy that a crashed earlier pass already committed.
const storeBatchSQL = `
WITH batch AS (
    SELECT seq
      FROM elitea_runtime.security_audit_outbox
     WHERE stored_at IS NULL
     ORDER BY seq
     LIMIT $1
       FOR UPDATE SKIP LOCKED
), copied AS (
    INSERT INTO elitea_identity.security_audit_events
        (seq, partition_key, partition_seq, event_id, event_type, event, prev_hash, hash, created_at)
    SELECT o.seq, o.partition_key, o.partition_seq, o.event_id, o.event_type, o.event, o.prev_hash, o.hash, o.created_at
      FROM elitea_runtime.security_audit_outbox o
      JOIN batch USING (seq)
     ORDER BY o.seq
    ON CONFLICT (seq) DO NOTHING
    RETURNING seq
)
UPDATE elitea_runtime.security_audit_outbox o
   SET stored_at = clock_timestamp(),
       exported_at = CASE WHEN $2 THEN clock_timestamp() ELSE NULL END
  FROM batch
 WHERE o.seq = batch.seq
RETURNING o.seq, (SELECT count(*) FROM copied)`

// checkStoredSQL confirms that every row of a batch is in the durable store
// with the same chain position and hash. ON CONFLICT DO NOTHING must never
// hide a different row under the same seq.
const checkStoredSQL = `
SELECT count(*)
  FROM elitea_runtime.security_audit_outbox o
  JOIN elitea_identity.security_audit_events d
    ON d.seq = o.seq AND d.partition_key = o.partition_key
   AND d.partition_seq = o.partition_seq AND d.hash = o.hash
 WHERE o.seq = ANY($1)`

// StoreBatch runs one store-stage batch and returns the rows it stored.
func (r *Relay) StoreBatch(ctx context.Context) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("security audit store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if ok, err := tryStageLock(ctx, tx, "store"); err != nil || !ok {
		return 0, err
	}
	rows, err := tx.Query(ctx, storeBatchSQL, r.batch, r.exporter == nil)
	if err != nil {
		return 0, fmt.Errorf("security audit store: %w", err)
	}
	var seqs []int64
	for rows.Next() {
		var seq, copied int64
		if err := rows.Scan(&seq, &copied); err != nil {
			rows.Close()
			return 0, fmt.Errorf("security audit store: scan: %w", err)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("security audit store: %w", err)
	}
	if len(seqs) == 0 {
		return 0, nil
	}
	var matched int
	if err := tx.QueryRow(ctx, checkStoredSQL, seqs).Scan(&matched); err != nil {
		return 0, fmt.Errorf("security audit store: check copy: %w", err)
	}
	if matched != len(seqs) {
		return 0, fmt.Errorf("security audit store: %d of %d rows differ from the durable store copy", len(seqs)-matched, len(seqs))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("security audit store: commit: %w", err)
	}
	return len(seqs), nil
}

const exportBatchSQL = `
SELECT seq, partition_key, partition_seq, event_id::text, event_type, event::text, prev_hash, hash, created_at
  FROM elitea_runtime.security_audit_outbox
 WHERE exported_at IS NULL AND stored_at IS NOT NULL
 ORDER BY seq
 LIMIT $1
   FOR UPDATE SKIP LOCKED`

const markExportedSQL = `
UPDATE elitea_runtime.security_audit_outbox
   SET exported_at = clock_timestamp()
 WHERE seq = ANY($1) AND exported_at IS NULL`

// exportBatchUnlockedSQL is exportBatchSQL without the row lock: the
// session-level stage lock already makes this relay the only exporter.
const exportBatchUnlockedSQL = `
SELECT seq, partition_key, partition_seq, event_id::text, event_type, event::text, prev_hash, hash, created_at
  FROM elitea_runtime.security_audit_outbox
 WHERE exported_at IS NULL AND stored_at IS NOT NULL
 ORDER BY seq
 LIMIT $1`

// connAcquirer is the slice of *pgxpool.Pool that lets ExportBatch hold a
// session-level stage lock on one connection without an open transaction.
type connAcquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// ExportBatch runs one export-stage batch and returns the rows it exported.
// An exporter error leaves the rows unexported.
//
// The send to the exporter (a SIEM, possibly slow, with its own retries) runs
// with NO transaction open (#1504 review): a transaction held across it would
// pin a pool connection for up to the export timeout and be killed by
// idle_in_transaction_session_timeout mid-send, after the receiver accepted
// the batch, so the batch would be sent again on every pass. With a pool, the
// pass holds a session-level advisory lock on one connection, reads, sends,
// then marks; the mark is idempotent (exported_at IS NULL).
func (r *Relay) ExportBatch(ctx context.Context) (int, error) {
	if r.exporter == nil {
		return 0, nil
	}
	if pool, ok := r.db.(connAcquirer); ok {
		return r.exportBatchOnConn(ctx, pool)
	}
	return r.exportBatchInTx(ctx)
}

func (r *Relay) exportBatchOnConn(ctx context.Context, pool connAcquirer) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("security audit export: acquire: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, stageLockKey("export")).Scan(&locked); err != nil {
		return 0, fmt.Errorf("security audit export: advisory lock: %w", err)
	}
	if !locked {
		return 0, nil
	}
	defer func() {
		// A failed unlock (a dead connection) releases the lock with the session.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, stageLockKey("export"))
	}()
	rows, err := conn.Query(ctx, exportBatchUnlockedSQL, r.batch)
	if err != nil {
		return 0, fmt.Errorf("security audit export: %w", err)
	}
	records, err := scanRecords(rows)
	if err != nil {
		return 0, fmt.Errorf("security audit export: %w", err)
	}
	if len(records) == 0 {
		return 0, nil
	}
	if err := r.export(ctx, records); err != nil {
		return 0, err
	}
	if _, err := conn.Exec(ctx, markExportedSQL, recordSeqs(records)); err != nil {
		return 0, fmt.Errorf("security audit export: mark: %w", err)
	}
	return len(records), nil
}

func (r *Relay) export(ctx context.Context, records []Record) error {
	exportCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.exporter.Export(exportCtx, records); err != nil {
		return fmt.Errorf("security audit export: exporter %s: %w", r.exporter.Name(), err)
	}
	return nil
}

func recordSeqs(records []Record) []int64 {
	seqs := make([]int64, len(records))
	for i, record := range records {
		seqs[i] = record.Seq
	}
	return seqs
}

// exportBatchInTx is the path for a TxStarter that is not a pool (test
// doubles): the whole pass in one transaction under the stage's xact lock.
func (r *Relay) exportBatchInTx(ctx context.Context) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("security audit export: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if ok, err := tryStageLock(ctx, tx, "export"); err != nil || !ok {
		return 0, err
	}
	rows, err := tx.Query(ctx, exportBatchSQL, r.batch)
	if err != nil {
		return 0, fmt.Errorf("security audit export: %w", err)
	}
	records, err := scanRecords(rows)
	if err != nil {
		return 0, fmt.Errorf("security audit export: %w", err)
	}
	if len(records) == 0 {
		return 0, nil
	}
	if err := r.export(ctx, records); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, markExportedSQL, recordSeqs(records)); err != nil {
		return 0, fmt.Errorf("security audit export: mark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("security audit export: commit: %w", err)
	}
	return len(records), nil
}

func scanRecords(rows pgx.Rows) ([]Record, error) {
	defer rows.Close()
	var records []Record
	for rows.Next() {
		var (
			record         Record
			eventType      string
			event          string
			prevHash, hash []byte
		)
		if err := rows.Scan(&record.Seq, &record.PartitionKey, &record.PartitionSeq, &record.EventID,
			&eventType, &event, &prevHash, &hash, &record.CreatedAt); err != nil {
			return nil, err
		}
		if len(prevHash) != len(Hash{}) || len(hash) != len(Hash{}) {
			return nil, fmt.Errorf("row seq %d has a hash of the wrong length", record.Seq)
		}
		record.EventType = EventType(eventType)
		record.Event = []byte(event)
		copy(record.PrevHash[:], prevHash)
		copy(record.Hash[:], hash)
		records = append(records, record)
	}
	return records, rows.Err()
}

// tryStageLock takes the stage's transaction-scoped advisory lock. false
// means another relay holds it; this pass skips the stage.
func tryStageLock(ctx context.Context, tx pgx.Tx, stage string) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, stageLockKey(stage)).Scan(&ok); err != nil {
		return false, fmt.Errorf("security audit %s: advisory lock: %w", stage, err)
	}
	return ok, nil
}

func stageLockKey(stage string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("elitea-platform:securityaudit:relay:" + stage))
	return int64(h.Sum64()) // PostgreSQL advisory locks use the bit pattern.
}
