package system_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

// The runtime system test drives the configuration-validation route
// (capability configuration.validation, ValidationDispatchPolicy), whose
// contract stream and durable are these (docs/runtime-command-bus.md).
const (
	commandStream   = commandbus.StreamValidate
	commandConsumer = commandbus.ConsumerValidate
)

// deadLetterStream is the KV bucket's backing stream.
const deadLetterStream = "KV_" + commandbus.DeadLetterBucket

// runtimeBus is the test's view of the command bus. Every read and write goes
// through an identity of the chart's permission table, with exactly the grant
// that identity holds in production: the test has no superuser.
//
//   - elitea-main-runtime: stream info, consumer info and the direct get
//     (the producer's own reads), and the publish of a command copy;
//   - elitea-worker: the dead-letter bucket's stream info and the NAK that
//     replaces the old XCLAIM IDLE ageing (both are worker grants).
type runtimeBus struct {
	server     *natstest.Server
	mainConn   *nats.Conn
	mainJS     jetstream.JetStream
	workerConn *nats.Conn
	workerJS   jetstream.JetStream
}

func newRuntimeBus(t *testing.T, server *natstest.Server) *runtimeBus {
	t.Helper()
	mainConn, mainJS := connectRuntimeIdentity(t, server, natsconn.IdentityMainRuntime)
	workerConn, workerJS := connectRuntimeIdentity(t, server, natsconn.IdentityWorker)
	return &runtimeBus{
		server:     server,
		mainConn:   mainConn,
		mainJS:     mainJS,
		workerConn: workerConn,
		workerJS:   workerJS,
	}
}

func connectRuntimeIdentity(t *testing.T, server *natstest.Server, identity string) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	material := server.Material(identity)
	conn, err := nats.Connect(server.URL(),
		nats.Name("runtime-system-test-"+identity),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(identity)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs),
		// The test restarts the server mid-run; the observers reconnect.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("connect to NATS as %s: %v", identity, err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn, jetstream.WithDefaultTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return conn, js
}

// state reads the command stream and its durable as elitea-main-runtime.
// An error (a reconnect in progress) is returned, never fatal, so polling
// callers retry.
func (b *runtimeBus) state(ctx context.Context) (*jetstream.StreamInfo, *jetstream.ConsumerInfo, error) {
	stream, err := b.mainJS.Stream(ctx, commandStream)
	if err != nil {
		return nil, nil, err
	}
	consumer, err := b.mainJS.Consumer(ctx, commandStream, commandConsumer)
	if err != nil {
		return nil, nil, err
	}
	return stream.CachedInfo(), consumer.CachedInfo(), nil
}

func (b *runtimeBus) requireState(t *testing.T, ctx context.Context) (*jetstream.StreamInfo, *jetstream.ConsumerInfo) {
	t.Helper()
	stream, consumer, err := b.state(ctx)
	if err != nil {
		t.Fatalf("read %s and %s: %v", commandStream, commandConsumer, err)
	}
	return stream, consumer
}

// liveCommand is the producer's own conflict read: DIRECT.GET last_by_subj.
func (b *runtimeBus) liveCommand(ctx context.Context, subject string) (*jetstream.RawStreamMsg, error) {
	stream, err := b.mainJS.Stream(ctx, commandStream)
	if err != nil {
		return nil, err
	}
	return stream.GetLastMsgForSubject(ctx, subject)
}

// deadLetterState reports how many records the bucket holds under key and
// the bucket's last sequence (any put, to any key, moves it).
func (b *runtimeBus) deadLetterState(t *testing.T, ctx context.Context, key string) (uint64, uint64) {
	t.Helper()
	subject := "$KV." + commandbus.DeadLetterBucket + "." + key
	stream, err := b.workerJS.Stream(ctx, deadLetterStream)
	if err != nil {
		t.Fatalf("bind the dead-letter bucket as %s: %v", natsconn.IdentityWorker, err)
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		t.Fatalf("read the dead-letter bucket as %s: %v", natsconn.IdentityWorker, err)
	}
	return info.State.Subjects[subject], info.State.LastSeq
}

// waitForWorkerPulling waits until a worker has a pull request open on the
// shared durable: the JetStream form of "joined the consumer group".
func (b *runtimeBus) waitForWorkerPulling(t *testing.T, ctx context.Context, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		_, consumer, err := b.state(ctx)
		return err == nil && consumer.NumWaiting > 0, nil
	}); err != nil {
		t.Fatalf("worker never pulled from %s/%s: %v\n%s", commandStream, commandConsumer, err, process.logs())
	}
}

// waitForUnsettledDelivery waits until the one live command was delivered
// again after previousDelivery (a consumer sequence) and is still owned:
// ack-pending, not acked, PostgreSQL still DISPATCHED. It returns the new
// consumer delivery sequence.
func (b *runtimeBus) waitForUnsettledDelivery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID string, previousDelivery uint64, label string, process *childProcess) uint64 {
	t.Helper()
	var delivered uint64
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		stream, consumer, err := b.state(ctx)
		if err != nil || stream.State.Msgs != 1 || consumer.NumAckPending != 1 || consumer.NumPending != 0 {
			return false, nil
		}
		if consumer.Delivered.Consumer <= previousDelivery {
			return false, nil
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
			return false, nil
		}
		delivered = consumer.Delivered.Consumer
		return state == "DISPATCHED", nil
	}); err != nil {
		t.Fatalf("unauthorized worker %q did not take the one live command and leave it unsettled: %v\n%s", label, err, process.logs())
	}
	return delivered
}

// waitForDeadLetter waits until a worker recorded key in the dead-letter
// bucket. The record is written first; the worker then terminates the
// message (signature or subject failure) or parks it with the 24h nak.
func (b *runtimeBus) waitForDeadLetter(t *testing.T, ctx context.Context, key string, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		records, _ := b.deadLetterState(t, ctx, key)
		return records == 1, nil
	}); err != nil {
		t.Fatalf("poison delivery was not dead-lettered under %s: %v\n%s", key, err, process.logs())
	}
}

// waitForLiveCommand waits until the one command is in the stream and not
// yet delivered (it survived any restart in between), and returns its live
// copy after the reference-only checks.
func (b *runtimeBus) waitForLiveCommand(t *testing.T, ctx context.Context, subject, deliveryID, settingsMarker string) *jetstream.RawStreamMsg {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		stream, info, err := b.state(ctx)
		return err == nil && stream.State.Msgs == 1 && info.NumPending == 1 && info.NumAckPending == 0, nil
	}); err != nil {
		t.Fatalf("the published command never became the one undelivered message of %s: %v", commandStream, err)
	}
	return assertReferenceOnlyCommand(t, ctx, b, subject, deliveryID, settingsMarker)
}

// waitForTerminated waits until a terminated delivery has left the
// WorkQueue stream: nothing pending, nothing ack-pending, no message.
func (b *runtimeBus) waitForTerminated(t *testing.T, ctx context.Context, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		stream, info, err := b.state(ctx)
		return err == nil && stream.State.Msgs == 0 && info.NumPending == 0 && info.NumAckPending == 0, nil
	}); err != nil {
		t.Fatalf("the unverifiable command was not terminated (still in %s): %v\n%s", commandStream, err, process.logs())
	}
}

// redeliverNow replaces the Redis-era XCLAIM IDLE ageing. The live command is
// ack-pending: parked by a poison NAK (24h), nakked for retry, or owned by a
// stopped worker until AckWait (60s). Instead of waiting, the test sends the
// NAK a worker would send for that delivery, as elitea-worker, on the
// delivery's reply subject $JS.ACK.<stream>.<durable>.<dc>.<sseq>.<dseq>.<ts>.<pending>,
// which the server resolves by its stream and consumer sequences. Only the
// most recent delivery is in flight, so ConsumerInfo.Delivered names it.
//
// It first waits for that one ack-pending delivery to be readable, which
// also proves it survived any server restart in between.
func (b *runtimeBus) redeliverNow(t *testing.T, ctx context.Context) {
	t.Helper()
	var consumer *jetstream.ConsumerInfo
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		stream, info, err := b.state(ctx)
		if err != nil || stream.State.Msgs != 1 || info.NumAckPending != 1 || info.NumPending != 0 {
			return false, nil
		}
		consumer = info
		return true, nil
	}); err != nil {
		t.Fatalf("locate the one ack-pending delivery before accelerated redelivery: %v", err)
	}
	reply := fmt.Sprintf(
		"$JS.ACK.%s.%s.1.%d.%d.%d.0",
		commandStream,
		commandConsumer,
		consumer.Delivered.Stream,
		consumer.Delivered.Consumer,
		time.Now().UnixNano(),
	)
	if err := b.workerConn.Publish(reply, []byte("-NAK")); err != nil {
		t.Fatalf("NAK the parked delivery: %v", err)
	}
	if err := b.workerConn.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush the NAK of the parked delivery: %v", err)
	}
}

// waitForSettlementAndRetirement waits for the PostgreSQL settlement and the
// worker's post-settlement double ack: WorkQueue retention removes an acked
// message, so the stream, the durable and the delivery subject are all empty
// (the "drained" definition of docs/runtime-command-bus.md).
func (b *runtimeBus) waitForSettlementAndRetirement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID, subject string, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
			return false, nil
		}
		if state != "SUCCEEDED" {
			return false, nil
		}
		return b.drained(ctx), nil
	}); err != nil {
		t.Fatalf("runtime did not durably settle and then double-ack the command: %v\n%s", err, process.logs())
	}
	if _, err := b.liveCommand(ctx, subject); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Fatalf("the bus retained the settled command on %s: %v", subject, err)
	}
}

// waitForReplayRetired waits until a worker took the replayed copy (a new
// delivery after previousDelivery) and acked it away.
func (b *runtimeBus) waitForReplayRetired(t *testing.T, ctx context.Context, previousDelivery uint64, process *childProcess) {
	t.Helper()
	if err := eventually(ctx, 100*time.Millisecond, func() (bool, error) {
		process.ensureRunning(t)
		stream, consumer, err := b.state(ctx)
		return err == nil &&
			consumer.Delivered.Consumer > previousDelivery &&
			stream.State.Msgs == 0 &&
			stream.State.NumSubjects == 0 &&
			consumer.NumAckPending == 0 &&
			consumer.NumPending == 0, nil
	}); err != nil {
		t.Fatalf("the worker did not double-ack a redelivered settled command: %v\n%s", err, process.logs())
	}
}

func (b *runtimeBus) drained(ctx context.Context) bool {
	stream, consumer, err := b.state(ctx)
	return err == nil &&
		stream.State.Msgs == 0 &&
		stream.State.NumSubjects == 0 &&
		consumer.NumAckPending == 0 &&
		consumer.NumPending == 0 &&
		consumer.NumRedelivered == 0
}

// expectedViolation is one refusal the least-privilege check provoked on
// purpose; anything else an identity hits is a missing grant or a real-code
// overreach.
type expectedViolation struct {
	identity string
	natstest.Violation
}

// assertBusLeastPrivilege replaces the Redis ACL NOPERM checks. The server
// logs every refused publish with the mapped identity, so each refusal is
// asserted, not inferred from a timeout:
//
//   - the worker cannot publish a command, purge the stream, delete a message
//     or create a consumer;
//   - the producer cannot pull, ack or delete a message;
//   - neither reaches another plane's subjects (MAIN's presence bucket,
//     GATEWAY's spend deltas);
//   - RUNTIME's bootstrap identity (the one remaining operator principal; the
//     Redis observer is gone) cannot publish a command or pull one.
//
// None of it may have changed the stream or the durable.
func assertBusLeastPrivilege(t *testing.T, ctx context.Context, bus *runtimeBus) []expectedViolation {
	t.Helper()
	bootstrapConn, _ := connectRuntimeIdentity(t, bus.server, natsconn.IdentityBootstrapRuntime)
	forbiddenCommand, err := commandbus.DeliverySubject(commandStream, "system-forbidden-command")
	if err != nil {
		t.Fatal(err)
	}
	pull := fmt.Sprintf("$JS.API.CONSUMER.MSG.NEXT.%s.%s", commandStream, commandConsumer)
	ack := fmt.Sprintf("$JS.ACK.%s.%s.1.1.1.1.0", commandStream, commandConsumer)
	attempts := []struct {
		identity string
		conn     *nats.Conn
		subject  string
		payload  string
	}{
		{natsconn.IdentityWorker, bus.workerConn, forbiddenCommand, "forbidden"},
		{natsconn.IdentityWorker, bus.workerConn, "$JS.API.STREAM.PURGE." + commandStream, "{}"},
		{natsconn.IdentityWorker, bus.workerConn, "$JS.API.STREAM.MSG.DELETE." + commandStream, `{"seq":1}`},
		{natsconn.IdentityWorker, bus.workerConn, fmt.Sprintf("$JS.API.CONSUMER.CREATE.%s.forbidden-worker", commandStream), "{}"},
		{natsconn.IdentityMainRuntime, bus.mainConn, pull, `{"batch":1}`},
		{natsconn.IdentityMainRuntime, bus.mainConn, ack, "+ACK"},
		{natsconn.IdentityMainRuntime, bus.mainConn, "$JS.API.STREAM.MSG.DELETE." + commandStream, `{"seq":1}`},
		// Other planes' subjects: RUNTIME is its own account, and no grant in
		// it names MAIN's presence bucket or GATEWAY's spend deltas.
		{natsconn.IdentityMainRuntime, bus.mainConn, "$KV.ELITEA_CANVAS_PRESENCE.system", "{}"},
		{natsconn.IdentityWorker, bus.workerConn, "gateway.budget.delta", "{}"},
		{natsconn.IdentityBootstrapRuntime, bootstrapConn, forbiddenCommand, "forbidden"},
		{natsconn.IdentityBootstrapRuntime, bootstrapConn, pull, `{"batch":1}`},
	}
	expected := make([]expectedViolation, 0, len(attempts))
	for _, attempt := range attempts {
		if err := attempt.conn.Publish(attempt.subject, []byte(attempt.payload)); err != nil {
			t.Fatalf("send forbidden %s publish on %s: %v", attempt.identity, attempt.subject, err)
		}
		if err := attempt.conn.FlushWithContext(ctx); err != nil {
			t.Fatalf("flush forbidden %s publish on %s: %v", attempt.identity, attempt.subject, err)
		}
		bus.server.RequireViolation(t, attempt.identity, "Publish", attempt.subject)
		expected = append(expected, expectedViolation{
			identity:  attempt.identity,
			Violation: natstest.Violation{Kind: "Publish", Subject: attempt.subject},
		})
	}
	if t.Failed() {
		t.FailNow()
	}
	stream, consumer := bus.requireState(t, ctx)
	if stream.State.Msgs != 0 || stream.State.LastSeq != 0 || consumer.Delivered.Consumer != 0 || consumer.NumAckPending != 0 {
		t.Fatalf("forbidden bus operations changed the command stream: msgs=%d last=%d delivered=%d ack-pending=%d",
			stream.State.Msgs, stream.State.LastSeq, consumer.Delivered.Consumer, consumer.NumAckPending)
	}
	return expected
}

// assertOnlyExpectedViolations fails when any identity the runtime uses hit a
// permissions violation other than the ones provoked on purpose: the real
// elitea-main producer and the real Python worker must run inside the chart's
// grants.
func assertOnlyExpectedViolations(t *testing.T, server *natstest.Server, expected []expectedViolation) {
	t.Helper()
	allowed := make(map[expectedViolation]bool, len(expected))
	for _, violation := range expected {
		allowed[violation] = true
	}
	for _, identity := range []string{natsconn.IdentityMainRuntime, natsconn.IdentityWorker, natsconn.IdentityBootstrapRuntime} {
		var unexpected []string
		for _, violation := range server.Violations(identity) {
			if !allowed[expectedViolation{identity: identity, Violation: violation}] {
				unexpected = append(unexpected, violation.Kind+" "+violation.Subject)
			}
		}
		if len(unexpected) > 0 {
			t.Errorf("%s hit permissions violation(s) on its real code path: %s", identity, strings.Join(unexpected, ", "))
		}
	}
}

// replaySettledCommand puts a copy of an already settled command back on its
// delivery subject, as elitea-main-runtime: what a worker meets when its
// post-settlement ack never reached the server and AckWait redelivers. This
// is the JetStream counterpart of the Redis retirement-response loss: the
// worker must reach the claim, learn the execution is settled, and double-ack
// it, without a second claim and without dead-lettering it. The copy carries
// no Nats-Msg-Id, so the 2m de-duplication window cannot swallow it.
func (b *runtimeBus) replaySettledCommand(t *testing.T, ctx context.Context, settled *jetstream.RawStreamMsg) {
	t.Helper()
	msg := nats.NewMsg(settled.Subject)
	msg.Data = append([]byte(nil), settled.Data...)
	msg.Header.Set(commandbus.HeaderDeliveryID, settled.Header.Get(commandbus.HeaderDeliveryID))
	ack, err := b.mainJS.PublishMsg(ctx, msg, jetstream.WithExpectStream(commandStream))
	if err != nil {
		t.Fatalf("replay the settled command: %v", err)
	}
	if ack.Duplicate {
		t.Fatal("the settled command replay was de-duplicated instead of stored")
	}
}
