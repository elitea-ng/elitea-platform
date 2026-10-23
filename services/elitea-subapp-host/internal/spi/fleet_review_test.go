package spi_test

// The fixes of the #scaling review: a full replica falls through to the
// fleet, refusals by reason, the stop fence, a mixed fleet in a rolling
// upgrade, and liveness that moves capacity and load together. The tests
// that need PostgreSQL skip without ELITEA_SUBAPP_HOST_TEST_DSN.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// fenceRunner is a DeepWiki runner whose generate_wiki is labelled with the
// request's project and holds a slot. A run ends when release closes; once
// a stop is requested, it ends (cancelled) only when yield closes, so a
// test can hold a deletion inside its wait.
type fenceRunner struct {
	release chan struct{}
	yield   chan struct{}
	// stopSeen, when not nil, receives the id of a run that sees its stop
	// (without blocking the run).
	stopSeen chan string
}

func (fenceRunner) Name() string { return "fence" }

func (fenceRunner) HoldsSlot(call spi.Invoke) bool {
	return call.Tool == "generate_wiki" && call.Family.Name != "query"
}

func (fenceRunner) Labels(call spi.Invoke) map[string]string {
	if call.Tool != "generate_wiki" {
		return nil
	}
	project, _ := call.Request["project"].(string)
	return map[string]string{"project": project}
}

func (r fenceRunner) Invoke(ctx context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error) {
	if call.Tool != "generate_wiki" {
		return spi.Completed(tc.InvocationID()), nil
	}
	for {
		if tc.StopRequested() {
			select {
			case r.stopSeen <- tc.InvocationID():
			default:
			}
			select {
			case <-r.yield:
				return nil, spi.ErrCancelled
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-r.release:
			return spi.Completed(tc.InvocationID()), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

var (
	_ spi.SlotHolder = fenceRunner{}
	_ spi.Labeler    = fenceRunner{}
)

func generate(t *testing.T, server *spi.Server, project string) string {
	t.Helper()
	invocation, err := server.Accept(context.Background(), spi.Identity{ProjectID: project}, "Wikis", "generate_wiki", map[string]any{"project": project})
	if err != nil {
		t.Fatalf("accept generate_wiki: %v", err)
	}
	return invocation.ID
}

// assertFenced checks the refusal of an invocation that a deletion in
// progress holds back: a runtime error, not service_busy, with no busy
// reason (it is not demand for slots).
func assertFenced(t *testing.T, body map[string]any) {
	t.Helper()
	if body["status"] != "Error" || body["error_category"] == spi.CategoryServiceBusy || body[spi.BusyReasonKey] != nil ||
		!strings.Contains(messageOf(t, body), "A deletion of what this invocation works on is in progress") {
		t.Fatalf("not the stop-fence refusal: %v", body)
	}
}

// secondStore opens a sibling replica's store on the schema of first.
func secondStore(t *testing.T, first *spi.PostgresStore, owner string, options ...spi.StoreOption) *spi.PostgresStore {
	t.Helper()
	store, err := spi.NewPostgresStore(context.Background(), dsnOf(t, first), owner, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func uniqueOwner(name string) string { return fmt.Sprintf("%s-%d/1", name, time.Now().UnixNano()) }

// Finding 1 and 8, against PostgreSQL: with 2 replicas of 1 slot, both
// generations that land on replica A run (A goes above its own slot count,
// the fleet has room). The third is refused because the FLEET is full: it
// is counted as fleet_full on /metrics, and its stored result carries
// busy_reason = fleet_full, which is what the KEDA query selects.
func TestAFullReplicaFallsThroughToTheFleet(t *testing.T) {
	storeA, assertions := testStore(t, uniqueOwner("spi-through-a"), spi.WithReplicaSlots(1))
	storeB := secondStore(t, storeA, uniqueOwner("spi-through-b"), spi.WithReplicaSlots(1))
	runner := slotRunner{release: make(chan struct{})}
	a := slotServer(t, runner, 1, spi.WithStore(storeA))
	b := slotServer(t, runner, 1, spi.WithStore(storeB))

	first := accept(t, a, "Wikis", "generate_wiki")
	second := accept(t, a, "Wikis", "generate_wiki")
	waitStatus(t, a, "Wikis", "generate_wiki", first, "InProgress")
	waitStatus(t, a, "Wikis", "generate_wiki", second, "InProgress")
	if got := a.Manager().LocalHolders(); got != 2 {
		t.Fatalf("replica A runs %d, want 2 (its own slot is full, the fleet had room)", got)
	}
	if slots := b.SlotsDocument(context.Background()); slots["total"] != 2 || slots["active"] != 2 || slots["can_start"] != false {
		t.Fatalf("fleet slots %v", slots)
	}
	refused := accept(t, a, "Wikis", "generate_wiki")
	body := pollOnce(t, b, "Wikis", "generate_wiki", refused)
	assertNoSlot(t, body, 2)
	if body[spi.BusyReasonKey] != spi.RefusalFleetFull {
		t.Fatalf("the fleet refusal does not say fleet_full: %v", body)
	}
	if got := a.Manager().RefusalsFor(spi.RefusalFleetFull); got != 1 || a.Manager().Refusals() != 1 {
		t.Fatalf("fleet_full refusals = %d (total %d), want 1", got, a.Manager().Refusals())
	}
	if metrics := a.MetricsText(); !regexp.MustCompile(`(?m)^` + spi.MetricSlotRefusals + `\{app="[^"]+",reason="fleet_full"\} 1$`).MatchString(metrics) {
		t.Fatalf("the fleet refusal is not on /metrics as fleet_full:\n%s", metrics)
	}
	// The chart's KEDA query selects the fleet-full refusals by the stored
	// result; a draining refusal is not selected.
	b.Manager().SetAdmitting(false)
	draining := accept(t, b, "Wikis", "generate_wiki")
	if body := pollOnce(t, a, "Wikis", "generate_wiki", draining); body[spi.BusyReasonKey] != spi.RefusalDraining {
		t.Fatalf("draining refusal %v", body)
	}
	var demand int
	if err := assertions.QueryRow(context.Background(),
		`SELECT count(*)::int FROM invocations WHERE finished_at IS NOT NULL AND result->>'error_category' = 'service_busy' AND result->>'busy_reason' = 'fleet_full'`).Scan(&demand); err != nil || demand != 1 {
		t.Fatalf("fleet-full refusals by SQL = %d %v, want 1", demand, err)
	}
	close(runner.release)
	pollTerminal(t, b, "Wikis", "generate_wiki", first)
	pollTerminal(t, b, "Wikis", "generate_wiki", second)
}

// Finding 3, against PostgreSQL, at the store: a live fence refuses a
// matching new invocation (ErrFenced) and inserts nothing; another project,
// another tool and an expired fence do not; an invocation with no label
// matches (the safe side); Unfence lifts it.
func TestPostgresStoreFenceMatchesByToolAndLabels(t *testing.T) {
	store, _ := testStore(t, uniqueOwner("spi-fence-store"), spi.WithReplicaSlots(10))
	ctx := context.Background()
	n := 0
	create := func(tool string, labels map[string]string) error {
		n++
		return store.Create(ctx, &spi.Invocation{ID: fmt.Sprintf("invocation_f%d", n), Toolkit: "Wikis", Tool: tool, Status: "pending", Labels: labels})
	}
	id, err := store.Fence(ctx, spi.StopFence{Tool: "generate_wiki", Labels: map[string]string{"project": "7", "wiki": ""}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := create("generate_wiki", map[string]string{"project": "7", "wiki": "w1"}); !errors.Is(err, spi.ErrFenced) {
		t.Fatalf("a matching invocation = %v, want ErrFenced", err)
	}
	if row, _ := store.Get(ctx, "Wikis", "generate_wiki", fmt.Sprintf("invocation_f%d", n)); row != nil {
		t.Fatalf("a fenced invocation was inserted: %+v", row)
	}
	if err := create("generate_wiki", nil); !errors.Is(err, spi.ErrFenced) {
		t.Fatalf("an invocation with no labels = %v, want ErrFenced", err)
	}
	if err := create("generate_wiki", map[string]string{"project": "8", "wiki": "w1"}); err != nil {
		t.Fatalf("another project was fenced: %v", err)
	}
	if err := create("ask", map[string]string{"project": "7"}); err != nil {
		t.Fatalf("another tool was fenced: %v", err)
	}
	if err := store.Unfence(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := create("generate_wiki", map[string]string{"project": "7", "wiki": "w1"}); err != nil {
		t.Fatalf("Unfence did not lift the fence: %v", err)
	}
	if _, err := store.Fence(ctx, spi.StopFence{Tool: "generate_wiki", Labels: map[string]string{"project": "7"}}, -time.Second); err != nil {
		t.Fatal(err)
	}
	if err := create("generate_wiki", map[string]string{"project": "7"}); err != nil {
		t.Fatalf("an expired fence refused an invocation: %v", err)
	}
}

// Finding 3, against PostgreSQL, the race: replica B stops project 7's
// generation that runs on replica A, and the run takes its time to end. A
// generation of project 7 that arrives on EITHER replica after B read the
// running rows and before the deletion ends is refused; one of project 8
// runs. When the deletion's context ends, the fence goes and project 7 can
// generate again. No refusal here is busy demand.
func TestAStopFenceRefusesAMatchingRunAdmittedDuringTheStop(t *testing.T) {
	storeA, assertions := testStore(t, uniqueOwner("spi-fence-a"), spi.WithReplicaSlots(4), spi.WithClaimCheckInterval(100*time.Millisecond))
	storeB := secondStore(t, storeA, uniqueOwner("spi-fence-b"), spi.WithReplicaSlots(4), spi.WithClaimCheckInterval(100*time.Millisecond))
	runner := fenceRunner{release: make(chan struct{}), yield: make(chan struct{})}
	defer close(runner.release)
	a := slotServer(t, runner, 4, spi.WithStore(storeA))
	b := slotServer(t, runner, 4, spi.WithStore(storeB))
	ctx := context.Background()

	running := generate(t, a, "7")
	waitStatus(t, a, "Wikis", "generate_wiki", running, "InProgress")
	deletion, endDeletion := context.WithCancel(ctx)
	defer endDeletion()
	stopped := make(chan int, 1)
	go func() {
		stopped <- b.Manager().StopMatching(deletion, spi.StopFence{Tool: "generate_wiki", Labels: map[string]string{"project": "7"}}, 10*time.Second)
	}()
	// The stop is on the row: B has read the running rows, and is waiting.
	waitFor(t, "the stop to be recorded", func() bool {
		var requested bool
		_ = assertions.QueryRow(ctx, `SELECT stop_requested FROM invocations WHERE invocation_id = $1`, running).Scan(&requested)
		return requested
	})
	for _, server := range []*spi.Server{a, b} {
		assertFenced(t, pollOnce(t, server, "Wikis", "generate_wiki", generate(t, server, "7")))
	}
	other := generate(t, a, "8")
	waitStatus(t, a, "Wikis", "generate_wiki", other, "InProgress")

	close(runner.yield)
	select {
	case remaining := <-stopped:
		if remaining != 0 {
			t.Fatalf("StopMatching left %d running", remaining)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopMatching did not return")
	}
	// The deletion is still in progress: the fence holds.
	assertFenced(t, pollOnce(t, a, "Wikis", "generate_wiki", generate(t, a, "7")))
	endDeletion()
	waitFor(t, "the fence to be removed", func() bool {
		var fences int
		_ = assertions.QueryRow(ctx, `SELECT count(*)::int FROM host_stop_fences`).Scan(&fences)
		return fences == 0
	})
	again := generate(t, a, "7")
	waitStatus(t, a, "Wikis", "generate_wiki", again, "InProgress")
	if a.Manager().Refusals() != 0 || b.Manager().Refusals() != 0 {
		t.Fatalf("a fence refusal was counted as busy: %d %d", a.Manager().Refusals(), b.Manager().Refusals())
	}
}

// Finding 3 with the in-memory store (one replica): the fence is in the
// manager, and a matching generation submitted while the stop waits is
// refused all the same.
func TestAStopFenceHoldsBackLocalRunsWithTheMemoryStore(t *testing.T) {
	runner := fenceRunner{release: make(chan struct{}), yield: make(chan struct{}), stopSeen: make(chan string, 1)}
	defer close(runner.release)
	server := slotServer(t, runner, 4)
	running := generate(t, server, "7")
	waitStatus(t, server, "Wikis", "generate_wiki", running, "InProgress")
	deletion, endDeletion := context.WithCancel(context.Background())
	defer endDeletion()
	stopped := make(chan int, 1)
	go func() {
		stopped <- server.Manager().StopMatching(deletion, spi.StopFence{Tool: "generate_wiki", Labels: map[string]string{"project": "7"}}, 10*time.Second)
	}()
	// The run sees its stop: StopMatching has read the runs and waits.
	select {
	case id := <-runner.stopSeen:
		if id != running {
			t.Fatalf("the stop reached %s, want %s", id, running)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never saw its stop")
	}
	assertFenced(t, pollOnce(t, server, "Wikis", "generate_wiki", generate(t, server, "7")))
	other := generate(t, server, "8")
	waitStatus(t, server, "Wikis", "generate_wiki", other, "InProgress")
	close(runner.yield)
	if remaining := <-stopped; remaining != 0 {
		t.Fatalf("StopMatching left %d running", remaining)
	}
	endDeletion()
	waitFor(t, "the local fence to be removed", func() bool {
		id := generate(t, server, "7")
		body := pollOnce(t, server, "Wikis", "generate_wiki", id)
		return body["status"] != "Error"
	})
}

// legacyRow inserts the unfinished row of a replica from before the fleet:
// no labels, no slot flag, and an owner that never registered.
func legacyRow(t *testing.T, assertions *pgxpool.Pool, id, owner, tool string) {
	t.Helper()
	if _, err := assertions.Exec(context.Background(),
		`INSERT INTO invocations (invocation_id, toolkit_name, tool_name, status, owner) VALUES ($1, 'Wikis', $2, 'running', $3)`,
		id, tool, owner); err != nil {
		t.Fatal(err)
	}
}

// Finding 5, against PostgreSQL: in a rolling upgrade, a replica from
// before the fleet writes no slot flag and does not register. Its running
// generation counts as slot load (it adds no capacity), a deletion's stop
// matches its row though it has no labels, and when that replica is gone
// (its liveness lock free for the owner grace) reconcile ends the row, so
// it stops counting. A row from before #1359 (no claim prefix) never
// counts: only a process start ends it.
func TestALegacyReplicasGenerationTakesASlotAndIsStopped(t *testing.T) {
	store, assertions := testStore(t, uniqueOwner("spi-mixed"), spi.WithReplicaSlots(1), spi.WithOwnerGrace(100*time.Millisecond))
	runner := slotRunner{release: make(chan struct{})}
	close(runner.release)
	server := slotServer(t, runner, 1, spi.WithStore(store))
	ctx := context.Background()
	const oldOwner = "claim1:old-image/1"
	legacyRow(t, assertions, "invocation_old_generate", oldOwner, "generate_wiki")
	legacyRow(t, assertions, "invocation_old_ask", oldOwner, "ask")
	legacyRow(t, assertions, "invocation_older_generate", "pod-older/1", "generate_wiki")

	if capacity, err := store.FleetSlots(ctx); err != nil || capacity.Total != 1 || capacity.Active != 1 {
		t.Fatalf("fleet %+v %v, want 1 slot with the legacy generation as its load", capacity, err)
	}
	refused := accept(t, server, "Wikis", "generate_wiki")
	assertNoSlot(t, pollOnce(t, server, "Wikis", "generate_wiki", refused), 1)

	// The deletion stops the legacy generation by its tool (it has no
	// labels) and not the legacy question. Nobody runs it here, so it does
	// not end, and the deletion is refused (the safe side).
	deletion, endDeletion := context.WithCancel(ctx)
	defer endDeletion()
	if remaining := server.Manager().StopMatching(deletion, spi.StopFence{Tool: "generate_wiki", Labels: map[string]string{"project": "7", "wiki": "w"}}, 200*time.Millisecond); remaining == 0 {
		t.Fatal("the deletion did not wait for the legacy generation")
	}
	var generateStop, askStop bool
	if err := assertions.QueryRow(ctx, `SELECT
		(SELECT stop_requested FROM invocations WHERE invocation_id = 'invocation_old_generate'),
		(SELECT stop_requested FROM invocations WHERE invocation_id = 'invocation_old_ask')`).Scan(&generateStop, &askStop); err != nil {
		t.Fatal(err)
	}
	if !generateStop || askStop {
		t.Fatalf("stop requested: generate %v, ask %v; want only the generation", generateStop, askStop)
	}

	// The legacy owner is gone (nothing holds its lock): the periodic pass
	// ends its rows after the owner grace, and the slot is free again.
	if n, err := store.ReconcilePeriodic(ctx); err != nil || n != 0 {
		t.Fatalf("first sighting reconciled %d %v", n, err)
	}
	time.Sleep(200 * time.Millisecond)
	if n, err := store.ReconcilePeriodic(ctx); err != nil || n != 2 {
		t.Fatalf("reconciled %d %v, want the legacy owner's 2 rows", n, err)
	}
	if capacity, err := store.FleetSlots(ctx); err != nil || capacity.Active != 0 {
		t.Fatalf("fleet %+v %v, want no load after reconcile", capacity, err)
	}
	if body := pollTerminal(t, server, "Wikis", "generate_wiki", accept(t, server, "Wikis", "generate_wiki")); body["status"] != "Completed" {
		t.Fatalf("the freed slot was not taken: %v", body)
	}
}

// Finding 6, against PostgreSQL: a replica that loses its owner claim and
// cannot re-take it keeps its row live (the heartbeat does not wait for
// the claim check) but stops admitting; its slots AND its running
// generation leave the fleet's numbers together, so the healthy replica
// still admits. A registered replica whose row is not live (no heartbeat
// for longer than the live window) is out of capacity and load alike.
func TestCapacityAndLoadMoveTogetherWhenAReplicaIsNotLive(t *testing.T) {
	const check = 100 * time.Millisecond
	storeA, assertions := testStore(t, uniqueOwner("spi-live-a"), spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(check))
	storeB := secondStore(t, storeA, uniqueOwner("spi-live-b"), spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(check))
	runner := slotRunner{release: make(chan struct{})}
	defer close(runner.release)
	a := slotServer(t, runner, 1, spi.WithStore(storeA))
	b := slotServer(t, runner, 1, spi.WithStore(storeB))
	ctx := context.Background()

	onA := accept(t, a, "Wikis", "generate_wiki")
	waitStatus(t, a, "Wikis", "generate_wiki", onA, "InProgress")

	// A registered replica that stopped its heartbeat long ago, with a
	// running generation: neither its slots nor its run count.
	if _, err := assertions.Exec(ctx, `INSERT INTO host_replicas (owner, slots, admitting, seen_at) VALUES ('claim1:silent/1', 3, true, now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if _, err := assertions.Exec(ctx, `INSERT INTO invocations (invocation_id, toolkit_name, tool_name, status, owner, holds_slot) VALUES ('invocation_silent', 'Wikis', 'generate_wiki', 'running', 'claim1:silent/1', true)`); err != nil {
		t.Fatal(err)
	}
	if capacity, err := storeB.FleetSlots(ctx); err != nil || capacity.Total != 2 || capacity.Active != 1 {
		t.Fatalf("fleet %+v %v, want 2 slots and A's one run", capacity, err)
	}

	// A's claim session drops and another session holds the lock, so every
	// claim check of A fails.
	thief, err := assertions.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer thief.Release()
	var key int64
	if err := thief.QueryRow(ctx, `SELECT hashtextextended($1, $2)`, storeA.Owner(), 0x53_41_48_31).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := thief.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		  AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid`, key); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the thief to take the freed lock", func() bool {
		var took bool
		_ = thief.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&took)
		return took
	})
	waitFor(t, "A to notice its lost claim", func() bool { return !storeA.ClaimHeld() })
	// Several claim checks fail; the heartbeat keeps A's row live, and
	// says it does not admit.
	time.Sleep(5 * check)
	var live, admitting bool
	if err := assertions.QueryRow(ctx, `SELECT seen_at > now() - interval '1 second', admitting FROM host_replicas WHERE owner = $1`, storeA.Owner()).Scan(&live, &admitting); err != nil {
		t.Fatal(err)
	}
	if !live || admitting {
		t.Fatalf("A's row: live %v, admitting %v; want live and not admitting", live, admitting)
	}
	if capacity, err := storeB.FleetSlots(ctx); err != nil || capacity.Total != 1 || capacity.Active != 0 {
		t.Fatalf("fleet %+v %v, want B's slot only and no load", capacity, err)
	}
	onB := accept(t, b, "Wikis", "generate_wiki")
	waitStatus(t, b, "Wikis", "generate_wiki", onB, "InProgress")

	// A re-takes its claim: its slot and its run come back together.
	if _, err := thief.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "A to re-take its claim", storeA.ClaimHeld)
	waitFor(t, "A to admit again", func() bool {
		capacity, err := storeB.FleetSlots(ctx)
		return err == nil && capacity.Total == 2 && capacity.Active == 2
	})
}
