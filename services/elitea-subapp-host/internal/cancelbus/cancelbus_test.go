package cancelbus_test

// Two replicas of one host, one NATS server (embedded, in process), one
// shared invocation store: an invocation accepted by replica A is cancelled
// through replica B and STOPS on A. Without the bus the same cancel is
// recorded on the row and the run on A goes on to complete — which is the
// multi-replica defect the bus exists for, and the control that shows the
// bus, not the store, is what stops the run.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/elitea-ng/elitea-platform/libs/go/natsconn"
	"github.com/elitea-ng/elitea-platform/libs/go/natsconn/natstest"
	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/cancelbus"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi/spitest"
)

// sharedStore is what PostgreSQL is to the replicas: one set of rows, and
// every Get a fresh copy rather than the struct another replica's run holds.
type sharedStore struct {
	mu     sync.Mutex
	rows   map[string]spi.Invocation
	events map[string][]map[string]any
}

func newSharedStore() *sharedStore {
	return &sharedStore{rows: map[string]spi.Invocation{}, events: map[string][]map[string]any{}}
}

func (s *sharedStore) Durable() bool { return true }

func (s *sharedStore) Create(_ context.Context, invocation *spi.Invocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[invocation.ID] = spi.Invocation{ID: invocation.ID, Toolkit: invocation.Toolkit, Tool: invocation.Tool, Status: invocation.Status, CreatedAt: invocation.CreatedAt}
	return nil
}

func (s *sharedStore) Get(_ context.Context, toolkit, tool, id string) (*spi.Invocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.Toolkit != toolkit || row.Tool != tool {
		return nil, nil
	}
	return &row, nil
}

func (s *sharedStore) Update(_ context.Context, invocation *spi.Invocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rows[invocation.ID]
	row.Status, row.Result, row.FinishedAt = invocation.Status, invocation.Result, invocation.FinishedAt
	row.StopRequest = row.StopRequest || invocation.StopRequest
	s.rows[invocation.ID] = row
	return nil
}

func (s *sharedStore) RequestStop(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.rows[id]
	row.StopRequest = true
	s.rows[id] = row
	return nil
}

func (s *sharedStore) AppendEvent(_ context.Context, id, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[id] = append(s.events[id], map[string]any{"data": map[string]any{"message": message}})
	return nil
}

func (s *sharedStore) DrainEvents(_ context.Context, invocation *spi.Invocation) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.events[invocation.ID]
	delete(s.events, invocation.ID)
	return events, nil
}

func (s *sharedStore) Prune(context.Context, time.Duration) (int, error) { return 0, nil }

func startNATS(t *testing.T) string {
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

// replica is one host process: its own manager, its own listener, the shared
// store and — when url is set — its own NATS connection, presenting material
// when it is set.
func replica(t *testing.T, store spi.Store, url string, material ...natsconn.Material) subappv1.ProviderServiceClient {
	t.Helper()
	settings, err := spi.SettingsFromEnv(echo.EnvPrefix, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	options := []spi.Option{spi.WithStore(store)}
	if url != "" {
		config := cancelbus.Config{URL: url, App: "deepwiki"}
		if len(material) > 0 {
			config.Material = material[0]
		}
		conn, err := cancelbus.Connect(config, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Close)
		bus, err := cancelbus.New(conn, "deepwiki", nil)
		if err != nil {
			t.Fatal(err)
		}
		options = append(options, spi.WithCancelBus(bus))
	}
	// Twenty paced steps: long enough that a cancel lands mid-run.
	app := echo.App(0)
	steps := make([]string, 20)
	for i := range steps {
		steps[i] = "step"
	}
	app.Runner = spi.EchoRunner{Step: 50 * time.Millisecond, Steps: steps}
	server, err := spi.NewServer(settings, app, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	return spitest.Start(t, server).Provider()
}

// invokeOnA, cancelOnB, and the terminal body as either replica reads it.
func cancelAcrossReplicas(t *testing.T, url string, material ...natsconn.Material) map[string]any {
	t.Helper()
	store := newSharedStore()
	a, b := replica(t, store, url, material...), replica(t, store, url, material...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := a.Invoke(ctx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	id := accepted.GetInvocationId()
	time.Sleep(120 * time.Millisecond)
	if _, err := b.Cancel(ctx, &subappv1.CancelRequest{Toolkit: "Echo", Tool: "echo", InvocationId: id}); err != nil {
		t.Fatalf("replica B could not cancel A's invocation: %v", err)
	}
	for {
		read, err := b.GetInvocation(ctx, &subappv1.GetInvocationRequest{Toolkit: "Echo", Tool: "echo", InvocationId: id})
		if err != nil {
			t.Fatal(err)
		}
		if read.GetTerminal() {
			var body map[string]any
			if err := json.Unmarshal(read.GetBodyJson(), &body); err != nil {
				t.Fatal(err)
			}
			return body
		}
		select {
		case <-ctx.Done():
			t.Fatal("the invocation never ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestACancelOnOneReplicaStopsTheRunOnAnother(t *testing.T) {
	started := time.Now()
	body := cancelAcrossReplicas(t, startNATS(t))
	if body["status"] != "Error" || !strings.Contains(body["result"].(string), "Invocation cancelled") {
		t.Fatalf("the run on A was not stopped by the cancel on B: %v", body)
	}
	// Twenty steps of 50ms is a second; a stopped run ends well before.
	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Fatalf("the run took %v: it ran to its end", elapsed)
	}
}

// The same, against the NATS chart's own permission table (natstest: the
// rendered nats.conf, TLS, verify_and_map): the host's real code presents
// elitea-deepwiki and needs no grant the chart does not give. Skipped without
// the secured-NATS test environment, failed with it required (CI).
func TestTheFanOutWorksUnderTheChartsPermissionTable(t *testing.T) {
	s := natstest.Start(t)
	body := cancelAcrossReplicas(t, s.URL(), s.Material(natsconn.IdentitySubappDeepWiki))
	if body["status"] != "Error" || !strings.Contains(body["result"].(string), "Invocation cancelled") {
		t.Fatalf("the run on A was not stopped by the cancel on B: %v", body)
	}
	s.RequireNoViolations(t, natsconn.IdentitySubappDeepWiki)
}

func TestWithoutTheBusTheCancelIsRecordedButTheRunGoesOn(t *testing.T) {
	body := cancelAcrossReplicas(t, "")
	if body["status"] != "Completed" {
		t.Fatalf("without the bus the run should have completed (the defect the bus fixes): %v", body)
	}
}

func TestTheBusRefusesWhatIsNotOneSubjectToken(t *testing.T) {
	url := startNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := cancelbus.New(conn, "deep.wiki", nil); err == nil {
		t.Fatal("an application name with a separator was accepted")
	}
	bus, err := cancelbus.New(conn, "deepwiki", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "a.b", "*", ">", "a b"} {
		if err := bus.Publish(context.Background(), id); err == nil {
			t.Errorf("%q was published", id)
		}
	}
	// One application's cancels do not reach another's subscribers.
	got := make(chan string, 4)
	inventory, _ := cancelbus.New(conn, "inventory", nil)
	if err := inventory.Subscribe(t.Context(), func(id string) { got <- "inventory:" + id }); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe(t.Context(), func(id string) { got <- "deepwiki:" + id }); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), "invocation_1"); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-got:
		if first != "deepwiki:invocation_1" {
			t.Fatalf("%s", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel never arrived")
	}
	select {
	case extra := <-got:
		t.Fatalf("a cancel crossed applications: %s", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if natsconn.SubappCancelSubject("deepwiki", "x") != "elitea.subapp.deepwiki.cancel.x" {
		t.Fatal("the subject spelling moved")
	}
}

func TestConnectRefusesAnApplicationWithNoIdentityAndAPlaintextURLWithMaterial(t *testing.T) {
	if _, err := cancelbus.Connect(cancelbus.Config{URL: "nats://127.0.0.1:4222", App: "echo"}, nil); err == nil {
		t.Fatal("echo has no NATS identity and was connected")
	}
	material := natsconn.Material{CAFile: "/ca", CertFile: "/crt", KeyFile: "/key"}
	if _, err := cancelbus.Connect(cancelbus.Config{URL: "nats://127.0.0.1:4222", App: "deepwiki", Material: material}, nil); err == nil {
		t.Fatal("client material over a plaintext URL was accepted")
	}
}
