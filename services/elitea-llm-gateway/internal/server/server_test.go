package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	natsinfra "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

func testConfig() config.Config {
	return config.Config{
		HTTPAddr:            "127.0.0.1:0",
		ShutdownTimeout:     150 * time.Second,
		InitialPoolSize:     4,
		ProviderConcurrency: 3,
	}
}

func newTestServer(t *testing.T, account schemas.Account) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&nopWriter{}, nil))
	level := new(slog.LevelVar)
	mux := http.NewServeMux()
	srv, err := New(context.Background(), testConfig(), logger, level, account, mux)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNewInitialisesBifrostWithBootstrapAccount(t *testing.T) {
	srv := newTestServer(t, nil) // nil → bootstrap account
	if srv.Core() == nil {
		t.Fatal("Core() is nil after New")
	}
	// Clean up bifrost workers.
	_ = srv.Shutdown(context.Background())
}

func TestNewSetsSSESafeHTTPTimeouts(t *testing.T) {
	srv := newTestServer(t, nil)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	// §9.5: WriteTimeout MUST be 0 (disabled) so SSE streams are not
	// hard-killed by a per-connection write deadline.
	if srv.http.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (§9.5)", srv.http.WriteTimeout)
	}
	// A finite ReadHeaderTimeout is expected to bound slow-header attacks.
	if srv.http.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.http.ReadHeaderTimeout)
	}
}

func TestShutdownAppliesConfiguredGrace(t *testing.T) {
	srv := newTestServer(t, nil)

	// Server never started ListenAndServe; Shutdown on an unstarted server is
	// a no-op that must still return promptly and release bifrost.
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return promptly")
	}
}

// fakeNATS is a stand-in NATSClient recording only whether Close ran; the
// budget methods are unused by the wiring tests and return zero values.
type fakeNATS struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeNATS) IncrBudget(context.Context, string, int64) (int64, error) { return 0, nil }
func (f *fakeNATS) IncrBudgetIdempotent(_ context.Context, _ string, _ string, _ int64) (int64, bool, error) {
	return 0, false, nil
}
func (f *fakeNATS) ReadBudget(context.Context, string) (int64, error)      { return 0, nil }
func (f *fakeNATS) TryAlertCooldown(context.Context, string) (bool, error) { return false, nil }
func (f *fakeNATS) PublishDelta(context.Context, string, []byte) error     { return nil }
func (f *fakeNATS) PublishSoftAlertEvent(context.Context, string, []byte) error {
	return nil
}
func (f *fakeNATS) PublishOpsEvent(context.Context, []byte) error         { return nil }
func (f *fakeNATS) OnBreakerStateChange(_ func(from, to gobreaker.State)) {}
func (f *fakeNATS) BreakerState() gobreaker.State                         { return gobreaker.StateClosed }
func (f *fakeNATS) BudgetSubject(_, _ string, _ int64) string             { return "" }
func (f *fakeNATS) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}
func (f *fakeNATS) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func newServerWithConnector(t *testing.T, cfg config.Config, conn natsConnector) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&nopWriter{}, nil))
	level := new(slog.LevelVar)
	mux := http.NewServeMux()
	srv, err := New(context.Background(), cfg, logger, level, nil, mux, WithNATSConnector(conn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestNATSDisabledWhenURLUnset(t *testing.T) {
	called := false
	conn := func(context.Context, natsinfra.Config) (NATSClient, error) {
		called = true
		return nil, nil
	}
	// testConfig has no NATSURL → connector must not be called and NATS() is nil.
	srv := newServerWithConnector(t, testConfig(), conn)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if called {
		t.Error("connector called despite empty NATSURL")
	}
	if srv.NATS() != nil {
		t.Error("NATS() non-nil when disabled")
	}
}

func TestNATSConnectedAndClosedOnShutdown(t *testing.T) {
	fake := &fakeNATS{}
	var gotCfg natsinfra.Config
	conn := func(_ context.Context, c natsinfra.Config) (NATSClient, error) {
		gotCfg = c
		return fake, nil
	}
	cfg := testConfig()
	cfg.NATSURL = "nats://nats:4222"
	cfg.ServiceName = "gw-test"
	cfg.NATSTLSCAFile = "/etc/nats-client/ca.crt"
	cfg.NATSTLSCertFile = "/etc/nats-client/tls.crt"
	cfg.NATSTLSKeyFile = "/etc/nats-client/tls.key"
	cfg.CBFailureThreshold = 5
	cfg.CBOpenDuration = 20 * time.Second

	srv := newServerWithConnector(t, cfg, conn)

	if srv.NATS() == nil {
		t.Fatal("NATS() nil after successful connect")
	}
	// Config threads through to the nats client verbatim.
	if gotCfg.URL != "nats://nats:4222" || gotCfg.Name != "gw-test" ||
		gotCfg.TLSCAFile != "/etc/nats-client/ca.crt" || gotCfg.TLSCertFile != "/etc/nats-client/tls.crt" ||
		gotCfg.TLSKeyFile != "/etc/nats-client/tls.key" ||
		gotCfg.CBFailureThreshold != 5 || gotCfg.CBOpenDuration != 20*time.Second {
		t.Errorf("connector cfg = %+v, not threaded through", gotCfg)
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !fake.isClosed() {
		t.Error("NATS client not closed on shutdown")
	}
}

func TestNATSConnectErrorIsNonFatal(t *testing.T) {
	conn := func(context.Context, natsinfra.Config) (NATSClient, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	cfg := testConfig()
	cfg.NATSURL = "nats://unreachable:4222"

	// New MUST NOT fail when NATS is unreachable — the FSM owns degraded policy.
	srv := newServerWithConnector(t, cfg, conn)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if srv.NATS() != nil {
		t.Error("NATS() non-nil after connect error")
	}
}

func TestShutdownNilNATSDoesNotPanic(t *testing.T) {
	// testConfig disables NATS; Shutdown must not panic on the nil client.
	srv := newTestServer(t, nil)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&nopWriter{}, nil))
	level := new(slog.LevelVar)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cfg := testConfig()
	srv, err := New(context.Background(), cfg, logger, level, nil, mux)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	// Shut down; ListenAndServe must return nil (graceful).
	time.Sleep(50 * time.Millisecond)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

// --- RedialNATS (issue #315) -------------------------------------------------
//
// New dials NATS exactly once, and nats.go only resurrects a connection that
// succeeded at least once. A failed boot dial therefore left the client nil for
// the life of the process, so budget enforcement never came back. RedialNATS is
// the seam the composition root polls to end that.

// TestRedialNATSRecoversAfterFailedBootDial is the sequence issue #315 fixes: a
// gateway boots during a NATS outage, and a later dial succeeds.
func TestRedialNATSRecoversAfterFailedBootDial(t *testing.T) {
	fake := &fakeNATS{}
	var dials atomic.Int64
	up := make(chan struct{})
	conn := func(context.Context, natsinfra.Config) (NATSClient, error) {
		dials.Add(1)
		select {
		case <-up:
			return fake, nil
		default:
			return nil, errors.New("dial tcp: connection refused")
		}
	}
	cfg := testConfig()
	cfg.NATSURL = "nats://unreachable:4222"
	srv := newServerWithConnector(t, cfg, conn)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if srv.NATS() != nil {
		t.Fatal("NATS() non-nil after a failed boot dial")
	}
	if _, err := srv.RedialNATS(context.Background()); err == nil {
		t.Fatal("RedialNATS reported success while NATS was still unreachable")
	}
	if srv.NATS() != nil {
		t.Fatal("a failed re-dial published a client")
	}

	close(up)
	nc, err := srv.RedialNATS(context.Background())
	if err != nil {
		t.Fatalf("RedialNATS after NATS returned: %v", err)
	}
	if nc != NATSClient(fake) {
		t.Error("RedialNATS returned a client the connector did not produce")
	}
	if srv.NATS() != NATSClient(fake) {
		t.Error("the re-dialled client was not published to NATS()")
	}

	// A second call must NOT open a second connection: two clients would mean
	// two budget paths, and only one of them is ever closed.
	before := dials.Load()
	again, err := srv.RedialNATS(context.Background())
	if err != nil || again != NATSClient(fake) {
		t.Fatalf("second RedialNATS = (%v, %v), want the live client", again, err)
	}
	if dials.Load() != before {
		t.Errorf("a second RedialNATS dialled again (%d extra dials)", dials.Load()-before)
	}
}

// TestRedialNATSRefusesWhenUnconfigured keeps the NATS-less posture a choice
// rather than a fault: a caller must not poll a URL that does not exist.
func TestRedialNATSRefusesWhenUnconfigured(t *testing.T) {
	called := false
	srv := newServerWithConnector(t, testConfig(), func(context.Context, natsinfra.Config) (NATSClient, error) {
		called = true
		return nil, nil
	})
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if _, err := srv.RedialNATS(context.Background()); !errors.Is(err, ErrNATSNotConfigured) {
		t.Errorf("RedialNATS err = %v, want ErrNATSNotConfigured", err)
	}
	if called {
		t.Error("the connector ran for a gateway with no GATEWAY_NATS_URL")
	}
}

// TestRedialNATSRefusesAfterClose stops the recovery loop at shutdown. A client
// opened after Close is one nothing will ever close.
func TestRedialNATSRefusesAfterClose(t *testing.T) {
	var dials atomic.Int64
	conn := func(context.Context, natsinfra.Config) (NATSClient, error) {
		dials.Add(1)
		return nil, errors.New("dial tcp: connection refused")
	}
	cfg := testConfig()
	cfg.NATSURL = "nats://unreachable:4222"
	srv := newServerWithConnector(t, cfg, conn)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	srv.Close()
	before := dials.Load()
	if _, err := srv.RedialNATS(context.Background()); !errors.Is(err, ErrServerClosed) {
		t.Errorf("RedialNATS err = %v, want ErrServerClosed", err)
	}
	if dials.Load() != before {
		t.Error("RedialNATS dialled after the server closed")
	}
}

// TestRedialNATSIsSafeUnderConcurrentReaders runs re-dials against concurrent
// NATS() readers. The composition root does exactly this: /readyz and the
// shutdown path read the client while the recovery loop writes it.
func TestRedialNATSIsSafeUnderConcurrentReaders(t *testing.T) {
	fake := &fakeNATS{}
	conn := func(context.Context, natsinfra.Config) (NATSClient, error) {
		return fake, nil
	}
	cfg := testConfig()
	cfg.NATSURL = "nats://unreachable:4222"
	srv := newServerWithConnector(t, cfg, conn)
	defer func() { _ = srv.Shutdown(context.Background()) }()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = srv.NATS()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if _, err := srv.RedialNATS(context.Background()); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
}
