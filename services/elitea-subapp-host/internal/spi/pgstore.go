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
// Every replica of a host shares the tables (issue #1359): an invocation is
// read and cancelled through whichever replica a call lands on. A row's
// owner is the process running it, and that process holds a session
// advisory lock named after itself for as long as it lives (claim), so a
// replica that starts can tell a row whose owner is gone — orphaned, to be
// terminated — from one a sibling replica is running right now.
//
// Durable is true, and that is not decoration: /health reports it, and an
// operator uses it to tell a compliant deployment from a dev one.
type PostgresStore struct {
	pool   *pgxpool.Pool
	dsn    string
	owner  string
	logger *slog.Logger
	// claim is the connection that holds this owner's liveness lock.
	claim *pgxpool.Conn
}

// ownerLockClass is the first key of every owner's liveness lock
// (pg_advisory_lock(int4, int4)); the second is hashtext(owner). A constant
// of its own, so the locks cannot meet any other advisory lock in the
// database.
const ownerLockClass = 0x53_41_48_31 // "SAH1"

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
	store := &PostgresStore{pool: pool, dsn: dsn, owner: owner, logger: logger}
	if err := store.takeClaim(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("invocation store: claim the owner %q: %w", owner, err)
	}
	return store, nil
}

// takeClaim takes this owner's liveness lock on a connection of its own and
// keeps it until Close. A session lock dies with its session, so when this
// process dies (or its database connection does) the lock is free and the
// next replica to start reconciles its rows.
func (s *PostgresStore) takeClaim(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, ownerLockClass, s.owner).Scan(&held); err != nil {
		conn.Release()
		return err
	}
	if !held {
		conn.Release()
		return errors.New("another live process holds this owner id")
	}
	s.claim = conn
	return nil
}

// DSN is the connection string the store was built with.
func (s *PostgresStore) DSN() string { return s.dsn }

// Close releases the claim and the pool.
func (s *PostgresStore) Close() {
	if s.claim != nil {
		// Hijacked and closed rather than released: the session must END for
		// its lock to go, and a connection back in the pool keeps it.
		_ = s.claim.Hijack().Close(context.Background())
		s.claim = nil
	}
	s.pool.Close()
}

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
// finished_at. The stop flag is only ever raised here: a request another
// replica recorded (RequestStop) is not cleared by the owner's next write.
// Unlike the in-memory store, where the struct IS the state, this is the
// only thing that persists a change.
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
		`UPDATE invocations SET status = $1, stop_requested = stop_requested OR $2, result = $3,
		        finished_at = CASE WHEN $4 THEN now() ELSE finished_at END
		  WHERE invocation_id = $5`,
		invocation.Status, invocation.StopRequest, result, invocation.Terminal(), invocation.ID)
	return err
}

// RequestStop records a stop request on the row, and nothing else: the
// replica that owns the run may be writing its terminal body at the same
// moment (Manager.Cancel).
func (s *PostgresStore) RequestStop(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE invocations SET stop_requested = true WHERE invocation_id = $1`, id)
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

// Reconcile terminates in-flight rows left behind by an owner that is gone.
// Called at startup: a row still pending/running whose owner no longer
// holds its liveness lock is work no live process is doing; leaving it
// would make a poll return InProgress forever. It becomes a terminal error
// whose message says what happened, which is an answer the caller can act
// on. A row whose owner still holds its lock is a sibling replica's running
// invocation and is left alone — before #1359 every other owner counted as
// gone, and a second replica starting killed the first one's work.
func (s *PostgresStore) Reconcile(ctx context.Context) (int, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `SELECT DISTINCT owner FROM invocations WHERE finished_at IS NULL AND owner IS DISTINCT FROM $1`, s.owner)
	if err != nil {
		return 0, err
	}
	var owners []*string
	for rows.Next() {
		var owner *string
		if err := rows.Scan(&owner); err != nil {
			rows.Close()
			return 0, err
		}
		owners = append(owners, owner)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	reconciled := 0
	for _, owner := range owners {
		if owner != nil {
			// The lock is free only when its owner's session is gone. Taken
			// for the instant of the check and given back on the same
			// session, so a dead owner's id is never held by this one.
			var free bool
			if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, ownerLockClass, *owner).Scan(&free); err != nil {
				return reconciled, err
			}
			if !free {
				continue
			}
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1, hashtext($2))`, ownerLockClass, *owner); err != nil {
				return reconciled, err
			}
		}
		n, err := s.terminateOrphans(ctx, owner)
		reconciled += n
		if err != nil {
			return reconciled, err
		}
	}
	if reconciled > 0 {
		s.logger.Warn("reconciled invocations orphaned by a process that is gone", "count", reconciled)
	}
	return reconciled, nil
}

// terminateOrphans ends every in-flight row of owner (nil: rows with no
// owner at all) with the restart error.
func (s *PostgresStore) terminateOrphans(ctx context.Context, owner *string) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT invocation_id, tool_name FROM invocations WHERE finished_at IS NULL AND owner IS NOT DISTINCT FROM $1`, owner)
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
	for i, o := range orphans {
		body := ToolError(s.logger, o.id, o.tool, Failf(KindRuntime, "The service restarted while this invocation was running, so it did not complete. Start it again."))
		encoded, _ := json.Marshal(body)
		if _, err := s.pool.Exec(ctx,
			`UPDATE invocations SET status = $1, result = $2, finished_at = now(), owner = $3 WHERE invocation_id = $4 AND finished_at IS NULL`,
			statusStopped, encoded, s.owner, o.id); err != nil {
			return i, err
		}
	}
	return len(orphans), nil
}

// Count is the number of rows, for tests and health.
func (s *PostgresStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invocations`).Scan(&n)
	return n, err
}
