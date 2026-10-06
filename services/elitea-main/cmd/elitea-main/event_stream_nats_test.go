package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func envLookup(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, present := pairs[key]
		return value, present
	}
}

// No URL is a declared state — no live-update plane — not an error: the
// deployment must keep starting.
func TestEventsNATSConnIsAbsentWithoutTheURL(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"unset": {},
		"empty": {eventsNATSURLEnv: ""},
		"blank": {eventsNATSURLEnv: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn, err := newEventsNATSConn(envLookup(env), nil)
			if err != nil {
				t.Fatalf("newEventsNATSConn() error = %v, want nil", err)
			}
			if conn != nil {
				t.Fatal("newEventsNATSConn() returned a connection without " + eventsNATSURLEnv)
			}
		})
	}
}

func TestEventsNATSConnRequiresLookup(t *testing.T) {
	t.Parallel()

	if _, err := newEventsNATSConn(nil, nil); err == nil {
		t.Fatal("newEventsNATSConn(nil) error = nil, want an error")
	}
}

// A configured-but-unreachable server must fail startup rather than leave
// RouterConfig.EventSource nil: a nil source silently unregisters
// /api/v2/events/prompt_lib/{projectID}, which is the whole failure mode of
// #152. Better to refuse to boot than to boot without the route.
func TestEventsNATSConnFailsWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	url := "nats://user:s3cret@" + address
	conn, err := newEventsNATSConn(envLookup(map[string]string{eventsNATSURLEnv: url}), nil)
	if conn != nil {
		conn.Close()
		t.Fatal("newEventsNATSConn() returned a connection to an unreachable server")
	}
	if err == nil {
		t.Fatal("newEventsNATSConn() error = nil, want an error for an unreachable server")
	}
	if !strings.Contains(err.Error(), eventsNATSURLEnv) {
		t.Errorf("error %q does not name %s", err, eventsNATSURLEnv)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %q leaks the credential in the URL", err)
	}
}

func TestEventsNATSReplicas(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]int{"": 1, "  ": 1, "1": 1, "3": 3, " 3 ": 3} {
		got, err := eventsNATSReplicas(envLookup(map[string]string{eventsNATSReplicasEnv: value}))
		if err != nil || got != want {
			t.Errorf("%s=%q: got (%d, %v), want (%d, nil)", eventsNATSReplicasEnv, value, got, err, want)
		}
	}
	if got, err := eventsNATSReplicas(envLookup(nil)); err != nil || got != 1 {
		t.Errorf("unset: got (%d, %v), want (1, nil)", got, err)
	}
	for _, value := range []string{"0", "-1", "three", "1.5"} {
		if _, err := eventsNATSReplicas(envLookup(map[string]string{eventsNATSReplicasEnv: value})); err == nil {
			t.Errorf("%s=%q was accepted; a typo must not silently become a replica count", eventsNATSReplicasEnv, value)
		}
	}
}

func TestCanvasPresenceStoreRequiresAConnection(t *testing.T) {
	t.Parallel()

	if _, err := newCanvasPresenceStore(context.Background(), nil, envLookup(nil)); err == nil {
		t.Fatal("newCanvasPresenceStore(nil conn) error = nil, want an error")
	}
}

// The boot path end to end against a real JetStream server: the URL dials,
// and the presence bucket is created on that connection.
func TestEventsNATSBootComposesThePresenceStore(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_NATS_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_NATS_URL (a JetStream-enabled NATS) to run the live-update boot test")
	}
	conn, err := newEventsNATSConn(envLookup(map[string]string{eventsNATSURLEnv: url}), nil)
	if err != nil || conn == nil {
		t.Fatalf("newEventsNATSConn() = (%v, %v), want a connection", conn, err)
	}
	t.Cleanup(conn.Close)

	store, err := newCanvasPresenceStore(context.Background(), conn, envLookup(nil))
	if err != nil || store == nil {
		t.Fatalf("newCanvasPresenceStore() = (%v, %v), want a store", store, err)
	}
	t.Cleanup(store.Close)
	if conn.Opts.ReconnectedCB == nil {
		t.Fatal("the presence store did not hook the connection's reconnect handler for its resync")
	}
}

// The async error handler logs a slow consumer with its subject (never a
// payload) and collapses a burst into one line per interval.
func TestAsyncErrorLoggerDeduplicatesBursts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	now := time.Unix(1_000, 0)
	l := newAsyncErrorLogger(logger, time.Minute, func() time.Time { return now })
	sub := &nats.Subscription{Subject: "gateway.events.project.7.events"}

	for i := 0; i < 500; i++ {
		l.handle(nil, sub, nats.ErrSlowConsumer)
	}
	if got := strings.Count(buf.String(), "msg=\"live-update NATS slow consumer"); got != 1 {
		t.Fatalf("logged %d slow-consumer lines for one burst, want 1; log=%q", got, buf.String())
	}
	if !strings.Contains(buf.String(), "subject=gateway.events.project.7.events") {
		t.Fatalf("slow-consumer line does not name the subject; log=%q", buf.String())
	}

	// A different subject is its own key.
	l.handle(nil, &nats.Subscription{Subject: "gateway.events.project.8.events"}, nats.ErrSlowConsumer)
	if got := strings.Count(buf.String(), "msg=\"live-update NATS slow consumer"); got != 2 {
		t.Fatalf("a second subject was suppressed by the first one's window; log=%q", buf.String())
	}

	// After the interval the next one logs and reports what was suppressed.
	now = now.Add(2 * time.Minute)
	l.handle(nil, sub, nats.ErrSlowConsumer)
	if !strings.Contains(buf.String(), "suppressed_since_last_log=499") {
		t.Fatalf("the post-window line does not report the suppressed count; log=%q", buf.String())
	}

	// A connection-level error (no subscription) logs too.
	l.handle(nil, nil, errors.New("permissions violation"))
	if !strings.Contains(buf.String(), "async error") {
		t.Fatalf("a non-slow-consumer async error was not logged; log=%q", buf.String())
	}
	l.handle(nil, nil, nil) // must not panic
}
