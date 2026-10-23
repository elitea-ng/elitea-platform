package run_test

// Two Inventory host replicas over ONE durable store and ONE cancel bus
// (#scaling): the store is the Inventory engine's migration 0009, applied
// into a throwaway schema in place of inventory_graph, and the bus is an
// embedded NATS server. Each replica is a real spi.Server over the real
// Inventory runner (its Labels, StopIngests and DeleteToolkit); only the
// engine tool is a stand-in that counts its runs.
//
// Gated on ELITEA_SUBAPP_HOST_TEST_DSN (CI sets it in ci-go.yml): without it
// the suite reports the skip, and nothing here passes without a database.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/cancelbus"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/testpg"
)

// inventoryHostMigrations are the migrations that create the host's tables
// in the Inventory engine's schema, in order (0009 the invocation tables,
// 0010 the stop fences); the test applies them with the schema name
// replaced.
var inventoryHostMigrations = []string{
	"../../../../../elitea-inventory-engine/migrations/0009_host_invocations.sql",
	"../../../../../elitea-inventory-engine/migrations/0010_host_stop_fences.sql",
}

// fleetSchema creates a throwaway schema, applies the Inventory migrations
// 0009 and 0010 into it (inventory_graph rewritten to the schema) and
// returns the DSN and the schema. The schema is dropped at cleanup.
func fleetSchema(t *testing.T) (string, string) {
	t.Helper()
	dsn := testpg.DSN(os.Getenv("ELITEA_SUBAPP_HOST_TEST_DSN"))
	if dsn == "" {
		t.Skip("ELITEA_SUBAPP_HOST_TEST_DSN is not set — the Inventory multi-replica host did NOT run against a database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("inventory_fleet_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	for _, migration := range inventoryHostMigrations {
		sql, err := os.ReadFile(filepath.Clean(migration))
		if err != nil {
			t.Fatalf("the engine's migration is the schema of record: %v", err)
		}
		if _, err := admin.Exec(ctx, strings.ReplaceAll(string(sql), "inventory_graph", schema)); err != nil {
			t.Fatalf("apply the migration %s: %v", migration, err)
		}
	}
	return dsn, schema
}

func startFleetNATS(t *testing.T) string {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the embedded NATS server did not start")
	}
	t.Cleanup(server.Shutdown)
	return server.ClientURL()
}

// runLog counts how many times the engine tool ran each invocation, over
// both replicas.
type runLog struct {
	mu   sync.Mutex
	runs map[string]int
	host map[string]string
}

func (l *runLog) record(id, host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs[id]++
	l.host[id] = host
}

func (l *runLog) count(id string) (int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs[id], l.host[id]
}

// fleetHost is one replica: its server, its runner and its store.
type fleetHost struct {
	name   string
	server *spi.Server
	runner *run.Runner
	store  *spi.PostgresStore
}

// ingestMode is what the stand-in engine tool does.
type ingestMode int

const (
	// ingestQuick reports progress and completes at once.
	ingestQuick ingestMode = iota
	// ingestUntilStopped runs until a stop or the end of the run.
	ingestUntilStopped
)

// newFleetHost starts one replica over the shared schema and bus.
func newFleetHost(t *testing.T, name, dsn, schema, natsURL string, log *runLog, mode ingestMode) *fleetHost {
	t.Helper()
	ctx := context.Background()
	store, err := spi.NewPostgresStore(ctx, dsn, fmt.Sprintf("inventory-%s-%d", name, time.Now().UnixNano()), nil,
		spi.WithSchema(schema),
		spi.WithClaimCheckInterval(100*time.Millisecond),
		spi.WithReconcileInterval(100*time.Millisecond),
		spi.WithOwnerGrace(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	conn, err := cancelbus.Connect(cancelbus.Config{URL: natsURL, App: "inventory"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	bus, err := cancelbus.New(conn, "inventory", nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &run.Runner{
		RunnerName: "sidecar", RequireVerifiedProject: true, StopWait: 10 * time.Second,
		Tools: map[string]run.Tool{"run_ingestion": func(ctx context.Context, _ map[string]any, tc *spi.Context) (map[string]any, error) {
			log.record(tc.InvocationID(), name)
			if err := tc.Thinking(ctx, "ingesting on "+name); err != nil {
				return nil, err
			}
			if mode == ingestUntilStopped {
				for {
					if err := tc.Checkpoint(); err != nil {
						return nil, err
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			return engineDocument(map[string]any{"entities": 1}), nil
		}},
		DeleteGraph: func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
			return engineDocument(map[string]any{"deleted": true, "project_id": 7, "application_id": 70}), nil
		},
	}
	settings, err := spi.SettingsFromEnv(inventory.EnvPrefix, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, inventory.App(runner), nil, spi.WithStore(store), spi.WithCancelBus(bus))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	return &fleetHost{name: name, server: server, runner: runner, store: store}
}

// ingest submits run_ingestion for toolkit 70+offset of project 7 on host.
func (h *fleetHost) ingest(t *testing.T, toolkit int) string {
	t.Helper()
	request := map[string]any{"configuration": map[string]any{"application_id": toolkit},
		"parameters": map[string]any{"source": map[string]any{"toolkit": "git"}}}
	invocation, err := h.server.Accept(context.Background(), spi.Identity{ProjectID: "7", UserID: "1"}, "inventory", "run_ingestion", request)
	if err != nil {
		t.Fatalf("%s did not accept the ingest: %v", h.name, err)
	}
	return invocation.ID
}

func (h *fleetHost) poll(t *testing.T, id string) map[string]any {
	t.Helper()
	body, err := h.server.Manager().Poll(context.Background(), "inventory", "run_ingestion", id)
	if err != nil {
		t.Fatalf("%s: poll %s: %v", h.name, id, err)
	}
	if body == nil {
		t.Fatalf("%s does not know the invocation %s", h.name, id)
	}
	return body
}

// terminal polls through host until the invocation ends and returns the
// terminal body.
func (h *fleetHost) terminal(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if body := h.poll(t, id); spi.TerminalBody(body) {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never read %s as terminal", h.name, id)
	return nil
}

// resultText is the message of a terminal body.
func resultText(t *testing.T, body map[string]any) string {
	t.Helper()
	var objects []map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprint(body["result"])), &objects); err != nil {
		t.Fatalf("result %v: %v", body["result"], err)
	}
	var parts []string
	for _, object := range objects {
		parts = append(parts, fmt.Sprint(object["data"]))
	}
	return strings.Join(parts, "\n")
}

func waitRunning(t *testing.T, log *runLog, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := log.count(id); n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the ingest %s never started", id)
}

// Invocations submitted alternately to both replicas each run exactly once,
// on the replica that accepted them, and every one is read to its terminal
// body through EITHER replica.
func TestInventoryReplicasRunEachInvocationOnceAndServeItsPollEverywhere(t *testing.T) {
	dsn, schema := fleetSchema(t)
	url := startFleetNATS(t)
	log := &runLog{runs: map[string]int{}, host: map[string]string{}}
	a := newFleetHost(t, "replica-a/1", dsn, schema, url, log, ingestQuick)
	b := newFleetHost(t, "replica-b/1", dsn, schema, url, log, ingestQuick)
	hosts := []*fleetHost{a, b}

	const calls = 12
	ids := make([]string, calls)
	for i := range ids {
		ids[i] = hosts[i%2].ingest(t, 70+i)
	}
	for i, id := range ids {
		accepted, other := hosts[i%2], hosts[(i+1)%2]
		body := other.terminal(t, id)
		if body["status"] != "Completed" || body["invocation_id"] != id {
			t.Fatalf("%s read %s as %v", other.name, id, body)
		}
		if again := accepted.poll(t, id); again["status"] != "Completed" {
			t.Fatalf("%s read %s as %v", accepted.name, id, again)
		}
	}
	for i, id := range ids {
		n, ranOn := log.count(id)
		if n != 1 || ranOn != hosts[i%2].name {
			t.Fatalf("%s ran %d times, on %s (accepted by %s)", id, n, ranOn, hosts[i%2].name)
		}
	}
}

// A cancel that lands on replica B stops the run replica A owns: B records
// the stop on the shared row and fans it out on the bus.
func TestInventoryCancelOnOneReplicaStopsTheRunOnAnother(t *testing.T) {
	dsn, schema := fleetSchema(t)
	url := startFleetNATS(t)
	log := &runLog{runs: map[string]int{}, host: map[string]string{}}
	a := newFleetHost(t, "replica-a/1", dsn, schema, url, log, ingestUntilStopped)
	b := newFleetHost(t, "replica-b/1", dsn, schema, url, log, ingestUntilStopped)

	id := a.ingest(t, 70)
	waitRunning(t, log, id)
	if body := b.poll(t, id); body["status"] != "InProgress" {
		t.Fatalf("B read A's running ingest as %v", body)
	}
	known, err := b.server.Manager().Cancel(context.Background(), "inventory", "run_ingestion", id)
	if err != nil || !known {
		t.Fatalf("B could not cancel A's ingest: %v %v", known, err)
	}
	body := b.terminal(t, id)
	if body["status"] != "Error" || !strings.Contains(resultText(t, body), "Invocation cancelled") {
		t.Fatalf("A's ingest did not end as cancelled: %v", body)
	}
	if n, ranOn := log.count(id); n != 1 || ranOn != a.name {
		t.Fatalf("the ingest ran %d times, on %s", n, ranOn)
	}
}

// A toolkit deletion on replica B (the platform operation DeleteToolkit, and
// so StopIngests) stops the matching ingest that runs on replica A, and the
// ingest of another toolkit of the project runs on.
func TestInventoryToolkitDeletionOnOneReplicaStopsTheIngestOnAnother(t *testing.T) {
	dsn, schema := fleetSchema(t)
	url := startFleetNATS(t)
	log := &runLog{runs: map[string]int{}, host: map[string]string{}}
	a := newFleetHost(t, "replica-a/1", dsn, schema, url, log, ingestUntilStopped)
	b := newFleetHost(t, "replica-b/1", dsn, schema, url, log, ingestUntilStopped)

	deleted := a.ingest(t, 70)
	kept := a.ingest(t, 71)
	waitRunning(t, log, deleted)
	waitRunning(t, log, kept)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	deletion, err := b.runner.DeleteToolkit(ctx, 7, 70)
	if err != nil {
		t.Fatalf("B refused the deletion: %v", err)
	}
	if !deletion.Deleted {
		t.Fatalf("deletion %+v", deletion)
	}
	// The deletion returns only once the matching ingest is terminal.
	body := a.poll(t, deleted)
	if body["status"] != "Error" || !strings.Contains(resultText(t, body), "Invocation cancelled") {
		t.Fatalf("the ingest of the deleted toolkit did not end as cancelled: %v", body)
	}
	if body := b.poll(t, kept); body["status"] != "InProgress" {
		t.Fatalf("the ingest of another toolkit was touched: %v", body)
	}
	if n, _ := log.count(kept); n != 1 {
		t.Fatalf("the kept ingest ran %d times", n)
	}

	// A deletion of the whole project's ingests, from B, reaches the rest.
	if err := b.runner.StopIngests(ctx, "7", ""); err != nil {
		t.Fatalf("B could not stop the project's ingests: %v", err)
	}
	if body := a.poll(t, kept); body["status"] != "Error" {
		t.Fatalf("the project's last ingest is still running: %v", body)
	}
}

// When replica A's process is gone (its store closed: its liveness lock is
// free), replica B ends A's in-flight row with the restart error after the
// owner grace, so a poll does not answer InProgress for ever.
func TestInventoryReplicaReconcilesTheRunsOfAReplicaThatIsGone(t *testing.T) {
	dsn, schema := fleetSchema(t)
	url := startFleetNATS(t)
	log := &runLog{runs: map[string]int{}, host: map[string]string{}}
	a := newFleetHost(t, "replica-a/1", dsn, schema, url, log, ingestUntilStopped)
	b := newFleetHost(t, "replica-b/1", dsn, schema, url, log, ingestUntilStopped)

	id := a.ingest(t, 70)
	waitRunning(t, log, id)
	// Twice the owner grace: B leaves a LIVE owner's row alone.
	time.Sleep(600 * time.Millisecond)
	if body := b.poll(t, id); body["status"] != "InProgress" {
		t.Fatalf("B ended the run of a live replica: %v", body)
	}

	a.store.Close()
	body := b.terminal(t, id)
	if body["status"] != "Error" || !strings.Contains(resultText(t, body), "The service restarted") {
		t.Fatalf("B did not reconcile the gone replica's run: %v", body)
	}
}
