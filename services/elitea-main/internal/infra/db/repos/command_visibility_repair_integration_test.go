package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

const visibilityRepairServiceOptIn = "ELITEA_RUNTIME_VISIBILITY_REPAIR_TEST"

// TestPostgresNATSServiceBackedVisibilityRepair is an opt-in, real-service
// integration test of PostgreSQL's visibility lease over the JetStream
// command bus, as the elitea-main-runtime identity on the NATS chart's own
// permissions. It proves PostgreSQL re-offers the exact prepared envelope,
// the bus de-duplicates an ambiguous successful publish retry and a re-offer
// of a live delivery, a lost stream is repaired from the PostgreSQL winner
// without re-signing, and independent publishers converge on one message.
// (The Redis-era partial "one key lost" states cannot exist: there is no
// second key.) It is not a NATS cluster-failover or cross-process test.
func TestPostgresNATSServiceBackedVisibilityRepair(t *testing.T) {
	if os.Getenv(visibilityRepairServiceOptIn) != "1" {
		t.Skipf("set %s=1 with %s and the secured NATS test environment to run the visibility-repair integration test", visibilityRepairServiceOptIn, postgresIntegrationDatabaseURL)
	}
	server := natstest.Start(t)
	server.Bootstrap(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newMigratedPostgresIntegrationPool(t)

	stream := commandbus.StreamValidate
	primary := newVisibilityRepairJetStream(t, server)

	policy := testDispatchPolicy()
	policy.StreamName = stream
	policy.DeadlineTTL = 10 * time.Minute
	policy.MaxOutstanding = 4
	jobs, err := NewExecutionJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	admission := postgresCapacityAdmission(9_001)
	now := time.Now().UTC()
	admission.Record.Job.CreatedAt = now
	admission.Record.Outbox.CreatedAt = now
	if outcome, err := jobs.AdmitValidation(ctx, admission); err != nil || !outcome.Created {
		t.Fatalf("admit visibility-repair execution: outcome=%+v err=%v", outcome, err)
	}
	outboxID := admission.Record.Outbox.ID

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := commandbus.NewEd25519CommandSigner("visibility-repair-ed25519-v1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	firstDispatcher := newVisibilityRepairDispatcher(t, ctx, pool, primary, policy, signer)
	publisher, err := executionapp.NewOutboxPublisher(
		mustVisibilityRepairOutbox(t, pool, stream),
		firstDispatcher,
		executionapp.OutboxPublisherConfig{
			PollInterval:      time.Second,
			VisibilityTimeout: executionapp.MinOutboxVisibilityTimeout,
			BatchSize:         4,
			MaxConcurrent:     2,
			ReportFailure:     func(error) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("initial visibility publication: %v", err)
	}
	initialEnvelope := assertVisibilityRepairBusState(t, ctx, primary, stream, outboxID)
	storedEnvelope, attempts := visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(storedEnvelope, initialEnvelope) || attempts != 1 {
		t.Fatalf("initial durable publication bytes_equal=%t attempts=%d, want true/1", bytes.Equal(storedEnvelope, initialEnvelope), attempts)
	}

	// An ambiguous publish/MarkValidationPublished response loss: the retry
	// of the exact durable envelope must not take a second slot.
	if err := firstDispatcher.Dispatch(ctx, outboxID); err != nil {
		t.Fatalf("deduplicate ambiguous publication retry: %v", err)
	}
	retriedEnvelope := assertVisibilityRepairBusState(t, ctx, primary, stream, outboxID)
	storedEnvelope, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(retriedEnvelope, initialEnvelope) || !bytes.Equal(storedEnvelope, initialEnvelope) || attempts != 2 {
		t.Fatalf("ambiguous retry bus_equal=%t postgres_equal=%t attempts=%d, want true/true/2", bytes.Equal(retriedEnvelope, initialEnvelope), bytes.Equal(storedEnvelope, initialEnvelope), attempts)
	}

	// PostgreSQL's periodic re-offer of the still-unclaimed, still-live
	// delivery is idempotent too.
	ageVisibilityRepairRow(t, ctx, pool, outboxID)
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("re-offer of a live delivery: %v", err)
	}
	reofferedEnvelope := assertVisibilityRepairBusState(t, ctx, primary, stream, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(reofferedEnvelope, initialEnvelope) || attempts != 3 {
		t.Fatalf("re-offer changed bytes=%t attempts=%d, want false/3", !bytes.Equal(reofferedEnvelope, initialEnvelope), attempts)
	}

	// Stream loss (the JetStream store was lost; the bootstrap re-created
	// the stream empty) is repaired from the PostgreSQL winner without re-signing.
	recreateVisibilityRepairStream(t, server, primary, stream)
	ageVisibilityRepairRow(t, ctx, pool, outboxID)
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("repair the lost stream: %v", err)
	}
	restoredEnvelope := assertVisibilityRepairBusState(t, ctx, primary, stream, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(restoredEnvelope, initialEnvelope) || attempts != 4 {
		t.Fatalf("stream-loss repair changed bytes=%t attempts=%d, want true/4", bytes.Equal(restoredEnvelope, initialEnvelope), attempts)
	}

	// Independent processes may race on the same stale durable row. The bus
	// must hold exactly one message and both callers must see success.
	recreateVisibilityRepairStream(t, server, primary, stream)
	secondDispatcher := newVisibilityRepairDispatcher(t, ctx, pool, newVisibilityRepairJetStream(t, server), policy, signer)
	start := make(chan struct{})
	results := make(chan error, 2)
	var racers sync.WaitGroup
	racers.Add(2)
	for _, dispatcher := range []*executionapp.ValidationDispatcher{firstDispatcher, secondDispatcher} {
		dispatcher := dispatcher
		go func() {
			defer racers.Done()
			<-start
			results <- dispatcher.Dispatch(ctx, outboxID)
		}()
	}
	close(start)
	racers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("scale-out visibility publisher failed: %v", err)
		}
	}
	racedEnvelope := assertVisibilityRepairBusState(t, ctx, primary, stream, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(racedEnvelope, initialEnvelope) || attempts != 6 {
		t.Fatalf("scale-out dedupe changed bytes=%t attempts=%d, want false/6", !bytes.Equal(racedEnvelope, initialEnvelope), attempts)
	}
	server.RequireNoViolations(t, natsconn.IdentityMainRuntime)
}

func newVisibilityRepairJetStream(t *testing.T, server *natstest.Server) jetstream.JetStream {
	t.Helper()
	m := server.Material(natsconn.IdentityMainRuntime)
	conn, err := nats.Connect(server.URL(),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityMainRuntime)),
		nats.Secure(natsconn.BaseTLSConfig()),
		nats.ClientTLSConfig(m.ClientCertificate, m.RootCAs),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn, jetstream.WithDefaultTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return js
}

func recreateVisibilityRepairStream(t *testing.T, server *natstest.Server, js jetstream.JetStream, stream string) {
	t.Helper()
	// No identity may delete a stream (#1076), so the loss is the store's:
	// the server comes back with an empty JetStream and the RUNTIME
	// bootstrap re-creates stream and durable empty.
	server.ResetJetStream(t)
	server.Bootstrap(t, nil)
	// The test's own connection reconnects on its own; wait until it reads
	// the re-created stream, so the next publish is not a reconnect race.
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := js.Stream(ctx, stream)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the test connection did not see the re-created %s: %v", stream, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func newVisibilityRepairDispatcher(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	js jetstream.JetStream,
	policy ValidationDispatchPolicy,
	signer commandbus.CommandSigner,
) *executionapp.ValidationDispatcher {
	t.Helper()
	handle, err := commandbus.BindStream(ctx, js, commandbus.StreamRequirement{
		Stream: policy.StreamName, MinMaxAge: policy.DeadlineTTL + commandbus.MaxAgeMargin, MaxMessageBytes: commandbus.MaxMessageBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	appender, err := commandbus.NewJetStreamAppender(js, handle)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := commandbus.NewProducer(commandbus.ProducerConfig{
		Stream:                 policy.StreamName,
		ProtocolRevision:       "elitea.runtime.v1",
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		Limits: commandbus.Limits{
			Revision:                 policy.LimitsRevision,
			MaxWorkerCommandBytes:    32 * 1024,
			MaxSignedEnvelopeBytes:   48 * 1024,
			MaxTransportPayloadBytes: 48 * 1024,
			MaxTransportMessageBytes: 64 * 1024,
			MaxSignatureBytes:        256,
			MaxStringBytes:           256,
		},
	}, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := executionapp.NewValidationDispatcher(mustVisibilityRepairOutbox(t, pool, policy.StreamName), producer)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func mustVisibilityRepairOutbox(t *testing.T, pool *pgxpool.Pool, stream string) *CommandOutboxRepository {
	t.Helper()
	outbox, err := NewCommandOutboxRepository(pool, stream)
	if err != nil {
		t.Fatal(err)
	}
	return outbox
}

func ageVisibilityRepairRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, outboxID string) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
WITH aged_visibility AS MATERIALIZED (
    SELECT clock_timestamp() - interval '2 seconds' AS observed_at
)
UPDATE elitea_runtime.command_outbox
SET published_at = aged_visibility.observed_at,
    last_visibility_at = aged_visibility.observed_at
FROM aged_visibility
WHERE outbox_id = $1 AND published_at IS NOT NULL`, outboxID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age PostgreSQL visibility observation: affected=%d err=%v", tag.RowsAffected(), err)
	}
}

func visibilityRepairPostgresState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, outboxID string) ([]byte, int64) {
	t.Helper()
	var envelope []byte
	var publishedAt, lastVisibilityAt time.Time
	var attempts int64
	if err := pool.QueryRow(ctx, `
SELECT prepared_signed_envelope_bytes, published_at, last_visibility_at, publish_attempts
FROM elitea_runtime.command_outbox
WHERE outbox_id = $1`, outboxID).Scan(&envelope, &publishedAt, &lastVisibilityAt, &attempts); err != nil {
		t.Fatalf("read PostgreSQL visibility state: %v", err)
	}
	if len(envelope) == 0 || publishedAt.IsZero() || lastVisibilityAt.IsZero() || lastVisibilityAt.Before(publishedAt) {
		t.Fatalf("invalid PostgreSQL visibility state: bytes=%d published=%s last=%s", len(envelope), publishedAt, lastVisibilityAt)
	}
	return append([]byte(nil), envelope...), attempts
}

// assertVisibilityRepairBusState requires exactly one live message on the
// stream, on the delivery's subject, and returns its bytes.
func assertVisibilityRepairBusState(t *testing.T, ctx context.Context, js jetstream.JetStream, stream, outboxID string) []byte {
	t.Helper()
	handle, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := handle.Info(ctx)
	if err != nil || info.State.Msgs != 1 || info.State.NumSubjects != 1 {
		t.Fatalf("bus visibility state msgs=%d subjects=%d err=%v, want 1/1", info.State.Msgs, info.State.NumSubjects, err)
	}
	subject, err := commandbus.DeliverySubject(stream, outboxID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := handle.GetLastMsgForSubject(ctx, subject)
	if err != nil || len(live.Data) == 0 {
		t.Fatalf("the delivery's subject holds no live command: %v", err)
	}
	if got := live.Header.Get(commandbus.HeaderDeliveryID); got != outboxID || !strings.HasSuffix(subject, live.Header.Get(commandbus.HeaderMsgID)) {
		t.Fatalf("headers %v do not name the delivery %q", live.Header, outboxID)
	}
	return append([]byte(nil), live.Data...)
}
