package runtimecomposition

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// The runtime plane's real NATS path — NewRuntimeNATSConn as the
// elitea-main-runtime identity, the boot-time binding of every configured
// stream, and the execution-replay wake-up between two replicas — on a server
// started from the NATS chart's own rendered permissions, after the real
// bootstrap.sh. A grant the chart lacks is a violation here.
func TestRuntimeNATSPlaneOnTheChartsPermissions(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	config, err := ConfigFromEnv(s.Lookup(runtimeNATSPrefix, natsconn.IdentityMainRuntime, map[string]string{
		"ELITEA_RUNTIME_ENABLED":                          "true",
		"ELITEA_RUNTIME_COMMAND_STREAM":                   "ELITEA_RT_V1_VALIDATE",
		"ELITEA_RUNTIME_MAX_OUTSTANDING":                  "64",
		"ELITEA_RUNTIME_NATS_URL":                         s.URL(),
		"ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED":    "true",
		"ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM":      "ELITEA_RT_V1_INDEX",
		"ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED": "true",
		"ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM":   "ELITEA_RT_V1_AGENT",
		"ELITEA_RUNTIME_SIGNING_KEY_ID":                   "k",
		"ELITEA_RUNTIME_SIGNING_KEY_FILE":                 "/run/k",
		"ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE":        "/run/r",
		"ELITEA_RUNTIME_CONTROL_ADDRESS":                  ":9443",
		"ELITEA_RUNTIME_OUTPUT_ADDRESS":                   ":9444",
		"ELITEA_RUNTIME_CONTENT_ADDRESS":                  ":9445",
		"ELITEA_RUNTIME_CONTROL_TLS_CERT_FILE":            "/run/c", "ELITEA_RUNTIME_CONTROL_TLS_KEY_FILE": "/run/c", "ELITEA_RUNTIME_CONTROL_TLS_CLIENT_CA_FILE": "/run/c",
		"ELITEA_RUNTIME_OUTPUT_TLS_CERT_FILE": "/run/c", "ELITEA_RUNTIME_OUTPUT_TLS_KEY_FILE": "/run/c", "ELITEA_RUNTIME_OUTPUT_TLS_CLIENT_CA_FILE": "/run/c",
		"ELITEA_RUNTIME_CONTENT_TLS_CERT_FILE": "/run/c", "ELITEA_RUNTIME_CONTENT_TLS_KEY_FILE": "/run/c", "ELITEA_RUNTIME_CONTENT_TLS_CLIENT_CA_FILE": "/run/c",
	}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewRuntimeNATSConn(NATSConnConfig{URL: config.NATSURL, Material: config.NATSMaterial}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	js, err := NewRuntimeJetStream(first)
	if err != nil {
		t.Fatal(err)
	}
	route, err := configuredToolkitRoute(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	handles, err := bindCommandStreams(ctx, js, config, route)
	if err != nil || len(handles) != 3 {
		t.Fatalf("bind the configured streams: %d handles, %v", len(handles), err)
	}

	second, err := NewRuntimeNATSConn(NATSConnConfig{URL: config.NATSURL, Material: config.NATSMaterial}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	publisher, err := newNATSExecutionReplayWakeBus(first, logger)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := newNATSExecutionReplayWakeBus(second, logger)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = publisher.Run(runCtx) }()
	go func() { _ = listener.Run(runCtx) }()
	woken := make(chan error, 1)
	go func() {
		_, err := listener.Wait(ctx, "42", "execution-1", 6)
		woken <- err
	}()
	waitForReplayWaiters(t, listener, 1)
	deadline := time.After(10 * time.Second)
	for delivered := false; !delivered; {
		// The listener's subscription is asynchronous; re-notify until the
		// first wake arrives (each one is a no-op for a satisfied waiter).
		publisher.Notify("42", "execution-1", 7)
		select {
		case err := <-woken:
			if err != nil {
				t.Fatal(err)
			}
			delivered = true
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("the replay wake did not cross replicas over NATS")
		}
	}
	stop()
	s.RequireNoViolations(t, natsconn.IdentityMainRuntime)
}

// Nothing in elitea-main's production code reads the runtime Redis anymore:
// the command bus is NATS, Form auth is on PostgreSQL, and the runtime-redis
// deployment is left without a client until its own removal.
func TestNoRuntimeRedisLookups(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	forbidden := []string{"ELITEA_RUNTIME_REDIS", "redis-producer-password", "redis-worker-password", "elitea:runtime:execution-replay"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, needle := range forbidden {
			if strings.Contains(string(raw), needle) {
				t.Errorf("%s still names %q", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
