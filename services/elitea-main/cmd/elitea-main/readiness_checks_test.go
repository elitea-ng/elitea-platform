package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
)

type staticRunners []string

func (s staticRunners) DegradedRunners() []string { return s }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(level, msgPrefix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, line := range strings.Split(b.buf.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if msg, _ := entry["msg"].(string); entry["level"] == level && strings.HasPrefix(msg, msgPrefix) {
			n++
		}
	}
	return n
}

func readyz(t *testing.T, deps health.Deps) (int, health.Status) {
	t.Helper()
	rec := httptest.NewRecorder()
	health.RoutesWithDeps(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var status health.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("readiness body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, status
}

func TestMainReadinessReportsDegradedRunnersWithoutLoggingThem(t *testing.T) {
	logs := &syncBuffer{}
	checks := mainReadinessChecks(nil, nil, staticRunners{"toolkit discovery housekeeping"})
	monitor, err := health.NewMonitor(checks, slog.New(slog.NewJSONHandler(logs, nil)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	code, status := readyz(t, health.Deps{Checks: checks, Monitor: monitor})
	if code != http.StatusOK || status.Status != "degraded" || status.Checks["runners"] != health.StateDegraded || status.Checks["db"] != health.StateOK {
		t.Fatalf("readiness = %d %+v, want 200 degraded with the runner named only in the log", code, status)
	}
	if logs.count("WARN", "") != 0 {
		t.Fatalf("the runners check logged on its own: restarts are the supervisor's lines")
	}
}

func TestMainReadinessIsNotReadyWhileTheDatabaseIsUnreachable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://elitea:elitea@127.0.0.1:1/elitea?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	code, status := readyz(t, health.Deps{Checks: mainReadinessChecks([]*pgxpool.Pool{pool}, nil, nil)})
	if code != http.StatusServiceUnavailable || status.Status != "not_ready" || status.Checks["db"] != health.StateUnavailable {
		t.Fatalf("readiness = %d %+v, want 503 not_ready", code, status)
	}
}

// A real nats-server killed and started again under a live connection with
// Main's reconnect settings: readiness reports degraded (200) while the
// connection reconnects and ready after, and the monitor logs one loss and
// one recovery line for the outage. Needs a nats-server binary
// (ELITEA_TEST_NATS_SERVER_BIN), because the test owns the server's process.
func TestMainReadinessFollowsARealNATSRestart(t *testing.T) {
	bin := os.Getenv("ELITEA_TEST_NATS_SERVER_BIN")
	if bin == "" {
		t.Skip("set ELITEA_TEST_NATS_SERVER_BIN (a nats-server binary) to run the NATS restart test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	storeDir := t.TempDir()
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "-js", "-a", "127.0.0.1", "-p", strconv.Itoa(port), "-sd", storeDir)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start nats-server: %v", err)
		}
		return cmd
	}
	stop := func(cmd *exec.Cmd) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	server := start()
	t.Cleanup(func() { stop(server) })

	var conn *nats.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(conn.Close)

	logs := &syncBuffer{}
	checks := mainReadinessChecks(nil, []func() nats.Status{conn.Status}, staticRunners(nil))
	monitor, err := health.NewMonitor(checks, slog.New(slog.NewJSONHandler(logs, nil)), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go monitor.Run(ctx)
	deps := health.Deps{Checks: checks, Monitor: monitor}

	if code, status := readyz(t, deps); code != http.StatusOK || status.Status != "ready" {
		t.Fatalf("readiness before the restart = %d %+v", code, status)
	}

	stop(server)
	waitFor(t, "readiness to report NATS reconnecting", func() bool {
		code, status := readyz(t, deps)
		return code == http.StatusOK && status.Status == "degraded" && status.Checks["nats"] == health.StateReconnecting
	})
	time.Sleep(200 * time.Millisecond) // several failing monitor evaluations

	server = start()
	waitFor(t, "readiness to report ready again", func() bool {
		code, status := readyz(t, deps)
		return code == http.StatusOK && status.Status == "ready"
	})
	waitFor(t, "the recovery line", func() bool { return logs.count("INFO", "Main reached NATS again after ") == 1 })
	if n := logs.count("WARN", "Main lost NATS;"); n != 1 {
		t.Fatalf("loss lines = %d, want exactly 1 for one outage\n%s", n, logs.buf.String())
	}
	if n := logs.count("INFO", "Main reached NATS again after "); n != 1 {
		t.Fatalf("recovery lines = %d, want exactly 1", n)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMainReadinessIsNotReadyWhenANATSConnectionIsClosed(t *testing.T) {
	closed := func() nats.Status { return nats.CLOSED }
	reconnecting := func() nats.Status { return nats.RECONNECTING }
	code, status := readyz(t, health.Deps{Checks: mainReadinessChecks(nil, []func() nats.Status{reconnecting, closed}, nil)})
	if code != http.StatusServiceUnavailable || status.Checks["nats"] != health.StateUnavailable {
		t.Fatalf("readiness with a closed NATS connection = %d %+v, want 503 with nats unavailable", code, status)
	}
	code, status = readyz(t, health.Deps{Checks: mainReadinessChecks(nil, []func() nats.Status{reconnecting}, nil)})
	if code != http.StatusOK || status.Status != "degraded" || status.Checks["nats"] != health.StateReconnecting {
		t.Fatalf("readiness while reconnecting = %d %+v, want 200 degraded", code, status)
	}
}
