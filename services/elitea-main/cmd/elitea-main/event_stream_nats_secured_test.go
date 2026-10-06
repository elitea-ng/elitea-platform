package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/natsbus"
)

// TestSecuredLiveUpdatePlaneRunsOnTheChartsPermissions runs elitea-main's real
// boot path — newEventsNATSConn from the environment the chart renders,
// newCanvasPresenceStore, the presence store's writes, watcher and resync,
// and the natsbus relay — as the elitea-main identity against a nats-server
// started from the NATS chart's own rendered config, after the real
// bootstrap.sh created the assets (#1076). A grant missing from
// deploy/helm/nats/values.yaml fails here, not in a cluster.
func TestSecuredLiveUpdatePlaneRunsOnTheChartsPermissions(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()
	env := s.Lookup(eventsNATSPrefix, natsconn.IdentityMain, map[string]string{eventsNATSURLEnv: s.URL()})

	replicas := make([]*v2canvaspresence.NATSStore, 2)
	conn, err := newEventsNATSConn(env, nil)
	if err != nil || conn == nil {
		t.Fatalf("newEventsNATSConn as elitea-main = (%v, %v)\nserver log:\n%s", conn, err, s.Log())
	}
	t.Cleanup(conn.Close)
	for i := range replicas {
		c := conn
		if i > 0 {
			c, err = newEventsNATSConn(env, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)
		}
		store, err := newCanvasPresenceStore(ctx, c)
		if err != nil {
			t.Fatalf("newCanvasPresenceStore: %v\nserver log:\n%s", err, s.Log())
		}
		t.Cleanup(store.Close)
		replicas[i] = store
	}

	// Presence: write on one replica, read on the other (the watcher), remove.
	key := "canvas:prompt_lib:42:1"
	ada := v2canvaspresence.Editor{UserID: "1", UserName: "ada"}
	if err := replicas[0].Touch(ctx, key, ada, v2canvaspresence.TTL); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	waitRoster(t, replicas[1], key, 1)
	replicas[1].Resync() // a fresh watcher: consumer delete + create
	if err := replicas[0].Remove(ctx, key, ada.UserID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitRoster(t, replicas[1], key, 0)

	// The relay: what the SSE route reads, and what presence publishes.
	bus := natsbus.NewFromConn(conn, "elitea-main")
	events, cancel, err := bus.Raw(ctx, "project:42:events")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "project:42:events", v2canvaspresence.EventType, map[string]any{"editors": []any{}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ev := <-events:
		if ev.Type != v2canvaspresence.EventType {
			t.Errorf("relayed %q", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the published roster never reached the subscriber")
	}

	// A server restart: the connection redials with the same callbacks and
	// the store keeps working.
	s.Restart(t)
	deadline := time.Now().Add(15 * time.Second)
	for replicas[0].Touch(ctx, key, ada, v2canvaspresence.TTL) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("no beat landed after the restart\nserver log:\n%s", s.Log())
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitRoster(t, replicas[1], key, 1)
	s.RequireNoViolations(t, natsconn.IdentityMain)

	// What elitea-main's identity must NOT do.
	_ = conn.Publish("gateway.budget.counter.project.42.1700000000", []byte("1"))
	_ = conn.Publish("gateway.budget.delta", []byte("{}"))
	_ = conn.Publish("gateway.events.ops.budget", []byte("{}"))
	_ = conn.Publish("elitea.rt.v1.agent.d.x", []byte("{}"))
	if _, err := conn.SubscribeSync("gateway.budget.>"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.SubscribeSync("_INBOX_elitea-llm-gateway.>"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Flush()
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "gateway.budget.counter.project.42.1700000000")
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "gateway.budget.delta")
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "gateway.events.ops.budget")
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "elitea.rt.v1.agent.d.x")
	s.RequireViolation(t, natsconn.IdentityMain, "Subscription", "gateway.budget.>")
	s.RequireViolation(t, natsconn.IdentityMain, "Subscription", "_INBOX_elitea-llm-gateway.>")

	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	op := func() context.Context {
		c, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)
		return c
	}
	if _, err := js.UpdateStream(op(), jetstream.StreamConfig{
		Name: "KV_" + v2canvaspresence.PresenceBucket, Subjects: []string{"$KV." + v2canvaspresence.PresenceBucket + ".>"}, MaxAge: time.Hour,
	}); err == nil {
		t.Error("elitea-main reconfigured the presence bucket")
	}
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "$JS.API.STREAM.UPDATE.KV_"+v2canvaspresence.PresenceBucket)
	if err := js.DeleteKeyValue(op(), v2canvaspresence.PresenceBucket); err == nil {
		t.Error("elitea-main deleted the presence bucket")
	}
	s.RequireViolation(t, natsconn.IdentityMain, "Publish", "$JS.API.STREAM.DELETE.KV_"+v2canvaspresence.PresenceBucket)
}

// Booting before the bootstrap ran fails with the Job named, and binding
// (a stream info) is not itself a violation.
func TestSecuredLiveUpdatePlaneWithoutBootstrapNamesTheJob(t *testing.T) {
	s := natstest.Start(t)
	env := s.Lookup(eventsNATSPrefix, natsconn.IdentityMain, map[string]string{eventsNATSURLEnv: s.URL()})
	conn, err := newEventsNATSConn(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	if _, err := newCanvasPresenceStore(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "nats-bootstrap") {
		t.Fatalf("newCanvasPresenceStore before the bootstrap = %v; want an error naming nats-bootstrap", err)
	}
	s.RequireNoViolations(t, natsconn.IdentityMain)
}

// Another service's certificate is not elitea-main: it connects, but as the
// user its SAN names, with that user's permissions.
func TestSecuredLiveUpdatePlaneRefusesAPlaintextURL(t *testing.T) {
	s := natstest.Start(t)
	env := s.Lookup(eventsNATSPrefix, natsconn.IdentityMain, map[string]string{eventsNATSURLEnv: s.PlainURL()})
	if conn, err := newEventsNATSConn(env, nil); err == nil {
		conn.Close()
		t.Fatal("newEventsNATSConn accepted a nats:// URL beside a client identity")
	}
	noIdentity := func(k string) (string, bool) {
		if k == eventsNATSURLEnv {
			return s.URL(), true
		}
		return "", false
	}
	if conn, err := newEventsNATSConn(noIdentity, nil); err == nil {
		conn.Close()
		t.Fatal("newEventsNATSConn dialled tls:// with no client identity")
	}
}

func waitRoster(t *testing.T, store *v2canvaspresence.NATSStore, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		roster, err := store.List(context.Background(), key)
		if err == nil && len(roster) == want {
			return
		}
		if time.Now().After(deadline) {
			b, _ := json.Marshal(roster)
			t.Fatalf("roster %s = %s (err %v), want %d editor(s)", key, b, err, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
