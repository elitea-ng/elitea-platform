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
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
)

// bootstrapPresenceBucket creates the presence bucket with the settings
// deploy/helm/nats-bootstrap/files/bootstrap.sh gives it, on a plaintext test
// server that has no bootstrap of its own.
func bootstrapPresenceBucket(t *testing.T, conn *nats.Conn) {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: v2canvaspresence.PresenceBucket, History: 1, TTL: 2 * time.Minute, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("bootstrap the presence bucket: %v", err)
	}
}

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

// The client identity and the URL must agree before anything is dialled:
// with a certificate every URL is tls:// with no credential, and a tls://
// URL needs a certificate (#1076).
func TestEventsNATSConnRefusesAnIdentityAndURLThatDisagree(t *testing.T) {
	t.Parallel()

	material := map[string]string{
		eventsNATSTLSCAFileEnv:   "/etc/nats-client/ca.crt",
		eventsNATSTLSCertFileEnv: "/etc/nats-client/tls.crt",
		eventsNATSTLSKeyFileEnv:  "/etc/nats-client/tls.key",
	}
	with := func(url string, drop ...string) map[string]string {
		env := map[string]string{eventsNATSURLEnv: url}
		for k, v := range material {
			env[k] = v
		}
		for _, k := range drop {
			delete(env, k)
		}
		return env
	}
	for name, env := range map[string]map[string]string{
		"tls url, no identity":     {eventsNATSURLEnv: "tls://elitea-nats:4222"},
		"nats url with identity":   with("nats://elitea-nats:4222"),
		"credential with identity": with("tls://user:s3cret@elitea-nats:4222"),
		"identity without its key": with("tls://elitea-nats:4222", eventsNATSTLSKeyFileEnv),
		"identity without the CA":  with("tls://elitea-nats:4222", eventsNATSTLSCAFileEnv),
		"unreadable identity":      with("tls://elitea-nats:4222"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn, err := newEventsNATSConn(envLookup(env), nil)
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Fatal("newEventsNATSConn() accepted it")
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q leaks the credential", err)
			}
		})
	}
}

// The three TLS variables are the ones natsconn reads for this prefix; the
// constants exist so the env-drift gate can see them.
func TestEventsNATSTLSEnvNamesMatchNatsconn(t *testing.T) {
	t.Parallel()

	got := natsconn.EnvNames(eventsNATSPrefix)
	want := [3]string{eventsNATSTLSCAFileEnv, eventsNATSTLSCertFileEnv, eventsNATSTLSKeyFileEnv}
	if got != want {
		t.Fatalf("natsconn.EnvNames(%q) = %v, the constants say %v", eventsNATSPrefix, got, want)
	}
}

func TestCanvasPresenceStoreRequiresAConnection(t *testing.T) {
	t.Parallel()

	if _, err := newCanvasPresenceStore(context.Background(), nil); err == nil {
		t.Fatal("newCanvasPresenceStore(nil conn) error = nil, want an error")
	}
}

// The boot path end to end against a real JetStream server: the URL dials,
// and the store binds to the presence bucket the bootstrap created (created
// here the way the nats-bootstrap Job creates it).
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

	bootstrapPresenceBucket(t, conn)
	store, err := newCanvasPresenceStore(context.Background(), conn)
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
