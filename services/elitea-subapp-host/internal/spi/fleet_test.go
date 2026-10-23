package spi_test

// Slot holders (spi.SlotHolder) and the fleet (#scaling): admission against
// the slots, the local gate, the slots report, and — against a real
// PostgreSQL (ELITEA_SUBAPP_HOST_TEST_DSN) — fleet admission, the replica
// registry and the fleet-wide listing a deletion matches.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// slotRunner is a DeepWiki runner whose generate_wiki holds a slot and runs
// until release is closed; every other tool completes at once.
type slotRunner struct {
	release chan struct{}
}

func (slotRunner) Name() string { return "slots" }

func (r slotRunner) HoldsSlot(call spi.Invoke) bool {
	return call.Tool == "generate_wiki" && call.Family.Name != "query"
}

func (r slotRunner) Invoke(ctx context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error) {
	if call.Tool != "generate_wiki" {
		return spi.Completed(tc.InvocationID()), nil
	}
	for {
		if err := tc.Checkpoint(); err != nil {
			return nil, err
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

var _ spi.SlotHolder = slotRunner{}

// slotServer is a started DeepWiki server over runner with `slots` slots.
func slotServer(t *testing.T, runner spi.Runner, slots int, options ...spi.Option) *spi.Server {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": fmt.Sprint(slots)}))
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, deepwiki.App(runner), nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	return server
}

func accept(t *testing.T, server *spi.Server, toolkit, tool string) string {
	t.Helper()
	invocation, err := server.Accept(context.Background(), spi.Identity{ProjectID: "1"}, toolkit, tool, map[string]any{})
	if err != nil {
		t.Fatalf("accept %s/%s: %v", toolkit, tool, err)
	}
	return invocation.ID
}

func pollOnce(t *testing.T, server *spi.Server, toolkit, tool, id string) map[string]any {
	t.Helper()
	body, err := server.Manager().Poll(context.Background(), toolkit, tool, id)
	if err != nil || body == nil {
		t.Fatalf("poll %s: %v %v", id, body, err)
	}
	return body
}

func pollTerminal(t *testing.T, server *spi.Server, toolkit, tool, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body := pollOnce(t, server, toolkit, tool, id); spi.TerminalBody(body) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never ended", id)
	return nil
}

func waitStatus(t *testing.T, server *spi.Server, toolkit, tool, id, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body := pollOnce(t, server, toolkit, tool, id); body["status"] == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s", id, status)
}

func messageOf(t *testing.T, body map[string]any) string {
	t.Helper()
	var objects []map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprint(body["result"])), &objects); err != nil || len(objects) == 0 {
		t.Fatalf("result %v: %v", body["result"], err)
	}
	return fmt.Sprint(objects[0]["data"])
}

// assertNoSlot checks the refusal body of a slot holder that found every
// slot busy.
func assertNoSlot(t *testing.T, body map[string]any, total int) {
	t.Helper()
	want := fmt.Sprintf("All %d generation slots are busy", total)
	if body["status"] != "Error" || body["error_category"] != spi.CategoryServiceBusy || !strings.Contains(messageOf(t, body), want) {
		t.Fatalf("not the no-slot refusal (%q): %v", want, body)
	}
}

// With the in-memory store a replica admits slot holders up to its own
// slots. One more is still accepted with an id (the SPI is asynchronous) and
// ends at once as service_busy; a call that holds no slot runs all the same.
// The slots report counts the slot holders only.
func TestTheMemoryStoreAdmitsSlotHoldersUpToItsSlots(t *testing.T) {
	runner := slotRunner{release: make(chan struct{})}
	server := slotServer(t, runner, 1)

	first := accept(t, server, "Wikis", "generate_wiki")
	waitStatus(t, server, "Wikis", "generate_wiki", first, "InProgress")
	refused := accept(t, server, "Wikis", "generate_wiki")
	body := pollOnce(t, server, "Wikis", "generate_wiki", refused)
	assertNoSlot(t, body, 1)
	if body[spi.BusyReasonKey] != spi.RefusalLocalFull {
		t.Fatalf("the refusal does not say its reason: %v", body)
	}
	// The refusal is on /metrics with its reason. Without a durable store
	// there is one replica, so it is local_full and not fleet demand.
	if metrics := server.MetricsText(); !strings.Contains(metrics, "# TYPE "+spi.MetricSlotRefusals+" counter") ||
		!regexp.MustCompile(`(?m)^`+spi.MetricSlotRefusals+`\{app="[^"]+",reason="local_full"\} 1$`).MatchString(metrics) ||
		!regexp.MustCompile(`(?m)^`+spi.MetricSlotRefusals+`\{app="[^"]+",reason="fleet_full"\} 0$`).MatchString(metrics) ||
		!regexp.MustCompile(`(?m)^`+spi.MetricSlotRefusals+`\{app="[^"]+",reason="draining"\} 0$`).MatchString(metrics) {
		t.Fatalf("the refusal is not counted on /metrics by reason:\n%s", metrics)
	}

	question := accept(t, server, "Wikis", "ask")
	if body := pollTerminal(t, server, "Wikis", "ask", question); body["status"] != "Completed" {
		t.Fatalf("a call that holds no slot was refused: %v", body)
	}
	slots := server.SlotsDocument(context.Background())
	if slots["active"] != 1 || slots["total"] != 1 || slots["available"] != 0 || slots["can_start"] != false ||
		slots["canStart"] != false || slots["mode"] != "subprocess" {
		t.Fatalf("slots %v", slots)
	}

	close(runner.release)
	if body := pollTerminal(t, server, "Wikis", "generate_wiki", first); body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	if slots := server.SlotsDocument(context.Background()); slots["active"] != 0 || slots["can_start"] != true {
		t.Fatalf("slots after the run %v", slots)
	}
	again := accept(t, server, "Wikis", "generate_wiki")
	if body := pollTerminal(t, server, "Wikis", "generate_wiki", again); body["status"] != "Completed" {
		t.Fatalf("a freed slot was not taken: %v", body)
	}
}

// admitAll is a store that admits every slot holder in Create, as a fleet
// with free slots does: what is left is the LOCAL admission.
type admitAll struct{ *spi.MemoryStore }

func (admitAll) FleetSlots(context.Context) (spi.FleetCapacity, error) {
	return spi.FleetCapacity{Total: 10, Replicas: 10}, nil
}

// A replica whose own slots are busy does NOT refuse a slot holder while
// the fleet admits it (finding 1 of the #scaling review): /slots reports
// the fleet, so a call that lands on a busy replica while a sibling is idle
// must run, or can_start would lie and the refusal would count as demand.
// The replica runs above its own slot count; the fleet is the bound.
func TestAFullReplicaAdmitsWhileTheFleetHasSlots(t *testing.T) {
	runner := slotRunner{release: make(chan struct{})}
	server := slotServer(t, runner, 1, spi.WithStore(admitAll{spi.NewMemoryStore()}))

	first := accept(t, server, "Wikis", "generate_wiki")
	waitStatus(t, server, "Wikis", "generate_wiki", first, "InProgress")
	second := accept(t, server, "Wikis", "generate_wiki")
	waitStatus(t, server, "Wikis", "generate_wiki", second, "InProgress")
	if got := server.Manager().Refusals(); got != 0 {
		t.Fatalf("refusals = %d, want 0", got)
	}
	if got := server.Manager().LocalHolders(); got != 2 {
		t.Fatalf("local holders = %d, want 2 (above the replica's 1 slot)", got)
	}
	close(runner.release)
	for _, id := range []string{first, second} {
		if body := pollTerminal(t, server, "Wikis", "generate_wiki", id); body["status"] != "Completed" {
			t.Fatalf("%v", body)
		}
	}
	if got := server.Manager().LocalHolders(); got != 0 {
		t.Fatalf("local holders = %d after the runs, want 0", got)
	}
}

// A replica that drains (SetAdmitting(false), what main does at SIGTERM)
// refuses every new slot holder at once, though it has a free slot; a call
// that holds no slot still runs.
func TestADrainingReplicaRefusesNewSlotHolders(t *testing.T) {
	runner := slotRunner{release: make(chan struct{})}
	close(runner.release)
	server := slotServer(t, runner, 2, spi.WithStore(admitAll{spi.NewMemoryStore()}))
	server.Manager().SetAdmitting(false)

	refused := accept(t, server, "Wikis", "generate_wiki")
	body := pollOnce(t, server, "Wikis", "generate_wiki", refused)
	if body["status"] != "Error" || body["error_category"] != spi.CategoryServiceBusy ||
		!strings.Contains(messageOf(t, body), "[SERVICE_BUSY] This replica is shutting down") {
		t.Fatalf("a draining replica admitted a slot holder: %v", body)
	}
	if body[spi.BusyReasonKey] != spi.RefusalDraining || server.Manager().RefusalsFor(spi.RefusalDraining) != 1 ||
		server.Manager().RefusalsFor(spi.RefusalFleetFull) != 0 {
		t.Fatalf("the draining refusal is not counted as draining: %v", body)
	}
	if got := server.Manager().LocalHolders(); got != 0 {
		t.Fatalf("local holders = %d, want 0", got)
	}
	question := accept(t, server, "Wikis", "ask")
	if body := pollTerminal(t, server, "Wikis", "ask", question); body["status"] != "Completed" {
		t.Fatalf("a call that holds no slot was refused: %v", body)
	}
}

// A runner with no slot holders keeps the per-pod report of every
// invocation in flight, and no limit: the conformance fixtures pin that.
func TestARunnerWithoutSlotHoldersHasNoSlotLimit(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := engine{name: "slow", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		<-release
		return spi.Completed(tc.InvocationID()), nil
	}}
	server := slotServer(t, slow, 1)
	ids := []string{accept(t, server, "Wikis", "generate_wiki"), accept(t, server, "Wikis", "generate_wiki")}
	for _, id := range ids {
		waitStatus(t, server, "Wikis", "generate_wiki", id, "InProgress")
	}
	if slots := server.SlotsDocument(context.Background()); slots["active"] != 2 || slots["note"] != "Per-pod availability only (subprocess mode)" {
		t.Fatalf("slots %v", slots)
	}
}

// Against PostgreSQL: fleet admission counts the slots of EVERY live replica
// that admits work, and the slots report on any replica says the same.
// SetAdmitting(false) and Close take a replica's slots out at once.
func TestFleetAdmissionCountsTheSlotsOfEveryLiveReplica(t *testing.T) {
	storeA, _ := testStore(t, "spi-fleet-a/1", spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(200*time.Millisecond))
	storeB, err := spi.NewPostgresStore(context.Background(), dsnOf(t, storeA), "spi-fleet-b/1", nil,
		spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storeB.Close)
	runner := slotRunner{release: make(chan struct{})}
	// A has a second LOCAL slot that it does not register in the fleet, so
	// the third call below passes A's own admission and meets the fleet's.
	a := slotServer(t, runner, 2, spi.WithStore(storeA))
	b := slotServer(t, runner, 1, spi.WithStore(storeB))

	report := func(server *spi.Server) map[string]any { return server.SlotsDocument(context.Background()) }
	if slots := report(a); slots["total"] != 2 || slots["active"] != 0 || slots["can_start"] != true ||
		slots["note"] != "Fleet-wide availability (2 replicas)" || slots["mode"] != "subprocess" {
		t.Fatalf("idle fleet %v", slots)
	}
	onA := accept(t, a, "Wikis", "generate_wiki")
	onB := accept(t, b, "Wikis", "generate_wiki")
	waitStatus(t, a, "Wikis", "generate_wiki", onA, "InProgress")
	waitStatus(t, b, "Wikis", "generate_wiki", onB, "InProgress")
	for _, server := range []*spi.Server{a, b} {
		if slots := report(server); slots["total"] != 2 || slots["active"] != 2 || slots["available"] != 0 || slots["canStart"] != false {
			t.Fatalf("busy fleet %v", slots)
		}
	}
	// The third is refused on A although A has a free local slot: the fleet
	// is full. It is pollable through B.
	refused := accept(t, a, "Wikis", "generate_wiki")
	assertNoSlot(t, pollOnce(t, b, "Wikis", "generate_wiki", refused), 2)
	// A question holds no slot and runs.
	if body := pollTerminal(t, b, "Wikis", "ask", accept(t, a, "Wikis", "ask")); body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}

	close(runner.release)
	pollTerminal(t, b, "Wikis", "generate_wiki", onA)
	pollTerminal(t, a, "Wikis", "generate_wiki", onB)
	if slots := report(b); slots["active"] != 0 || slots["total"] != 2 {
		t.Fatalf("after the runs %v", slots)
	}

	// B drains: its slots leave the fleet at once.
	if err := storeB.SetAdmitting(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if slots := report(a); slots["total"] != 1 || slots["note"] != "Fleet-wide availability (1 replicas)" {
		t.Fatalf("a draining replica's slots are counted: %v", slots)
	}
	// A claim check does not put them back.
	time.Sleep(500 * time.Millisecond)
	if slots := report(a); slots["total"] != 1 {
		t.Fatalf("a claim check re-admitted a draining replica: %v", slots)
	}
	storeB.Close()
	if capacity, err := storeA.FleetSlots(context.Background()); err != nil || capacity.Total != 1 || capacity.Replicas != 1 {
		t.Fatalf("after B closed %+v %v", capacity, err)
	}
}

// Against PostgreSQL: the store refuses a slot holder when the fleet is full
// (ErrNoSlot, inserting nothing), keeps the labels a deletion matches on
// every replica, and refuses a schema name that is not an identifier.
func TestPostgresStoreAdmitsListsAndQualifies(t *testing.T) {
	store, assertions := testStore(t, "spi-fleet-only/1", spi.WithReplicaSlots(1))
	ctx := context.Background()
	holder := &spi.Invocation{ID: "invocation_h1", Toolkit: "Wikis", Tool: "generate_wiki", Status: "pending",
		HoldsSlot: true, Labels: map[string]string{"project": "7", "wiki": "w1"}}
	if err := store.Create(ctx, holder); err != nil {
		t.Fatal(err)
	}
	second := &spi.Invocation{ID: "invocation_h2", Toolkit: "Wikis", Tool: "generate_wiki", Status: "pending", HoldsSlot: true}
	err := store.Create(ctx, second)
	var noSlot *spi.NoSlotError
	if !errors.Is(err, spi.ErrNoSlot) || !errors.As(err, &noSlot) || noSlot.Total != 1 {
		t.Fatalf("a full fleet admitted a slot holder: %v", err)
	}
	if row, _ := store.Get(ctx, "Wikis", "generate_wiki", second.ID); row != nil {
		t.Fatalf("a refused slot holder was inserted: %+v", row)
	}
	plain := &spi.Invocation{ID: "invocation_p1", Toolkit: "Wikis", Tool: "ask", Status: "pending"}
	if err := store.Create(ctx, plain); err != nil {
		t.Fatalf("a call that holds no slot was refused: %v", err)
	}
	running, err := store.RunningInvocations(ctx)
	if err != nil || len(running) != 2 {
		t.Fatalf("running %v %v", running, err)
	}
	for _, info := range running {
		if info.ID == holder.ID && (info.Labels["project"] != "7" || info.Labels["wiki"] != "w1") {
			t.Fatalf("labels %v", info.Labels)
		}
		if info.ID == plain.ID && len(info.Labels) != 0 {
			t.Fatalf("labels %v", info.Labels)
		}
	}
	var holds bool
	if err := assertions.QueryRow(ctx, `SELECT holds_slot FROM invocations WHERE invocation_id = $1`, holder.ID).Scan(&holds); err != nil || !holds {
		t.Fatalf("holds_slot %v %v", holds, err)
	}
	if _, err := spi.NewPostgresStore(ctx, dsnOf(t, store), "bad-schema/1", nil, spi.WithSchema(`x"; DROP TABLE invocations; --`)); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("a schema that is not an identifier was accepted: %v", err)
	}
}

// Against PostgreSQL, with NO cancel bus: a stop that replica B records on
// the row of a run that replica A owns reaches the run at A's next claim
// check (the store polls its own rows), and a deletion's StopMatching on B
// sees every match end. A stop that arrives twice, or after the end, leaves
// the one terminal state the run recorded.
func TestAStopRecordedByASiblingReachesTheRunWithoutABus(t *testing.T) {
	const check = 100 * time.Millisecond
	storeA, assertions := testStore(t, fmt.Sprintf("spi-poll-a-%d/1", time.Now().UnixNano()), spi.WithClaimCheckInterval(check))
	storeB, err := spi.NewPostgresStore(context.Background(), dsnOf(t, storeA), fmt.Sprintf("spi-poll-b-%d/1", time.Now().UnixNano()), nil,
		spi.WithClaimCheckInterval(check))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storeB.Close)
	untilStopped := engine{name: "until-stopped", invoke: func(ctx context.Context, _ spi.Invoke, tc *spi.Context) (map[string]any, error) {
		for {
			if err := tc.Checkpoint(); err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}}
	a := slotServer(t, untilStopped, 2, spi.WithStore(storeA))
	b := slotServer(t, untilStopped, 2, spi.WithStore(storeB))
	ctx := context.Background()

	// A cancel on B: recorded on the row, and no bus to carry it.
	id := accept(t, a, "Wikis", "ask")
	waitStatus(t, a, "Wikis", "ask", id, "InProgress")
	started := time.Now()
	if known, err := b.Manager().Cancel(ctx, "Wikis", "ask", id); !known || err != nil {
		t.Fatalf("cancel %v %v", known, err)
	}
	body := pollTerminal(t, b, "Wikis", "ask", id)
	if body["status"] != "Error" || !strings.Contains(messageOf(t, body), "Invocation cancelled") {
		t.Fatalf("the polled stop did not end the run: %v", body)
	}
	if took := time.Since(started); took > 10*check {
		t.Fatalf("the stop took %s, more than a few claim checks of %s", took, check)
	}

	// Idempotent: a second cancel through either replica, and further
	// claim checks, change nothing on the terminal row.
	var finished time.Time
	if err := assertions.QueryRow(ctx, `SELECT finished_at FROM invocations WHERE invocation_id = $1`, id).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	for _, server := range []*spi.Server{a, b} {
		if _, err := server.Manager().Cancel(ctx, "Wikis", "ask", id); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * check)
	var again time.Time
	if err := assertions.QueryRow(ctx, `SELECT finished_at FROM invocations WHERE invocation_id = $1`, id).Scan(&again); err != nil || !again.Equal(finished) {
		t.Fatalf("a second stop rewrote the terminal row: %v -> %v (%v)", finished, again, err)
	}
	if after := pollOnce(t, a, "Wikis", "ask", id); after["status"] != body["status"] || after["result"] != body["result"] {
		t.Fatalf("the terminal body changed: %v -> %v", body, after)
	}

	// A deletion's StopMatching on B, still with no bus, sees A's run end.
	matched := accept(t, a, "Wikis", "deep_research")
	other := accept(t, a, "Wikis", "ask")
	waitStatus(t, a, "Wikis", "deep_research", matched, "InProgress")
	waitStatus(t, a, "Wikis", "ask", other, "InProgress")
	remaining := b.Manager().StopMatching(ctx, spi.StopFence{Tool: "deep_research"}, 5*time.Second)
	if remaining != 0 {
		t.Fatalf("StopMatching on B reports %d still running", remaining)
	}
	if body := pollOnce(t, b, "Wikis", "deep_research", matched); body["status"] != "Error" {
		t.Fatalf("the matched run did not end: %v", body)
	}
	if body := pollOnce(t, b, "Wikis", "ask", other); body["status"] != "InProgress" {
		t.Fatalf("a run StopMatching did not select was stopped: %v", body)
	}
}

// unfinishedHolders counts the slot-holding rows that have not ended.
func unfinishedHolders(t *testing.T, assertions *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := assertions.QueryRow(context.Background(),
		`SELECT count(*)::int FROM invocations WHERE holds_slot AND finished_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Against PostgreSQL: a replica that drains takes its slots out of the
// capacity AND its running generation out of the load. With 2 replicas of 1
// slot, A runs one and drains; B still admits one, then refuses the next.
func TestADrainingReplicasRunDoesNotBlockTheRestOfTheFleet(t *testing.T) {
	const check = 200 * time.Millisecond
	storeA, assertions := testStore(t, fmt.Sprintf("spi-drain-a-%d/1", time.Now().UnixNano()), spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(check))
	storeB, err := spi.NewPostgresStore(context.Background(), dsnOf(t, storeA), fmt.Sprintf("spi-drain-b-%d/1", time.Now().UnixNano()), nil,
		spi.WithReplicaSlots(1), spi.WithClaimCheckInterval(check))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storeB.Close)
	runner := slotRunner{release: make(chan struct{})}
	defer close(runner.release)
	a := slotServer(t, runner, 1, spi.WithStore(storeA))
	b := slotServer(t, runner, 1, spi.WithStore(storeB))
	ctx := context.Background()

	onA := accept(t, a, "Wikis", "generate_wiki")
	waitStatus(t, a, "Wikis", "generate_wiki", onA, "InProgress")
	// SIGTERM on A, as main does it: the process and the store.
	a.Manager().SetAdmitting(false)
	if err := storeA.SetAdmitting(ctx, false); err != nil {
		t.Fatal(err)
	}
	if slots := b.SlotsDocument(ctx); slots["total"] != 1 || slots["active"] != 0 || slots["can_start"] != true {
		t.Fatalf("the draining replica's run blocks the fleet: %v", slots)
	}
	onB := accept(t, b, "Wikis", "generate_wiki")
	waitStatus(t, b, "Wikis", "generate_wiki", onB, "InProgress")
	if slots := b.SlotsDocument(ctx); slots["total"] != 1 || slots["active"] != 1 || slots["can_start"] != false {
		t.Fatalf("after B took its slot %v", slots)
	}
	// Claim checks keep both facts: A's row stays live and not admitting.
	time.Sleep(2 * check)
	refused := accept(t, b, "Wikis", "generate_wiki")
	assertNoSlot(t, pollOnce(t, a, "Wikis", "generate_wiki", refused), 1)
	if got := unfinishedHolders(t, assertions); got != 2 {
		t.Fatalf("unfinished slot holders = %d, want 2 (one on each replica)", got)
	}
}

// Against PostgreSQL: a fleet refusal gives the local slot back and leaves
// no unfinished slot-holding row. The replica has 2 local slots but
// registers 1 in the fleet, so the fleet is full first.
func TestAFleetRefusalGivesTheLocalSlotBack(t *testing.T) {
	store, assertions := testStore(t, fmt.Sprintf("spi-give-back-%d/1", time.Now().UnixNano()), spi.WithReplicaSlots(1))
	runner := slotRunner{release: make(chan struct{})}
	server := slotServer(t, runner, 2, spi.WithStore(store))

	first := accept(t, server, "Wikis", "generate_wiki")
	waitStatus(t, server, "Wikis", "generate_wiki", first, "InProgress")
	for range 3 {
		refused := accept(t, server, "Wikis", "generate_wiki")
		assertNoSlot(t, pollOnce(t, server, "Wikis", "generate_wiki", refused), 1)
	}
	if got := server.Manager().LocalHolders(); got != 1 {
		t.Fatalf("local holders = %d after fleet refusals, want 1", got)
	}
	if got := server.Manager().Refusals(); got != 3 {
		t.Fatalf("refusals = %d, want 3", got)
	}
	if got := unfinishedHolders(t, assertions); got != 1 {
		t.Fatalf("a refusal left a slot-holding row unfinished: %d unfinished, want 1", got)
	}
	close(runner.release)
	pollTerminal(t, server, "Wikis", "generate_wiki", first)
	if got := unfinishedHolders(t, assertions); got != 0 {
		t.Fatalf("unfinished slot holders = %d, want 0", got)
	}
}
