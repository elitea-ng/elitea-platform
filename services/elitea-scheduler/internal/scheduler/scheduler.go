package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/rpc"
)

// Schedule mirrors the centry.schedule table row.
type Schedule struct {
	ID        int
	Name      string
	ProjectID *int
	Cron      string
	Active    bool
	RPCFunc   string
	RPCKwargs map[string]any
	LastRun   *time.Time
}

// scheduleStore is the slice of *pgxpool.Pool this package uses. It exists so a
// test can hand the dispatch path a store that records what it was asked to
// write — the assertion that matters here is about the row, not about the call.
type scheduleStore interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// dispatcher is the slice of *rpc.Client this package uses. The int64 is the
// number of subscribers Redis delivered the payload to — see rpc.Client.Call.
type dispatcher interface {
	Call(ctx context.Context, funcName string, kwargs map[string]any) (int64, error)
}

// Scheduler is the core scheduling daemon that reads centry.schedule
// and dispatches RPC calls to pylon services.
type Scheduler struct {
	pool      scheduleStore
	rdb       *redis.Client
	rpcClient dispatcher
	cfg       config.Config
	parser    cron.Parser
	stop      chan struct{}
}

// New creates a Scheduler.
func New(pool *pgxpool.Pool, rdb *redis.Client, rpcClient *rpc.Client, cfg config.Config) *Scheduler {
	return newWithDeps(pool, rdb, rpcClient, cfg)
}

// newWithDeps is New over the interfaces, so the dispatch path can be exercised
// with a real database and a dispatcher whose subscriber count is chosen by the
// test. Production always arrives through New.
func newWithDeps(pool scheduleStore, rdb *redis.Client, rpcClient dispatcher, cfg config.Config) *Scheduler {
	return &Scheduler{
		pool:      pool,
		rdb:       rdb,
		rpcClient: rpcClient,
		cfg:       cfg,
		parser:    cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		stop:      make(chan struct{}),
	}
}

// Run starts the scheduler loop. Blocks until ctx is cancelled or Stop is called.
func (s *Scheduler) Run(ctx context.Context) {
	// Align first tick to the start of the next minute
	now := time.Now()
	nextMinute := now.Truncate(time.Minute).Add(time.Minute)
	delay := time.Until(nextMinute)

	slog.Info("scheduler: waiting for first tick", "delay", delay.Round(time.Second))
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// Stop signals the scheduler to shut down.
func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	lock := newLock(s.rdb, "scheduler_tick", s.cfg.InstanceID)
	ok, err := lock.TryAcquire(ctx)
	if err != nil {
		slog.Error("scheduler: lock acquire error", "err", err)
		return
	}
	if !ok {
		slog.Debug("scheduler: lock held by another instance, skipping")
		return
	}
	defer func() { _ = lock.Release(ctx) }()

	// Maintenance is checked AFTER the lock, so only the instance that would
	// actually have dispatched pays for the read — the other replicas have
	// already returned above.
	//
	// It suppresses the dispatch pass and nothing else: the loop keeps ticking,
	// so the window is re-checked every minute and work resumes on the first
	// tick after it closes. No schedule is stamped while suppressed, so each
	// one is due exactly once when it lifts. See maintenance.go.
	if s.MaintenanceActive(ctx) {
		slog.Info("scheduler: maintenance mode is active; not dispatching",
			"instance", s.cfg.InstanceID)
		return
	}

	s.runSchedules(ctx)
}

func (s *Scheduler) runSchedules(ctx context.Context) {
	// Use FOR UPDATE SKIP LOCKED to prevent concurrent execution (matches pylon behavior)
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, project_id, cron, rpc_func, rpc_kwargs, last_run
		FROM centry.schedule
		WHERE active = true
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		slog.Error("scheduler: query schedules", "err", err)
		return
	}
	defer rows.Close()

	now := time.Now().UTC()
	var dispatched, skipped, undelivered int

	for rows.Next() {
		var sc Schedule
		var kwargsRaw []byte
		err := rows.Scan(&sc.ID, &sc.Name, &sc.ProjectID, &sc.Cron, &sc.RPCFunc, &kwargsRaw, &sc.LastRun)
		if err != nil {
			slog.Error("scheduler: scan row", "err", err)
			continue
		}

		if kwargsRaw != nil {
			if err := json.Unmarshal(kwargsRaw, &sc.RPCKwargs); err != nil {
				slog.Error("scheduler: unmarshal rpc_kwargs", "id", sc.ID, "err", err)
				continue
			}
		}

		if !s.timeToRun(sc, now) {
			skipped++
			continue
		}

		if s.dispatch(ctx, sc, now) {
			dispatched++
		} else {
			undelivered++
		}
	}

	if dispatched > 0 || undelivered > 0 || slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.Info("scheduler: tick complete",
			"dispatched", dispatched, "skipped", skipped, "undelivered", undelivered)
	}
}

// dispatch publishes one due schedule and stamps last_run ONLY if the dispatch
// reached a consumer. It reports whether the work was accepted.
//
// last_run is the only record that a schedule ran, and the admin listing and
// timeToRun both read it as one. Stamping it after a publish that reached zero
// subscribers writes "this ran" for work that nothing will ever perform, which
// is the defect in issue #305: `elitea_rpc` is consumed by legacy Pylon and by
// nothing in this repository, so a Go-only stack marked every schedule run
// every minute while executing none of them, and the schedule history looked
// healthier the more completely the product was broken.
//
// Leaving last_run alone on an undelivered dispatch is also the recoverable
// choice: timeToRun still returns true next tick, so a consumer that was
// restarting picks the job up a minute later instead of having its window
// silently consumed. The cost is a repeated publish while no consumer exists,
// which is why the log is at ERROR — an operator has to see it.
func (s *Scheduler) dispatch(ctx context.Context, sc Schedule, now time.Time) bool {
	slog.Info("scheduler: dispatching", "id", sc.ID, "name", sc.Name, "rpc_func", sc.RPCFunc)

	receivers, err := s.rpcClient.Call(ctx, sc.RPCFunc, sc.RPCKwargs)
	if err != nil {
		slog.Error("scheduler: dispatch failed", "id", sc.ID, "name", sc.Name, "err", err)
		return false
	}
	if receivers == 0 {
		// Redis accepted the PUBLISH and delivered it to nobody. Not an error
		// from Redis's point of view, and the reason this went unnoticed.
		slog.Error("scheduler: dispatch reached no consumer; NOT marking the schedule as run",
			"id", sc.ID, "name", sc.Name, "rpc_func", sc.RPCFunc,
			"channel", s.cfg.RPCChannel, "receivers", 0,
			"hint", "this stack has no subscriber on the RPC channel — legacy Pylon is the only consumer of the arbiter wire format")
		return false
	}

	// Update last_run
	if _, err := s.pool.Exec(ctx,
		`UPDATE centry.schedule SET last_run = $1 WHERE id = $2`, now, sc.ID); err != nil {
		slog.Error("scheduler: update last_run", "id", sc.ID, "err", err)
	}
	return true
}

// timeToRun checks if a schedule is due, matching pylon's croniter logic:
// if last_run is nil, it's always due; otherwise next(last_run) <= now.
func (s *Scheduler) timeToRun(sc Schedule, now time.Time) bool {
	if sc.LastRun == nil {
		return true
	}

	sched, err := s.parser.Parse(sc.Cron)
	if err != nil {
		slog.Debug("scheduler: invalid cron", "id", sc.ID, "name", sc.Name, "cron", sc.Cron, "err", err)
		return false
	}

	nextRun := sched.Next(*sc.LastRun)
	return !nextRun.After(now)
}
