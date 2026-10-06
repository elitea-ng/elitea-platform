package nats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// TestSecuredGatewayRunsOnTheChartsPermissions runs the gateway's real NATS
// client — Connect, the bind, every counter, KV and publish operation — as
// the elitea-llm-gateway identity against a nats-server started from the NATS
// chart's own rendered config, with the assets the real bootstrap.sh created
// (#1076). A grant missing from deploy/helm/nats/values.yaml fails here.
func TestSecuredGatewayRunsOnTheChartsPermissions(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	m := s.Material(natsconn.IdentityGateway)
	ctx := context.Background()

	c, err := Connect(ctx, Config{URL: s.URL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile})
	if err != nil {
		t.Fatalf("Connect as the gateway: %v\nserver log:\n%s", err, s.Log())
	}
	defer c.Close()

	subject := BudgetSubject("project", "42", 1700000000)
	if total, err := c.IncrBudget(ctx, subject, 500); err != nil || total != 500 {
		t.Fatalf("IncrBudget = %d, %v", total, err)
	}
	if total, err := c.ReadBudget(ctx, subject); err != nil || total != 500 {
		t.Fatalf("ReadBudget = %d, %v", total, err)
	}
	if total, applied, err := c.IncrBudgetIdempotent(ctx, subject, "recovery.1", 100); err != nil || !applied || total != 600 {
		t.Fatalf("IncrBudgetIdempotent = %d, %v, %v", total, applied, err)
	}
	if _, applied, err := c.IncrBudgetIdempotent(ctx, subject, "recovery.1", 100); err != nil || applied {
		t.Fatalf("IncrBudgetIdempotent replay: applied=%v err=%v; the bootstrap's duplicate window must suppress it", applied, err)
	}
	rl := RateLimitSubject(RateKindRequests, "rule1_p42", WindowStart(time.Now()))
	if total, err := c.IncrRateLimit(ctx, rl, 1); err != nil || total != 1 {
		t.Fatalf("IncrRateLimit = %d, %v", total, err)
	}
	if total, err := c.ReadRateLimit(ctx, rl); err != nil || total != 1 {
		t.Fatalf("ReadRateLimit = %d, %v", total, err)
	}
	if fire, err := c.TryAlertCooldown(ctx, "project.42.soft"); err != nil || !fire {
		t.Fatalf("TryAlertCooldown first = %v, %v", fire, err)
	}
	if fire, err := c.TryAlertCooldown(ctx, "project.42.soft"); err != nil || fire {
		t.Fatalf("TryAlertCooldown second = %v, %v", fire, err)
	}
	if err := c.PublishDelta(ctx, "evt-1", []byte(`{"event_id":"evt-1"}`)); err != nil {
		t.Fatalf("PublishDelta: %v", err)
	}
	if err := c.PublishSoftAlertEvent(ctx, "42", []byte(`{}`)); err != nil {
		t.Fatalf("PublishSoftAlertEvent: %v", err)
	}
	if err := c.PublishOpsEvent(ctx, []byte(`{}`)); err != nil {
		t.Fatalf("PublishOpsEvent: %v", err)
	}
	s.RequireNoViolations(t, natsconn.IdentityGateway)

	// What the gateway's identity must NOT do, asserted against the server's
	// own log rather than inferred from a client error.
	nc := dialAs(t, s, natsconn.IdentityGateway)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// A refused request gets no reply, so each one waits out its own timeout.
	op := func() context.Context {
		octx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)
		return octx
	}
	if _, err := js.CreateOrUpdateStream(op(), jetstream.StreamConfig{Name: BudgetStream, Subjects: []string{budgetSubjectRoot + ".>"}, AllowMsgCounter: true}); err == nil {
		t.Error("the gateway updated a stream")
	}
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.UPDATE."+BudgetStream)
	budgetStream, err := js.Stream(op(), BudgetStream)
	if err != nil {
		t.Fatalf("stream info on %s: %v", BudgetStream, err)
	}
	if err := budgetStream.Purge(op()); err == nil {
		t.Error("the gateway purged the budget counters")
	}
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.PURGE."+BudgetStream)
	if err := js.DeleteStream(op(), DeltasStream); err == nil {
		t.Error("the gateway deleted the deltas stream")
	}
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.STREAM.DELETE."+DeltasStream)
	// No consumer of any kind: the gateway only publishes and reads counters,
	// so it cannot point a push consumer's deliver subject anywhere, nor
	// redefine the scheduler's write-back consumer.
	if _, err := js.CreateOrUpdateConsumer(op(), DeltasStream, jetstream.ConsumerConfig{Durable: "budget-writeback", FilterSubject: DeltaSubject}); err == nil {
		t.Error("the gateway created or redefined a consumer on the deltas stream")
	}
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$JS.API.CONSUMER.CREATE."+DeltasStream+".budget-writeback."+DeltaSubject)
	_ = nc.Publish("$KV.ELITEA_CANVAS_PRESENCE.x.y", []byte("{}"))
	_ = nc.Publish("elitea.rt.v1.agent.d.x", []byte("{}"))
	if _, err := nc.SubscribeSync("gateway.events.project.>"); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "$KV.ELITEA_CANVAS_PRESENCE.x.y")
	s.RequireViolation(t, natsconn.IdentityGateway, "Publish", "elitea.rt.v1.agent.d.x")
	s.RequireViolation(t, natsconn.IdentityGateway, "Subscription", "gateway.events.project.>")
}

// The client refuses what the server would refuse, and the server refuses a
// certificate that maps to no user.
func TestSecuredGatewayRefusals(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()

	if _, err := Connect(ctx, Config{URL: s.URL()}); err == nil {
		t.Error("Connect to a tls:// URL with no client material succeeded")
	}
	m := s.Material(natsconn.IdentityGateway)
	if _, err := Connect(ctx, Config{URL: s.PlainURL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile}); err == nil {
		t.Error("Connect accepted a nats:// URL beside client material")
	}
	other := s.Material(natstest.IdentityNoSAN)
	_, err := Connect(ctx, Config{URL: s.URL(), TLSCAFile: other.CAFile, TLSCertFile: other.CertFile, TLSKeyFile: other.KeyFile})
	if err == nil {
		t.Fatal("a certificate with no URI SAN connected")
	}
	if !errors.Is(err, nats.ErrAuthorization) && !strings.Contains(strings.ToLower(err.Error()), "authorization") {
		t.Errorf("refusal is not an authorization error: %v", err)
	}
}

// A gateway started before the bootstrap ran fails with the fix named.
func TestSecuredGatewayWithoutBootstrapNamesTheJob(t *testing.T) {
	s := natstest.Start(t)
	m := s.Material(natsconn.IdentityGateway)
	_, err := Connect(context.Background(), Config{URL: s.URL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile})
	if err == nil || !strings.Contains(err.Error(), "nats-bootstrap") {
		t.Fatalf("Connect before the bootstrap: %v; want an error naming nats-bootstrap", err)
	}
	s.RequireNoViolations(t, natsconn.IdentityGateway)
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
