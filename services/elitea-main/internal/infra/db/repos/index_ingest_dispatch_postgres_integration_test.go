package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// TestPostgresServiceBackedIndexIngestDispatch is a real PostgreSQL 16-18
// service-integration gate. Command-bus failures are injected at the StreamAppender
// boundary; the real-NATS tests separately prove atomic capacity and
// delivery-index behavior.
func TestPostgresServiceBackedIndexIngestDispatch(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    16,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	busOutage := errors.New("test command bus unavailable")

	for _, test := range []struct {
		name       string
		prefix     string
		appendErr  error
		errorMatch func(error) bool
	}{
		{
			name:      "command-bus outage retains exact durable bytes",
			prefix:    "outage",
			appendErr: busOutage,
			errorMatch: func(err error) bool {
				return errors.Is(err, busOutage)
			},
		},
		{
			name:   "capacity retains exact durable bytes",
			prefix: "capacity",
			appendErr: &commandbus.ControlStreamSaturatedError{
				Stream:          "ELITEA_RT_V1_INDEX",
				CurrentMessages: 8,
				MaxMessages:     8,
			},
			errorMatch: func(err error) bool {
				return errors.Is(err, executionapp.ErrDispatchBackpressured)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, outboxID := admitPostgresIndexDispatch(t, ctx, jobs, test.prefix)
			signer := &postgresIndexDispatchSigner{keyID: "key-" + test.prefix}
			appender := &postgresIndexDispatchAppender{err: test.appendErr}
			producer := newPostgresIndexProducer(t, policy, signer, appender)
			dispatcher, err := indexingapp.NewIndexIngestDispatcher(outbox, producer)
			if err != nil {
				t.Fatal(err)
			}

			if err := dispatcher.Dispatch(ctx, outboxID); !test.errorMatch(err) {
				t.Fatalf("injected append failure = %v", err)
			}
			prepared := assertPostgresIndexDispatchState(t, ctx, pool, outcome.ExecutionID, false, "PENDING", 0)
			assertPreparedIndexCorrelations(t, prepared, "request-"+test.prefix)
			if signer.callCount() != 1 || appender.callCount() != 1 || !bytes.Equal(prepared, appender.callBytes(0)) {
				t.Fatal("failed append did not retain its exact prepared envelope")
			}

			appender.setError(nil)
			if err := dispatcher.Dispatch(ctx, outboxID); err != nil {
				t.Fatalf("retry retained index dispatch: %v", err)
			}
			assertPostgresIndexDispatchState(t, ctx, pool, outcome.ExecutionID, true, "DISPATCHED", 1)
			if signer.callCount() != 1 || appender.callCount() != 2 || !bytes.Equal(appender.callBytes(0), appender.callBytes(1)) {
				t.Fatal("retry re-signed or changed the durable index envelope")
			}
		})
	}

	t.Run("competing publishers select one envelope and mark idempotently", func(t *testing.T) {
		outcome, _ := admitPostgresIndexDispatch(t, ctx, jobs, "competing")
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		appender := &postgresIndexDispatchAppender{}
		signers := []*postgresIndexDispatchSigner{
			{keyID: "competing-a", started: started, release: release},
			{keyID: "competing-b", started: started, release: release},
		}
		publishers := make([]*executionapp.OutboxPublisher, 0, 2)
		for _, signer := range signers {
			publishers = append(publishers, newPostgresIndexPublisher(t, policy, outbox, signer, appender))
		}

		results := make(chan error, 2)
		for _, publisher := range publishers {
			go func(publisher *executionapp.OutboxPublisher) {
				results <- publisher.RunOnce(ctx)
			}(publisher)
		}
		for range 2 {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatalf("competing publishers did not reach signing barrier: %v", ctx.Err())
			}
		}
		close(release)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatalf("competing publisher: %v", err)
			}
		}

		prepared := assertPostgresIndexDispatchState(t, ctx, pool, outcome.ExecutionID, true, "DISPATCHED", 2)
		if appender.callCount() != 2 || !bytes.Equal(appender.callBytes(0), prepared) || !bytes.Equal(appender.callBytes(1), prepared) || signers[0].callCount() != 1 || signers[1].callCount() != 1 {
			t.Fatal("competing publishers did not append the one durable CAS winner")
		}
	})

	// The competing subtest above releases both signers together, so the loser
	// usually reaches the envelope CAS while the job is still PENDING. A
	// saturated machine can delay the loser until the winner has published and
	// moved the job to DISPATCHED. This subtest makes that order deterministic:
	// it holds the loser at the signing barrier until the winner has finished.
	// The loser must still receive the durable winner, append it, and count its
	// publish attempt. A refusal there is reported as success and silently
	// drops one publish attempt.
	t.Run("a publisher held past the winner still appends the durable winner", func(t *testing.T) {
		outcome, _ := admitPostgresIndexDispatch(t, ctx, jobs, "held")
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		appender := &postgresIndexDispatchAppender{}
		heldSigner := &postgresIndexDispatchSigner{keyID: "held-loser", started: started, release: release}
		winnerSigner := &postgresIndexDispatchSigner{keyID: "held-winner"}
		heldPublisher := newPostgresIndexPublisher(t, policy, outbox, heldSigner, appender)
		winnerPublisher := newPostgresIndexPublisher(t, policy, outbox, winnerSigner, appender)

		heldResult := make(chan error, 1)
		go func() {
			heldResult <- heldPublisher.RunOnce(ctx)
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("held publisher did not reach the signing barrier: %v", ctx.Err())
		}

		if err := winnerPublisher.RunOnce(ctx); err != nil {
			t.Fatalf("winning publisher: %v", err)
		}
		assertPostgresIndexDispatchState(t, ctx, pool, outcome.ExecutionID, true, "DISPATCHED", 1)

		close(release)
		select {
		case err := <-heldResult:
			if err != nil {
				t.Fatalf("held publisher: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("held publisher did not return: %v", ctx.Err())
		}

		prepared := assertPostgresIndexDispatchState(t, ctx, pool, outcome.ExecutionID, true, "DISPATCHED", 2)
		if appender.callCount() != 2 || !bytes.Equal(appender.callBytes(0), prepared) || !bytes.Equal(appender.callBytes(1), prepared) {
			t.Fatalf("held publisher dropped its append of the durable winner: appends=%d", appender.callCount())
		}
		if winnerSigner.callCount() != 1 || heldSigner.callCount() != 1 {
			t.Fatal("held publisher race re-signed the durable index envelope")
		}
	})

	t.Run("capability and stream views cannot cross", func(t *testing.T) {
		_, outboxID := admitPostgresIndexDispatch(t, ctx, jobs, "isolation")
		pending, err := outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !containsAdmissionID(pending, outboxID) {
			t.Fatalf("index view omitted its own pending command: %v", pending)
		}
		validationView, err := NewCommandOutboxRepository(pool, policy.StreamName)
		if err != nil {
			t.Fatal(err)
		}
		validationIDs, err := validationView.ListPendingValidationIDs(ctx, 16, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if containsAdmissionID(validationIDs, outboxID) {
			t.Fatalf("index command leaked into validation capability view: %v", validationIDs)
		}
		wrongStream, err := NewCommandOutboxRepository(pool, "elitea:runtime:validation:commands")
		if err != nil {
			t.Fatal(err)
		}
		wrongStreamIDs, err := wrongStream.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if containsAdmissionID(wrongStreamIDs, outboxID) {
			t.Fatalf("index command leaked into another stream: %v", wrongStreamIDs)
		}
	})
}

func assertPreparedIndexCorrelations(t *testing.T, prepared []byte, requestID string) {
	t.Helper()
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(prepared, envelope); err != nil {
		t.Fatal(err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), command); err != nil {
		t.Fatal(err)
	}
	index := command.GetIndexIngest()
	if index == nil ||
		index.GetClientStreamId() != "stream-"+requestID ||
		index.GetClientMessageId() != "message-"+requestID ||
		index.GetSioEvent() != indexingapp.CurrentIndexSIOEvent ||
		index.GetEmbeddingBinding().GetEntryId() != "embedding-binding" ||
		len(index.GetEmbeddingBinding().GetContentDigest().GetValue()) != sha256.Size {
		t.Fatalf("prepared index command lost browser correlation: %+v", index)
	}
}

func admitPostgresIndexDispatch(t *testing.T, ctx context.Context, jobs *IndexIngestJobsRepository, prefix string) (indexingapp.AdmissionOutcome, string) {
	t.Helper()
	factory, err := indexingapp.NewInputBundleFactory(indexingapp.InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, postgresIndexIDs(
		prefix+"-bundle",
		prefix+"-toolkit-content",
		prefix+"-parameters-content",
		prefix+"-embedding-content",
	))
	if err != nil {
		t.Fatal(err)
	}
	service, err := indexingapp.NewAdmissionService(jobs, factory, time.Now, postgresIndexIDs(
		prefix+"-execution",
		prefix+"-command",
		prefix+"-outbox",
		prefix+"-index-meta",
	))
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest("request-"+prefix, "idx-"+prefix)
	request.Inputs.EmbeddingBinding = &indexingapp.EmbeddingBinding{
		SchemaVersion:          indexingapp.CurrentEmbeddingBindingSchema,
		ModelName:              "text-embedding-3-small",
		ResolvedModelGroup:     "1_text-embedding-3-small",
		Route:                  "public",
		ConfigurationProjectID: 1,
		ConfigurationUUID:      "00000000-0000-0000-0000-000000000111",
		ConfigurationDigest:    runtimedomain.SHA256([]byte("configuration:" + prefix)),
	}
	outcome, err := service.Submit(ctx, request)
	if err != nil || !outcome.Created {
		t.Fatalf("admit %s index dispatch: outcome=%+v err=%v", prefix, outcome, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     outcome.ExecutionID,
		Generation:      outcome.Generation,
		IndexGeneration: outcome.IndexGeneration,
		MetaID:          outcome.IndexMetaID,
		CorrelationID:   outcome.IndexMetaCorrelationID,
	}); err != nil {
		t.Fatalf("initialize %s index metadata: %v", prefix, err)
	}
	return outcome, prefix + "-outbox"
}

// newPostgresIndexPublisher builds one bounded single-item publisher over the
// shared outbox repository. Each publisher owns its own signer, so a test can
// hold one publisher at the signing barrier.
func newPostgresIndexPublisher(
	t *testing.T,
	policy IndexIngestDispatchPolicy,
	outbox *CommandOutboxRepository,
	signer commandbus.CommandSigner,
	appender commandbus.StreamAppender,
) *executionapp.OutboxPublisher {
	t.Helper()
	producer := newPostgresIndexProducer(t, policy, signer, appender)
	dispatcher, err := indexingapp.NewIndexIngestDispatcher(outbox, producer)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := indexingapp.NewIndexIngestOutboxPublisher(outbox, dispatcher, executionapp.OutboxPublisherConfig{
		PollInterval:      time.Second,
		VisibilityTimeout: time.Minute,
		BatchSize:         1,
		MaxConcurrent:     1,
		ReportFailure:     func(error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func newPostgresIndexProducer(t *testing.T, policy IndexIngestDispatchPolicy, signer commandbus.CommandSigner, appender commandbus.StreamAppender) *commandbus.IndexIngestProducer {
	t.Helper()
	producer, err := commandbus.NewIndexIngestProducer(commandbus.IndexIngestProducerConfig{
		Stream:                 policy.StreamName,
		Consumer:               "elitea-indexer-worker-v1",
		ValidationStream:       "elitea:runtime:validation:commands",
		ProtocolRevision:       "runtime-v1",
		EnvelopeSchemaRevision: "signed-worker-command-v1",
		CapabilityVersion:      policy.CapabilityVersion,
		Limits: commandbus.Limits{
			Revision:                 policy.LimitsRevision,
			MaxWorkerCommandBytes:    8 * 1024,
			MaxSignedEnvelopeBytes:   12 * 1024,
			MaxTransportPayloadBytes: 12 * 1024,
			MaxTransportMessageBytes: 16 * 1024,
			MaxSignatureBytes:        128,
			MaxStringBytes:           512,
		},
		AllowTestOnlyHMAC: true,
	}, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	return producer
}

func assertPostgresIndexDispatchState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID string, published bool, state string, attempts int32) []byte {
	t.Helper()
	var prepared []byte
	var storedPublished bool
	var storedState string
	var storedAttempts int32
	if err := pool.QueryRow(ctx, `
SELECT o.prepared_signed_envelope_bytes,
       o.published_at IS NOT NULL,
       j.state,
       o.publish_attempts
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.capability_id = 'index.ingest.v1'`, executionID).Scan(
		&prepared,
		&storedPublished,
		&storedState,
		&storedAttempts,
	); err != nil {
		t.Fatal(err)
	}
	if len(prepared) == 0 || storedPublished != published || storedState != state || storedAttempts != attempts {
		t.Fatalf("unexpected durable index dispatch state: bytes=%d published=%v state=%s attempts=%d", len(prepared), storedPublished, storedState, storedAttempts)
	}
	return bytes.Clone(prepared)
}

type postgresIndexDispatchSigner struct {
	keyID   string
	started chan<- struct{}
	release <-chan struct{}

	mu    sync.Mutex
	calls int
}

func (s *postgresIndexDispatchSigner) SignWorkerCommand(ctx context.Context, exact []byte) (commandbus.Signature, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		case <-ctx.Done():
			return commandbus.Signature{}, ctx.Err()
		}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return commandbus.Signature{}, ctx.Err()
		}
	}
	material := append([]byte(s.keyID+":"), exact...)
	signature := sha256.Sum256(material)
	return commandbus.Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyID:   s.keyID,
		Value:   signature[:],
	}, nil
}

func (s *postgresIndexDispatchSigner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type postgresIndexDispatchAppender struct {
	mu    sync.Mutex
	err   error
	calls [][]byte
}

func (a *postgresIndexDispatchAppender) Append(_ context.Context, _, deliveryID string, value []byte) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, bytes.Clone(value))
	if a.err != nil {
		return "", a.err
	}
	return fmt.Sprintf("%s-%d", deliveryID, len(a.calls)), nil
}

func (a *postgresIndexDispatchAppender) setError(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

func (a *postgresIndexDispatchAppender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *postgresIndexDispatchAppender) callBytes(index int) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return bytes.Clone(a.calls[index])
}
