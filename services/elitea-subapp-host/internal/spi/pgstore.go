package spi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the durable invocation store (ADR-0023 H2b): the
// `invocations` and `invocation_events` tables the DeepWiki engine's
// migrations own (services/elitea-deepwiki-engine/migrations/0002_invocations.sql),
// used exactly as the retired Python shell used them — one statement per operation,
// events drained with a single DELETE … RETURNING so two pollers cannot
// interleave, rows owned by the process that accepted them.
//
// Durable is true, and that is not decoration: /health reports it, and an
// operator uses it to tell a compliant deployment from a dev one.
type PostgresStore struct {
	pool   *pgxpool.Pool
	dsn    string
	owner  string
	logger *slog.Logger
}

// OwnerID is hostname plus pid: in Kubernetes the hostname is the pod name,
// so a restarted pod has a new one, and a restarted process in the same pod
// a new pid. Either change makes the previous owner's rows recognisable.
func OwnerID() string {
	host := os.Getenv("HOSTNAME")
	if host == "" {
		host, _ = os.Hostname()
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// NewPostgresStore connects and proves the connection. owner defaults to
// OwnerID().
func NewPostgresStore(ctx context.Context, dsn, owner string, logger *slog.Logger) (*PostgresStore, error) {
	if owner == "" {
		owner = OwnerID()
	}
	if logger == nil {
		logger = slog.Default()
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	return &PostgresStore{pool: pool, dsn: dsn, owner: owner, logger: logger}, nil
}

// DSN is the connection string the store was built with.
func (s *PostgresStore) DSN() string { return s.dsn }

// Close releases the pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// Owner is the id this process writes on the rows it accepts.
func (s *PostgresStore) Owner() string { return s.owner }

// Durable reports true: a row survives this process.
func (s *PostgresStore) Durable() bool { return true }

// Create inserts the accepted invocation under this owner.
func (s *PostgresStore) Create(ctx context.Context, invocation *Invocation) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO invocations (invocation_id, toolkit_name, tool_name, status, owner) VALUES ($1, $2, $3, $4, $5)`,
		invocation.ID, invocation.Toolkit, invocation.Tool, invocation.Status, s.owner)
	return err
}

// Get reads one row; nil when there is none.
func (s *PostgresStore) Get(ctx context.Context, toolkit, tool, id string) (*Invocation, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT invocation_id, toolkit_name, tool_name, status, stop_requested, result, created_at, finished_at
		   FROM invocations WHERE toolkit_name = $1 AND tool_name = $2 AND invocation_id = $3`,
		toolkit, tool, id)
	var (
		invocation Invocation
		result     []byte
		finished   *time.Time
	)
	if err := row.Scan(&invocation.ID, &invocation.Toolkit, &invocation.Tool, &invocation.Status, &invocation.StopRequest, &result, &invocation.CreatedAt, &finished); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if finished != nil {
		invocation.FinishedAt = *finished
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &invocation.Result); err != nil {
			return nil, fmt.Errorf("invocation %s: stored result is not an object: %w", id, err)
		}
	}
	return &invocation, nil
}

// Update persists status, the stop flag, the result and — once terminal —
// finished_at. Unlike the in-memory store, where the struct IS the state,
// this is the only thing that persists a change.
func (s *PostgresStore) Update(ctx context.Context, invocation *Invocation) error {
	var result []byte
	if invocation.Result != nil {
		encoded, err := json.Marshal(invocation.Result)
		if err != nil {
			return err
		}
		result = encoded
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE invocations SET status = $1, stop_requested = $2, result = $3,
		        finished_at = CASE WHEN $4 THEN now() ELSE finished_at END
		  WHERE invocation_id = $5`,
		invocation.Status, invocation.StopRequest, result, invocation.Terminal(), invocation.ID)
	return err
}

// AppendEvent stores one thinking event.
func (s *PostgresStore) AppendEvent(ctx context.Context, id, message string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO invocation_events (invocation_id, message) VALUES ($1, $2)`, id, message)
	return err
}

// DrainEvents returns the accumulated events in order and clears them, in
// ONE statement, so the read and the clear cannot interleave with another
// poller's.
func (s *PostgresStore) DrainEvents(ctx context.Context, invocation *Invocation) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `DELETE FROM invocation_events WHERE invocation_id = $1 RETURNING message, id`, invocation.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type event struct {
		message string
		id      int64
	}
	var drained []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.message, &e.id); err != nil {
			return nil, err
		}
		drained = append(drained, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(drained, func(i, j int) bool { return drained[i].id < drained[j].id })
	events := make([]map[string]any, 0, len(drained))
	for _, e := range drained {
		events = append(events, map[string]any{"data": map[string]any{"message": e.message}})
	}
	return events, nil
}

// Prune deletes terminal rows older than the retention; events cascade.
func (s *PostgresStore) Prune(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM invocations WHERE finished_at IS NOT NULL AND finished_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Reconcile terminates in-flight rows left behind by a previous owner.
// Called at startup: a row still pending/running under a different owner
// is work no live process is doing; leaving it would make a poll return
// InProgress forever. It becomes a terminal error whose message says what
// happened, which is an answer the caller can act on.
func (s *PostgresStore) Reconcile(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT invocation_id, tool_name FROM invocations WHERE finished_at IS NULL AND owner IS DISTINCT FROM $1`, s.owner)
	if err != nil {
		return 0, err
	}
	type orphan struct{ id, tool string }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.tool); err != nil {
			rows.Close()
			return 0, err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, o := range orphans {
		body := ToolError(s.logger, o.id, o.tool, Failf(KindRuntime, "The service restarted while this invocation was running, so it did not complete. Start it again."))
		encoded, _ := json.Marshal(body)
		if _, err := s.pool.Exec(ctx,
			`UPDATE invocations SET status = $1, result = $2, finished_at = now(), owner = $3 WHERE invocation_id = $4`,
			statusStopped, encoded, s.owner, o.id); err != nil {
			return 0, err
		}
	}
	if len(orphans) > 0 {
		s.logger.Warn("reconciled invocations orphaned by a previous process", "count", len(orphans))
	}
	return len(orphans), nil
}

// Count is the number of rows, for tests and health.
func (s *PostgresStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invocations`).Scan(&n)
	return n, err
}
