package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// TestPostgresServiceBackedIndexIngestAdmission is a real PostgreSQL 16-18
// service-integration gate. It crosses the typed application use case, SQLC
// queries, migration constraints and one database transaction; it is not a
// command-bus/worker/system E2E, load, soak or penetration test.
func TestPostgresServiceBackedIndexIngestAdmission(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	repository, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest("request-1", "docs")
	service := newPostgresIndexAdmissionService(t, repository, "first")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	created, err := service.Submit(ctx, request)
	if err != nil || !created.Created {
		t.Fatalf("create index admission: outcome=%+v err=%v", created, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE capability_id = 'index.ingest.v1'`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.index_ingest_jobs`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.input_bundles`)
	assertPostgresCount(t, ctx, pool, 2, `SELECT count(*) FROM elitea_runtime.input_bundle_entries`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.command_outbox WHERE stream_name = $1`, policy.StreamName)

	var toolkitID int32
	var indexName, initiator, streamName, indexMetaID, indexMetaCorrelationID string
	var clientStreamID, clientMessageID, sioEvent string
	var indexMetaInitialized bool
	var validationColumns int
	var runtimeGeneration, indexGeneration, preparedBytes, inputBytes int64
	if err := pool.QueryRow(ctx, `
SELECT j.generation, i.index_generation,
       i.toolkit_id, i.index_name, i.initiator, o.stream_name,
       i.index_meta_id, i.index_meta_correlation_id,
       i.client_stream_id, i.client_message_id, i.sio_event,
       i.index_meta_initialized_at IS NOT NULL,
       num_nonnulls(
           j.configuration_revision_id, j.configuration_type,
           j.catalog_revision, j.catalog_digest, j.schema_id,
           j.schema_revision, j.schema_digest, j.settings_entry_id
       ),
       COALESCE(octet_length(o.prepared_signed_envelope_bytes), 0),
       sum(octet_length(e.content_bytes))
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id AND i.generation = j.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = $1
GROUP BY j.generation, i.index_generation,
         i.toolkit_id, i.index_name, i.initiator, o.stream_name,
         i.index_meta_id, i.index_meta_correlation_id,
         i.client_stream_id, i.client_message_id, i.sio_event,
         i.index_meta_initialized_at,
         j.configuration_revision_id, j.configuration_type,
         j.catalog_revision, j.catalog_digest, j.schema_id,
         j.schema_revision, j.schema_digest, j.settings_entry_id,
         o.prepared_signed_envelope_bytes`, created.ExecutionID).Scan(
		&runtimeGeneration, &indexGeneration,
		&toolkitID, &indexName, &initiator, &streamName,
		&indexMetaID, &indexMetaCorrelationID,
		&clientStreamID, &clientMessageID, &sioEvent, &indexMetaInitialized,
		&validationColumns, &preparedBytes, &inputBytes,
	); err != nil {
		t.Fatal(err)
	}
	if runtimeGeneration != 1 || indexGeneration != 1 ||
		toolkitID != 19 || indexName != "docs" || initiator != "user" || streamName != policy.StreamName ||
		indexMetaID != created.IndexMetaID || indexMetaCorrelationID != created.IndexMetaCorrelationID ||
		clientStreamID != request.ClientStreamID ||
		clientMessageID != request.ClientMessageID ||
		sioEvent != indexingapp.CurrentIndexSIOEvent ||
		indexMetaInitialized || validationColumns != 0 || preparedBytes != 0 || inputBytes == 0 {
		t.Fatalf(
			"unexpected durable index binding: runtime_generation=%d index_generation=%d toolkit=%d index=%s initiator=%s stream=%s meta=%s correlation=%s initialized=%v validation=%d outbox_bytes=%d input_bytes=%d",
			runtimeGeneration,
			indexGeneration,
			toolkitID,
			indexName,
			initiator,
			streamName,
			indexMetaID,
			indexMetaCorrelationID,
			indexMetaInitialized,
			validationColumns,
			preparedBytes,
			inputBytes,
		)
	}

	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsAdmissionID(pending, "first-outbox") {
		t.Fatalf("uninitialized metadata became dispatchable: %v", pending)
	}
	signer := &postgresIndexDispatchSigner{keyID: "must-not-sign"}
	appender := &postgresIndexDispatchAppender{}
	producer := newPostgresIndexProducer(t, policy, signer, appender)
	dispatcher, err := indexingapp.NewIndexIngestDispatcher(outbox, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(ctx, "first-outbox"); !errors.Is(err, ErrPendingIndexIngestDispatchNotFound) {
		t.Fatalf("uninitialized direct dispatch error=%v", err)
	}
	if signer.callCount() != 0 || appender.callCount() != 0 {
		t.Fatal("uninitialized admission reached signing or the command-bus publish")
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.command_outbox
WHERE outbox_id = 'first-outbox'
  AND (
      prepared_signed_envelope_bytes IS NOT NULL
      OR published_at IS NOT NULL
  )`)
	if _, err := repository.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     created.ExecutionID,
		Generation:      created.Generation,
		IndexGeneration: created.IndexGeneration,
		MetaID:          created.IndexMetaID,
		CorrelationID:   "wrong-correlation",
	}); !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("mismatched metadata transition error=%v", err)
	}
	if _, err := repository.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     created.ExecutionID,
		Generation:      created.Generation,
		IndexGeneration: created.IndexGeneration + 1,
		MetaID:          created.IndexMetaID,
		CorrelationID:   created.IndexMetaCorrelationID,
	}); !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("mismatched logical generation transition error=%v", err)
	}
	initializedAt, err := repository.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     created.ExecutionID,
		Generation:      created.Generation,
		IndexGeneration: created.IndexGeneration,
		MetaID:          created.IndexMetaID,
		CorrelationID:   created.IndexMetaCorrelationID,
	})
	if err != nil || initializedAt.IsZero() {
		t.Fatalf("initialize index metadata: at=%v err=%v", initializedAt, err)
	}
	replayedInitialization, err := repository.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     created.ExecutionID,
		Generation:      created.Generation,
		IndexGeneration: created.IndexGeneration,
		MetaID:          created.IndexMetaID,
		CorrelationID:   created.IndexMetaCorrelationID,
	})
	if err != nil || !replayedInitialization.Equal(initializedAt) {
		t.Fatalf("replayed metadata initialization changed timestamp: first=%v replay=%v err=%v", initializedAt, replayedInitialization, err)
	}
	pending, err = outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAdmissionID(pending, "first-outbox") {
		t.Fatalf("initialized metadata did not open dispatch gate: %v", pending)
	}

	replayed, err := newPostgresIndexAdmissionService(t, repository, "replay").Submit(ctx, request)
	if err != nil || replayed.Created ||
		replayed.ExecutionID != created.ExecutionID || replayed.CommandID != created.CommandID ||
		replayed.Generation != created.Generation ||
		replayed.IndexGeneration != created.IndexGeneration ||
		replayed.IndexMetaID != created.IndexMetaID || replayed.IndexMetaCorrelationID != created.IndexMetaCorrelationID ||
		replayed.IndexMetaInitializedAt == nil || !replayed.IndexMetaInitializedAt.Equal(initializedAt) ||
		!replayed.AdmittedAt.Equal(created.AdmittedAt) || !replayed.Deadline.Equal(created.Deadline) {
		t.Fatalf("exact replay changed durable identity: created=%+v replay=%+v err=%v", created, replayed, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
	assertPostgresCount(t, ctx, pool, 2, `SELECT count(*) FROM elitea_runtime.input_bundle_entries`)

	conflict := request
	conflict.Inputs.ToolParameters = json.RawMessage(`{"index_name":"other"}`)
	if _, err := newPostgresIndexAdmissionService(t, repository, "conflict").Submit(ctx, conflict); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("changed request reused idempotency key: %v", err)
	}

	capacity := postgresIndexSubmitRequest("request-2", "second")
	if _, err := newPostgresIndexAdmissionService(t, repository, "capacity").Submit(ctx, capacity); !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) {
		t.Fatalf("active index capacity was not enforced: %v", err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET configuration_type = 'must-not-exist'
WHERE execution_id = $1`, created.ExecutionID); err == nil {
		t.Fatal("index.ingest.v1 accepted a validation-only execution column")
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id,
    resource_project_id, projection_project_id, actor_id, principal_ref,
    capability_id, capability_version, input_bundle_id, request_digest,
    idempotency_scope, idempotency_key, state, desired_state
)
SELECT 'invalid-validation-shape', 1, 'invalid-validation-command', tenant_id,
       resource_project_id, projection_project_id, actor_id, principal_ref,
       'configuration.validate.v1', '1', input_bundle_id, request_digest,
       'invalid-validation-scope', 'invalid-validation-key', 'PENDING', 'RUNNING'
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1`, created.ExecutionID); err == nil {
		t.Fatal("configuration.validate.v1 accepted missing validation columns")
	}
}

func TestPostgresIndexIngestSameTargetAdmissionExclusion(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	request := postgresIndexSubmitRequest("same-target-first", "shared-index")
	first, err := newPostgresIndexAdmissionService(t, jobs, "same-target-first").Submit(ctx, request)
	if err != nil || !first.Created {
		t.Fatalf("first target admission: outcome=%+v err=%v", first, err)
	}
	replayed, err := newPostgresIndexAdmissionService(t, jobs, "same-target-replay").Submit(ctx, request)
	if err != nil || replayed.Created || replayed.ExecutionID != first.ExecutionID {
		t.Fatalf("same-key replay changed execution: first=%+v replay=%+v err=%v", first, replayed, err)
	}

	conflict := postgresIndexSubmitRequest("same-target-conflict", "shared-index")
	if _, err := newPostgresIndexAdmissionService(t, jobs, "same-target-conflict").Submit(ctx, conflict); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("different key admitted the active target: %v", err)
	} else {
		var active *indexingapp.ActiveIndexConflictError
		if !errors.As(err, &active) || active.TaskID != first.ExecutionID {
			t.Fatalf("active conflict=%v task=%+v want=%q", err, active, first.ExecutionID)
		}
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE toolkit_id = 19
  AND index_name = 'shared-index'`)

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'SUCCEEDED',
    settled_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		first.ExecutionID,
		int64(first.Generation),
	); err != nil {
		t.Fatal(err)
	}
	afterTerminal := postgresIndexSubmitRequest("same-target-after-terminal", "shared-index")
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "same-target-after-terminal").Submit(ctx, afterTerminal)
	if err != nil || !admitted.Created || admitted.ExecutionID == first.ExecutionID {
		t.Fatalf("terminal target did not permit a new execution: first=%+v admitted=%+v err=%v", first, admitted, err)
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE toolkit_id = 19
  AND index_name = 'shared-index'`)
}

func TestPostgresConcurrentSameTargetReturnsExactWinner(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type result struct {
		outcome indexingapp.AdmissionOutcome
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	firstService := newPostgresIndexAdmissionService(t, jobs, "concurrent-a")
	secondService := newPostgresIndexAdmissionService(t, jobs, "concurrent-b")
	for _, attempt := range []struct {
		service *indexingapp.AdmissionService
		request indexingapp.SubmitRequest
	}{
		{
			service: firstService,
			request: postgresIndexSubmitRequest(
				"concurrent-a",
				"concurrent-index",
			),
		},
		{
			service: secondService,
			request: postgresIndexSubmitRequest(
				"concurrent-b",
				"concurrent-index",
			),
		},
	} {
		attempt := attempt
		go func() {
			<-start
			outcome, submitErr := attempt.service.Submit(ctx, attempt.request)
			results <- result{outcome: outcome, err: submitErr}
		}()
	}
	close(start)

	var admitted indexingapp.AdmissionOutcome
	var conflict *indexingapp.ActiveIndexConflictError
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			if !result.outcome.Created || admitted.ExecutionID != "" {
				t.Fatalf("unexpected admitted result=%+v prior=%+v", result, admitted)
			}
			admitted = result.outcome
		case errors.As(result.err, &conflict):
		default:
			t.Fatalf("unexpected concurrent result=%+v", result)
		}
	}
	if admitted.ExecutionID == "" || conflict == nil ||
		conflict.TaskID != admitted.ExecutionID {
		t.Fatalf("admitted=%+v conflict=%+v", admitted, conflict)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
WHERE j.resource_project_id = 1
  AND i.toolkit_id = 19
  AND i.index_name = 'concurrent-index'
  AND j.state = 'PENDING'`)
}

func TestPostgresIndexMetaInitializationRejectsCancelledAdmission(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "cancel-before-init").Submit(
		ctx,
		postgresIndexSubmitRequest("request-cancel-before-init", "cancelled"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1
  AND generation = $2
  AND state = 'PENDING'`,
		admitted.ExecutionID,
		int64(admitted.Generation),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     admitted.ExecutionID,
		Generation:      admitted.Generation,
		IndexGeneration: admitted.IndexGeneration,
		MetaID:          admitted.IndexMetaID,
		CorrelationID:   admitted.IndexMetaCorrelationID,
	}); !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("cancelled metadata transition error=%v", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_meta_initialized_at IS NOT NULL`, admitted.ExecutionID)
	pending, err := outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsAdmissionID(pending, "cancel-before-init-outbox") {
		t.Fatalf("cancelled admission became dispatchable: %v", pending)
	}
	if _, err := outbox.LoadPendingIndexIngest(ctx, "cancel-before-init-outbox"); !errors.Is(err, ErrPendingIndexIngestDispatchNotFound) {
		t.Fatalf("cancelled direct dispatch load=%v", err)
	}
}

func postgresIndexSubmitRequest(idempotencyKey, indexName string) indexingapp.SubmitRequest {
	return indexingapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-postgres",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "7",
		},
		IdempotencyKey:  idempotencyKey,
		CorrelationID:   "message-" + idempotencyKey,
		ClientStreamID:  "stream-" + idempotencyKey,
		ClientMessageID: "message-" + idempotencyKey,
		SIOEvent:        indexingapp.CurrentIndexSIOEvent,
		ToolkitID:       19,
		Initiator:       executiondomain.IndexIngestInitiatorUser,
		Inputs: indexingapp.AuthoritativeInputs{
			ToolkitConfiguration: json.RawMessage(`{"id":19,"type":"confluence","settings":{"token":"secret-ref://toolkit/19"}}`),
			ToolParameters:       json.RawMessage(fmt.Sprintf(`{"index_name":%q}`, indexName)),
		},
	}
}

func newPostgresIndexAdmissionService(t *testing.T, repository *IndexIngestJobsRepository, prefix string) *indexingapp.AdmissionService {
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
	service, err := indexingapp.NewAdmissionService(repository, factory, time.Now, postgresIndexIDs(
		prefix+"-execution",
		prefix+"-command",
		prefix+"-outbox",
		prefix+"-index-meta",
	))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func postgresIndexIDs(values ...string) executionapp.IDGenerator {
	return func() (string, error) {
		if len(values) == 0 {
			return "", errors.New("unexpected index ID allocation")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
}
