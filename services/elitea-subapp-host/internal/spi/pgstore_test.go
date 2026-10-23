package spi_test

// The durable store against a real PostgreSQL, in a schema of its own that
// the DeepWiki engine's migration creates — the SAME migration production
// applies (services/elitea-deepwiki-engine/migrations/0002_invocations.sql).
// Gated on ELITEA_SUBAPP_HOST_TEST_DSN: without it the suite reports the
// skip loudly rather than passing a store it never touched.

import (
	"context"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/testpg"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi/spitest"
)

// migrations are the DeepWiki engine's migrations that create and extend the
// invocation tables, in order: 0002 creates them, 0009 adds the fleet
// (labels, holds_slot, host_replicas), 0010 the stop fences.
var migrations = []string{
	"../../../elitea-deepwiki-engine/migrations/0002_invocations.sql",
	"../../../elitea-deepwiki-engine/migrations/0009_host_fleet.sql",
	"../../../elitea-deepwiki-engine/migrations/0010_host_stop_fences.sql",
}

// testStore creates a throwaway schema, applies the migration into it, and
// returns a store bound to that schema plus a second connection for
// assertions. The schema is dropped at cleanup.
func testStore(t *testing.T, owner string, options ...spi.StoreOption) (*spi.PostgresStore, *pgxpool.Pool) {
	t.Helper()
	dsn := testpg.DSN(os.Getenv("ELITEA_SUBAPP_HOST_TEST_DSN"))
	if dsn == "" {
		t.Skip("ELITEA_SUBAPP_HOST_TEST_DSN is not set — the PostgreSQL store did NOT run against a database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("subapp_host_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	scoped := dsn
	if filepath.Ext(scoped) == "" { // a URL: append the search_path option
		sep := "?"
		for _, c := range scoped {
			if c == '?' {
				sep = "&"
			}
		}
		scoped = scoped + sep + "search_path=" + schema
	}
	pool, err := pgxpool.New(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		sql, err := os.ReadFile(filepath.Clean(migration))
		if err != nil {
			t.Fatalf("the engine's migration is the schema of record: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply the migration %s: %v", migration, err)
		}
	}
	pool.Close()
	store, err := spi.NewPostgresStore(ctx, scoped, owner, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	assertions, err := pgxpool.New(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(assertions.Close)
	return store, assertions
}

func TestPostgresStoreRoundTripsAnInvocationAndDrainsEventsOnce(t *testing.T) {
	store, _ := testStore(t, "owner-a")
	ctx := context.Background()
	if !store.Durable() {
		t.Fatal("the store does not claim durability")
	}
	invocation := &spi.Invocation{ID: "invocation_pg1", Toolkit: "Wikis", Tool: "generate_wiki", Status: "pending", CreatedAt: time.Now()}
	if err := store.Create(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "Wikis", "generate_wiki", "invocation_pg1")
	if err != nil || got == nil || got.Status != "pending" || got.StopRequest || got.Result != nil || !got.FinishedAt.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	if missing, err := store.Get(ctx, "Wikis", "ask", "invocation_pg1"); err != nil || missing != nil {
		t.Fatalf("a row under another tool was returned: %+v %v", missing, err)
	}
	for _, m := range []string{"Cloning", "Indexing", "Writing"} {
		if err := store.AppendEvent(ctx, invocation.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.DrainEvents(ctx, invocation)
	if err != nil || len(events) != 3 || events[0]["data"].(map[string]any)["message"] != "Cloning" || events[2]["data"].(map[string]any)["message"] != "Writing" {
		t.Fatalf("events %v %v", events, err)
	}
	// Read once: a second drain is empty.
	if again, err := store.DrainEvents(ctx, invocation); err != nil || len(again) != 0 {
		t.Fatalf("a second drain returned %v %v", again, err)
	}
	invocation.Status = "stopped"
	invocation.Result = map[string]any{"invocation_id": invocation.ID, "status": "Completed", "result": "[]", "result_type": "String"}
	if err := store.Update(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(ctx, "Wikis", "generate_wiki", invocation.ID)
	if got.Status != "stopped" || got.Result["status"] != "Completed" || got.FinishedAt.IsZero() {
		t.Fatalf("terminal row %+v", got)
	}
	// Nothing older than an hour: no prune; everything: pruned, events with it.
	if n, err := store.Prune(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("prune %d %v", n, err)
	}
	if n, err := store.Prune(ctx, 0); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if got, _ := store.Get(ctx, "Wikis", "generate_wiki", invocation.ID); got != nil {
		t.Fatal("a pruned row is still readable")
	}
}

func TestPostgresStoreReconcilesAnotherOwnersOrphans(t *testing.T) {
	old, _ := testStore(t, "pod-old/1")
	ctx := context.Background()
	running := &spi.Invocation{ID: "invocation_orphan", Toolkit: "Wikis", Tool: "generate_wiki", Status: "running", CreatedAt: time.Now()}
	if err := old.Create(ctx, running); err != nil {
		t.Fatal(err)
	}
	mine := &spi.Invocation{ID: "invocation_mine", Toolkit: "Wikis", Tool: "ask", Status: "running", CreatedAt: time.Now()}
	// The new owner shares the schema: build it on the same DSN.
	fresh, err := spi.NewPostgresStore(ctx, dsnOf(t, old), "pod-new/1", nil, spi.WithOwnerGrace(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	// The old owner is ALIVE (its session holds the owner lock): another
	// replica's running row is not an orphan, and reconcile leaves it be.
	if n, err := fresh.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a live sibling's row was reconciled: %d %v", n, err)
	}
	if live, _ := fresh.Get(ctx, "Wikis", "generate_wiki", "invocation_orphan"); live.Status != "running" {
		t.Fatalf("a live sibling's row was touched: %+v", live)
	}
	// A second process cannot claim a live owner id.
	if _, err := spi.NewPostgresStore(ctx, dsnOf(t, old), "pod-old/1", nil); err == nil {
		t.Fatal("a live owner id was claimed twice")
	}
	// The old owner dies: its lock is free. One sighting is not death — an
	// owner whose session dropped is re-taking it — but a lock still free
	// the owner grace later is: now its running row IS an orphan.
	old.Close()
	if n, err := fresh.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a free lock seen once was taken for death: %d %v", n, err)
	}
	// Closing the pool does not end the old backend at once: the server may
	// still hold its session lock for a moment, so the free sighting the
	// grace counts from can come one pass later. Wait for it, bounded, and
	// never accept more than the one orphan.
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(350 * time.Millisecond)
		n, err := fresh.Reconcile(ctx)
		if err != nil || n > 1 {
			t.Fatalf("reconciled %d %v", n, err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a dead owner's orphan was never reconciled")
		}
	}
	orphan, _ := fresh.Get(ctx, "Wikis", "generate_wiki", "invocation_orphan")
	if orphan.Status != "stopped" || orphan.Result["status"] != "Error" || orphan.Result["error_category"] != "runtime_error" || orphan.FinishedAt.IsZero() {
		t.Fatalf("orphan %+v", orphan)
	}
	if own, _ := fresh.Get(ctx, "Wikis", "ask", "invocation_mine"); own.Status != "running" {
		t.Fatalf("my own row was reconciled: %+v", own)
	}
	// Idempotent: a second pass finds nothing.
	if n, _ := fresh.Reconcile(ctx); n != 0 {
		t.Fatalf("a second reconcile touched %d rows", n)
	}
}

// The claim's session drops (a PostgreSQL restart, a failover, an idle
// kill): the lock is free and siblings would take this process for dead. It
// must notice, refuse new work while it cannot re-take the claim, and
// resume once it has.
func TestPostgresStoreRefusesWorkWhileItsClaimIsLostAndResumesOnceRetaken(t *testing.T) {
	store, assertions := testStore(t, "pod-flaky/1", spi.WithClaimCheckInterval(200*time.Millisecond))
	ctx := context.Background()
	if !store.ClaimHeld() {
		t.Fatal("a new store does not hold its claim")
	}
	// Another session: it kills the claim's session and takes the lock
	// before the store's next check can, standing in for whatever might
	// hold it in the gap.
	thief, err := assertions.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer thief.Release()
	var key int64
	if err := thief.QueryRow(ctx, `SELECT hashtextextended($1, $2)`, store.Owner(), 0x53_41_48_31).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := thief.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		  AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid`, key); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the thief to take the freed lock", func() bool {
		var took bool
		_ = thief.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&took)
		return took
	})
	waitFor(t, "the store to notice its lost claim", func() bool { return !store.ClaimHeld() })
	refused := &spi.Invocation{ID: "invocation_refused", Toolkit: "Wikis", Tool: "ask", Status: "pending", CreatedAt: time.Now()}
	if err := store.Create(ctx, refused); !errors.Is(err, spi.ErrClaimLost) {
		t.Fatalf("Create without the claim = %v, want ErrClaimLost", err)
	}
	if _, err := thief.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the store to re-take its claim", store.ClaimHeld)
	if err := store.Create(ctx, refused); err != nil {
		t.Fatalf("Create after the claim was re-taken: %v", err)
	}
	// Alive again: a sibling leaves its row alone.
	sibling, err := spi.NewPostgresStore(ctx, dsnOf(t, store), "pod-sibling/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	if n, err := sibling.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a sibling reconciled a live owner's row: %d %v", n, err)
	}
}

// An owner whose claim session drops re-takes the lock within a check or
// two. A sibling that sees the lock free in that gap waits the owner grace
// before it believes it, and a held sighting resets the wait, so the owner
// keeps its runs; only a lock free for the whole grace is death.
func TestPostgresStoreLeavesABrieflyDisconnectedOwnersRunsAlone(t *testing.T) {
	owner, assertions := testStore(t, "pod-blip/1", spi.WithClaimCheckInterval(100*time.Millisecond))
	ctx := context.Background()
	running := &spi.Invocation{ID: "invocation_blip", Toolkit: "Wikis", Tool: "generate_wiki", Status: "running", CreatedAt: time.Now()}
	if err := owner.Create(ctx, running); err != nil {
		t.Fatal(err)
	}
	const grace = 600 * time.Millisecond
	sibling, err := spi.NewPostgresStore(ctx, dsnOf(t, owner), "pod-sibling/1", nil, spi.WithOwnerGrace(grace))
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	var key int64
	if err := assertions.QueryRow(ctx, `SELECT hashtextextended($1, $2)`, owner.Owner(), 0x53_41_48_31).Scan(&key); err != nil {
		t.Fatal(err)
	}
	// The owner's claim session drops; the sibling sees the lock free before
	// the owner's next check re-takes it.
	if _, err := assertions.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		  AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid`, key); err != nil {
		t.Fatal(err)
	}
	lockFree := func() bool {
		var held bool
		_ = assertions.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 1
			AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid)`, key).Scan(&held)
		return !held
	}
	waitFor(t, "the lock to be free", lockFree)
	if n, err := sibling.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a sibling reaped on one free sighting: %d %v", n, err)
	}
	waitFor(t, "the owner to re-take its claim", owner.ClaimHeld)
	time.Sleep(grace + 100*time.Millisecond)
	if n, err := sibling.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a sibling reaped an owner that re-took its claim: %d %v", n, err)
	}
	if got, _ := owner.Get(ctx, "Wikis", "generate_wiki", running.ID); got.Status != "running" {
		t.Fatalf("the re-taken owner's run was touched: %+v", got)
	}
	// The held sighting reset the wait: the owner now dies for good, and the
	// first free sighting after it starts a new grace rather than reaping at
	// once on the stale one.
	owner.Close()
	// Close returns before the server has ended the claim session, so the
	// lock can still read as held for a moment. A Reconcile in that window
	// sees it held, which resets the wait instead of starting the grace, and
	// the reap below then came one grace too early (reconciled 0, want 1).
	// Wait for the server to free it, so this Reconcile is the first free
	// sighting the comment above describes.
	waitFor(t, "the closed owner's lock to be free", lockFree)
	if n, err := sibling.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a sibling reaped on a free sighting that predates a held one: %d %v", n, err)
	}
	time.Sleep(grace + 100*time.Millisecond)
	if n, err := sibling.Reconcile(ctx); err != nil || n != 1 {
		t.Fatalf("an owner gone for the whole grace: reconciled %d %v, want 1", n, err)
	}
}

// A row a sibling terminated as an orphan keeps that answer: the original
// owner, still running it, writes nothing over it.
func TestPostgresStoreUpdateLeavesARowAnotherOwnerTerminated(t *testing.T) {
	store, assertions := testStore(t, "pod-slow/1")
	ctx := context.Background()
	invocation := &spi.Invocation{ID: "invocation_reaped", Toolkit: "Wikis", Tool: "ask", Status: "running", CreatedAt: time.Now()}
	if err := store.Create(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := assertions.Exec(ctx, `UPDATE invocations SET status = 'stopped', result = '{"status":"Error"}', finished_at = now(), owner = 'claim1:pod-reaper/1' WHERE invocation_id = $1`, invocation.ID); err != nil {
		t.Fatal(err)
	}
	invocation.Status = "stopped"
	invocation.Result = map[string]any{"status": "Completed"}
	if err := store.Update(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, "Wikis", "ask", invocation.ID); got.Result["status"] != "Error" {
		t.Fatalf("the original owner overwrote the reaped answer: %+v", got)
	}
}

// A row from an image before #1359 names an owner that takes no lock, so a
// free lock says nothing about it, and nothing else tells whether it is
// alive. It keeps the rule the old image applied itself: a process STARTING
// terminates it (its startup reconcile), the periodic pass never does, however
// long it has been in flight.
func TestPostgresStoreEndsALegacyOwnersRowsAtStartupOnly(t *testing.T) {
	store, assertions := testStore(t, "pod-new/1")
	ctx := context.Background()
	if _, err := assertions.Exec(ctx, `INSERT INTO invocations (invocation_id, toolkit_name, tool_name, status, owner, created_at) VALUES ('invocation_legacy', 'Wikis', 'generate_wiki', 'running', 'pod-old/7', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if n, err := store.ReconcilePeriodic(ctx); err != nil || n != 0 {
			t.Fatalf("the periodic pass reconciled a legacy owner's row: %d %v", n, err)
		}
	}
	if live, _ := store.Get(ctx, "Wikis", "generate_wiki", "invocation_legacy"); live.Status != "running" {
		t.Fatalf("a legacy owner's row was touched by the periodic pass: %+v", live)
	}
	// A process starts: its startup pass ends the row at once.
	restarted, err := spi.NewPostgresStore(ctx, dsnOf(t, store), "pod-new/2", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if n, err := restarted.Reconcile(ctx); err != nil || n != 1 {
		t.Fatalf("the startup pass: reconciled %d %v, want the legacy row", n, err)
	}
	if orphan, _ := store.Get(ctx, "Wikis", "generate_wiki", "invocation_legacy"); orphan.Status != "stopped" || orphan.Result["status"] != "Error" {
		t.Fatalf("legacy orphan %+v", orphan)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The SPI over the durable store: an invoke is polled to completion with
// its events, a stop lands on the running call, and the terminal body is
// the stored one — through the same routes the facade uses.
func TestTheServerServesTheSPIOverTheDurableStore(t *testing.T) {
	store, _ := testStore(t, "owner-spi")
	settings, _ := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{"ELITEA_ECHO_MAX_PARALLEL_WORKERS": "2"}))
	server, err := spi.NewServer(settings, echo.App(30*time.Millisecond), nil, spi.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	wire := spitest.Handler(t, server)

	if _, health := do(wire, http.MethodGet, "/health", nil); health["extra_info"].(map[string]any)["durable_invocations"] != true {
		t.Fatalf("health does not report durability: %v", health)
	}
	recorder, accepted := do(wire, http.MethodPost, "/tools/Echo/echo/invoke", []byte(`{"parameters":{"query":"hi"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("invoke %d %s", recorder.Code, recorder.Body.String())
	}
	id := accepted["invocation_id"].(string)
	body := pollUntilTerminal(t, wire, "/tools/Echo/echo/invocations/"+id)
	if body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	// The terminal body is the stored one: a fresh store over the same rows
	// answers the same.
	again, err := spi.NewPostgresStore(context.Background(), dsnOf(t, store), "owner-other", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	row, _ := again.Get(context.Background(), "Echo", "echo", id)
	if row == nil || row.Result["status"] != "Completed" {
		t.Fatalf("stored %+v", row)
	}

	// A stop lands on the running call through the live struct, and is
	// persisted.
	recorder, accepted = do(wire, http.MethodPost, "/tools/Echo/echo/invoke", []byte(`{"parameters":{"query":"slow"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
	id = accepted["invocation_id"].(string)
	time.Sleep(20 * time.Millisecond)
	if recorder, _ := do(wire, http.MethodDelete, "/tools/Echo/echo/invocations/"+id, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("cancel %d", recorder.Code)
	}
	body = pollUntilTerminal(t, wire, "/tools/Echo/echo/invocations/"+id)
	if body["status"] != "Error" || body["error_category"] != "runtime_error" {
		t.Fatalf("a stopped run ended as %v", body)
	}
	row, _ = again.Get(context.Background(), "Echo", "echo", id)
	if !row.StopRequest || row.Status != "stopped" {
		t.Fatalf("the stop was not persisted: %+v", row)
	}
}

// dsnOf recovers the schema-scoped DSN a store was built with, for a second
// owner over the same rows.
func dsnOf(t *testing.T, store *spi.PostgresStore) string {
	t.Helper()
	return store.DSN()
}

// <PREFIX>DATABASE_MAX_CONNS reaches the pool: a replica's share of the
// server's connections is that number plus the owner claim's one.
func TestPostgresStoreSizesItsPoolFromTheSetting(t *testing.T) {
	store, _ := testStore(t, "pod-sized/1", spi.WithMaxConns(3))
	if got := store.PoolMaxConns(); got != 3 {
		t.Fatalf("pool MaxConns = %d, want 3", got)
	}
}
