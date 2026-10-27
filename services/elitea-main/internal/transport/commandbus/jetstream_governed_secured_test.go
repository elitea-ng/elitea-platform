package commandbus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/elitea-ng/elitea-platform/libs/go/natsconn"
	"github.com/elitea-ng/elitea-platform/libs/go/natsconn/natstest"
)

// A governed command (ADR-0039, #1115) is published on the governed subject
// and reaches only the governed durable: a worker bound to the legacy durable
// never receives it, whatever it pulls. Run on the chart's permission table
// after the real bootstrap.sh, as every secured command bus test.
func TestGovernedCommandReachesOnlyTheGovernedDurable(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mainJS := securedJetStream(t, s, natsconn.IdentityMainRuntime)
	handle, err := BindStream(ctx, mainJS, StreamRequirement{Stream: StreamAgent, MinMaxAge: 24*time.Hour + MaxAgeMargin, MaxMessageBytes: MaxMessageBytes, Governed: true})
	if err != nil {
		t.Fatalf("the bootstrap's governed agent stream is not the Go contract's: %v", err)
	}
	appender, err := NewJetStreamAppender(mainJS, handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appender.AppendGoverned(ctx, StreamAgent, "outbox-governed", []byte("governed-envelope")); err != nil {
		t.Fatalf("append governed: %v", err)
	}
	if _, err := appender.Append(ctx, StreamAgent, "outbox-legacy", []byte("legacy-envelope")); err != nil {
		t.Fatalf("append legacy: %v", err)
	}

	workerJS := securedJetStream(t, s, natsconn.IdentityWorker)
	fetchAll := func(durable string) []jetstream.Msg {
		t.Helper()
		consumer, err := workerJS.Consumer(ctx, StreamAgent, durable)
		if err != nil {
			t.Fatalf("the worker cannot bind %s: %v", durable, err)
		}
		batch, err := consumer.Fetch(4, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		var msgs []jetstream.Msg
		for msg := range batch.Messages() {
			msgs = append(msgs, msg)
		}
		return msgs
	}
	legacy := fetchAll(ConsumerAgent)
	if len(legacy) != 1 || string(legacy[0].Data()) != "legacy-envelope" || !strings.HasPrefix(legacy[0].Subject(), "elitea.rt.v1.agent.d.") {
		t.Fatalf("the legacy durable received %d commands, want only the legacy one", len(legacy))
	}
	governed := fetchAll(ConsumerAgentGoverned)
	if len(governed) != 1 || string(governed[0].Data()) != "governed-envelope" {
		t.Fatalf("the governed durable received %d commands, want only the governed one", len(governed))
	}
	want, _ := GovernedDeliverySubject(StreamAgent, "outbox-governed")
	if governed[0].Subject() != want || governed[0].Headers().Get(HeaderDeliveryID) != "outbox-governed" {
		t.Fatalf("governed command on %s (delivery %q), want %s", governed[0].Subject(), governed[0].Headers().Get(HeaderDeliveryID), want)
	}
	for _, msg := range append(legacy, governed...) {
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	s.RequireNoViolations(t, natsconn.IdentityWorker)
	s.RequireNoViolations(t, natsconn.IdentityMainRuntime)
}

// A stream without the governed subject is refused when the profile needs
// it, and accepted when it does not: a flag-off Main binds either shape.
func TestBindStreamRequiresTheGovernedSubjectOnlyWhenGoverned(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bootstrapCLI(t, s, "stream", "edit", StreamIndex, "--subjects", "elitea.rt.v1.index.d.*", "--force")

	mainJS := securedJetStream(t, s, natsconn.IdentityMainRuntime)
	want := StreamRequirement{Stream: StreamIndex, MinMaxAge: 24*time.Hour + MaxAgeMargin, MaxMessageBytes: MaxMessageBytes}
	if _, err := BindStream(ctx, mainJS, want); err != nil {
		t.Fatalf("a flag-off bind of a legacy-only stream: %v", err)
	}
	want.Governed = true
	if _, err := BindStream(ctx, mainJS, want); err == nil || !strings.Contains(err.Error(), "governed subject") {
		t.Fatalf("a governed bind of a legacy-only stream = %v", err)
	}
}
