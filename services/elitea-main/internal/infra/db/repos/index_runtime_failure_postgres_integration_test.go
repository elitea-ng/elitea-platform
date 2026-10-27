package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

// TestPostgresServiceBackedIndexRuntimeFailureSettlement crosses real
// PostgreSQL admission, index claim, capability-bound failure projection,
// durable replay and settlement. It is not a gRPC-listener or browser E2E.
func TestPostgresServiceBackedIndexRuntimeFailureSettlement(t *testing.T) {
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
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "runtime-failure").Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-runtime-failure", "fail"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}

	indexResults, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := indexResults.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatalf("load admitted index binding: %v", err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	frame := postgresIndexRuntimeFailureFrame(t, expected, fence)
	service := newPostgresRuntimeFailureService(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inserted, err := service.IngestFailure(ctx, frame)
	if err != nil || !inserted.Inserted || inserted.Cursor == 0 || inserted.CommittedSequence != 1 {
		t.Fatalf("project index runtime failure: outcome=%+v err=%v", inserted, err)
	}
	replayed, err := service.IngestFailure(ctx, frame)
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor || replayed.CommittedSequence != inserted.CommittedSequence {
		t.Fatalf("replay index runtime failure: first=%+v replay=%+v err=%v", inserted, replayed, err)
	}

	var eventType string
	var eventBytes, eventDigest []byte
	if err := pool.QueryRow(ctx, `
SELECT event_type, event_bytes, event_digest
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND projection_project_id = 1`, expected.ExecutionID).Scan(
		&eventType, &eventBytes, &eventDigest,
	); err != nil {
		t.Fatal(err)
	}
	wantEvent := []byte(`{"code":"INTERNAL","safe_message":"The runtime operation failed.","retryable":false}`)
	wantEventDigest := runtimedomain.SHA256(eventBytes)
	if eventType != replayEventRuntimeFailure || !bytes.Equal(eventBytes, wantEvent) || !json.Valid(eventBytes) || !bytes.Equal(eventDigest, wantEventDigest[:]) {
		t.Fatalf("unsafe or invalid index failure replay: type=%q data=%s digest=%x", eventType, eventBytes, eventDigest)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1 AND payload_type = 'RUNTIME_FAILURE' AND projected_at IS NOT NULL`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.index_ingest_results WHERE execution_id = $1`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.index_result_artifacts WHERE execution_id = $1`, expected.ExecutionID)

	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || receipt.ID == "" || receipt.Outcome != executionapp.SettlementFailed {
		t.Fatalf("settle index runtime failure: receipt=%+v err=%v", receipt, err)
	}
	replayedReceipt, err := settlements.PrepareSettlement(ctx, frame.Settlement)
	if err != nil || replayedReceipt != receipt {
		t.Fatalf("replay index failure settlement: first=%+v replay=%+v err=%v", receipt, replayedReceipt, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_settlements WHERE execution_id = $1 AND disposition = 'FAILED' AND error_code = 'INTERNAL'`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND state = 'FAILED'`, expected.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_claims WHERE execution_id = $1 AND release_reason = 'SETTLED' AND released_at IS NOT NULL`, expected.ExecutionID)
}

func postgresIndexRuntimeFailureFrame(t *testing.T, expected outputapp.ExpectedIndexIngest, fence runtimedomain.Fence) outputapp.RuntimeFailureFrame {
	t.Helper()
	failure := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL,
		SafeMessage: "The runtime operation failed.",
		Retryable:   false,
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := runtimedomain.SHA256(payload)
	eventID := fence.CommandID + ":1"
	frame := outputapp.RuntimeFailureFrame{
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
		EncodedFailure:      payload,
		Failure: outputapp.RuntimeFailure{
			Code:        "INTERNAL",
			SafeMessage: failure.GetSafeMessage(),
			Retryable:   failure.GetRetryable(),
		},
	}
	frame.Settlement = executionapp.SettlementProposal{
		Fence:                   fence,
		ProposalID:              fence.CommandID + ":settlement",
		Outcome:                 executionapp.SettlementFailed,
		TerminalLogicalOutputID: frame.LogicalOutputID,
		TerminalEventID:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   frame.PayloadDigest,
		IdempotencyKey:          fence.CommandID + ":prepare-settlement",
	}
	wireSettlement := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED,
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
		t.Fatalf("build PostgreSQL index runtime failure: %v", err)
	}
	return frame
}
