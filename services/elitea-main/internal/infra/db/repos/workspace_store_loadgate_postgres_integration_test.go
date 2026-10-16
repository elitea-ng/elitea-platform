//go:build loadgate

package repos

// The ADR-0034 decision 15 load gate, against a REAL PostgreSQL
// (ELITEA_TEST_DATABASE_URL). It is behind the `loadgate` build tag because it
// is a measurement, not a unit test: CI runs it in its own job
// (.github/workflows/ci-handoff-loadgate.yml).
//
//	go test -tags loadgate -run TestLoadGate -v ./internal/infra/db/repos/
//
// Decision 15: "1,000 cloud turns publishing at the same time shows a p99
// publish time under 50 ms, no growth in Main's database connections, and no
// lock waits between threads." Each case runs 1,000 publishes, one per
// thread, through Main's pool (ELITEA_LOADGATE_MAX_CONNS, default 32; Main's
// ELITEA_DATABASE_MAX_CONNS allows 1-64), and samples pg_stat_activity on a
// separate connection while they run.
//
// The gated rate is the owner's decision (2026-10-15): 300 publishes per
// second (one every 3.3 ms; about 18 times the ADR's peak estimate of 17 per
// second). A GitHub runner drains about 1,500 publishes per second, so at
// 1,000 per second its p99 is the tail of a queue, not the handoff.
//
// GATED (p99 of the whole Publish call, the reads before the transaction, the
// pool wait and the transaction, under 50 ms; no row or transaction lock wait
// between threads; no pool growth past its maximum):
//   - 300/s, one thread per project;
//   - same-thread contention: the 300/s load plus 50 publishes released at
//     once on ONE thread (exactly one wins the compare-and-set; the others
//     answer a ref conflict without waiting), gated on the other threads;
//   - 20 threads per project at 300/s (a wait on the project's quota lock is
//     allowed, a row lock wait is not).
//
// RUN AND LOGGED, not gated: 1,000/s (the starts 1 ms apart) and the burst
// (all 1,000 released at one instant: p99 is the pool's drain time).
//
// "Lock waits between threads" are row and transaction lock waits
// (wait_event tuple, transactionid, relation, advisory ...). A relation
// EXTENSION wait (wait_event `extend`) is PostgreSQL allocating a new page for
// a growing table, held for microseconds and not a wait on another thread's
// row; it is logged and not counted.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	loadGateThreads = 1000
	// loadGateQuotaProjects is the project count of the same-project case:
	// 20 threads per project share its quota lock.
	loadGateQuotaProjects = 50
	loadGateP99           = 50 * time.Millisecond
)

type loadGateThread struct {
	thread  workspacestore.Thread
	owner   int64
	content []byte
}

type loadGateSample struct {
	// lockWaiters counts row and transaction lock waits; extendWaiters the
	// relation extension waits.
	lockWaiters   int
	extendWaiters int
	// quotaWaiters counts waits on a project's quota lock (an advisory
	// lock): publishes of the SAME project serialise their quota check.
	quotaWaiters int
	backends     int
	// waits names the lock waits seen, as wait_event: left(query, 60).
	waits []string
}

func loadGateMaxConns(t *testing.T) int32 {
	raw := os.Getenv("ELITEA_LOADGATE_MAX_CONNS")
	if raw == "" {
		return 32
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 64 {
		t.Fatalf("ELITEA_LOADGATE_MAX_CONNS must be 1-64, got %q", raw)
	}
	return int32(value)
}

// loadGatePool opens Main's pool on the test database, with the configured
// maximum, and a separate connection for the sampler.
func loadGatePool(t *testing.T, setup *pgxpool.Pool, maxConns int32) (*pgxpool.Pool, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	config := setup.Config().Copy()
	config.MaxConns = maxConns
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sampler, err := pgx.ConnectConfig(ctx, setup.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close(context.Background()) })
	return pool, sampler
}

// seedLoadGateThreads creates the handoff rows in HANDING_OFF at epoch 1 and
// stores each thread's segment (checksum-verified, as on S3 and RustFS).
func seedLoadGateThreads(t *testing.T, pool *pgxpool.Pool, store *workspacestoretest.Store, count, projects int) []loadGateThread {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs
    (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id, has_upstream)
SELECT 1 + (n % $2), gen_random_uuid(), gen_random_uuid(), 'HANDING_OFF', 1, 100000 + n, false
FROM generate_series(0, $1 - 1) AS n
RETURNING project_id, conversation_uuid::text, owner_user_id`, count, projects)
	if err != nil {
		t.Fatal(err)
	}
	threads := []loadGateThread{}
	for rows.Next() {
		var thread loadGateThread
		if err := rows.Scan(&thread.thread.ProjectID, &thread.thread.ConversationUUID, &thread.owner); err != nil {
			t.Fatal(err)
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for index := range threads {
		threads[index].content = []byte(fmt.Sprintf("load gate segment %d %s", index, threads[index].thread.ConversationUUID))
		ref, err := storage.NewObjectRef(strconv.FormatInt(threads[index].thread.ProjectID, 10), storage.WorkspaceBucketName,
			workspacestore.ObjectKey(threads[index].thread.ConversationUUID, digestOf(threads[index].content)))
		if err != nil {
			t.Fatal(err)
		}
		store.SeedWithChecksum(ref, threads[index].content)
	}
	if _, err := pool.Exec(ctx, `ANALYZE elitea_runtime.workspace_handoffs`); err != nil {
		t.Fatal(err)
	}
	return threads
}

func loadGatePublish(thread loadGateThread, seed string) workspacestore.PublishRequest {
	return desktopPublish(thread.thread, thread.owner, 1, digestOf(thread.content), int64(len(thread.content)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of(seed + "-base")},
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: sha1Of(seed)})
}

// sampleDatabase polls pg_stat_activity until stop closes: the backends of
// this database other than the sampler, and those waiting on a lock.
func sampleDatabase(t *testing.T, sampler *pgx.Conn, pool *pgxpool.Pool, stop <-chan struct{}) (samples []loadGateSample, maxPoolConns int32) {
	t.Helper()
	ctx := context.Background()
	for {
		select {
		case <-stop:
			return samples, maxPoolConns
		default:
		}
		var sample loadGateSample
		if err := sampler.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE wait_event_type = 'Lock' AND wait_event NOT IN ('extend', 'advisory')),
       count(*) FILTER (WHERE wait_event_type = 'Lock' AND wait_event = 'extend'),
       count(*) FILTER (WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'), count(*),
       COALESCE(array_agg(DISTINCT wait_event || ': ' || left(regexp_replace(query, '\s+', ' ', 'g'), 60))
                FILTER (WHERE wait_event_type = 'Lock'), '{}')
FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'`).
			Scan(&sample.lockWaiters, &sample.extendWaiters, &sample.quotaWaiters, &sample.backends, &sample.waits); err != nil {
			t.Errorf("sample: %v", err)
			return samples, maxPoolConns
		}
		samples = append(samples, sample)
		if total := pool.Stat().TotalConns(); total > maxPoolConns {
			maxPoolConns = total
		}
		time.Sleep(500 * time.Microsecond)
	}
}

func percentile(durations []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted))*p+0.5) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

type loadGateRun struct {
	durations    []time.Duration
	errs         []error
	samples      []loadGateSample
	maxPoolConns int32
	wall         time.Duration
}

// runLoadGate starts every publish at its offset after a common start (all
// at once when offsets is nil) and samples the database while they run.
func runLoadGate(t *testing.T, pool *pgxpool.Pool, sampler *pgx.Conn, publishes []func() error, offsets []time.Duration) loadGateRun {
	t.Helper()
	run := loadGateRun{durations: make([]time.Duration, len(publishes)), errs: make([]error, len(publishes))}
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		run.samples, run.maxPoolConns = sampleDatabase(t, sampler, pool, stop)
		close(sampled)
	}()
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	for index, publish := range publishes {
		ready.Add(1)
		done.Add(1)
		go func(index int, publish func() error) {
			defer done.Done()
			ready.Done()
			<-start
			if offsets != nil && offsets[index] > 0 {
				time.Sleep(offsets[index])
			}
			began := time.Now()
			run.errs[index] = publish()
			run.durations[index] = time.Since(began)
		}(index, publish)
	}
	ready.Wait()
	began := time.Now()
	close(start)
	done.Wait()
	run.wall = time.Since(began)
	close(stop)
	<-sampled
	return run
}

func (run loadGateRun) report(t *testing.T, name string, indexes []int) (p50, p99, max time.Duration) {
	t.Helper()
	durations := make([]time.Duration, 0, len(indexes))
	for _, index := range indexes {
		durations = append(durations, run.durations[index])
	}
	p50, p99, max = percentile(durations, 0.50), percentile(durations, 0.99), percentile(durations, 1)
	lockSamples, maxWaiters, maxBackends := 0, 0, 0
	waits := map[string]int{}
	for _, sample := range run.samples {
		if sample.lockWaiters > 0 {
			lockSamples++
		}
		for _, wait := range sample.waits {
			waits[wait]++
		}
		maxWaiters = max2(maxWaiters, sample.lockWaiters)
		maxBackends = max2(maxBackends, sample.backends)
	}
	t.Logf("%s: n=%d wall=%s p50=%s p99=%s max=%s | samples=%d with-lock-waits=%d max-lock-waiters=%d max-backends=%d max-pool-conns=%d",
		name, len(durations), run.wall.Round(time.Millisecond), p50.Round(10*time.Microsecond), p99.Round(10*time.Microsecond),
		max.Round(10*time.Microsecond), len(run.samples), lockSamples, maxWaiters, maxBackends, run.maxPoolConns)
	for wait, count := range waits {
		t.Logf("%s: lock wait seen in %d samples: %s", name, count, wait)
	}
	return p50, p99, max
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// loadGateFixture is one isolated database with 1,000 threads ready to
// publish, Main's pool and the sampler connection.
type loadGateFixture struct {
	pool     *pgxpool.Pool
	sampler  *pgx.Conn
	service  *workspacestore.Service
	threads  []loadGateThread
	maxConns int32
}

// newLoadGateFixture seeds 1,000 threads over `projects` projects.
func newLoadGateFixture(t *testing.T, projects int) loadGateFixture {
	t.Helper()
	setup := newMigratedPostgresIntegrationPool(t)
	maxConns := loadGateMaxConns(t)
	pool, sampler := loadGatePool(t, setup, maxConns)
	store := workspacestoretest.New(true)
	store.Checksum = true
	service, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	threads := seedLoadGateThreads(t, setup, store, loadGateThreads, projects)
	// Each project's bucket exists, as it does after the upload grant that
	// comes before every publish.
	repo, err := NewWorkspaceStoreRepository(setup)
	if err != nil {
		t.Fatal(err)
	}
	for project := int64(1); project <= int64(projects); project++ {
		if _, err := repo.EnsureWorkspaceBucket(context.Background(), project); err != nil {
			t.Fatalf("bucket of project %d: %v", project, err)
		}
	}
	setup.Reset()
	return loadGateFixture{pool: pool, sampler: sampler, service: service, threads: threads, maxConns: maxConns}
}

func (f loadGateFixture) publish(thread loadGateThread, seed string) func() error {
	request := loadGatePublish(thread, seed)
	return func() error {
		_, err := f.service.Publish(context.Background(), request)
		return err
	}
}

// loadGateRate is the gated publish rate per second (owner decision).
const loadGateRate = 300

// atRate spaces n starts at `rate` per second.
func atRate(n, rate int) []time.Duration {
	offsets := make([]time.Duration, n)
	for index := range offsets {
		offsets[index] = time.Duration(index) * time.Second / time.Duration(rate)
	}
	return offsets
}

// checkLocksAndConnections fails on a row or transaction lock wait beyond
// allowedWaiters, on a quota lock wait when quotaWaits is false, and on any
// connection growth.
func (f loadGateFixture) checkLocksAndConnections(t *testing.T, run loadGateRun, allowedWaiters int, quotaWaits bool) {
	t.Helper()
	if len(run.samples) < 10 {
		t.Errorf("only %d samples were taken; the lock-wait check proves nothing", len(run.samples))
	}
	for _, sample := range run.samples {
		if !quotaWaits && sample.quotaWaiters > 0 {
			t.Errorf("%d backends waited on a quota lock while every thread is in its own project: %v",
				sample.quotaWaiters, sample.waits)
			break
		}
	}
	for _, sample := range run.samples {
		if sample.lockWaiters > allowedWaiters {
			t.Errorf("%d backends waited on a row or transaction lock (at most %d may): %v",
				sample.lockWaiters, allowedWaiters, sample.waits)
			break
		}
	}
	for _, sample := range run.samples {
		if sample.backends > int(f.maxConns) {
			t.Errorf("the database saw %d backends, more than the pool's %d", sample.backends, f.maxConns)
			break
		}
	}
	if run.maxPoolConns > f.maxConns {
		t.Errorf("the pool grew to %d connections, above its maximum %d", run.maxPoolConns, f.maxConns)
	}
}

func allIndexes(n int) []int {
	out := make([]int, n)
	for index := range out {
		out[index] = index
	}
	return out
}

func TestLoadGatePublishesAcrossThreads(t *testing.T) {
	for _, mode := range []struct {
		name    string
		offsets func(int) []time.Duration
		gated   bool
	}{
		{"sustained_300", func(n int) []time.Duration { return atRate(n, loadGateRate) }, true},
		{"sustained_1000", func(n int) []time.Duration { return atRate(n, 1000) }, false},
		{"burst", func(int) []time.Duration { return nil }, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// One thread per project, as with personal projects: no two
			// publishes share a project's quota lock.
			f := newLoadGateFixture(t, loadGateThreads)
			publishes := make([]func() error, 0, len(f.threads))
			for index, thread := range f.threads {
				publishes = append(publishes, f.publish(thread, mode.name+"-"+strconv.Itoa(index)))
			}
			run := runLoadGate(t, f.pool, f.sampler, publishes, mode.offsets(len(publishes)))
			for index, err := range run.errs {
				if err != nil {
					t.Fatalf("publish %d: %v", index, err)
				}
			}
			label := "1000 threads, " + mode.name
			if !mode.gated {
				label += " (not gated)"
			}
			_, p99, _ := run.report(t, label, allIndexes(len(publishes)))
			var segments int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_runtime.workspace_segments`).Scan(&segments); err != nil || segments != loadGateThreads {
				t.Fatalf("segments = %d, %v; want one per thread", segments, err)
			}
			if !mode.gated {
				return
			}
			if p99 >= loadGateP99 {
				t.Errorf("p99 publish = %s, gate is under %s", p99, loadGateP99)
			}
			// 1,000 DIFFERENT threads: no backend may wait on another's row.
			f.checkLocksAndConnections(t, run, 0, false)
		})
	}
}

func TestLoadGateSameThreadContention(t *testing.T) {
	f := newLoadGateFixture(t, loadGateThreads)
	const contenders = 50
	hot := f.threads[0]
	var wins, conflicts atomic.Int64
	publishes := make([]func() error, 0, contenders+len(f.threads)-1)
	offsets := []time.Duration{}
	// 50 publishes of the SAME first snapshot on the hot thread, released at
	// once, each with a different handoff/1 value and no expected old value:
	// exactly one creates the refs, the others meet a ref conflict.
	for index := 0; index < contenders; index++ {
		request := loadGatePublish(hot, "hot-"+strconv.Itoa(index))
		request.Refs[0].New = sha1Of("hot-base")
		publishes = append(publishes, func() error {
			_, err := f.service.Publish(context.Background(), request)
			var conflict *workspacestore.RefConflictError
			switch {
			case err == nil:
				wins.Add(1)
				return nil
			case errors.As(err, &conflict):
				conflicts.Add(1)
				return nil
			default:
				return err
			}
		})
		// Released together in the middle of the run.
		offsets = append(offsets, time.Duration(len(f.threads)/2)*time.Second/loadGateRate)
	}
	others := []int{}
	spread := atRate(len(f.threads)-1, loadGateRate)
	for index, thread := range f.threads[1:] {
		others = append(others, len(publishes))
		publishes = append(publishes, f.publish(thread, "other-"+strconv.Itoa(index)))
		offsets = append(offsets, spread[index])
	}
	run := runLoadGate(t, f.pool, f.sampler, publishes, offsets)
	for index, err := range run.errs {
		if err != nil {
			t.Fatalf("publish %d: %v", index, err)
		}
	}
	run.report(t, "hot thread", allIndexes(contenders))
	_, p99, _ := run.report(t, "other threads", others)
	if wins.Load() != 1 || conflicts.Load() != contenders-1 {
		t.Fatalf("hot thread: %d wins, %d conflicts; want 1 and %d", wins.Load(), conflicts.Load(), contenders-1)
	}
	if p99 >= loadGateP99 {
		t.Errorf("other threads' p99 = %s under same-thread contention, gate is under %s", p99, loadGateP99)
	}
	// Only the hot thread's other publishers may wait on its row lock.
	f.checkLocksAndConnections(t, run, contenders-1, false)
	var seq int64
	if err := f.pool.QueryRow(context.Background(), `
SELECT last_seq FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		hot.thread.ProjectID, hot.thread.ConversationUUID).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("hot thread last_seq = %d, %v; want 1", seq, err)
	}
}

// TestLoadGateSameProjectQuota: 1,000 threads in 50 projects (20 each), at
// the gated rate. Publishes of one project serialise only on the project's quota
// lock, held for one SUM and the commit (ADR-0034 decision 11, the
// max_project_workspace_gb ceiling); they never wait on each other's rows,
// and p99 stays under the gate.
func TestLoadGateSameProjectQuota(t *testing.T) {
	f := newLoadGateFixture(t, loadGateQuotaProjects)
	publishes := make([]func() error, 0, len(f.threads))
	for index, thread := range f.threads {
		publishes = append(publishes, f.publish(thread, "quota-"+strconv.Itoa(index)))
	}
	run := runLoadGate(t, f.pool, f.sampler, publishes, atRate(len(publishes), loadGateRate))
	for index, err := range run.errs {
		if err != nil {
			t.Fatalf("publish %d: %v", index, err)
		}
	}
	_, p99, _ := run.report(t, "1000 threads in 50 projects, 300/s", allIndexes(len(publishes)))
	if p99 >= loadGateP99 {
		t.Errorf("p99 publish = %s, gate is under %s", p99, loadGateP99)
	}
	f.checkLocksAndConnections(t, run, 0, true)
}
