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

// TestSecuredSchedulerCannotStoreIntoGatewayByReplySubject is the closure
// proof for the reply-subject write (#1076): the server checks a client's
// publish permissions on the subject it publishes to, NOT on the reply
// subject of its request, and it publishes a JetStream API answer to that
// reply subject unchecked (only $JS.ACK. and _GR_. replies are refused). In
// the GATEWAY account the scheduler — allowed to pull, read info and ack on
// budget-writeback — could therefore name gateway.budget.delta as the reply
// of a pull and have the server copy deltas back into GATEWAY_BUDGET_DELTAS
// (double-counted spend once past the dedup window; with discard-old, a flood
// evicts real deltas), or steer answers into the counter and KV streams.
//
// The scheduler is now in its own SCHEDULER account, which holds no stream,
// and reaches the consumer through service imports: an answer goes to the
// reply subject IN SCHEDULER. This test makes every such request, as the
// scheduler, and asserts that no GATEWAY stream gains a message.
func TestSecuredSchedulerCannotStoreIntoGatewayByReplySubject(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx := context.Background()

	// Deltas WITHOUT a Nats-Msg-Id, so the stream's duplicate window cannot
	// mask a copy: a delivery stored back would be a new message.
	gw, err := jetstream.New(dialAs(t, s, natsconn.IdentityGateway))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		d := deltaFor(fmt.Sprintf("00000000-0000-0000-0000-0000000000b%d", i), "project", "9", 1_700_000_000, 1_000_000)
		body, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gw.Publish(ctx, DeltaSubject, body); err != nil {
			t.Fatalf("publish delta as the gateway: %v", err)
		}
	}

	admin, err := jetstream.New(dialAs(t, s, natsconn.IdentityBootstrapGateway))
	if err != nil {
		t.Fatal(err)
	}
	streams := []string{DeltasStream, "GATEWAY_BUDGET", "GATEWAY_RATELIMIT", "KV_GATEWAY_ALERT_COOLDOWN"}
	counts := func() map[string]uint64 {
		t.Helper()
		out := map[string]uint64{}
		for _, name := range streams {
			st, err := admin.Stream(ctx, name)
			if err != nil {
				t.Fatalf("stream %s: %v", name, err)
			}
			out[name] = st.CachedInfo().State.Msgs
		}
		return out
	}
	before := counts()
	if before[DeltasStream] != 4 {
		t.Fatalf("%s holds %d messages before the attack, want the 4 just published", DeltasStream, before[DeltasStream])
	}

	m := s.Material(natsconn.IdentityScheduler)
	dial := DialConfig{URL: s.URL(), TLSCAFile: m.CAFile, TLSCertFile: m.CertFile, TLSKeyFile: m.KeyFile}
	nc, err := Dial(dial, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	api := natsconn.SchedulerGatewayJSAPIPrefix
	pull := api + ".CONSUMER.MSG.NEXT." + DeltasStream + "." + DurableName
	info := api + ".CONSUMER.INFO." + DeltasStream + "." + DurableName

	// Control: the same raw requests DO reach the consumer. A one-message
	// pull answered on the scheduler's own inbox delivers a delta, so the
	// requests below are served, not dropped for some unrelated reason.
	inbox := nc.NewRespInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishRequest(pull, inbox, []byte(`{"batch":1,"expires":2000000000}`)); err != nil {
		t.Fatal(err)
	}
	got, err := sub.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("control pull on the scheduler's own inbox delivered nothing: %v\nserver log:\n%s", err, s.Log())
	}
	if got.Reply == "" {
		t.Fatalf("control delivery carries no ack subject: %+v", got)
	}
	if _, err := nc.Request(info, nil, 2*time.Second); err != nil {
		t.Fatalf("control consumer info on the scheduler's own inbox: %v", err)
	}

	// The attack: every request the scheduler may make, answered onto a
	// GATEWAY stream's subject.
	replies := []string{
		DeltaSubject,                        // GATEWAY_BUDGET_DELTAS
		"gateway.budget.counter.project.9",  // GATEWAY_BUDGET
		"gateway.ratelimit.counter.key.9",   // GATEWAY_RATELIMIT
		"$KV.GATEWAY_ALERT_COOLDOWN.attack", // KV_GATEWAY_ALERT_COOLDOWN
	}
	for _, reply := range replies {
		for _, req := range []*nats.Msg{
			{Subject: pull, Reply: reply, Data: []byte(`{"batch":10,"expires":500000000}`)},
			{Subject: pull, Reply: reply, Data: []byte(`{"batch":10,"no_wait":true}`)},
			{Subject: info, Reply: reply},
			// An ack with a reply is answered (AckSync); the control
			// delivery's ack subject is one the scheduler may publish.
			{Subject: got.Reply, Reply: reply, Data: []byte("+NXT")},
		} {
			if err := nc.PublishMsg(req); err != nil {
				t.Fatalf("publish %s (reply %s): %v", req.Subject, reply, err)
			}
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	// Longer than the pulls' expiry, so every answer the server would send
	// has been sent.
	time.Sleep(1500 * time.Millisecond)

	after := counts()
	for _, name := range streams {
		if after[name] != before[name] {
			t.Errorf("%s went from %d to %d messages: the scheduler made the server store a reply into GATEWAY", name, before[name], after[name])
		}
	}
	// The requests themselves were permitted (the scheduler's grants), so a
	// pass here is account isolation, not a permissions refusal.
	s.RequireNoViolations(t, natsconn.IdentityScheduler)
}
