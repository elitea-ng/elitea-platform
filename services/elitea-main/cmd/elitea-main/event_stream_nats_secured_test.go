package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
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

// The planes are separate NATS accounts (#1076 F1). A JetStream push
// consumer's deliver subject is not checked against its creator's publish
// permissions, so elitea-main's presence-watcher grant (consumer create on
// its own bucket) lets it point a delivery at ANY subject. In one shared
// account that wrote into the gateway's GATEWAY_BUDGET_DELTAS; with one
// account per plane the delivery lands in MAIN, where no gateway stream is.
func TestSecuredRedirectedDeliveryStaysInMain(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()
	env := s.Lookup(eventsNATSPrefix, natsconn.IdentityMain, map[string]string{eventsNATSURLEnv: s.URL()})
	conn, err := newEventsNATSConn(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, v2canvaspresence.PresenceBucket)
	if err != nil {
		t.Fatal(err)
	}
	forged := []byte(`{"event_id":"forged-1","scope":"project","scope_id":"7","amount_nano":-1000000000}`)
	if _, err := kv.Put(ctx, "evil", forged); err != nil {
		t.Fatal(err)
	}
	stream := "KV_" + v2canvaspresence.PresenceBucket
	filter := "$KV." + v2canvaspresence.PresenceBucket + ".evil"

	// The control: a redirected push consumer DOES deliver, onto a subject
	// main may read. Without it, "nothing arrived" below could mean the
	// consumer never delivered at all.
	control, err := conn.SubscribeSync("gateway.events.project.9.events")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdatePushConsumer(ctx, stream, jetstream.ConsumerConfig{
		Name: "redirect-control", FilterSubject: filter, DeliverSubject: "gateway.events.project.9.events",
		AckPolicy: jetstream.AckNonePolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("create the control push consumer: %v\nserver log:\n%s", err, s.Log())
	}
	if msg, err := control.NextMsg(5 * time.Second); err != nil || string(msg.Data) != string(forged) {
		t.Fatalf("the control consumer delivered %v, %v; the redirect under test would prove nothing", msg, err)
	}

	// The attack: deliver the forged value onto the gateway's delta subject.
	if _, err := js.CreateOrUpdatePushConsumer(ctx, stream, jetstream.ConsumerConfig{
		Name: "redirect-to-deltas", FilterSubject: filter, DeliverSubject: "gateway.budget.delta",
		AckPolicy: jetstream.AckNonePolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("create the redirected push consumer: %v", err)
	}
	gw, err := nats.Connect(s.URL(),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityGateway)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(s.Material(natsconn.IdentityGateway).ClientCertificate, s.Material(natsconn.IdentityGateway).RootCAs),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	gwJS, err := jetstream.New(gw)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the redirected consumer has had its chance to deliver
	deltas, err := gwJS.Stream(ctx, "GATEWAY_BUDGET_DELTAS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := deltas.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("GATEWAY_BUDGET_DELTAS holds %d message(s) after elitea-main redirected a delivery at gateway.budget.delta: the planes share an account", info.State.Msgs)
	}
	s.RequireNoViolations(t, natsconn.IdentityGateway)
}

// The one flow that crosses accounts: the gateway's per-project soft alert,
// exported by GATEWAY and imported by MAIN, reaches the project relay.
func TestSecuredSoftAlertCrossesFromGatewayToMain(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()
	env := s.Lookup(eventsNATSPrefix, natsconn.IdentityMain, map[string]string{eventsNATSURLEnv: s.URL()})
	conn, err := newEventsNATSConn(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	bus := natsbus.NewFromConn(conn, "elitea-main")
	events, cancel, err := bus.Raw(ctx, "project:42:events")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	gw, err := nats.Connect(s.URL(),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityGateway)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(s.Material(natsconn.IdentityGateway).ClientCertificate, s.Material(natsconn.IdentityGateway).RootCAs),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	alert, err := json.Marshal(natsbus.Event{Type: "budget.soft_alert", Source: "elitea-llm-gateway", Payload: json.RawMessage(`{"project_id":42}`), Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Publish("gateway.events.project.42.events", alert); err != nil {
		t.Fatal(err)
	}
	if err := gw.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Type != "budget.soft_alert" {
			t.Errorf("relayed %q", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the gateway's soft alert never reached MAIN\nserver log:\n%s", s.Log())
	}
	s.RequireNoViolations(t, natsconn.IdentityMain)
	s.RequireNoViolations(t, natsconn.IdentityGateway)
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
