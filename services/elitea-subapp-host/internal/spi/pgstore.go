package spi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
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
// The Inventory host keeps the same tables in the Inventory engine's schema
// (services/elitea-inventory-engine/migrations/0009_host_invocations.sql,
// WithSchema("inventory_graph")). Both applications' migration 0009 adds
// what makes the host a fleet (#scaling): the labels a deletion on any
// replica matches (RunningInvocations), the slot flag that fleet admission
// counts (Create), and host_replicas, one row for each live replica with
// its slots (FleetSlots, SetAdmitting).
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

	// schema qualifies every table name (WithSchema); empty leaves the names
	// unqualified, as DeepWiki's tables are. invocations, events and
	// replicas are the table names as the statements spell them.
	schema      string
	invocations string
	events      string
	replicas    string

	// slots is this replica's slot count in host_replicas (WithReplicaSlots);
	// admitting is whether it takes new work (SetAdmitting). fleetKey is the
	// transaction lock that serialises fleet admission (Create).
	slots     int
	admitting atomic.Bool
	fleetKey  int64

	// stopWatch is the callback that pollStops hands a stop request to
	// (WatchStops); nil until a server registers one.
	stopWatch atomic.Pointer[func(invocationID string)]
}

// ErrClaimLost refuses a new invocation while this process does not hold its
// owner liveness lock: siblings may be terminating its rows as orphans, so
// it must not accept work under that owner.
var ErrClaimLost = errors.New("invocation store: this process does not hold its owner claim")

// ErrNoSlot refuses a slot-holding invocation (Invocation.HoldsSlot) while
// every slot of the fleet is taken. The store inserts nothing then. The
// error that Create returns is a *NoSlotError, which says how many slots the
// fleet has.
var ErrNoSlot = errors.New("invocation store: every slot of the fleet is busy")

// NoSlotError is ErrNoSlot with the fleet capacity that was full.
type NoSlotError struct {
	// Total is the fleet capacity: the sum of the slots of the live
	// replicas that admit work.
	Total int
}

func (e *NoSlotError) Error() string {
	return fmt.Sprintf("invocation store: all %d slots of the fleet are busy", e.Total)
}

// Is makes errors.Is(err, ErrNoSlot) true.
func (e *NoSlotError) Is(target error) bool { return target == ErrNoSlot }

// fleetLockSeed seeds hashtextextended('fleet-slots:' || schema, seed), the
// key of the transaction lock that serialises fleet admission. A seed of its
// own, so the key is not an owner's liveness lock key.
const fleetLockSeed = 0x53_41_46_31 // "SAF1"

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
	// replicaLiveChecks is how many claim-check intervals a replica row
	// stays live after its last upsert. A replica upserts at every check,
	// so a live replica is never older than one interval.
	replicaLiveChecks = 3
	// replicaStaleChecks is the age, in claim-check intervals, at which
	// reconcile deletes a replica row (a process that died without Close).
	replicaStaleChecks = 30
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

// WithSchema schema-qualifies every table the store reads and writes. The
// Inventory host passes "inventory_graph", where the Inventory engine's
// migration 0009 creates the tables; DeepWiki's host passes "" and its
// tables stay unqualified, as before. NewPostgresStore refuses a name that
// is not a plain lower-case identifier.
func WithSchema(name string) StoreOption {
	return func(s *PostgresStore) { s.schema = name }
}

// WithReplicaSlots sets the slot count this replica registers in
// host_replicas: <PREFIX>MAX_PARALLEL_WORKERS. Zero or less registers one.
func WithReplicaSlots(n int) StoreOption {
	return func(s *PostgresStore) { s.slots = n }
}

// schemaName is the identifier WithSchema accepts.
var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// qualify returns the table name as a statement spells it.
func (s *PostgresStore) qualify(table string) string {
	if s.schema == "" {
		return table
	}
	return pgx.Identifier{s.schema, table}.Sanitize()
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
	if store.schema != "" && !schemaName.MatchString(store.schema) {
		return nil, fmt.Errorf("%w: invocation store schema %q is not a plain lower-case identifier", ErrConfig, store.schema)
	}
	store.invocations = store.qualify("invocations")
	store.events = store.qualify("invocation_events")
	store.replicas = store.qualify("host_replicas")
	if store.slots <= 0 {
		store.slots = 1
	}
	store.admitting.Store(true)
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
	if err := pool.QueryRow(ctx, `SELECT hashtextextended($1, $2)`, "fleet-slots:"+store.schema, fleetLockSeed).Scan(&store.fleetKey); err != nil {
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
	// The replica row is what fleet admission and the fleet slots report
	// count. A database without the table (the engine has not applied its
	// migration yet) is a boot failure, as a missing invocations table is.
	if err := store.upsertReplica(ctx); err != nil {
		_ = store.claim.Close(ctx)
		store.claim = nil
		pool.Close()
		return nil, fmt.Errorf("invocation store: register the replica: %w", err)
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
			s.pollStops()
		case <-reconcile.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.reconcileEvery)
			if _, err := s.reconcile(ctx, false); err != nil {
				s.logger.Warn("could not reconcile orphaned invocations", "error", err)
			}
			cancel()
		}
	}
}

// WatchStops registers stop, which the store calls with the id of each run
// of THIS owner whose row records a stop request (StopWatcher). The store
// reads the rows once each claim check, and no more often. That is how a
// stop that a sibling recorded reaches the run when the cancel bus is
// absent or dropped the message. stop must be idempotent: the store calls
// it again at each check until the run ends.
func (s *PostgresStore) WatchStops(stop func(invocationID string)) {
	s.stopWatch.Store(&stop)
}

// pollStops reads this owner's unfinished rows that record a stop request
// (one query, on the owner index of in-flight rows) and hands each id to
// the WatchStops callback. It does nothing while the claim is lost: a
// sibling may be ending those rows.
func (s *PostgresStore) pollStops() {
	watch := s.stopWatch.Load()
	if watch == nil || !s.held.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.claimCheck)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT invocation_id FROM `+s.invocations+` WHERE owner = $1 AND finished_at IS NULL AND stop_requested`, s.owner)
	if err != nil {
		s.logger.Warn("could not read the stop requests of this replica's runs", "owner", s.owner, "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.logger.Warn("could not read the stop requests of this replica's runs", "owner", s.owner, "error", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("could not read the stop requests of this replica's runs", "owner", s.owner, "error", err)
		return
	}
	for _, id := range ids {
		(*watch)(id)
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
			s.refreshReplica(ctx)
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
		s.refreshReplica(ctx)
	}
}

// upsertReplica writes this replica's row in host_replicas: its slots,
// whether it admits work, and now as the time it was last seen.
func (s *PostgresStore) upsertReplica(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO `+s.replicas+` (owner, slots, admitting, seen_at) VALUES ($1, $2, $3, now())
		 ON CONFLICT (owner) DO UPDATE SET slots = EXCLUDED.slots, admitting = EXCLUDED.admitting, seen_at = now()`,
		s.owner, s.slots, s.admitting.Load())
	return err
}

// refreshReplica is upsertReplica at a claim check. A failure is logged, and
// the row goes stale (the fleet stops counting its slots) until a later
// check writes it.
func (s *PostgresStore) refreshReplica(ctx context.Context) {
	if err := s.upsertReplica(ctx); err != nil {
		s.logger.Warn("could not refresh this replica's row in the fleet registry", "owner", s.owner, "error", err)
	}
}

// SetAdmitting records at once whether this replica takes new work. The
// host calls SetAdmitting(false) when it starts to drain (SIGTERM). From
// then on the fleet capacity does not count this replica's slots, so no new
// generation is admitted on the slots of a replica that is going away.
func (s *PostgresStore) SetAdmitting(ctx context.Context, admitting bool) error {
	s.admitting.Store(admitting)
	return s.upsertReplica(ctx)
}

// liveSeconds is the age, in seconds, below which a replica row is live.
func (s *PostgresStore) liveSeconds() float64 {
	return (replicaLiveChecks * s.claimCheck).Seconds()
}

// capacityQuery reads the fleet capacity and the live replica count.
func (s *PostgresStore) capacityQuery() string {
	return `SELECT COALESCE(sum(slots), 0)::int, count(*)::int FROM ` + s.replicas +
		` WHERE admitting AND seen_at > now() - make_interval(secs => $1)`
}

// activeQuery counts the unfinished slot-holding invocations.
func (s *PostgresStore) activeQuery() string {
	return `SELECT count(*)::int FROM ` + s.invocations + ` WHERE holds_slot AND finished_at IS NULL`
}

// FleetSlots reads the fleet's slot report (FleetSlotter).
func (s *PostgresStore) FleetSlots(ctx context.Context) (FleetCapacity, error) {
	var capacity FleetCapacity
	if err := s.pool.QueryRow(ctx, s.capacityQuery(), s.liveSeconds()).Scan(&capacity.Total, &capacity.Replicas); err != nil {
		return FleetCapacity{}, err
	}
	if err := s.pool.QueryRow(ctx, s.activeQuery()).Scan(&capacity.Active); err != nil {
		return FleetCapacity{}, err
	}
	return capacity, nil
}

// RunningInvocations lists every unfinished invocation of the fleet with its
// labels (FleetLister): what a deletion matches on any replica.
func (s *PostgresStore) RunningInvocations(ctx context.Context) ([]RunningInvocation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT invocation_id, toolkit_name, tool_name, labels FROM `+s.invocations+` WHERE finished_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var running []RunningInvocation
	for rows.Next() {
		var (
			info   RunningInvocation
			labels []byte
		)
		if err := rows.Scan(&info.ID, &info.Toolkit, &info.Tool, &labels); err != nil {
			return nil, err
		}
		info.Labels = map[string]string{}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &info.Labels); err != nil {
				return nil, fmt.Errorf("invocation %s: stored labels are not a string map: %w", info.ID, err)
			}
		}
		running = append(running, info)
	}
	return running, rows.Err()
}

// DSN is the connection string the store was built with.
func (s *PostgresStore) DSN() string { return s.dsn }

// Close stops the supervisor, ends the claim's session (and so frees its
// lock) and closes the pool.
func (s *PostgresStore) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		// The fleet stops counting this replica's slots at once, not when
		// its row goes stale.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := s.pool.Exec(ctx, `DELETE FROM `+s.replicas+` WHERE owner = $1`, s.owner); err != nil {
			s.logger.Warn("could not remove this replica from the fleet registry", "owner", s.owner, "error", err)
		}
		cancel()
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

// Create inserts the accepted invocation under this owner, with its labels
// and its slot flag. It refuses the invocation with ErrClaimLost while the
// owner claim is not held. An invocation that is terminal already (a
// refusal the manager records at once) is inserted with its result and
// finished_at.
//
// A slot-holding invocation (HoldsSlot) is admitted against the FLEET, in
// one transaction under the fleet lock. The capacity is the sum of the
// slots of the live replicas that admit work; the load is the unfinished
// slot-holding rows. When the load is at the capacity, Create inserts
// nothing and returns a *NoSlotError (errors.Is ErrNoSlot). The lock makes
// two replicas that admit at the same moment see each other's row.
func (s *PostgresStore) Create(ctx context.Context, invocation *Invocation) error {
	if !s.held.Load() {
		return ErrClaimLost
	}
	labels := []byte(`{}`)
	if len(invocation.Labels) > 0 {
		encoded, err := json.Marshal(invocation.Labels)
		if err != nil {
			return err
		}
		labels = encoded
	}
	var result []byte
	if invocation.Result != nil {
		encoded, err := json.Marshal(invocation.Result)
		if err != nil {
			return err
		}
		result = encoded
	}
	insert := `INSERT INTO ` + s.invocations + ` (invocation_id, toolkit_name, tool_name, status, owner, labels, holds_slot, result, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $9::boolean THEN now() END)`
	arguments := []any{invocation.ID, invocation.Toolkit, invocation.Tool, invocation.Status, s.owner,
		string(labels), invocation.HoldsSlot, result, invocation.Terminal()}
	if !invocation.HoldsSlot {
		_, err := s.pool.Exec(ctx, insert, arguments...)
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.fleetKey); err != nil {
		return err
	}
	var capacity, replicas, active int
	if err := tx.QueryRow(ctx, s.capacityQuery(), s.liveSeconds()).Scan(&capacity, &replicas); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, s.activeQuery()).Scan(&active); err != nil {
		return err
	}
	if active >= capacity {
		return &NoSlotError{Total: capacity}
	}
	if _, err := tx.Exec(ctx, insert, arguments...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Get reads one row; nil when there is none.
func (s *PostgresStore) Get(ctx context.Context, toolkit, tool, id string) (*Invocation, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT invocation_id, toolkit_name, tool_name, status, stop_requested, result, created_at, finished_at
		   FROM `+s.invocations+` WHERE toolkit_name = $1 AND tool_name = $2 AND invocation_id = $3`,
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
		`UPDATE `+s.invocations+` SET status = $1, stop_requested = stop_requested OR $2, result = $3,
		        finished_at = CASE WHEN $4 THEN now() ELSE finished_at END
		  WHERE invocation_id = $5 AND owner = $6 AND finished_at IS NULL`,
		invocation.Status, invocation.StopRequest, result, invocation.Terminal(), invocation.ID, s.owner)
	return err
}

// RequestStop records a stop request on the row, and nothing else: the
// replica that owns the run may be writing its terminal body at the same
// moment (Manager.Cancel). A row that ended already is left as it is: a
// stop that arrives late (a second cancel, a bus message after the poll)
// changes nothing on a terminal row.
func (s *PostgresStore) RequestStop(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE `+s.invocations+` SET stop_requested = true WHERE invocation_id = $1 AND finished_at IS NULL`, id)
	return err
}

// AppendEvent stores one thinking event.
func (s *PostgresStore) AppendEvent(ctx context.Context, id, message string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO `+s.events+` (invocation_id, message) VALUES ($1, $2)`, id, message)
	return err
}

// DrainEvents returns the accumulated events in order and clears them, in
// ONE statement, so the read and the clear cannot interleave with another
// poller's.
func (s *PostgresStore) DrainEvents(ctx context.Context, invocation *Invocation) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `DELETE FROM `+s.events+` WHERE invocation_id = $1 RETURNING message, id`, invocation.ID)
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
		`DELETE FROM `+s.invocations+` WHERE finished_at IS NOT NULL AND finished_at < now() - make_interval(secs => $1)`,
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
	rows, err := conn.Query(ctx, `SELECT DISTINCT owner FROM `+s.invocations+` WHERE finished_at IS NULL AND owner IS DISTINCT FROM $1`, s.owner)
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
	// A replica that died without Close leaves its row. The fleet stopped
	// counting it when it went stale; it is deleted much later, so a replica
	// that was cut off for some time finds its row again.
	if _, err := conn.Exec(ctx, `DELETE FROM `+s.replicas+` WHERE seen_at < now() - make_interval(secs => $1)`,
		(replicaStaleChecks * s.claimCheck).Seconds()); err != nil {
		return reconciled, err
	}
	return reconciled, nil
}

// terminateOrphans ends every in-flight row of owner (nil: rows with no
// owner at all) with the restart error.
func (s *PostgresStore) terminateOrphans(ctx context.Context, owner *string) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT invocation_id, tool_name FROM `+s.invocations+` WHERE finished_at IS NULL AND owner IS NOT DISTINCT FROM $1`, owner)
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
			`UPDATE `+s.invocations+` SET status = $1, result = $2, finished_at = now(), owner = $3 WHERE invocation_id = $4 AND finished_at IS NULL`,
			statusStopped, encoded, s.owner, o.id); err != nil {
			return i, err
		}
	}
	return len(orphans), nil
}

// Count is the number of rows, for tests and health.
func (s *PostgresStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+s.invocations).Scan(&n)
	return n, err
}
