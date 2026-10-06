package commandbus

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

// The command bus's producer path, run against a nats-server started from the
// NATS chart's own rendered permission table, after the real bootstrap.sh
// created the streams (libs/go/natsconn/natstest). Every grant
// elitea-main-runtime uses is exercised here, and the test fails on any
// permissions violation the server logs for it. The worker half is played by
// the elitea-worker identity with plain nats.go, so the two identities'
// boundary is exercised from both sides.
func TestJetStreamAppenderOnTheChartsPermissions(t *testing.T) {
	s := natstest.Start(t)
	// A two-command validation stream makes saturation reachable.
	s.Bootstrap(t, map[string]string{"NATS_RT_VALIDATE_MAX_MSGS": "2"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mainJS := securedJetStream(t, s, natsconn.IdentityMainRuntime)
	handles := make([]jetstream.Stream, 0, 3)
	for stream, deadline := range map[string]time.Duration{
		StreamValidate: time.Minute, StreamAgent: 24 * time.Hour, StreamIndex: 24 * time.Hour,
	} {
		handle, err := BindStream(ctx, mainJS, StreamRequirement{Stream: stream, MinMaxAge: deadline + MaxAgeMargin, MaxMessageBytes: MaxMessageBytes})
		if err != nil {
			t.Fatalf("the bootstrap's %s is not the Go contract's: %v", stream, err)
		}
		handles = append(handles, handle)
	}
	appender, err := NewJetStreamAppender(mainJS, handles...)
	if err != nil {
		t.Fatal(err)
	}

	first := []byte("signed-envelope-1")
	entry, err := appender.Append(ctx, StreamValidate, "outbox-1", first)
	if err != nil || entry != StreamValidate+":1" {
		t.Fatalf("append = %q, %v", entry, err)
	}
	// An ambiguous retry inside the duplicate window: the same entry, no
	// second slot.
	if again, err := appender.Append(ctx, StreamValidate, "outbox-1", first); err != nil || again != entry {
		t.Fatalf("idempotent re-append = %q, %v; want %q", again, err, entry)
	}
	if _, err := appender.Append(ctx, StreamValidate, "outbox-1", []byte("other bytes")); !errors.Is(err, ErrControlDeliveryConflict) {
		t.Fatalf("conflicting bytes for a live delivery = %v", err)
	}

	// The per-subject refusal (what a PostgreSQL re-offer meets once the
	// duplicate window has passed) and the stream-full refusal are both
	// JetStream error 10077. Pin that with raw publishes, then drive the
	// appender's per-subject path by shrinking the window as the bootstrap.
	subject, _ := DeliverySubject(StreamValidate, "outbox-1")
	raw := nats.NewMsg(subject)
	raw.Data = first
	raw.Header.Set(HeaderMsgID, "a-different-id")
	_, err = mainJS.PublishMsg(ctx, raw)
	requireAPIError(t, err, jsErrStoreFailed, "maximum messages per subject exceeded")
	bootstrapCLI(t, s, "stream", "edit", StreamValidate, "--dupe-window", "1s", "--force")
	time.Sleep(1500 * time.Millisecond)
	if again, err := appender.Append(ctx, StreamValidate, "outbox-1", first); err != nil || again != entry {
		t.Fatalf("re-offer of a live delivery past the duplicate window = %q, %v; want %q", again, err, entry)
	}
	if _, err := appender.Append(ctx, StreamValidate, "outbox-1", []byte("other bytes")); !errors.Is(err, ErrControlDeliveryConflict) {
		t.Fatalf("conflicting re-offer past the duplicate window = %v", err)
	}

	// Capacity: two live commands, the third is backpressure, not loss.
	if _, err := appender.Append(ctx, StreamValidate, "outbox-2", []byte("signed-envelope-2")); err != nil {
		t.Fatal(err)
	}
	_, err = appender.Append(ctx, StreamValidate, "outbox-3", []byte("signed-envelope-3"))
	var saturated *ControlStreamSaturatedError
	if !errors.As(err, &saturated) || !errors.Is(err, executionapp.ErrDispatchBackpressured) ||
		saturated.CurrentMessages != 2 || saturated.MaxMessages != 2 {
		t.Fatalf("a full stream = %v (%+v)", err, saturated)
	}
	full := nats.NewMsg(DeliverySubjectMust(t, StreamValidate, "outbox-raw"))
	full.Data = []byte("x")
	_, err = mainJS.PublishMsg(ctx, full)
	requireAPIError(t, err, jsErrStoreFailed, "maximum messages exceeded")

	// The drained gate sees both live commands.
	reader, err := NewIndexV2CutoverReader(mainJS, StreamValidate)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.ReadIndexControlState(ctx)
	if err != nil || state.StreamEntries != 2 || state.DeliveryMappings != 2 || state.PendingEntries != 2 {
		t.Fatalf("live state = %+v, %v", state, err)
	}

	// The worker takes both; an ack (after settlement) frees capacity, and
	// a delivered-but-unacked command still counts as pending.
	workerJS := securedJetStream(t, s, natsconn.IdentityWorker)
	consumer, err := workerJS.Consumer(ctx, StreamValidate, ConsumerValidate)
	if err != nil {
		t.Fatalf("the worker cannot bind its durable: %v", err)
	}
	batch, err := consumer.Fetch(2, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var delivered []jetstream.Msg
	for msg := range batch.Messages() {
		delivered = append(delivered, msg)
	}
	if len(delivered) != 2 || batch.Error() != nil {
		t.Fatalf("worker fetched %d commands: %v", len(delivered), batch.Error())
	}
	for _, msg := range delivered {
		if msg.Headers().Get(HeaderMsgID) != strings.TrimPrefix(msg.Subject(), "elitea.rt.v1.validate.d.") {
			t.Fatalf("the message ID is not the subject's hash token: %q on %s", msg.Headers().Get(HeaderMsgID), msg.Subject())
		}
		if id := msg.Headers().Get(HeaderDeliveryID); DeliveryToken(id) != strings.TrimPrefix(msg.Subject(), "elitea.rt.v1.validate.d.") {
			t.Fatalf("Elitea-Delivery-Id %q does not hash to the subject %s", id, msg.Subject())
		}
	}
	if err := delivered[0].InProgress(); err != nil {
		t.Fatalf("+WPI: %v", err)
	}
	if err := delivered[0].DoubleAck(ctx); err != nil {
		t.Fatalf("double ack: %v", err)
	}
	if _, err := appender.Append(ctx, StreamValidate, "outbox-3", []byte("signed-envelope-3")); err != nil {
		t.Fatalf("an ack did not free capacity: %v", err)
	}
	if err := delivered[1].NakWithDelay(time.Hour); err != nil {
		t.Fatal(err)
	}
	state, err = reader.ReadIndexControlState(ctx)
	if err != nil || state.StreamEntries != 2 || state.PendingEntries == 0 {
		t.Fatalf("state with one nak-delayed and one new command = %+v, %v", state, err)
	}

	// Drift refusal: a MaxAge that would remove live agent commands.
	bootstrapCLI(t, s, "stream", "edit", StreamAgent, "--max-age", "1h", "--force")
	if _, err := BindStream(ctx, mainJS, StreamRequirement{Stream: StreamAgent, MinMaxAge: 24*time.Hour + MaxAgeMargin, MaxMessageBytes: MaxMessageBytes}); err == nil ||
		!strings.Contains(err.Error(), "max_age") {
		t.Fatalf("a stream that would expire live commands was accepted: %v", err)
	}

	// Concurrent publishers of one delivery converge on one live message.
	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = appender.Append(ctx, StreamIndex, "outbox-race", []byte("racing-envelope"))
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("racing publisher %d = %q, %v; first = %q", i, results[i], errs[i], results[0])
		}
	}
	s.RequireNoViolations(t, natsconn.IdentityMainRuntime)
	s.RequireNoViolations(t, natsconn.IdentityWorker)
}

// The producer identity cannot consume, and the worker identity cannot
// inject a command: the boundary, from the service code's own client.
func TestCommandBusIdentitiesAreRefusedTheOtherHalf(t *testing.T) {
	s := natstest.Start(t)
	s.Bootstrap(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	workerJS := securedJetStream(t, s, natsconn.IdentityWorker)
	forged := NewCommandMessage(DeliverySubjectMust(t, StreamAgent, "forged"), "forged", []byte("x"))
	refused, cancelRefused := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRefused()
	if _, err := workerJS.PublishMsg(refused, forged); err == nil {
		t.Fatal("the worker identity published a command")
	}
	s.RequireViolation(t, natsconn.IdentityWorker, "Publish", forged.Subject)

	mainJS := securedJetStream(t, s, natsconn.IdentityMainRuntime)
	consumer, err := mainJS.Consumer(ctx, StreamAgent, ConsumerAgent)
	if err != nil {
		t.Fatalf("consumer info as the producer (granted for the drained gate): %v", err)
	}
	// Fetch reports the refusal through its batch, not always the call; the
	// server's log is the assertion.
	_, _ = consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	s.RequireViolation(t, natsconn.IdentityMainRuntime, "Publish", "$JS.API.CONSUMER.MSG.NEXT."+StreamAgent+"."+ConsumerAgent)
}

func securedJetStream(t *testing.T, s *natstest.Server, identity string) jetstream.JetStream {
	t.Helper()
	m := s.Material(identity)
	conn, err := nats.Connect(s.URL(),
		nats.Name(identity),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(identity)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(m.ClientCertificate, m.RootCAs),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("connect as %s: %v", identity, err)
	}
	t.Cleanup(conn.Close)
	// The worker reaches RUNTIME's durables from its own WORKER account,
	// through service imports mapped under natsconn.WorkerRuntimeJSAPIPrefix;
	// that is the prefix both workers open the command-bus context with.
	var js jetstream.JetStream
	if identity == natsconn.IdentityWorker {
		js, err = jetstream.NewWithAPIPrefix(conn, natsconn.WorkerRuntimeJSAPIPrefix, jetstream.WithDefaultTimeout(5*time.Second))
	} else {
		js, err = jetstream.New(conn, jetstream.WithDefaultTimeout(5*time.Second))
	}
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// securedConn is a raw connection as identity, for requests whose reply
// subject the test chooses.
func securedConn(t *testing.T, s *natstest.Server, identity string) *nats.Conn {
	t.Helper()
	m := s.Material(identity)
	conn, err := nats.Connect(s.URL(),
		nats.Name(identity),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(identity)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(m.ClientCertificate, m.RootCAs),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("connect as %s: %v", identity, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func bootstrapCLI(t *testing.T, s *natstest.Server, args ...string) {
	t.Helper()
	bootstrapCLIAs(t, s, natsconn.IdentityBootstrapRuntime, args...)
}

func bootstrapCLIAs(t *testing.T, s *natstest.Server, identity string, args ...string) {
	t.Helper()
	m := s.Material(identity)
	full := append([]string{
		"--server", s.URL(), "--tlsca", m.CAFile, "--tlscert", m.CertFile, "--tlskey", m.KeyFile,
		"--inbox-prefix", natsconn.InboxPrefix(identity), "--timeout", "5s",
	}, args...)
	cmd := exec.Command(natstest.CLI(), full...)
	cmd.Env = append(cmd.Environ(), "HOME="+s.Dir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nats %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func requireAPIError(t *testing.T, err error, code jetstream.ErrorCode, description string) {
	t.Helper()
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != code || !strings.Contains(apiErr.Description, description) {
		t.Fatalf("publish error = %v (%s), want JetStream error %d %q — re-pin the producer's error mapping after a nats-server bump",
			err, fmt.Sprintf("%+v", apiErr), code, description)
	}
}

// DeliverySubjectMust is DeliverySubject for a known-good stream.
func DeliverySubjectMust(t *testing.T, stream, deliveryID string) string {
	t.Helper()
	subject, err := DeliverySubject(stream, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	return subject
}
