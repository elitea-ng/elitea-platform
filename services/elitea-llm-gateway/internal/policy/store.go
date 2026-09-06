package policy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultRefreshInterval is how often the gateway re-reads
// gateway.governance_config when no warm-reload signal arrives.
//
// The design offers two paths — "at load, or when a warm-reload event is
// issued" — and this gateway implements ONE of them, on purpose. The poll is
// the path that converges unconditionally: a replica that missed an event
// because it was starting, because NATS was down, or because it is not on the
// cluster the event went to, still picks the change up. An event path on top
// would shorten the window; it would not change the guarantee, and a second
// path that is usually redundant is a second path that can be silently broken.
//
// The operational consequence, stated plainly: a definition saved in the admin
// UI takes effect within this interval, not instantly. GET /governance/status
// reports the snapshot's loaded_at so the wait is observable rather than
// guessed at.
const DefaultRefreshInterval = 30 * time.Second

// loadTimeout bounds one refresh read. It is generous compared with the /llm
// hot path because it runs on a background goroutine, and short enough that a
// wedged pool cannot hold a refresh open across several intervals.
const loadTimeout = 5 * time.Second

// The initial-load retry schedule.
//
// On a FRESH install the gateway can win the race against the migration job:
// `gateway.governance_config` does not exist yet, the first load fails with an
// undefined-relation error, and the store then waits a whole
// DefaultRefreshInterval before it tries again. For up to 30 seconds a gateway
// that already serves requests enforces NO authored definition — no model
// allowlist, no MCP allowlist, no rate policy, and no egress allowlist beyond
// the environment floor.
//
// Compose can order the two services, and now does (see
// deploy/docker-compose.standalone-full.yml). Kubernetes cannot be relied on to:
// a Deployment has no ordering against a Helm hook Job that is still running,
// and a CrashLoopBackOff on the migration extends the window arbitrarily.
//
// So the store retries the FIRST load quickly before it falls back to the poll.
// Five delays of 500ms, 1s, 2s, 3s and 3s bound the extra boot wait at 9.5
// seconds. That is short enough to keep a readiness probe happy and long enough
// to cover a migration job that started at the same moment.
//
// This changes WHEN the gateway converges, never WHAT it enforces. A store that
// exhausts every attempt still starts on the Empty snapshot and still logs the
// error, and Load's fail-closed contract is untouched: a failed read never
// replaces a good snapshot.
const (
	initialLoadAttempts = 6
	initialLoadDelay    = 500 * time.Millisecond
	initialLoadMaxDelay = 3 * time.Second
)

// ErrNoDatabase is returned by Load when the store was built without a pool.
var ErrNoDatabase = errors.New("policy: no database handle")

// querier is the minimal pgx surface the store needs, mirroring the seams in
// internal/cost and internal/account so the store is unit-testable without a
// live Postgres.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PoolQuerier adapts a *pgxpool.Pool to querier.
type PoolQuerier struct{ Pool *pgxpool.Pool }

// NewPoolQuerier wraps a pgxpool.Pool for use with NewStore.
func NewPoolQuerier(pool *pgxpool.Pool) *PoolQuerier { return &PoolQuerier{Pool: pool} }

// Query runs a multi-row query on the pool.
func (p *PoolQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.Pool.Query(ctx, sql, args...)
}

// selectEnabled reads every enabled definition. The gateway filters by scope in
// process rather than in SQL: the corpus is operator-authored and small (tens
// of rows), and one cached read serves every request, so a per-request
// project-filtered query would trade a trivial memory cost for a database
// round-trip on the hot path.
const selectEnabled = `SELECT id::text, type, section, name, data, enabled
	FROM gateway.governance_config
	WHERE enabled = true
	ORDER BY section, type, name`

// Store holds the current compiled Snapshot and keeps it fresh.
//
// Current() is the hot-path read and is a single atomic load — the /llm path
// never touches Postgres for a definition, and never blocks behind a refresh.
type Store struct {
	db  querier
	log *slog.Logger

	snap atomic.Pointer[Snapshot]

	// interval is the poll period; 0 selects DefaultRefreshInterval.
	interval time.Duration

	// retryDelay is the FIRST delay of the initial-load retry schedule.
	retryDelay time.Duration

	// lastErr is the most recent refresh failure, served on the status surface.
	// A store that cannot read its definitions must be able to SAY so: silently
	// serving a stale snapshot is how a revoked allowlist stays in force.
	mu       sync.RWMutex
	lastErr  error
	lastTry  time.Time
	lastGood time.Time

	// now is injected for tests.
	now func() time.Time
}

// Config configures a Store.
type Config struct {
	// DB is the Postgres handle. nil builds a store that serves Empty forever
	// and reports that it has no database — the posture of a gateway booted
	// without DATABASE_URL.
	DB querier
	// Logger receives load results and rejection reports. nil uses the default.
	Logger *slog.Logger
	// RefreshInterval overrides DefaultRefreshInterval.
	RefreshInterval time.Duration
	// Now is injected for tests; nil uses time.Now.
	Now func() time.Time
	// InitialRetryDelay is the FIRST delay of the initial-load retry schedule;
	// each later delay doubles, capped at initialLoadMaxDelay. 0 selects
	// initialLoadDelay. Tests set it small so the schedule runs in
	// microseconds. Production has no reason to change it.
	InitialRetryDelay time.Duration
}

// NewStore builds a Store holding the Empty snapshot. Call Start to begin
// refreshing, or Load for a one-shot read.
func NewStore(cfg Config) *Store {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	s := &Store{
		db:         cfg.DB,
		log:        log,
		interval:   cfg.RefreshInterval,
		retryDelay: cfg.InitialRetryDelay,
		now:        nowFn,
	}
	if s.interval <= 0 {
		s.interval = DefaultRefreshInterval
	}
	if s.retryDelay <= 0 {
		s.retryDelay = initialLoadDelay
	}
	s.snap.Store(Empty)
	return s
}

// Current returns the live snapshot. It never returns nil: before the first
// successful load it is Empty, whose every accessor is enforcement-neutral.
func (s *Store) Current() *Snapshot {
	if s == nil {
		return Empty
	}
	if snap := s.snap.Load(); snap != nil {
		return snap
	}
	return Empty
}

// EgressAllowlist returns the authored egress entries of the CURRENT snapshot.
//
// It exists so the store itself satisfies account.EgressSource: the account
// holds one long-lived reference and always reads the live snapshot through it,
// rather than being handed a snapshot that goes stale on the next refresh. A
// nil store reports nothing, which leaves the account on its environment floor.
func (s *Store) EgressAllowlist() []string {
	if s == nil {
		return nil
	}
	return s.Current().EgressAllowlist()
}

// Load performs one read-and-compile, publishing the result on success.
//
// A FAILED read leaves the previous snapshot in place. That is deliberate and
// it is the safer of the two options: dropping to Empty on a transient database
// blip would silently lift every allowlist and every rate limit at exactly the
// moment the platform is least healthy. The failure is recorded and surfaced
// instead, so the staleness is visible rather than invented.
func (s *Store) Load(ctx context.Context) error {
	if s.db == nil {
		s.recordAttempt(ErrNoDatabase)
		return ErrNoDatabase
	}
	qctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	rows, err := s.db.Query(qctx, selectEnabled)
	if err != nil {
		s.recordAttempt(err)
		return err
	}
	defer rows.Close()

	var (
		out      []Row
		scanErrs int
	)
	for rows.Next() {
		var (
			row       Row
			dataBytes []byte
		)
		if scanErr := rows.Scan(&row.ID, &row.Type, &row.Section, &row.Name, &dataBytes, &row.Enabled); scanErr != nil {
			scanErrs++
			continue
		}
		if len(dataBytes) > 0 {
			if jsonErr := json.Unmarshal(dataBytes, &row.Data); jsonErr != nil {
				// Keep the row: Compile rejects it by name, which is far more
				// useful to an operator than a row that vanished during scan.
				row.Data = nil
			}
		}
		out = append(out, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		s.recordAttempt(rowsErr)
		return rowsErr
	}

	snap := Compile(out, s.now())
	if scanErrs > 0 {
		s.log.Error("policy: governance rows failed to scan and are NOT enforced",
			"count", scanErrs)
	}
	s.snap.Store(snap)
	s.recordSuccess()
	s.logSnapshot(snap)
	return nil
}

// logSnapshot reports what loaded and, individually, everything that did not.
// Each rejection and each inert row gets its own line with its own reason: a
// count alone tells an operator that something is wrong without telling them
// which rule of theirs it is.
func (s *Store) logSnapshot(snap *Snapshot) {
	d := snap.Diagnostics()
	s.log.Info("policy: governance definitions loaded",
		"rows", d.Rows,
		"budgets", d.Budgets,
		"rate_limits", d.RateLimits,
		"model_configs", d.ModelConfigs,
		"mcp_allowlists", d.MCPAllowlists,
		"credential_policies", d.CredentialPolicy,
		"routing_rules", d.RoutingRules,
		"egress_allowlists", d.EgressAllowlists,
		"rejected", len(d.Rejected),
		"inert", len(d.Inert),
	)
	for _, r := range d.Rejected {
		s.log.Error("policy: governance row REJECTED and not enforced",
			"id", r.ID, "type", r.Type, "name", r.Name, "reason", r.Reason)
	}
	for _, r := range d.Inert {
		s.log.Warn("policy: governance row loaded but can enforce nothing",
			"id", r.ID, "type", r.Type, "name", r.Name, "reason", r.Reason)
	}
}

// Start runs the refresh loop until ctx is done. It performs the first load
// synchronously — with a short retry schedule — so the gateway is enforcing
// authored definitions before it serves its first request, then refreshes on
// the poll interval.
//
// The retries answer one measured failure: on a fresh install the gateway boots
// before the migration creates gateway.governance_config, the first read fails,
// and the store enforced nothing for a whole refresh interval. See the
// initialLoad* constants.
func (s *Store) Start(ctx context.Context) {
	s.loadInitial(ctx)
	go s.loop(ctx)
}

// loadInitial reads the definitions, and retries a bounded number of times on a
// short backoff before it leaves convergence to the poll loop.
//
// ErrNoDatabase is not retried. It is a composition fact, not a transient
// failure: the store was built without a pool, and no amount of waiting gives it
// one. Retrying it would add ten seconds to the boot of every gateway that runs
// without DATABASE_URL and change nothing.
func (s *Store) loadInitial(ctx context.Context) {
	delay := s.retryDelay
	for attempt := 1; ; attempt++ {
		err := s.Load(ctx)
		if err == nil {
			if attempt > 1 {
				// Say that the race happened. Without this line the recovery is
				// invisible, and the operator sees only the failures above it.
				s.log.Info("policy: governance definitions loaded after a retry; "+
					"the first read raced the database migration",
					"attempts", attempt)
			}
			return
		}
		if errors.Is(err, ErrNoDatabase) {
			return
		}
		if attempt >= initialLoadAttempts || ctx.Err() != nil {
			s.log.Error("policy: initial governance load failed; the gateway is enforcing NO authored definitions "+
				"until a refresh succeeds", "err", err, "attempts", attempt,
				"next_attempt_within", s.interval.String())
			return
		}
		s.log.Warn("policy: initial governance load failed; retrying shortly",
			"err", err, "attempt", attempt, "retry_in", delay.String())
		if !sleepContext(ctx, delay) {
			s.log.Error("policy: initial governance load abandoned during shutdown; the gateway is "+
				"enforcing NO authored definitions", "err", err, "attempts", attempt)
			return
		}
		if delay *= 2; delay > initialLoadMaxDelay {
			delay = initialLoadMaxDelay
		}
	}
}

// sleepContext waits for d and reports whether the wait completed. It answers
// false as soon as ctx is done, so a shutdown during boot is not held up by the
// retry schedule.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Store) loop(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Load(ctx); err != nil && !errors.Is(err, ErrNoDatabase) {
				s.log.Warn("policy: governance refresh failed; continuing with the previous definitions",
					"err", err, "loaded_at", s.Current().LoadedAt)
			}
		}
	}
}

func (s *Store) recordAttempt(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.lastTry = s.now()
	s.mu.Unlock()
}

func (s *Store) recordSuccess() {
	s.mu.Lock()
	s.lastErr = nil
	s.lastTry = s.now()
	s.lastGood = s.lastTry
	s.mu.Unlock()
}

// Status is the store's operator-facing health, served alongside Diagnostics.
type Status struct {
	// HasDatabase is false when the gateway booted without a pool. Every other
	// field is then meaningless and the snapshot is Empty.
	HasDatabase bool `json:"has_database"`
	// LastAttempt and LastSuccess are RFC3339 timestamps, empty when never.
	LastAttempt string `json:"last_attempt,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	// Error is the most recent refresh failure, empty when the last read
	// succeeded. A non-empty Error with a populated LastSuccess means the
	// snapshot being enforced is STALE, which is the state this field exists
	// to make visible.
	Error string `json:"error,omitempty"`
	// RefreshInterval is the poll period, as a duration string.
	RefreshInterval string `json:"refresh_interval"`
}

// Status reports the store's refresh health.
func (s *Store) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{HasDatabase: s.db != nil, RefreshInterval: s.interval.String()}
	if !s.lastTry.IsZero() {
		st.LastAttempt = s.lastTry.UTC().Format(time.RFC3339)
	}
	if !s.lastGood.IsZero() {
		st.LastSuccess = s.lastGood.UTC().Format(time.RFC3339)
	}
	if s.lastErr != nil {
		st.Error = s.lastErr.Error()
	}
	return st
}
