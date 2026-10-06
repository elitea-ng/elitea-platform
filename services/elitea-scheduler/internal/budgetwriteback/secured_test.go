package budgetwriteback

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// TestSecuredWriteBackRunsOnTheChartsPermissions runs the scheduler's real
// write-back path — Dial with its identity, Bind (the durable consumer's
// create-or-update), the drain loop's fetches and acks, and a second Bind as
// a restarted scheduler — as the elitea-scheduler identity against a
// nats-server started from the NATS chart's own rendered config, after the
// real bootstrap.sh created the stream (#1076). The deltas are published as
// the gateway, which is who publishes them in production.
func TestSecuredWriteBackRunsOnTheChartsPermissions(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()

	gw := dialAs(t, s, natsconn.IdentityGateway)
	gwJS, err := jetstream.New(gw)
	if err != nil {
		t.Fatal(err)
	}
	const period = int64(1_700_000_000)
	for i := 0; i < 3; i++ {
		d := deltaFor(fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", i), "project", "42", period, 1_000_000_000)
		body, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gwJS.Publish(ctx, DeltaSubject, body, jetstream.WithMsgID(d.EventID)); err != nil {
			t.Fatalf("publish delta as the gateway: %v", err)
		}
	}

	m := s.Material(natsconn.IdentityScheduler)
	dial := DialConfig{URL: s.URL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile}
	nc, err := Dial(dial, nil)
	if err != nil {
		t.Fatalf("Dial as the scheduler: %v\nserver log:\n%s", err, s.Log())
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	tbl := newAcctTable()
	bindCtx, cancelBind := context.WithTimeout(ctx, 5*time.Second)
	cons, err := Bind(bindCtx, js, &tableDB{tbl: tbl}, Config{BatchSize: 10, FetchWait: 200 * time.Millisecond}, nil)
	cancelBind()
	if err != nil {
		t.Fatalf("Bind as the scheduler: %v\nserver log:\n%s", err, s.Log())
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { cons.Run(runCtx); close(done) }()

	// Watch the consumer drain from an identity that may read its info, so
	// the table is only read once the drain loop has stopped.
	admin, err := jetstream.New(dialAs(t, s, natsconn.IdentityBootstrapGateway))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := admin.Consumer(ctx, DeltasStream, DurableName)
		if err == nil {
			info, err := c.Info(ctx)
			if err == nil && info.Delivered.Consumer >= 3 && info.NumAckPending == 0 && info.NumPending == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			stop()
			<-done
			t.Fatalf("the write-back consumer did not ack all three deltas\nserver log:\n%s", s.Log())
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-done
	if got := tbl.rows[deltaKey{scope: "project", scopeID: "42", periodStart: period}]; got == nil || got.accumulatedNano != 3_000_000_000 {
		t.Fatalf("accumulator = %+v, want 3 USD from three deltas", got)
	}

	// A restarted scheduler binds the existing durable consumer again.
	bindCtx, cancelBind = context.WithTimeout(ctx, 5*time.Second)
	defer cancelBind()
	if _, err := Bind(bindCtx, js, &tableDB{tbl: newAcctTable()}, Config{BatchSize: 10}, nil); err != nil {
		t.Fatalf("re-Bind: %v", err)
	}
	s.RequireNoViolations(t, natsconn.IdentityScheduler)

	// What the scheduler's identity must NOT do: inject deltas, read events,
	// read another client's replies, make or remove other consumers, purge.
	_ = nc.Publish(DeltaSubject, []byte("{}"))
	if _, err := nc.SubscribeSync("gateway.events.>"); err != nil {
		t.Fatal(err)
	}
	if _, err := nc.SubscribeSync("_INBOX_elitea-main.>"); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	s.RequireViolation(t, natsconn.IdentityScheduler, "Publish", DeltaSubject)
	s.RequireViolation(t, natsconn.IdentityScheduler, "Subscription", "gateway.events.>")
	s.RequireViolation(t, natsconn.IdentityScheduler, "Subscription", "_INBOX_elitea-main.>")

	op := func() context.Context {
		c, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)
		return c
	}
	if _, err := js.CreateOrUpdateConsumer(op(), DeltasStream, jetstream.ConsumerConfig{Durable: "rogue", FilterSubject: DeltaSubject}); err == nil {
		t.Error("the scheduler created a consumer other than its own")
	}
	s.RequireViolation(t, natsconn.IdentityScheduler, "Publish", "$JS.API.CONSUMER.CREATE."+DeltasStream+".rogue."+DeltaSubject)
	if err := js.DeleteConsumer(op(), DeltasStream, DurableName); err == nil {
		t.Error("the scheduler deleted its durable consumer")
	}
	s.RequireViolation(t, natsconn.IdentityScheduler, "Publish", "$JS.API.CONSUMER.DELETE."+DeltasStream+"."+DurableName)
}

// Dial refuses what the server would refuse.
func TestSecuredWriteBackDialRefusals(t *testing.T) {
	s := natstest.Start(t)
	m := s.Material(natsconn.IdentityScheduler)
	for name, cfg := range map[string]DialConfig{
		"tls url, no identity":     {URL: s.URL()},
		"nats url with identity":   {URL: s.PlainURL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile},
		"credential with identity": {URL: "tls://u:p@127.0.0.1:1", TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile},
		"half identity":            {URL: s.URL(), TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile},
	} {
		if nc, err := Dial(cfg, nil); err == nil {
			nc.Close()
			t.Errorf("%s: Dial succeeded", name)
		}
	}
	other := s.Material(natstest.IdentityUnknown)
	if nc, err := Dial(DialConfig{URL: s.URL(), TLSCAFile: other.CAFile, TLSCertFile: other.CertFile, TLSKeyFile: other.KeyFile}, nil); err == nil {
		nc.Close()
		t.Error("a certificate that names no user connected")
	}
}

func dialAs(t *testing.T, s *natstest.Server, identity string) *nats.Conn {
	t.Helper()
	m := s.Material(identity)
	nc, err := nats.Connect(s.URL(),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(identity)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(m.ClientCertificate, m.RootCAs),
		nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}),
	)
	if err != nil {
		t.Fatalf("dial as %s: %v", identity, err)
	}
	t.Cleanup(nc.Close)
	return nc
}
