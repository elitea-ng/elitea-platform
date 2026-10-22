package spi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
// replica can tell a row whose owner is gone — orphaned, to be terminated —
// from one a sibling replica is running right now.
//
// The claim is held on a dedicated connection, outside the pool, and checked
// every claimCheckInterval. A session that drops (a PostgreSQL restart, a
// failover, an idle kill) frees the lock, and siblings would then take this
// process for dead. So a lost claim stops this store taking new invocations
// (Create answers ErrClaimLost, and ClaimHeld feeds readiness) until it is
// re-taken on a new connection. A sibling does not take one free sighting
// for death: it terminates an owner's rows only when the lock is still free
// ownerGrace after it first saw it free, and ownerGrace is longer than the
// owner needs to notice and re-take (one check interval plus that check's
// timeout), so a briefly disconnected owner keeps its runs. A row a sibling
// did terminate (an owner cut off for longer) stays terminated: Update writes
// only a row this owner still has in flight.
//
// Durable is true, and that is not decoration: /health reports it, and an
// operator uses it to tell a compliant deployment from a dev one.
type PostgresStore struct {
	pool    *pgxpool.Pool
	dsn     string
	owner   string
	logger  *slog.Logger
	lockKey int64

	claimCheck     time.Duration
	reconcileEvery time.Duration
	ownerGrace     time.Duration

	// claimMu guards claim, the dedicated connection that holds this
	// owner's liveness lock; held mirrors whether it does.
	claimMu sync.Mutex
	claim   *pgx.Conn
	held    atomic.Bool

	// reconcileMu serialises Reconcile; freeSince is when this process first
	// saw each claimed owner's lock free, in a run of free sightings with no
	// held one between.
	reconcileMu sync.Mutex
	freeSince   map[string]time.Time

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// maxConns is the pool's ceiling (WithMaxConns); zero keeps pgx's.
	maxConns int
}

// ErrClaimLost refuses a new invocation while this process does not hold its
// owner liveness lock: siblings may be terminating its rows as orphans, so
// it must not accept work under that owner.
var ErrClaimLost = errors.New("invocation store: this process does not hold its owner claim")

// ownerLockSeed seeds hashtextextended(owner, seed), the 64-bit key of every
// owner's liveness lock (pg_advisory_lock(bigint)). Sixty-four bits, so two
// owners do not collide the way hashtext's 32 did, and a seed of its own, so
// the keys are not those of any other hashtextextended-keyed lock.
const ownerLockSeed = 0x53_41_48_31 // "SAH1"

// claimedOwnerPrefix marks the owner of a row written by a process that holds
// a liveness lock. A row without it was written by an image from before
// #1359, which takes no lock: an absent lock says nothing about whether that
// process is alive, so Reconcile does not read one as death. Those rows keep
// the rule that image applied itself: terminated by the startup pass of a
// process starting, never by the periodic one.
const claimedOwnerPrefix = "claim1:"

const (
	claimCheckInterval = 10 * time.Second
	reconcileInterval  = time.Minute
	// ownerGraceChecks is ownerGrace in claim-check intervals. An owner
	// whose session dropped notices at its next check (up to one interval)
	// and re-takes the lock within that check's timeout (up to one more), so
	// a sibling waits longer than both before it reads a free lock as death.
	ownerGraceChecks = 3
)

// StoreOption tunes a PostgresStore.
type StoreOption func(*PostgresStore)

// WithClaimCheckInterval sets how often the store checks its owner claim.
func WithClaimCheckInterval(d time.Duration) StoreOption {
	return func(s *PostgresStore) { s.claimCheck = d }
}

// WithReconcileInterval sets how often the store reconciles orphans after
// the startup pass.
func WithReconcileInterval(d time.Duration) StoreOption {
	return func(s *PostgresStore) { s.reconcileEvery = d }
}

// maxDatabaseConns bounds <PREFIX>DATABASE_MAX_CONNS, as elitea-main bounds
// its own pools.
const maxDatabaseConns = 64

// WithMaxConns sets the pool's connection ceiling. Zero or less keeps the
// pgx default. The owner claim's connection is outside the pool and not
// counted here.
func WithMaxConns(n int) StoreOption {
	return func(s *PostgresStore) { s.maxConns = n }
}

// WithOwnerGrace sets how long a claimed owner's lock must stay free, from
// this process's first sighting of it free, before its rows are orphans.
// The default is ownerGraceChecks claim-check intervals.
func WithOwnerGrace(d time.Duration) StoreOption {
	return func(s *PostgresStore) { s.ownerGrace = d }
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

// NewPostgresStore connects, proves the connection and takes the owner
// claim. owner defaults to OwnerID(); the rows carry it behind
// claimedOwnerPrefix (Owner).
func NewPostgresStore(ctx context.Context, dsn, owner string, logger *slog.Logger, options ...StoreOption) (*PostgresStore, error) {
	if owner == "" {
		owner = OwnerID()
	}
	if logger == nil {
		logger = slog.Default()
	}
	store := &PostgresStore{
		dsn: dsn, owner: claimedOwnerPrefix + owner, logger: logger,
		claimCheck: claimCheckInterval, reconcileEvery: reconcileInterval,
		freeSince: map[string]time.Time{},
		stop:      make(chan struct{}), done: make(chan struct{}),
	}
	for _, option := range options {
		option(store)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	if store.maxConns > 0 {
		config.MaxConns = int32(min(store.maxConns, maxDatabaseConns))
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	store.pool = pool
	if store.ownerGrace <= 0 {
		store.ownerGrace = ownerGraceChecks * store.claimCheck
	}
	if err := pool.QueryRow(ctx, `SELECT hashtextextended($1, $2)`, store.owner, ownerLockSeed).Scan(&store.lockKey); err != nil {
		pool.Close()
		return nil, fmt.Errorf("invocation store: %w", err)
	}
	held, err := store.takeClaim(ctx)
	if err == nil && !held {
		err = errors.New("another live process holds this owner id")
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("invocation store: claim the owner %q: %w", owner, err)
	}
	go store.supervise()
	return store, nil
}

// takeClaim takes this owner's liveness lock on a new connection of its own,
// outside the pool, and keeps it. A session lock dies with its session, so
// when this process dies (or its database connection does) the lock is free
// and a sibling reconciles its rows. The caller holds claimMu or owns the
// store exclusively.
func (s *PostgresStore) takeClaim(ctx context.Context) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return false, err
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, s.lockKey).Scan(&held); err != nil || !held {
		_ = conn.Close(ctx)
		return false, err
	}
	s.claim = conn
	s.held.Store(true)
	return true, nil
}

// ClaimHeld reports whether this process holds its owner claim, as of the
// last check. Readiness reads it.
func (s *PostgresStore) ClaimHeld() bool { return s.held.Load() }

// supervise checks the claim every claimCheck and reconciles every
// reconcileEvery, until Close.
func (s *PostgresStore) supervise() {
	defer close(s.done)
	check := time.NewTicker(s.claimCheck)
	defer check.Stop()
	reconcile := time.NewTicker(s.reconcileEvery)
	defer reconcile.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-check.C:
			s.checkClaim()
		case <-reconcile.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.reconcileEvery)
			if _, err := s.reconcile(ctx, false); err != nil {
				s.logger.Warn("could not reconcile orphaned invocations", "error", err)
			}
			cancel()
		}
	}
}

// checkClaim proves the claim's session still holds the lock and, when it
// does not, re-takes it on a new connection. Until then ClaimHeld is false.
func (s *PostgresStore) checkClaim() {
	ctx, cancel := context.WithTimeout(context.Background(), s.claimCheck)
	defer cancel()
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	if s.claim != nil {
		var held bool
		err := s.claim.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
			    AND granted AND objsubid = 1 AND classid = $1::bigint::oid AND objid = $2::bigint::oid)`,
			int64(uint64(s.lockKey)>>32), int64(uint32(s.lockKey))).Scan(&held)
		if err == nil && held {
			return
		}
		if s.held.Swap(false) {
			s.logger.Error("the invocation store lost its owner claim; new invocations are refused until it is re-taken",
				"owner", s.owner, "error", err)
		}
		_ = s.claim.Close(ctx)
		s.claim = nil
	}
	held, err := s.takeClaim(ctx)
	switch {
	case err != nil:
		s.logger.Warn("could not re-take the owner claim", "owner", s.owner, "error", err)
	case !held:
		s.logger.Error("another session holds this process's owner claim; new invocations stay refused", "owner", s.owner)
	default:
		s.logger.Warn("the invocation store re-took its owner claim", "owner", s.owner)
	}
}

// DSN is the connection string the store was built with.
func (s *PostgresStore) DSN() string { return s.dsn }

// Close stops the supervisor, ends the claim's session (and so frees its
// lock) and closes the pool.
func (s *PostgresStore) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.claimMu.Lock()
		if s.claim != nil {
			_ = s.claim.Close(context.Background())
			s.claim = nil
		}
		s.held.Store(false)
		s.claimMu.Unlock()
		s.pool.Close()
	})
}

// Owner is the id this process writes on the rows it accepts.
func (s *PostgresStore) Owner() string { return s.owner }

// Durable reports true: a row survives this process.
func (s *PostgresStore) Durable() bool { return true }

// Create inserts the accepted invocation under this owner, and refuses it
// with ErrClaimLost while the owner claim is not held.
func (s *PostgresStore) Create(ctx context.Context, invocation *Invocation) error {
	if !s.held.Load() {
		return ErrClaimLost
	}
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
// only thing that persists a change. It writes only a row this owner still
// has in flight: a row a sibling terminated as an orphan (while this
// process's claim was lost) keeps that answer.
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
		  WHERE invocation_id = $5 AND owner = $6 AND finished_at IS NULL`,
		invocation.Status, invocation.StopRequest, result, invocation.Terminal(), invocation.ID, s.owner)
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

// Reconcile is the startup pass: it terminates in-flight rows left behind
// by an owner that is gone. The store runs the same pass every
// reconcileEvery itself (reconcile), except for legacy rows (below). A row
// still pending/running whose owner is gone is work no live process is doing;
// leaving it would make a poll return InProgress forever. It becomes a
// terminal error whose message says what happened, which is an answer the
// caller can act on.
//
// Whether an owner is gone depends on what wrote the row:
//   - claimedOwnerPrefix: gone when its liveness lock is free on two
//     sightings at least ownerGrace apart, with no held sighting between. A
//     row whose owner holds it is a sibling replica's running invocation and
//     is left alone — before #1359 every other owner counted as gone, and a
//     second replica starting killed the first one's work — and so is the
//     row of an owner whose session dropped a moment ago and is re-taking
//     the lock (checkClaim).
//   - no prefix (an image from before #1359, which takes no lock and leaves
//     no other sign of life): gone at the startup pass, never at the
//     periodic one. That is the rule the old image itself applied — its only
//     reconcile ran when a process started, and it terminated every row
//     another owner had in flight — so an old-image owner is treated
//     exactly as it was before the upgrade, with no time guess. A rolling
//     deploy therefore ends what an old replica is running when a new one
//     starts, as before; a row an old replica accepted after the last new
//     replica started is ended at the next process start.
//   - no owner at all: gone.
func (s *PostgresStore) Reconcile(ctx context.Context) (int, error) {
	return s.reconcile(ctx, true)
}

// reconcile is one pass; startup says whether legacy rows are terminated.
func (s *PostgresStore) reconcile(ctx context.Context, startup bool) (int, error) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
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
	now := time.Now()
	inFlight := make(map[string]bool, len(owners))
	reconciled := 0
	for _, owner := range owners {
		if owner != nil {
			inFlight[*owner] = true
		}
	}
	defer func() {
		for owner := range s.freeSince {
			if !inFlight[owner] {
				delete(s.freeSince, owner)
			}
		}
	}()
	for _, owner := range owners {
		switch {
		case owner == nil:
		case !strings.HasPrefix(*owner, claimedOwnerPrefix):
			if !startup {
				continue
			}
		default:
			// The lock is free only when its owner's session is gone. Taken
			// for the instant of the check and given back on the same
			// session, so a dead owner's id is never held by this one.
			var free bool
			if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, $2))`, *owner, ownerLockSeed).Scan(&free); err != nil {
				return reconciled, err
			}
			if !free {
				delete(s.freeSince, *owner)
				continue
			}
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, $2))`, *owner, ownerLockSeed); err != nil {
				return reconciled, err
			}
			since, seen := s.freeSince[*owner]
			if !seen {
				s.freeSince[*owner] = now
				continue
			}
			if now.Sub(since) < s.ownerGrace {
				continue
			}
			delete(s.freeSince, *owner)
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
