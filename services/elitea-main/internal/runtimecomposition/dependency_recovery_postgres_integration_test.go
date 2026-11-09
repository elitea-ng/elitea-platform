package runtimecomposition

// FU-275: Main's runtime runners against a real PostgreSQL that goes away and
// comes back. The pool reaches the CI PostgreSQL through an in-test TCP proxy
// the test cuts (refused dials, reset connections) and restores, and a fake
// server built on pgproto3 answers every login with 57P03 ("the database
// system is in recovery mode") the way a recovering server does. Nothing is
// mocked between the runners and the network.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

// cuttableProxy forwards TCP to target until Cut, and again after Restore,
// on the same address.
type cuttableProxy struct {
	t      *testing.T
	target string
	addr   string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
}

func startCuttableProxy(t *testing.T, target string) *cuttableProxy {
	t.Helper()
	p := &cuttableProxy{t: t, target: target, conns: make(map[net.Conn]struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = listener.Addr().String()
	p.serve(listener)
	t.Cleanup(p.Cut)
	return p
}

func (p *cuttableProxy) serve(listener net.Listener) {
	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.conns[client] = struct{}{}
			p.conns[server] = struct{}{}
			p.mu.Unlock()
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
}

// Cut refuses new connections and resets the open ones.
func (p *cuttableProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}
	for conn := range p.conns {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
		delete(p.conns, conn)
	}
}

// Restore accepts connections again on the same address.
func (p *cuttableProxy) Restore() {
	p.t.Helper()
	var listener net.Listener
	var err error
	for range 50 {
		if listener, err = net.Listen("tcp", p.addr); err == nil {
			p.serve(listener)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("restore proxy on %s: %v", p.addr, err)
}

func (p *cuttableProxy) port(t *testing.T) uint16 {
	t.Helper()
	_, port, err := net.SplitHostPort(p.addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}

// idleObjectStore is never called: the sweep finds no orphan to delete.
type idleObjectStore struct{ storage.ObjectStore }

// countingArtifactSweep counts the clean housekeeping passes.
type countingArtifactSweep struct {
	toolkitDiscoveryArtifactSweep
	clean atomic.Int64
}

func (c *countingArtifactSweep) SweepOrphanToolkitDiscoveryArtifacts(ctx context.Context, limit int) (int, error) {
	n, err := c.toolkitDiscoveryArtifactSweep.SweepOrphanToolkitDiscoveryArtifacts(ctx, limit)
	if err == nil {
		c.clean.Add(1)
	}
	return n, err
}

// eagerDatabaseRunner is a runner written the way the toolkit discovery
// runner used to be: it returns the first database error. The supervisor must
// restart it instead of letting it end Main.
type eagerDatabaseRunner struct {
	pool *pgxpool.Pool
	ok   atomic.Int64
}

func (r *eagerDatabaseRunner) Run(ctx context.Context) error {
	for {
		if _, err := r.pool.Exec(ctx, "SELECT 1"); err != nil {
			return err
		}
		r.ok.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// boundedWait keeps the real backoff decisions but sleeps at most 100 ms, so
// the test spends seconds, not minutes.
func boundedWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(min(delay, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countLines(t *testing.T, records *logRecords, level, prefix string) int {
	t.Helper()
	n := 0
	for _, entry := range records.entries(t) {
		msg, _ := entry["msg"].(string)
		if entry["level"] == level && strings.HasPrefix(msg, prefix) {
			n++
		}
	}
	return n
}

func databaseReadinessCheck(pool *pgxpool.Pool) health.Check {
	return health.Check{
		Name:     "db",
		Critical: true,
		Probe: func(ctx context.Context) (string, string) {
			if err := pool.Ping(ctx); err != nil {
				return health.StateUnavailable, depfailure.Describe(err)
			}
			return health.StateOK, ""
		},
		Lost:      "Main lost its database",
		Recovered: "Main reached its database again",
	}
}

func TestRuntimeRunnersSurviveADatabaseOutageAndLogEachTransitionOnce(t *testing.T) {
	direct := newArtifactRetentionPostgresPool(t)
	applyArtifactRetentionSharedMigrations(t, direct)
	target := net.JoinHostPort(direct.Config().ConnConfig.Host, strconv.Itoa(int(direct.Config().ConnConfig.Port)))
	proxy := startCuttableProxy(t, target)

	config := direct.Config().Copy()
	config.ConnConfig.Host = "127.0.0.1"
	config.ConnConfig.Port = proxy.port(t)
	config.ConnConfig.ConnectTimeout = time.Second
	config.ConnConfig.Fallbacks = nil
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	jobs, err := repos.NewToolkitAvailableToolsJobsRepository(pool, repos.ToolkitAvailableToolsDispatchPolicy{
		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "tool-run-limits-v1",
		MaxOutstanding:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactRepo, err := repos.NewToolkitDiscoveryArtifactRepository(pool, idleObjectStore{})
	if err != nil {
		t.Fatal(err)
	}
	sweep := &countingArtifactSweep{toolkitDiscoveryArtifactSweep: artifactRepo}
	records := &logRecords{}
	logger := slog.New(slog.NewJSONHandler(records, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("captured log:\n%s", records.buf.String())
		}
	})

	discovery := &currentToolkitDiscoveryRuntime{jobs: jobs, artifacts: sweep, logger: logger, wait: boundedWait}
	eager := &eagerDatabaseRunner{pool: pool}
	set, err := newPublisherSet(named("toolkit discovery housekeeping", discovery), named("eager database poller", eager))
	if err != nil {
		t.Fatal(err)
	}
	set.withLogger(logger)
	set.wait = boundedWait
	monitor, err := health.NewMonitor([]health.Check{databaseReadinessCheck(pool)}, logger, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- set.Run(ctx) }()
	go monitor.Run(ctx)

	eventually(t, "two clean housekeeping passes before the outage", func() bool { return sweep.clean.Load() >= 1 && eager.ok.Load() >= 2 })
	cleanBefore := sweep.clean.Load()

	proxy.Cut()
	eventually(t, "the outage to be seen by both runners and the monitor", func() bool {
		return countLines(t, records, "ERROR", "The toolkit discovery housekeeping pass failed; retrying in ") >= 2 &&
			countLines(t, records, "WARN", "Main restarts the eager database poller runner after a dependency failure") >= 2 &&
			countLines(t, records, "WARN", "Main lost its database") == 1
	})
	if evaluation := health.Evaluate(context.Background(), []health.Check{databaseReadinessCheck(pool)}); evaluation.Status != "not_ready" {
		t.Fatalf("readiness during the outage = %q, want not_ready", evaluation.Status)
	}
	time.Sleep(300 * time.Millisecond) // several more failing monitor evaluations
	select {
	case err := <-done:
		t.Fatalf("the runner set stopped during a database outage: %v", err)
	default:
	}

	proxy.Restore()
	okBefore := eager.ok.Load()
	eventually(t, "both runners and the monitor to see the database again", func() bool {
		return sweep.clean.Load() > cleanBefore && eager.ok.Load() > okBefore &&
			countLines(t, records, "INFO", "Main reached its database again after ") == 1
	})

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner set ended with %v, want only the owner's cancellation", err)
	}
	if n := countLines(t, records, "WARN", "Main lost its database"); n != 1 {
		t.Fatalf("loss lines = %d, want exactly 1 for one outage", n)
	}
	if n := countLines(t, records, "INFO", "Main reached its database again after "); n != 1 {
		t.Fatalf("recovery lines = %d, want exactly 1 for one outage", n)
	}
	if n := countLines(t, records, "ERROR", "The "); n != countLines(t, records, "ERROR", "The toolkit discovery housekeeping pass failed") {
		t.Fatalf("an ERROR other than the housekeeping pass failures was logged: %d", n)
	}
	records.mu.Lock()
	captured := records.buf.String()
	records.mu.Unlock()
	for _, detail := range []string{config.ConnConfig.Database, proxy.addr, target} {
		if strings.Contains(captured, detail) {
			t.Fatalf("a log line carries the connection detail %q", detail)
		}
	}
}

// recoveringServer answers every login with 57P03, as PostgreSQL does while
// it replays WAL after a crash.
func startRecoveringServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				for {
					message, err := backend.ReceiveStartupMessage()
					if err != nil {
						return
					}
					if _, ok := message.(*pgproto3.SSLRequest); ok {
						if _, err := conn.Write([]byte("N")); err != nil {
							return
						}
						continue
					}
					backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "57P03", Message: "the database system is in recovery mode"})
					_ = backend.Flush()
					return
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func TestRuntimeRunnersWaitOutADatabaseInRecoveryMode(t *testing.T) {
	addr := startRecoveringServer(t)
	host, port, _ := net.SplitHostPort(addr)
	pool, err := pgxpool.New(context.Background(), "postgres://elitea:elitea@"+host+":"+port+"/elitea?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	records := &logRecords{}
	logger := slog.New(slog.NewJSONHandler(records, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("captured log:\n%s", records.buf.String())
		}
	})
	eager := &eagerDatabaseRunner{pool: pool}
	set, err := newPublisherSet(named("eager database poller", eager))
	if err != nil {
		t.Fatal(err)
	}
	set.withLogger(logger)
	set.wait = boundedWait
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- set.Run(ctx) }()

	eventually(t, "three restarts while the server recovers", func() bool {
		return countLines(t, records, "WARN", "Main restarts the eager database poller runner after a dependency failure") >= 3
	})
	if got := set.DegradedRunners(); len(got) != 1 || got[0] != "eager database poller" {
		t.Fatalf("DegradedRunners = %v", got)
	}
	evaluation := health.Evaluate(context.Background(), []health.Check{databaseReadinessCheck(pool)})
	if evaluation.Status != "not_ready" || evaluation.Checks["db"] != health.StateUnavailable {
		t.Fatalf("readiness while recovering = %+v", evaluation)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner set ended with %v, want only the owner's cancellation", err)
	}
	for _, entry := range records.entries(t) {
		if entry["level"] == "WARN" && entry["cause"] != "the database system is in recovery mode (SQLSTATE 57P03)" {
			t.Fatalf("restart cause = %v, want the server's recovery-mode answer", entry["cause"])
		}
	}
}

// FU-275 condition 2, against the real session registry: with the database
// up, an unknown, expired, revoked or foreign session is refused and not
// retryable; with the database cut off, even the valid session is refused,
// and only then is the refusal marked retryable. Nothing authorizes while the
// database is unreachable.
func TestWorkloadSessionRefusalsAgainstARealDatabaseAndACutOne(t *testing.T) {
	direct := newArtifactRetentionPostgresPool(t)
	applyArtifactRetentionSharedMigrations(t, direct)
	ctx := context.Background()
	if _, err := direct.Exec(ctx, `
INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at, revoked_at) VALUES
 ('live', 'dns:worker.runtime.example', 'producer-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', NULL),
 ('expired', 'dns:worker.runtime.example', 'producer-1', clock_timestamp() - interval '2 hours', clock_timestamp() - interval '1 hour', NULL),
 ('revoked', 'dns:worker.runtime.example', 'producer-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', clock_timestamp()),
 ('foreign', 'dns:other.runtime.example', 'producer-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', NULL)`); err != nil {
		t.Fatal(err)
	}
	target := net.JoinHostPort(direct.Config().ConnConfig.Host, strconv.Itoa(int(direct.Config().ConnConfig.Port)))
	proxy := startCuttableProxy(t, target)
	config := direct.Config().Copy()
	config.ConnConfig.Host = "127.0.0.1"
	config.ConnConfig.Port = proxy.port(t)
	config.ConnConfig.ConnectTimeout = time.Second
	config.ConnConfig.Fallbacks = nil
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sessions, err := repos.NewWorkloadSessionsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := workloadauth.NewPeerAuthorizer(sessions)
	if err != nil {
		t.Fatal(err)
	}
	records := &logRecords{}
	authorizer.WithLogger(slog.New(slog.NewJSONHandler(records, nil)), "runtime control")
	peerCtx := peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{{DNSNames: []string{"worker.runtime.example"}}}},
	}}})

	if identity, err := authorizer.AuthorizeWorkload(peerCtx, "live", "producer-1"); err != nil || identity == "" {
		t.Fatalf("the live session was refused with the database up: %v", err)
	}
	for _, session := range []string{"unknown", "expired", "revoked", "foreign"} {
		identity, err := authorizer.AuthorizeWorkload(peerCtx, session, "producer-1")
		if identity != "" || !errors.Is(err, workloadauth.ErrWorkloadUnauthorized) || workloadauth.SessionStoreUnavailable(err) {
			t.Fatalf("%s session with the database up = (%q, %v), want a plain, non-retryable refusal", session, identity, err)
		}
	}

	proxy.Cut()
	for range 5 {
		identity, err := authorizer.AuthorizeWorkload(peerCtx, "live", "producer-1")
		if identity != "" || !errors.Is(err, workloadauth.ErrWorkloadUnauthorized) || !workloadauth.SessionStoreUnavailable(err) {
			t.Fatalf("live session with the database cut off = (%q, %v), want a refusal marked retryable", identity, err)
		}
	}
	proxy.Restore()
	eventually(t, "the live session to be accepted again", func() bool {
		identity, err := authorizer.AuthorizeWorkload(peerCtx, "live", "producer-1")
		return err == nil && identity != ""
	})
	if n := countLines(t, records, "WARN", "The runtime control server cannot read workload sessions;"); n != 1 {
		t.Fatalf("outage WARN lines = %d, want 1 for five refused requests", n)
	}
	if n := countLines(t, records, "INFO", "The runtime control server reads workload sessions again after "); n != 1 {
		t.Fatalf("recovery INFO lines = %d, want 1", n)
	}
	records.mu.Lock()
	captured := records.buf.String()
	records.mu.Unlock()
	for _, leak := range []string{"live", "producer-1", config.ConnConfig.Database, proxy.addr} {
		if strings.Contains(captured, leak) {
			t.Fatalf("the outage log carries %q", leak)
		}
	}
}
