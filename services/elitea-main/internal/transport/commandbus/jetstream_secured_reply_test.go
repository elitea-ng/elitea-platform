package commandbus

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
)

// TestSecuredWorkerCannotStoreIntoCommandStreamsByReplySubject is the closure
// proof for the reply-subject write on the command bus (#1081 review, S1/S3).
//
// The server checks a client's publish permissions on the subject it
// publishes to, NOT on the reply subject of its request, and it publishes a
// JetStream API answer to that reply subject unchecked (only $JS.ACK. and
// _GR_. replies are refused). When elitea-worker was a RUNTIME user, its
// MSG.NEXT grant let it name elitea.rt.v1.<route>.d.<token> as the reply of a
// pull and have the server copy a signed command into another route's stream,
// and its STREAM.INFO/CONSUMER.INFO grants let it fill ELITEA_RT_V1_AGENT to
// MaxMsgs with info answers: the producer was then refused ("maximum messages
// exceeded") and, with no delete or purge grant anywhere, the junk stayed
// until MaxAge (up to 26h).
//
// The worker is now in its own WORKER account, which holds no command stream,
// and reaches the three durables through service imports: an answer goes to
// the reply subject IN WORKER. This test makes every request the worker may
// make — pull, consumer info, and an ack with a reply — with replies on every
// route's command subject, and asserts that no RUNTIME stream changes. It
// also asks for answers on WORKER's own JetStream API subjects (a stream a
// reply could create) and asserts WORKER still holds only its dead-letter
// bucket.
//
// The producer stays in RUNTIME (it is the streams' writer), so it can still
// steer its own requests' answers into a command stream — something its
// publish grant already allows. What it must NOT reach is the dead-letter
// bucket the workers write (S3): a forged record would raise the operator
// alert for a command that was never poison. That half is asserted too.
func TestSecuredWorkerCannotStoreIntoCommandStreamsByReplySubject(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	type route struct{ token, stream, durable string }
	routes := []route{
		{"validate", StreamValidate, ConsumerValidate},
		{"agent", StreamAgent, ConsumerAgent},
		{"index", StreamIndex, ConsumerIndex},
	}

	// Two commands per route, published by the producer exactly as
	// elitea-main does, so every pull below has something to copy.
	mainJS := securedJetStream(t, s, natsconn.IdentityMainRuntime)
	for _, r := range routes {
		for i := 0; i < 2; i++ {
			subject := DeliverySubjectMust(t, r.stream, fmt.Sprintf("closure-%s-%d", r.token, i))
			msg := NewCommandMessage(subject, fmt.Sprintf("closure-%s-%d", r.token, i), []byte("signed-envelope"))
			if _, err := mainJS.PublishMsg(ctx, msg); err != nil {
				t.Fatalf("publish a %s command as the producer: %v", r.token, err)
			}
		}
	}

	runtimeAdmin := securedJetStream(t, s, natsconn.IdentityBootstrapRuntime)
	workerAdmin := securedJetStream(t, s, natsconn.IdentityBootstrapWorker)
	type snapshot struct {
		runtime map[string]uint64
		worker  []string
		letters uint64
	}
	take := func() snapshot {
		t.Helper()
		snap := snapshot{runtime: map[string]uint64{}}
		for _, r := range routes {
			st, err := runtimeAdmin.Stream(ctx, r.stream)
			if err != nil {
				t.Fatalf("stream %s: %v", r.stream, err)
			}
			snap.runtime[r.stream] = st.CachedInfo().State.Msgs
		}
		names := workerAdmin.StreamNames(ctx)
		for name := range names.Name() {
			snap.worker = append(snap.worker, name)
		}
		if err := names.Err(); err != nil {
			t.Fatalf("WORKER stream names: %v", err)
		}
		st, err := workerAdmin.Stream(ctx, "KV_"+DeadLetterBucket)
		if err != nil {
			t.Fatalf("dead-letter bucket: %v", err)
		}
		snap.letters = st.CachedInfo().State.Msgs
		return snap
	}
	before := take()
	for _, r := range routes {
		if before.runtime[r.stream] != 2 {
			t.Fatalf("%s holds %d commands before the attack, want the 2 just published", r.stream, before.runtime[r.stream])
		}
	}
	if strings.Join(before.worker, ",") != "KV_"+DeadLetterBucket {
		t.Fatalf("WORKER holds streams %v, want only KV_%s", before.worker, DeadLetterBucket)
	}

	worker := securedConn(t, s, natsconn.IdentityWorker)
	api := natsconn.WorkerRuntimeJSAPIPrefix

	// Control: the same raw requests DO reach each durable. A one-message
	// pull answered on the worker's own inbox delivers a command, so the
	// requests below are served, not dropped for some unrelated reason.
	acks := map[string]string{}
	for _, r := range routes {
		inbox := worker.NewRespInbox()
		sub, err := worker.SubscribeSync(inbox)
		if err != nil {
			t.Fatal(err)
		}
		pull := api + ".CONSUMER.MSG.NEXT." + r.stream + "." + r.durable
		if err := worker.PublishRequest(pull, inbox, []byte(`{"batch":1,"expires":2000000000}`)); err != nil {
			t.Fatal(err)
		}
		got, err := sub.NextMsg(3 * time.Second)
		if err != nil {
			t.Fatalf("control pull of %s on the worker's own inbox delivered nothing: %v\nserver log:\n%s", r.stream, err, s.Log())
		}
		if !strings.HasPrefix(got.Reply, "$JS.ACK."+r.stream+"."+r.durable+".") {
			t.Fatalf("control delivery of %s carries ack subject %q", r.stream, got.Reply)
		}
		acks[r.stream] = got.Reply
		_ = sub.Unsubscribe()
		if _, err := worker.Request(api+".CONSUMER.INFO."+r.stream+"."+r.durable, nil, 2*time.Second); err != nil {
			t.Fatalf("control consumer info of %s on the worker's own inbox: %v", r.stream, err)
		}
	}

	// The attack: every request the worker may make, answered onto every
	// route's command subject and onto WORKER's own JetStream API.
	var replies []string
	for _, r := range routes {
		replies = append(replies,
			"elitea.rt.v1."+r.token+".d."+strings.Repeat("ab", 32),
			"elitea.rt.v1."+r.token+".d."+DeliveryToken("closure-"+r.token+"-0"),
		)
	}
	replies = append(replies,
		"$JS.API.STREAM.CREATE.ELITEA_RT_V1_AGENT",
		"$JS.API.STREAM.CREATE.ROGUE",
		"$JS.API.CONSUMER.CREATE.ELITEA_RT_V1_AGENT.rogue",
	)
	sent := 0
	for _, reply := range replies {
		for _, r := range routes {
			pull := api + ".CONSUMER.MSG.NEXT." + r.stream + "." + r.durable
			for _, req := range []*nats.Msg{
				{Subject: pull, Reply: reply, Data: []byte(`{"batch":10,"expires":500000000}`)},
				{Subject: pull, Reply: reply, Data: []byte(`{"batch":10,"no_wait":true}`)},
				{Subject: api + ".CONSUMER.INFO." + r.stream + "." + r.durable, Reply: reply},
				// An ack with a reply is answered (AckSync); +WPI keeps the
				// control delivery pending, so it stays a valid target.
				{Subject: acks[r.stream], Reply: reply, Data: []byte("+WPI")},
			} {
				if err := worker.PublishMsg(req); err != nil {
					t.Fatalf("publish %s (reply %s): %v", req.Subject, reply, err)
				}
				sent++
			}
		}
	}
	if err := worker.Flush(); err != nil {
		t.Fatal(err)
	}
	// Longer than the pulls' expiry, so every answer the server would send
	// has been sent.
	time.Sleep(1500 * time.Millisecond)

	// S3: the producer's own requests, answered onto the dead-letter
	// bucket's subjects. Its grants are STREAM.INFO, DIRECT.GET and
	// CONSUMER.INFO; each answer lands in RUNTIME, where no bucket lives.
	mainConn := securedConn(t, s, natsconn.IdentityMainRuntime)
	for _, r := range routes {
		for _, req := range []*nats.Msg{
			{Subject: "$JS.API.STREAM.INFO." + r.stream},
			{Subject: "$JS.API.CONSUMER.INFO." + r.stream + "." + r.durable},
			// The re-offer compare's direct get, in the last-by-subject form
			// the producer uses (its grant is DIRECT.GET.<stream>.>).
			{Subject: "$JS.API.DIRECT.GET." + r.stream + "." + DeliverySubjectMust(t, r.stream, "closure-"+r.token+"-1")},
		} {
			req.Reply = "$KV." + DeadLetterBucket + "." + r.token + "." + strings.Repeat("cd", 32)
			if err := mainConn.PublishMsg(req); err != nil {
				t.Fatalf("publish %s as the producer: %v", req.Subject, err)
			}
		}
	}
	if err := mainConn.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	after := take()
	for _, r := range routes {
		if after.runtime[r.stream] != before.runtime[r.stream] {
			t.Errorf("%s went from %d to %d messages after %d worker requests: a reply subject the worker chose stored a message into RUNTIME",
				r.stream, before.runtime[r.stream], after.runtime[r.stream], sent)
		}
	}
	if strings.Join(after.worker, ",") != strings.Join(before.worker, ",") {
		t.Errorf("WORKER's streams went from %v to %v: a reply subject created a stream", before.worker, after.worker)
	}
	if after.letters != before.letters {
		t.Errorf("the dead-letter bucket went from %d to %d records: a reply subject forged a dead letter", before.letters, after.letters)
	}
	// The requests themselves were permitted, so a pass here is account
	// isolation, not a permissions refusal.
	s.RequireNoViolations(t, natsconn.IdentityWorker)
	s.RequireNoViolations(t, natsconn.IdentityMainRuntime)

	// And the worker's own path still works end to end through the prefix:
	// bind, pull, ack.
	workerJS := securedJetStream(t, s, natsconn.IdentityWorker)
	consumer, err := workerJS.Consumer(ctx, StreamAgent, ConsumerAgent)
	if err != nil {
		t.Fatalf("bind through %s: %v", api, err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("double ack through the import: %v", err)
		}
	}
	if batch.Error() != nil {
		t.Fatalf("fetch through the import: %v", batch.Error())
	}
	s.RequireNoViolations(t, natsconn.IdentityWorker)
}
