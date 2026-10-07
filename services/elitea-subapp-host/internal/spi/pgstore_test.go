package spi_test

// The durable store against a real PostgreSQL, in a schema of its own that
// the DeepWiki engine's migration creates — the SAME migration production
// applies (services/elitea-deepwiki-engine/migrations/0002_invocations.sql).
// Gated on ELITEA_SUBAPP_HOST_TEST_DSN: without it the suite reports the
// skip loudly rather than passing a store it never touched.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const migration = "../../../elitea-deepwiki-engine/migrations/0002_invocations.sql"

// testStore creates a throwaway schema, applies the migration into it, and
// returns a store bound to that schema plus a second connection for
// assertions. The schema is dropped at cleanup.
func testStore(t *testing.T, owner string) (*spi.PostgresStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ELITEA_SUBAPP_HOST_TEST_DSN")
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
	sql, err := os.ReadFile(filepath.Clean(migration))
	if err != nil {
		t.Fatalf("the Python package's migration is the schema of record: %v", err)
	}
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
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}
	pool.Close()
	store, err := spi.NewPostgresStore(ctx, scoped, owner, nil)
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
	fresh, err := spi.NewPostgresStore(ctx, dsnOf(t, old), "pod-new/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	n, err := fresh.Reconcile(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reconciled %d %v", n, err)
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
	server.Start(context.Background())
	defer server.Stop()

	if _, health := do(server, http.MethodGet, "/health", nil); health["extra_info"].(map[string]any)["durable_invocations"] != true {
		t.Fatalf("health does not report durability: %v", health)
	}
	recorder, accepted := do(server, http.MethodPost, "/tools/Echo/echo/invoke", []byte(`{"parameters":{"query":"hi"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("invoke %d %s", recorder.Code, recorder.Body.String())
	}
	id := accepted["invocation_id"].(string)
	body := pollUntilTerminal(t, server, "/tools/Echo/echo/invocations/"+id)
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
	recorder, accepted = do(server, http.MethodPost, "/tools/Echo/echo/invoke", []byte(`{"parameters":{"query":"slow"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
	id = accepted["invocation_id"].(string)
	time.Sleep(20 * time.Millisecond)
	if recorder, _ := do(server, http.MethodDelete, "/tools/Echo/echo/invocations/"+id, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("cancel %d", recorder.Code)
	}
	body = pollUntilTerminal(t, server, "/tools/Echo/echo/invocations/"+id)
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
