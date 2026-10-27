package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// TestPostgresServiceBackedIndexIngestOutput is a real PostgreSQL 16-18
// integration gate. It proves the admitted input binding, durable artifact
// attestation, terminal projection, idempotent replay and settlement lifecycle.
// The artifact row is seeded as the contract of the future upload data plane;
// this test deliberately does not pretend that elitea-main stores artifact
// bytes or implements that upload path.
func TestPostgresServiceBackedIndexIngestOutput(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	dispatchPolicy := IndexIngestDispatchPolicy{

		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest("request-output", "output")
	request.Inputs.EmbeddingBinding = &indexingapp.EmbeddingBinding{
		SchemaVersion:          indexingapp.CurrentEmbeddingBindingSchema,
		ModelName:              "text-embedding-3-small",
		ResolvedModelGroup:     "1_text-embedding-3-small",
		Route:                  "public",
		ConfigurationProjectID: 1,
		ConfigurationUUID:      "00000000-0000-0000-0000-000000000111",
		ConfigurationDigest:    runtimedomain.SHA256([]byte("configuration:output")),
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "output").Submit(
		context.Background(),
		request,
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}

	outputPolicy := IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	}
	results, err := NewIndexIngestResultsRepository(pool, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatalf("load SQLC-backed admitted index binding: %v", err)
	}
	if expected.CommandID != admitted.CommandID || expected.ExecutionID != admitted.ExecutionID || expected.InputBundleID == "" || expected.Bindings.ToolkitConfiguration.EntryID == "" || expected.Bindings.ToolParameters.EntryID == "" || expected.Bindings.LLMModel.Present || expected.Bindings.LLMConfiguration.Present || expected.Bindings.MCPTokens.Present || !expected.Bindings.EmbeddingBinding.Present {
		t.Fatalf("unexpected admitted index output binding: %+v", expected)
	}

	fence := claimPostgresIndexExecution(t, pool, expected)
	frame, artifact := postgresIndexOutputFrame(t, expected, fence)
	service := newPostgresIndexOutputService(t, pool, results)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := service.IngestIndex(ctx, frame); !errors.Is(err, outputapp.ErrIndexIngestArtifactUnavailable) {
		t.Fatalf("terminal metadata was accepted without a durable artifact attestation: %v", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.output_inbox`)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.index_ingest_results`)

	seedPostgresIndexArtifactAttestation(t, pool, expected, artifact)
	inserted, err := service.IngestIndex(ctx, frame)
	if err != nil || !inserted.Inserted || inserted.Cursor == 0 || inserted.CommittedSequence != 1 {
		t.Fatalf("project durable index result: outcome=%+v err=%v", inserted, err)
	}
	replayed, err := service.IngestIndex(ctx, frame)
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor || replayed.CommittedSequence != inserted.CommittedSequence {
		t.Fatalf("exact index output replay changed durable position: first=%+v replay=%+v err=%v", inserted, replayed, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE payload_type = 'INDEX_INGEST_RESULT' AND projected_at IS NOT NULL`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.index_ingest_results`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = 'index.ingest.completed'`, expected.ExecutionID)

	var bundleID, artifactID, artifactVersion, storageRecordID string
	if err := pool.QueryRow(ctx, `
SELECT input_bundle_id, artifact_id, artifact_immutable_version,
       artifact_storage_record_id
FROM elitea_runtime.index_ingest_results
WHERE execution_id = $1 AND generation = 1`, expected.ExecutionID).Scan(
		&bundleID, &artifactID, &artifactVersion, &storageRecordID,
	); err != nil {
		t.Fatal(err)
	}
	if bundleID != expected.InputBundleID || artifactID != artifact.Reference.ArtifactID || artifactVersion != artifact.Reference.ImmutableVersion || storageRecordID != artifact.StorageRecordID {
		t.Fatalf("projected index result changed immutable identities: bundle=%q artifact=%q version=%q storage=%q", bundleID, artifactID, artifactVersion, storageRecordID)
	}

	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || receipt.ID == "" || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("settle projected index execution: receipt=%+v err=%v", receipt, err)
	}
	replayedReceipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || replayedReceipt != receipt {
		t.Fatalf("exact settlement replay changed receipt: first=%+v replay=%+v err=%v", receipt, replayedReceipt, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_settlements WHERE execution_id = $1 AND disposition = 'SUCCEEDED'`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND state = 'SUCCEEDED'`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_claims WHERE execution_id = $1 AND release_reason = 'SETTLED' AND released_at IS NOT NULL`, expected.ExecutionID)

	assertPostgresIndexArtifactMetadataOnly(t, ctx, pool)
}

// TestPostgresServiceBackedInlineIndexSummary proves the first GitHub
// index_data PoV path against real PostgreSQL. The current SDK's nested
// status/message result becomes durable without an artifact uploader, while
// the outer SDK result and redeemed toolkit configuration never cross this
// contract.
func TestPostgresServiceBackedInlineIndexSummary(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	dispatchPolicy := IndexIngestDispatchPolicy{

		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "inline-output").Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-inline-output", "inline"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit inline index execution: outcome=%+v err=%v", admitted, err)
	}

	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatalf("load admitted inline index binding: %v", err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	summary := outputapp.IndexIngestSummary{
		Status:         outputapp.IndexIngestStatusOK,
		Message:        "No new documents to index.",
		TerminalState:  outputapp.IndexIngestTerminalCreated,
		Indexed:        17,
		Updated:        0,
		ReindexPresent: true,
		Reindex:        false,
	}
	frame := postgresInlineIndexOutputFrame(t, expected, fence, summary)
	service := newPostgresIndexOutputService(t, pool, results)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inserted, err := service.IngestIndex(ctx, frame)
	if err != nil || !inserted.Inserted || inserted.Cursor == 0 {
		t.Fatalf("project inline index summary: outcome=%+v err=%v", inserted, err)
	}
	replayed, err := service.IngestIndex(ctx, frame)
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor {
		t.Fatalf("replay inline index summary: first=%+v replay=%+v err=%v", inserted, replayed, err)
	}

	var artifactID, artifactVersion, storageRecordID *string
	var status, message string
	if err := pool.QueryRow(ctx, `
SELECT artifact_id, artifact_immutable_version, artifact_storage_record_id,
       completion_status, completion_message
FROM elitea_runtime.index_ingest_results
WHERE execution_id = $1 AND generation = 1`, expected.ExecutionID).Scan(
		&artifactID, &artifactVersion, &storageRecordID, &status, &message,
	); err != nil {
		t.Fatal(err)
	}
	if artifactID != nil || artifactVersion != nil || storageRecordID != nil || status != string(summary.Status) || message != summary.Message {
		t.Fatalf("persisted terminal shape changed: artifact=%v/%v/%v status=%q message=%q", artifactID, artifactVersion, storageRecordID, status, message)
	}
	wantReplay := []byte(`{"status":"ok","message":"No new documents to index."}`)
	var replayBytes []byte
	if err := pool.QueryRow(ctx, `
SELECT event_bytes
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = 1 AND event_type = 'index.ingest.completed'`, expected.ExecutionID).Scan(&replayBytes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayBytes, wantReplay) {
		t.Fatalf("durable replay differs from current nested SDK result: got=%s want=%s", replayBytes, wantReplay)
	}
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.index_result_artifacts`)
	var notificationProjectID, notificationUserID int32
	var notificationMeta []byte
	if err := pool.QueryRow(ctx, `
SELECT project_id, user_id, meta
FROM centry.notifications
WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(frame.LogicalOutputID),
	).Scan(
		&notificationProjectID,
		&notificationUserID,
		&notificationMeta,
	); err != nil {
		t.Fatalf("load current terminal notification: %v", err)
	}
	var notification map[string]any
	if err := json.Unmarshal(notificationMeta, &notification); err != nil {
		t.Fatal(err)
	}
	if notificationProjectID != 1 ||
		notificationUserID != 7 ||
		notification["state"] != "created" ||
		notification["initiator"] != "user" ||
		notification["indexed"] != float64(17) ||
		notification["reindex"] != false ||
		notification["message"] !=
			`Index [inline]() is successfully created: {"indexed": 17}` {
		t.Fatalf(
			"current terminal notification changed: project=%d user=%d meta=%s",
			notificationProjectID,
			notificationUserID,
			notificationMeta,
		)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM centry.notifications
WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(frame.LogicalOutputID),
	)

	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || receipt.ID == "" || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("settle inline index execution: receipt=%+v err=%v", receipt, err)
	}
}

// TestPostgresServiceBackedIndexBusinessFailure crosses real PostgreSQL
// admission, failed typed projection, exact retry, UI replay, safe terminal
// error loading and failed settlement. The raw SDK error is intentionally not
// represented: the worker replaces it with the fixed safe summary first.
func TestPostgresServiceBackedIndexBusinessFailure(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := IndexIngestDispatchPolicy{
		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "business-failure").Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-business-failure", "failure"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit failed index execution: outcome=%+v err=%v", admitted, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(context.Background(), indexingapp.IndexMetaInitialization{
		ExecutionID:     admitted.ExecutionID,
		Generation:      admitted.Generation,
		IndexGeneration: admitted.IndexGeneration,
		MetaID:          admitted.IndexMetaID,
		CorrelationID:   admitted.IndexMetaCorrelationID,
	}); err != nil {
		t.Fatal(err)
	}
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    policy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, admitted.Generation)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	summary := outputapp.IndexIngestSummary{
		Status:  outputapp.IndexIngestStatusError,
		Message: "Indexing failed before completion.",
	}
	frame := postgresInlineIndexOutputFrame(t, expected, fence, summary)
	service := newPostgresIndexOutputService(t, pool, results)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	inserted, err := service.IngestIndex(ctx, frame)
	if err != nil || !inserted.Inserted || inserted.Cursor == 0 {
		t.Fatalf("project failed index summary: outcome=%+v err=%v", inserted, err)
	}
	replayed, err := service.IngestIndex(ctx, frame)
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor {
		t.Fatalf("retry failed index summary: first=%+v retry=%+v err=%v", inserted, replayed, err)
	}
	wantReplay := []byte(`{"status":"error","message":"Indexing failed before completion."}`)
	var replayType string
	var replayBytes, payloadBytes []byte
	if err := pool.QueryRow(ctx, `
SELECT r.event_type, r.event_bytes, o.payload_bytes
FROM elitea_runtime.execution_replay_events AS r
JOIN elitea_runtime.output_inbox AS o ON o.event_id = r.event_id
WHERE r.execution_id = $1 AND r.generation = 1`, expected.ExecutionID).Scan(
		&replayType,
		&replayBytes,
		&payloadBytes,
	); err != nil {
		t.Fatal(err)
	}
	canary := []byte("RAW_SDK_CREDENTIAL_CANARY")
	if replayType != replayEventIndexIngest || !bytes.Equal(replayBytes, wantReplay) ||
		bytes.Contains(replayBytes, canary) || bytes.Contains(payloadBytes, canary) {
		t.Fatalf("failed index replay or payload was unsafe: type=%q replay=%s", replayType, replayBytes)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*) FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_meta_terminal_state = 'failed'
  AND index_meta_terminal_status = 'PENDING'`, expected.ExecutionID)

	terminals, err := NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := terminals.ClaimPendingTerminalEffects(ctx, "business-failure-claim", 1, time.Minute)
	if err != nil || len(claims) != 1 ||
		claims[0].SafeError != summary.Message ||
		claims[0].State != indexingapp.CurrentIndexMetaFailed {
		t.Fatalf("load safe failed terminal intent: claims=%+v err=%v", claims, err)
	}
	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || receipt.Outcome != executionapp.SettlementFailed {
		t.Fatalf("settle failed index execution: receipt=%+v err=%v", receipt, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*) FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND state = 'FAILED'`, expected.ExecutionID)
}

func claimPostgresIndexExecution(t *testing.T, pool *pgxpool.Pool, expected outputapp.ExpectedIndexIngest) runtimedomain.Fence {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	envelope := []byte("signed-index-command:" + expected.CommandID)
	envelopeDigest := runtimedomain.SHA256(envelope)
	preparedAt := time.Now().UTC()
	var outboxID string
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.command_outbox
SET prepared_signed_envelope_bytes = $3,
    prepared_signed_envelope_digest = $4,
    prepared_signature_profile = 1,
    prepared_key_id = 'postgres-index-output-key',
    prepared_at = $5,
    published_at = $5,
    last_visibility_at = $5,
    published_envelope_digest = $4,
    publish_attempts = 1
WHERE execution_id = $1 AND generation = $2
RETURNING outbox_id`, expected.ExecutionID, int64(expected.Generation), envelope, envelopeDigest[:], preparedAt).Scan(&outboxID); err != nil {
		t.Fatalf("prepare admitted index command: %v", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = $1 AND generation = $2`, expected.ExecutionID, int64(expected.Generation)); err != nil {
		t.Fatalf("mark admitted index command dispatched: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit index dispatch fixture: %v", err)
	}

	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := claimService.Claim(ctx, executionapp.ClaimRequest{
		CommandID:            expected.CommandID,
		OutboxID:             outboxID,
		ExecutionID:          expected.ExecutionID,
		Generation:           expected.Generation,
		CapabilityID:         expected.CapabilityID,
		SignedEnvelopeDigest: envelopeDigest,
		WorkloadIdentity:     "spiffe://elitea.test/index-worker/1",
		WorkloadSessionID:    "index-worker-session-1",
		ProducerID:           "index-worker-1",
	})
	if err != nil || decision.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("claim admitted index command through production path: decision=%+v err=%v", decision, err)
	}
	return decision.Lease.Fence
}

func postgresIndexOutputFrame(t *testing.T, expected outputapp.ExpectedIndexIngest, fence runtimedomain.Fence) (outputapp.IndexIngestFrame, outputapp.DurableIndexArtifact) {
	t.Helper()
	artifactBytes := []byte("durable index artifact bytes remain outside PostgreSQL")
	artifactDigest := runtimedomain.SHA256(artifactBytes)
	artifactReference := outputapp.IndexArtifactReference{
		ArtifactID:       "artifact-" + expected.ExecutionID,
		ImmutableVersion: artifactDigest.String(),
		MediaType:        expected.ArtifactContract.MediaType,
		ByteLength:       uint64(len(artifactBytes)),
		Digest:           artifactDigest,
		Classification:   expected.ArtifactContract.Classification,
	}
	result := outputapp.IndexIngestResult{
		InputBundleID:     expected.InputBundleID,
		InputBundleDigest: expected.InputBundleDigest,
		Bindings:          expected.Bindings,
		ResultArtifact:    artifactReference,
	}
	wireResult := &runtimev1.IndexIngestResultV1{
		InputBundleId:        result.InputBundleID,
		InputBundleDigest:    postgresDigestV1(result.InputBundleDigest),
		ToolkitConfiguration: postgresIndexBindingV1(result.Bindings.ToolkitConfiguration),
		ToolParameters:       postgresIndexBindingV1(result.Bindings.ToolParameters),
		ResultArtifact: &runtimev1.IndexIngestArtifactReferenceV1{
			ArtifactId:       artifactReference.ArtifactID,
			ImmutableVersion: artifactReference.ImmutableVersion,
			MediaType:        artifactReference.MediaType,
			ByteLength:       artifactReference.ByteLength,
			Digest:           postgresDigestV1(artifactReference.Digest),
			Classification:   artifactReference.Classification,
		},
	}
	if result.Bindings.LLMModel.Present {
		wireResult.LlmModel = postgresIndexBindingV1(result.Bindings.LLMModel.Binding)
	}
	if result.Bindings.LLMConfiguration.Present {
		wireResult.LlmConfiguration = postgresIndexBindingV1(result.Bindings.LLMConfiguration.Binding)
	}
	if result.Bindings.MCPTokens.Present {
		wireResult.McpTokens = postgresIndexBindingV1(result.Bindings.MCPTokens.Binding)
	}
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireResult)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := runtimedomain.SHA256(encodedResult)
	eventID := fence.CommandID + ":1"
	frame := outputapp.IndexIngestFrame{
		StreamID:            fence.ExecutionID + ":1",
		TenantID:            expected.TenantID,
		ResourceProjectID:   expected.ResourceProjectID,
		ProjectionProjectID: expected.ProjectionProjectID,
		WorkloadSessionID:   fence.WorkloadSessionID,
		ProducerID:          fence.ProducerID,
		EventID:             eventID,
		LogicalOutputID:     expected.LogicalOutputID,
		Sequence:            1,
		OccurredAt:          time.Now().UTC().Truncate(time.Millisecond),
		Fence:               fence,
		PayloadDigest:       payloadDigest,
		EncodedResult:       encodedResult,
		Settlement: executionapp.SettlementProposal{
			Fence:                   fence,
			ProposalID:              fence.CommandID + ":settlement",
			Outcome:                 executionapp.SettlementSucceeded,
			TerminalLogicalOutputID: expected.LogicalOutputID,
			TerminalEventID:         eventID,
			TerminalSequence:        1,
			TerminalPayloadDigest:   payloadDigest,
			IdempotencyKey:          fence.CommandID + ":prepare-settlement",
		},
		Result: result,
	}
	wireSettlement := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: frame.LogicalOutputID,
		TerminalEventId:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   postgresDigestV1(frame.PayloadDigest),
		PrepareIdempotencyKey:   frame.Settlement.IdempotencyKey,
	}
	frame.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(wireSettlement)
	if err != nil {
		t.Fatal(err)
	}
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
	if err := frame.Validate(); err != nil {
		t.Fatalf("build PostgreSQL index output frame: %v", err)
	}
	return frame, outputapp.DurableIndexArtifact{
		Reference:       artifactReference,
		StorageRecordID: "storage-record-" + expected.ExecutionID,
		VerifiedAt:      time.Now().UTC().Truncate(time.Microsecond),
	}
}

func postgresInlineIndexOutputFrame(t *testing.T, expected outputapp.ExpectedIndexIngest, fence runtimedomain.Fence, summary outputapp.IndexIngestSummary) outputapp.IndexIngestFrame {
	t.Helper()
	frame, _ := postgresIndexOutputFrame(t, expected, fence)
	frame.Result.ResultArtifact = outputapp.IndexArtifactReference{}
	frame.Result.ResultSummary = summary
	wireResult := &runtimev1.IndexIngestResultV1{
		InputBundleId:        frame.Result.InputBundleID,
		InputBundleDigest:    postgresDigestV1(frame.Result.InputBundleDigest),
		ToolkitConfiguration: postgresIndexBindingV1(frame.Result.Bindings.ToolkitConfiguration),
		ToolParameters:       postgresIndexBindingV1(frame.Result.Bindings.ToolParameters),
		ResultSummary: &runtimev1.IndexIngestSummaryV1{
			Status:  postgresIndexStatusV1(summary.Status),
			Message: summary.Message,
		},
	}
	if frame.Result.Bindings.LLMModel.Present {
		wireResult.LlmModel = postgresIndexBindingV1(frame.Result.Bindings.LLMModel.Binding)
	}
	if frame.Result.Bindings.LLMConfiguration.Present {
		wireResult.LlmConfiguration = postgresIndexBindingV1(frame.Result.Bindings.LLMConfiguration.Binding)
	}
	if frame.Result.Bindings.MCPTokens.Present {
		wireResult.McpTokens = postgresIndexBindingV1(frame.Result.Bindings.MCPTokens.Binding)
	}
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireResult)
	if err != nil {
		t.Fatal(err)
	}
	frame.EncodedResult = encodedResult
	frame.PayloadDigest = runtimedomain.SHA256(encodedResult)
	frame.Settlement.TerminalPayloadDigest = frame.PayloadDigest
	wireOutcome := runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED
	if summary.Status == outputapp.IndexIngestStatusError {
		frame.Settlement.Outcome = executionapp.SettlementFailed
		wireOutcome = runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED
	}
	wireSettlement := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        wireOutcome,
		TerminalLogicalOutputId: frame.LogicalOutputID,
		TerminalEventId:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   postgresDigestV1(frame.PayloadDigest),
		PrepareIdempotencyKey:   frame.Settlement.IdempotencyKey,
	}
	frame.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(wireSettlement)
	if err != nil {
		t.Fatal(err)
	}
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
	if err := frame.Validate(); err != nil {
		t.Fatalf("build PostgreSQL inline index output frame: %v", err)
	}
	return frame
}

func postgresIndexStatusV1(status outputapp.IndexIngestStatus) runtimev1.IndexIngestStatusV1 {
	switch status {
	case outputapp.IndexIngestStatusOK:
		return runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK
	case outputapp.IndexIngestStatusPartlyIndexed:
		return runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_PARTLY_INDEXED
	case outputapp.IndexIngestStatusError:
		return runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_ERROR
	default:
		return runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_UNSPECIFIED
	}
}

func postgresIndexBindingV1(binding outputapp.IndexInputBinding) *runtimev1.IndexIngestInputBindingV1 {
	return &runtimev1.IndexIngestInputBindingV1{
		EntryId:          binding.EntryID,
		ImmutableVersion: binding.ImmutableVersion,
		ContentDigest:    postgresDigestV1(binding.ContentDigest),
	}
}

func seedPostgresIndexArtifactAttestation(t *testing.T, pool *pgxpool.Pool, expected outputapp.ExpectedIndexIngest, artifact outputapp.DurableIndexArtifact) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resourceProjectID, err := parseProjectID(expected.ResourceProjectID)
	if err != nil {
		t.Fatal(err)
	}
	metadataCreatedAt := artifact.VerifiedAt.Add(-time.Second)
	if _, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.index_result_artifacts (
    artifact_id, immutable_version, execution_id, generation,
    resource_project_id, media_type, byte_length, digest, classification,
    storage_record_id, bytes_verified_at, metadata_created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		artifact.Reference.ArtifactID,
		artifact.Reference.ImmutableVersion,
		expected.ExecutionID,
		int64(expected.Generation),
		resourceProjectID,
		artifact.Reference.MediaType,
		int64(artifact.Reference.ByteLength),
		artifact.Reference.Digest[:],
		artifact.Reference.Classification,
		artifact.StorageRecordID,
		artifact.VerifiedAt,
		metadataCreatedAt,
	); err != nil {
		t.Fatalf("seed future data-plane artifact attestation: %v", err)
	}
}

func newPostgresIndexOutputService(t *testing.T, pool *pgxpool.Pool, results *IndexIngestResultsRepository) *outputapp.IndexIngestService {
	t.Helper()
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service, err := outputapp.NewIndexIngestService(results, claimService, results, results)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func assertPostgresIndexArtifactMetadataOnly(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var columns []string
	if err := pool.QueryRow(ctx, `
SELECT array_agg(column_name::text ORDER BY ordinal_position)
FROM information_schema.columns
WHERE table_schema = 'elitea_runtime'
  AND table_name = 'index_result_artifacts'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"artifact_id", "immutable_version", "execution_id", "generation",
		"resource_project_id", "media_type", "byte_length", "digest",
		"classification", "storage_record_id", "bytes_verified_at",
		"metadata_created_at",
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("artifact attestation table unexpectedly contains data-plane fields: got %v want %v", columns, want)
	}
}
