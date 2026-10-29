// Package storetest is the contract every egressaudit.Store must meet, run
// over the in-memory store and over the Postgres store with the same cases
// (what the deployment depends on is the Postgres one: the in-memory store
// proves the Merger, this proves the claim).
//
// The cases model two Main replicas as two Mergers over ONE store sharing a
// clock, and a Main restart as a fresh Merger over the same store.
package storetest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// Recorder is an audit.Recorder that keeps what it is given.
type Recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

// Record implements audit.Recorder.
func (r *Recorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// All returns the recorded events.
func (r *Recorder) All() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Event(nil), r.events...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var epoch = time.Unix(1_800_000_000, 0).UTC()

type env struct {
	t     *testing.T
	store app.Store
	rec   *Recorder
	clock *clock
}

// main builds one Main replica: a Merger over the shared store, recorder and
// clock.
func (e *env) main() *app.Merger {
	return app.New(e.store, e.rec, nil, app.Config{
		Settle: 30 * time.Second, MaxHold: 10 * time.Minute, Retention: 48 * time.Hour,
	}).WithClock(e.clock.now)
}

func (e *env) report(m *app.Merger, replica string, final bool, reached ...app.HostReached) app.Report {
	e.t.Helper()
	r := app.Report{ReplicaID: replica, ExecutionID: "exec-1", ProjectID: 7, Final: final,
		TokenExpiresAt: epoch.Add(10 * time.Minute), Connections: int64(len(reached)), BytesUp: 10, BytesDown: 20,
		HostsReached: reached}
	if err := m.Report(context.Background(), r); err != nil {
		e.t.Fatalf("report: %v", err)
	}
	return r
}

func (e *env) sweep(m *app.Merger) {
	e.t.Helper()
	if err := m.Sweep(context.Background()); err != nil {
		e.t.Fatalf("sweep: %v", err)
	}
}

var pypi = app.HostReached{Host: "pypi.org", Port: 443, Connections: 2, BytesUp: 5, BytesDown: 9}
var github = app.HostReached{Host: "github.com", Port: 443, Connections: 3, BytesUp: 3, BytesDown: 3}

// Run runs the contract. newStore returns an empty store for one case.
func Run(t *testing.T, newStore func(t *testing.T) app.Store) {
	cases := map[string]func(*env){
		"two Main replicas write one event for one turn":                  twoMainReplicasOneEvent,
		"concurrent sweeps on many replicas write once":                   concurrentSweepsWriteOnce,
		"a restart between reports loses nothing":                         restartLosesNothing,
		"a pending turn is not lost when Main closes":                     closeWritesNothing,
		"a replica that never finishes is written partial after the hold": partialAfterHold,
		"a late non-final snapshot does not undo a final one":             finalIsSticky,
		"a report after the claim starts a new turn":                      reportAfterClaimIsANewTurn,
		"rows past retention are deleted":                                 retentionDeletes,
		"a retried report never counts twice":                             retryDoesNotDoubleCount,
		"a worker row is written once its run settles":                    workerFinalOnSettlement,
		"a worker row and proxy rows merge into one event":                workerAndProxyMerge,
		"a worker that never settles is written after its hold":           workerWithoutSettlement,
		"a resumed run's command keeps the earlier command's row":         workerResumeKeepsEachCommandsRow,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			run(&env{t: t, store: newStore(t), rec: &Recorder{}, clock: &clock{t: epoch}})
		})
	}
}

func twoMainReplicasOneEvent(e *env) {
	m1, m2 := e.main(), e.main()
	// Each proxy replica's connection landed on a different Main replica.
	e.report(m1, "proxy-a", false, pypi)
	e.report(m2, "proxy-b", false, github)
	e.report(m1, "proxy-a", true, pypi)
	e.report(m2, "proxy-b", true, github)
	e.clock.advance(time.Minute)
	e.sweep(m1)
	e.sweep(m2)
	events := e.rec.All()
	if len(events) != 1 {
		e.t.Fatalf("events: %d, want one per turn", len(events))
	}
	want := "egress proxy: reached github.com:443, pypi.org:443"
	if events[0].Action != want || events[0].EntityName != "exec-1" || *events[0].ProjectID != 7 {
		e.t.Fatalf("event %+v", events[0])
	}
	e.clock.advance(time.Hour)
	e.sweep(m1)
	e.sweep(m2)
	if len(e.rec.All()) != 1 {
		e.t.Fatal("a claimed turn is never written again")
	}
}

func concurrentSweepsWriteOnce(e *env) {
	mains := make([]*app.Merger, 8)
	for i := range mains {
		mains[i] = e.main()
	}
	for i, replica := range []string{"proxy-a", "proxy-b", "proxy-c"} {
		e.report(mains[i], replica, true, pypi)
	}
	e.clock.advance(time.Minute)
	var wg sync.WaitGroup
	for _, m := range mains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Sweep(context.Background()); err != nil {
				e.t.Errorf("sweep: %v", err)
			}
		}()
	}
	wg.Wait()
	events := e.rec.All()
	if len(events) != 1 {
		e.t.Fatalf("events: %d, want exactly one", len(events))
	}
	if !strings.Contains(events[0].Action, "pypi.org:443") || strings.Contains(events[0].Action, "partial") {
		e.t.Fatalf("event %+v", events[0])
	}
}

func restartLosesNothing(e *env) {
	before := e.main()
	e.report(before, "proxy-a", false, pypi)
	e.report(before, "proxy-b", false, github)
	before.Close() // Main restarts with the turn still open.
	e.clock.advance(5 * time.Second)
	after := e.main()
	e.sweep(after)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("an open turn is not written")
	}
	e.report(after, "proxy-a", true, pypi)
	e.report(after, "proxy-b", true, github)
	e.clock.advance(time.Minute)
	e.sweep(after)
	events := e.rec.All()
	if len(events) != 1 || events[0].Action != "egress proxy: reached github.com:443, pypi.org:443" {
		e.t.Fatalf("events %+v: the reports made before the restart must be in the turn", events)
	}
}

func closeWritesNothing(e *env) {
	m := e.main()
	m.Start()
	e.report(m, "proxy-a", false, pypi)
	m.Close()
	if len(e.rec.All()) != 0 {
		e.t.Fatal("Close must not write a turn that is still open")
	}
	// ...it is still there for the next Main.
	e.clock.advance(21 * time.Minute)
	e.sweep(e.main())
	if events := e.rec.All(); len(events) != 1 || !strings.HasSuffix(events[0].Action, "; partial") {
		e.t.Fatalf("events %+v", events)
	}
}

func partialAfterHold(e *env) {
	m := e.main()
	e.report(m, "proxy-a", false, pypi)
	e.clock.advance(15 * time.Minute)
	e.sweep(m)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("the hold counts from the token's expiry (10 min after the start) plus 10 min")
	}
	e.clock.advance(10 * time.Minute)
	e.sweep(m)
	events := e.rec.All()
	if len(events) != 1 || !strings.HasSuffix(events[0].Action, "; partial") {
		e.t.Fatalf("%+v", events)
	}
}

func finalIsSticky(e *env) {
	m := e.main()
	e.report(m, "proxy-a", true, pypi)
	e.report(m, "proxy-a", false, github) // a late, reordered periodic snapshot
	e.clock.advance(time.Minute)
	e.sweep(m)
	events := e.rec.All()
	if len(events) != 1 || events[0].Action != "egress proxy: reached pypi.org:443" {
		e.t.Fatalf("%+v", events)
	}
}

func reportAfterClaimIsANewTurn(e *env) {
	m1, m2 := e.main(), e.main()
	e.report(m1, "proxy-a", true, pypi)
	e.report(m2, "proxy-b", true, github)
	e.clock.advance(time.Minute)
	e.sweep(m1)
	if len(e.rec.All()) != 1 {
		e.t.Fatal("first turn")
	}
	// The turn is resumed: the same execution id, a new token, on proxy-a only.
	e.clock.advance(time.Minute)
	e.report(m2, "proxy-a", true, app.HostReached{Host: "crates.io", Port: 443, Connections: 1})
	e.clock.advance(time.Minute)
	e.sweep(m1)
	e.sweep(m2)
	events := e.rec.All()
	if len(events) != 2 {
		e.t.Fatalf("events: %d, want the resumed turn as a second event", len(events))
	}
	if events[1].Action != "egress proxy: reached crates.io:443" {
		e.t.Fatalf("the resumed turn carries only its own replicas: %q", events[1].Action)
	}
}

func retentionDeletes(e *env) {
	m := e.main()
	e.report(m, "proxy-a", true, pypi) // exec-1, updated at the epoch
	e.clock.advance(time.Minute)
	e.sweep(m) // claimed and written
	e.clock.advance(47 * time.Hour)
	if err := m.Report(context.Background(), app.Report{ReplicaID: "proxy-a", ExecutionID: "exec-2", ProjectID: 7,
		Final: true, TokenExpiresAt: e.clock.now().Add(time.Minute)}); err != nil {
		e.t.Fatal(err)
	}
	e.clock.advance(2 * time.Hour) // exec-1 is 49 h old, exec-2 is 2 h old; retention is 48 h
	e.sweep(m)                     // the merger's own sweep deletes what is past it
	if len(e.rec.All()) != 2 {
		e.t.Fatalf("events %d: both turns were written, exec-2 when it settled", len(e.rec.All()))
	}
	// What is left is exec-2's row only: exec-1's is gone, exec-2's is kept.
	left, err := e.store.DeleteOlderThan(context.Background(), e.clock.now().Add(time.Hour))
	if err != nil || left != 1 {
		e.t.Fatalf("rows left after the retention sweep: %d (%v), want exec-2's one", left, err)
	}
}

func retryDoesNotDoubleCount(e *env) {
	m := e.main()
	r := e.report(m, "proxy-a", true, pypi)
	if err := m.Report(context.Background(), r); err != nil {
		e.t.Fatal(err)
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	if len(e.rec.All()) != 1 {
		e.t.Fatalf("events %d", len(e.rec.All()))
	}
}

func (e *env) worker(m *app.Merger, reached ...app.HostReached) {
	e.t.Helper()
	e.workerCommand(m, "cmd-1", reached...)
}

func (e *env) workerCommand(m *app.Merger, commandID string, reached ...app.HostReached) {
	e.t.Helper()
	if err := m.ReportWorker(context.Background(), app.WorkerSummary{ExecutionID: "exec-1", CommandID: commandID, ProjectID: 7,
		HostsReached: reached,
		HostsRefused: []app.HostRefused{{Host: "evil.example", Port: 443, Reason: "egress_host_not_allowed", Count: 2}}}); err != nil {
		e.t.Fatalf("worker report: %v", err)
	}
}

func workerFinalOnSettlement(e *env) {
	m := e.main()
	e.worker(m, pypi)
	e.worker(m, pypi, github) // cumulative: replaces the first
	e.clock.advance(time.Hour)
	e.sweep(m)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("a running worker's row is not written before the run settles")
	}
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-1"); err != nil {
		e.t.Fatal(err)
	}
	e.sweep(m)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("settlement restarts the settle window")
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	events := e.rec.All()
	want := "worker egress: reached github.com:443, pypi.org:443; refused evil.example:443 (egress_host_not_allowed x2)"
	if len(events) != 1 || events[0].Action != want || *events[0].ProjectID != 7 {
		e.t.Fatalf("events %+v", events)
	}
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-1"); err != nil {
		e.t.Fatal("settling a claimed turn again is not an error")
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	if len(e.rec.All()) != 1 {
		e.t.Fatal("a settled, written turn is not written again")
	}
}

func workerAndProxyMerge(e *env) {
	m := e.main()
	e.report(m, "proxy-a", true, pypi)
	e.worker(m, github)
	e.clock.advance(time.Minute)
	e.sweep(m)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("the proxy's last report does not settle the worker's row")
	}
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-1"); err != nil {
		e.t.Fatal(err)
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	events := e.rec.All()
	if len(events) != 1 || !strings.HasPrefix(events[0].Action, "egress (worker and proxy): reached github.com:443, pypi.org:443") {
		e.t.Fatalf("events %+v", events)
	}
}

// A paused run that resumes is a new command of the same execution, and its
// worker reports only the new turn's hosts: its row must not replace the
// earlier command's, and each turn is written once with its own hosts.
func workerResumeKeepsEachCommandsRow(e *env) {
	m := e.main()
	e.workerCommand(m, "cmd-1", pypi)
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-1"); err != nil {
		e.t.Fatal(err)
	}
	e.workerCommand(m, "cmd-2", github)
	e.clock.advance(time.Minute)
	e.sweep(m)
	events := e.rec.All()
	if len(events) != 0 {
		e.t.Fatalf("the resumed command's running row holds the turn: events %+v", events)
	}
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-2"); err != nil {
		e.t.Fatal(err)
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	events = e.rec.All()
	want := "worker egress: reached github.com:443, pypi.org:443; refused evil.example:443 (egress_host_not_allowed x4)"
	if len(events) != 1 || events[0].Action != want {
		e.t.Fatalf("both commands' hosts in one turn: events %+v", events)
	}
	e.workerCommand(m, "cmd-3", github)
	if err := m.SettleWorker(context.Background(), "exec-1", "cmd-3"); err != nil {
		e.t.Fatal(err)
	}
	e.clock.advance(time.Minute)
	e.sweep(m)
	events = e.rec.All()
	want = "worker egress: reached github.com:443; refused evil.example:443 (egress_host_not_allowed x2)"
	if len(events) != 2 || events[1].Action != want {
		e.t.Fatalf("a later turn is its own event with only its hosts: events %+v", events)
	}
	if err := m.ReportWorker(context.Background(), app.WorkerSummary{ExecutionID: "exec-1", ProjectID: 7}); err == nil {
		e.t.Fatal("a worker summary without its command is refused")
	}
	if err := m.SettleWorker(context.Background(), "exec-1", ""); err == nil {
		e.t.Fatal("settling without the command is refused")
	}
}

func workerWithoutSettlement(e *env) {
	m := e.main()
	e.worker(m, pypi)
	e.clock.advance(app.WorkerHold)
	e.sweep(m)
	if len(e.rec.All()) != 0 {
		e.t.Fatal("written before the hold and MaxHold passed")
	}
	e.clock.advance(10 * time.Minute)
	e.sweep(m)
	if events := e.rec.All(); len(events) != 1 || !strings.HasSuffix(events[0].Action, "; partial") {
		e.t.Fatalf("events %+v", events)
	}
}
