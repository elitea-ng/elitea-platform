package repos

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeFailureReplayEventMatchesCurrentUIContract(t *testing.T) {
	if replayEventRuntimeFailure != "execution.failed" {
		t.Fatalf("runtime failure replay event = %q", replayEventRuntimeFailure)
	}
}

func TestCurrentAgentCancellationSettlesWithoutFailureMetadata(t *testing.T) {
	for _, test := range []struct {
		name           string
		failureCode    string
		isCancellation bool
	}{
		{name: "failure", failureCode: "INTERNAL"},
		{name: "cancellation", failureCode: "CANCELLED", isCancellation: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &scriptedExecutor{
				execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
			}
			err := persistCurrentAgentRuntimeTerminal(
				t.Context(),
				executor,
				7,
				executiondomain.AgentAdhocCapability,
				outputRecord{ExecutionID: "execution-1", Generation: 3},
				test.failureCode,
				"Execution stopped.",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(executor.execCalls) != 1 {
				t.Fatalf("terminal projection calls=%d", len(executor.execCalls))
			}
			call := executor.execCalls[0]
			if !strings.Contains(call.sql, "WHEN $4::boolean THEN message_group.meta - 'is_error' - 'error'") {
				t.Fatal("terminal projection does not preserve partial content without failure metadata")
			}
			if got, ok := call.args[3].(bool); !ok || got != test.isCancellation {
				t.Fatalf("cancellation discriminator=%#v", call.args[3])
			}
		})
	}
}

func TestCurrentAgentCancellationSettlesAfterEmptyResponseWasRemoved(t *testing.T) {
	executor := &scriptedExecutor{
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")},
	}
	err := persistCurrentAgentRuntimeTerminal(
		t.Context(),
		executor,
		7,
		executiondomain.AgentAdhocCapability,
		outputRecord{ExecutionID: "execution-1", Generation: 3},
		"CANCELLED",
		"Execution was cancelled.",
	)
	if err != nil {
		t.Fatalf("canonical cancellation did not settle after Stop removed an empty response: %v", err)
	}
}

func TestCurrentAgentFailureRequiresExistingResponse(t *testing.T) {
	executor := &scriptedExecutor{
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")},
	}
	err := persistCurrentAgentRuntimeTerminal(
		t.Context(),
		executor,
		7,
		executiondomain.AgentApplicationCapability,
		outputRecord{ExecutionID: "execution-1", Generation: 3},
		"INTERNAL",
		"Execution failed.",
	)
	if err == nil || err.Error() != "current agent terminal response message group is unavailable" {
		t.Fatalf("missing failure response was not rejected: %v", err)
	}
}

func TestConfigurationValidationProjectionUsesOneTenantTransaction(t *testing.T) {
	frame := testValidationFrame(t)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: pgx.ErrNoRows}, // event identity is new
			{err: pgx.ErrNoRows}, // logical identity is new
			{err: pgx.ErrNoRows}, // producer sequence is new
			{values: []any{"claim-1"}},
			{values: []any{"claim-1", "claim-1", "RUNNING", false, false, false}},
			{values: []any{"revision-1"}},
			{values: []any{int64(17)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}
	projects := &scriptedProjectStore{scriptedExecutor: executor}
	repository, err := newConfigurationValidationResultsRepository(projects)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if projects.projectID != 42 || !outcome.Inserted || outcome.Cursor != 17 || outcome.CommittedSequence != 1 {
		t.Fatalf("unexpected projection outcome: project=%d outcome=%+v", projects.projectID, outcome)
	}
	if len(executor.rowCalls) != 7 || len(executor.execCalls) != 2 {
		t.Fatalf("projection escaped its atomic script: row calls=%d exec calls=%d", len(executor.rowCalls), len(executor.execCalls))
	}
}

func TestConfigurationValidationProjectionReturnsTypedDatabaseDeadlineWinner(t *testing.T) {
	frame := testValidationFrame(t)
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"", "claim-1", "RUNNING", false, true, false}},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
	}}
	repository, err := newConfigurationValidationResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if !errors.Is(err, outputapp.ErrOutputDeadlineExceeded) {
		t.Fatalf("database deadline winner was not typed: %v", err)
	}
	if len(executor.rowCalls) != 8 || len(executor.execCalls) != 0 {
		t.Fatalf("late success mutated a business/replay plane: rows=%d execs=%d", len(executor.rowCalls), len(executor.execCalls))
	}
}

func TestConfigurationValidationProjectionDurablyMaterializesCancellationBeforeTypedRejection(t *testing.T) {
	frame := testValidationFrame(t)
	record, _, err := validationOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, browserData, err := canonicalCancellationOutput(record)
	if err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"", "claim-1", "CANCELLED", false, false, false}},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"claim-1", "claim-1", "CANCELLED", false, false, false}},
		{values: []any{int64(29)}},
	}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")}}
	repository, err := newConfigurationValidationResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if !errors.Is(err, outputapp.ErrOutputCancelled) {
		t.Fatalf("expected typed cancellation linearization, got %v", err)
	}
	if len(executor.rowCalls) != 11 || len(executor.execCalls) != 1 {
		t.Fatalf("typed cancellation was returned before durable projection: rows=%d execs=%d", len(executor.rowCalls), len(executor.execCalls))
	}
	insert := executor.rowCalls[9]
	if insert.args[13] != payloadTypeRuntimeFailure || insert.args[17] != string(executionapp.SettlementCancelled) {
		t.Fatalf("materialized output is not canonical cancellation: payload=%v outcome=%v", insert.args[13], insert.args[17])
	}
	if !bytes.Equal(insert.args[15].([]byte), cancelled.PayloadBytes) || !bytes.Equal(insert.args[18].([]byte), cancelled.SettlementBytes) {
		t.Fatal("materialized cancellation bytes differ from the canonical runtime output")
	}
	replay := executor.rowCalls[10]
	if replay.args[4] != replayEventRuntimeFailure || !bytes.Equal(replay.args[5].([]byte), browserData) {
		t.Fatal("materialized cancellation did not append its durable browser event")
	}
}

func TestRuntimeFailureProjectionDurablyMaterializesCancellationBeforeTypedRejection(t *testing.T) {
	frame := testRuntimeFailureFrame(t)
	record, _, err := failureOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, browserData, err := canonicalCancellationOutput(record)
	if err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"", "claim-1", "CANCELLED", false, false, false}},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"claim-1", "claim-1", "CANCELLED", false, false, false}},
		{values: []any{int64(31)}},
	}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")}}
	repository, err := newRuntimeFailureResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ProjectRuntimeFailure(context.Background(), outputapp.RuntimeFailureProjection{
		Frame:        frame,
		BrowserData:  []byte(`{"code":"UNSUPPORTED_CAPABILITY","safe_message":"Unsupported capability.","retryable":false}`),
		CapabilityID: executiondomain.ConfigurationValidationCapability,
	})
	if !errors.Is(err, outputapp.ErrOutputCancelled) {
		t.Fatalf("expected typed cancellation linearization, got %v", err)
	}
	if len(executor.rowCalls) != 11 || len(executor.execCalls) != 1 {
		t.Fatalf("typed cancellation was returned before durable projection: rows=%d execs=%d", len(executor.rowCalls), len(executor.execCalls))
	}
	insert := executor.rowCalls[9]
	if insert.args[13] != payloadTypeRuntimeFailure || insert.args[17] != string(executionapp.SettlementCancelled) {
		t.Fatalf("materialized output is not canonical cancellation: payload=%v outcome=%v", insert.args[13], insert.args[17])
	}
	if !bytes.Equal(insert.args[15].([]byte), cancelled.PayloadBytes) || !bytes.Equal(insert.args[18].([]byte), cancelled.SettlementBytes) {
		t.Fatal("materialized cancellation bytes differ from the canonical runtime output")
	}
	replay := executor.rowCalls[10]
	if replay.args[4] != replayEventRuntimeFailure || !bytes.Equal(replay.args[5].([]byte), browserData) {
		t.Fatal("materialized cancellation did not append its durable browser event")
	}
}

func TestRuntimeFailureProjectionSkipsIndexSideEffectsForNonIndexFailure(t *testing.T) {
	for _, capability := range []string{
		executiondomain.ConfigurationValidationCapability,
		executiondomain.ToolkitCallToolCapability,
		executiondomain.ToolkitAvailableToolsCapability,
	} {
		t.Run(capability, func(t *testing.T) {
			assertNonIndexRuntimeFailureProjection(t, capability)
		})
	}
}

func assertNonIndexRuntimeFailureProjection(t *testing.T, capability string) {
	t.Helper()
	frame := testRuntimeFailureFrame(t)
	record, _, err := failureOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{values: []any{"claim-1"}},
			{values: []any{"claim-1", "claim-1", "RUNNING", false, false, false}},
			{values: []any{int64(41)}},
			{values: outputRecordScanValues(record)},
			{values: outputRecordScanValues(record)},
			{values: outputRecordScanValues(record)},
			{values: []any{int64(41)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 0"),
		},
	}
	repository, err := newRuntimeFailureResultsRepository(
		&scriptedProjectStore{scriptedExecutor: executor},
	)
	if err != nil {
		t.Fatal(err)
	}
	activity := &recordingCurrentIndexActivityProjector{}
	repository.activity = activity
	projection := outputapp.RuntimeFailureProjection{
		Frame:        frame,
		CapabilityID: capability,
		BrowserData: []byte(
			`{"code":"UNSUPPORTED_CAPABILITY","safe_message":"Unsupported capability.","retryable":false}`,
		),
	}
	inserted, err := repository.ProjectRuntimeFailure(context.Background(), projection)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := repository.ProjectRuntimeFailure(context.Background(), projection)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted.Inserted || inserted.Cursor != 41 ||
		replayed.Inserted || replayed.Cursor != 41 ||
		len(executor.execCalls) != 1 {
		t.Fatalf(
			"inserted=%+v replayed=%+v execs=%d",
			inserted,
			replayed,
			len(executor.execCalls),
		)
	}
	if len(activity.terminals) != 0 {
		t.Fatalf("configuration failure reached index Activity projector: %+v", activity.terminals)
	}
}

func TestConfigurationValidationProjectionReplayedOriginalReturnsTypedCancellationForDurableMaterialization(t *testing.T) {
	frame := testValidationFrame(t)
	record, _, err := validationOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, _, err := canonicalCancellationOutput(record)
	if err != nil {
		t.Fatal(err)
	}
	row := scriptedRow{values: outputRecordScanValues(cancelled)}
	executor := &scriptedExecutor{rowResults: []scriptedRow{row, row, row, {values: []any{int64(29)}}}}
	repository, err := newConfigurationValidationResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if !errors.Is(err, outputapp.ErrOutputCancelled) {
		t.Fatalf("replayed original did not recover the durable cancellation winner: %v", err)
	}
	if len(executor.rowCalls) != 4 {
		t.Fatalf("durable cancellation replay reached insertion: %d calls", len(executor.rowCalls))
	}
}

func TestCanonicalCancellationOutputMatchesPythonGeneratedGolden(t *testing.T) {
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository test source")
	}
	corpusRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../../../testdata/proto/runtime/v1/configuration-validation"))
	readFrame := func(caseName, name string) *runtimev1.ExecutionOutputFrameV1 {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(corpusRoot, caseName, name))
		if err != nil {
			t.Fatal(err)
		}
		frame := &runtimev1.ExecutionOutputFrameV1{}
		if err := proto.Unmarshal(raw, frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	for _, caseName := range []string{"valid", "invalid", "unsupported"} {
		t.Run(caseName, func(t *testing.T) {
			original := readFrame(caseName, "expected-output.pb")
			pythonCancellation := readFrame(caseName, "expected-cancelled-output.pb")
			payloadType := payloadTypeConfigurationValidation
			settlementOutcome := executionapp.SettlementSucceeded
			payloadMessage := proto.Message(original.GetConfigurationValidation())
			if original.GetRuntimeError() != nil {
				payloadType = payloadTypeRuntimeFailure
				settlementOutcome = executionapp.SettlementFailed
				payloadMessage = original.GetRuntimeError()
			}
			payloadBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(payloadMessage)
			if err != nil {
				t.Fatal(err)
			}
			settlementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(original.GetSettlementProposal())
			if err != nil {
				t.Fatal(err)
			}
			var token runtimedomain.FenceToken
			copy(token[:], original.GetFence().GetFenceToken())
			source := outputRecord{
				EventID:               original.GetEventId(),
				LogicalOutputID:       original.GetLogicalOutputId(),
				ExecutionID:           original.GetIdentity().GetExecutionId(),
				Generation:            original.GetIdentity().GetGeneration(),
				TenantID:              original.GetIdentity().GetTenantId(),
				ResourceProjectID:     1,
				ProjectionProjectID:   1,
				CommandID:             original.GetIdentity().GetCommandId(),
				WorkloadIdentity:      "spiffe://elitea.test/python-reference",
				WorkloadSessionID:     original.GetFence().GetWorkloadSessionId(),
				ProducerID:            original.GetFence().GetProducerId(),
				ClaimAttempt:          original.GetFence().GetClaimAttempt(),
				LeaseEpoch:            original.GetFence().GetLeaseEpoch(),
				FenceToken:            token,
				StreamID:              original.GetStreamId(),
				Sequence:              original.GetSequence(),
				ClaimHandoffWatermark: original.GetClaimHandoffWatermark(),
				PayloadType:           payloadType,
				PayloadDigest:         runtimedomain.SHA256(payloadBytes),
				PayloadBytes:          payloadBytes,
				SettlementProposalID:  original.GetSettlementProposal().GetProposalId(),
				SettlementOutcome:     settlementOutcome,
				SettlementBytes:       settlementBytes,
				SettlementDigest:      runtimedomain.SHA256(settlementBytes),
				SettlementKey:         original.GetSettlementProposal().GetPrepareIdempotencyKey(),
				OccurredAt:            time.UnixMilli(original.GetOccurredAtUnixMillis()).UTC(),
			}

			actual, _, err := canonicalCancellationOutput(source)
			if err != nil {
				t.Fatal(err)
			}
			expectedPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(pythonCancellation.GetRuntimeError())
			if err != nil {
				t.Fatal(err)
			}
			expectedSettlement, err := proto.MarshalOptions{Deterministic: true}.Marshal(pythonCancellation.GetSettlementProposal())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual.PayloadBytes, expectedPayload) || !bytes.Equal(actual.SettlementBytes, expectedSettlement) {
				t.Fatal("Go canonical cancellation bytes differ from the Python-generated golden")
			}
			if actual.EventID != pythonCancellation.GetEventId() || actual.LogicalOutputID != pythonCancellation.GetLogicalOutputId() || actual.Sequence != pythonCancellation.GetSequence() || actual.OccurredAt.UnixMilli() != pythonCancellation.GetOccurredAtUnixMillis() {
				t.Fatal("Go canonical cancellation changed a terminal identity or occurrence binding")
			}
		})
	}
}

func TestConfigurationValidationProjectionReplaysIdenticalDurableOutputBeforeCancellation(t *testing.T) {
	frame := testValidationFrame(t)
	record, _, err := validationOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	row := scriptedRow{values: outputRecordScanValues(record)}
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		row,
		row,
		row,
		{values: []any{int64(23)}},
	}}
	repository, err := newConfigurationValidationResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Inserted || outcome.Cursor != 23 || outcome.CommittedSequence != frame.Sequence {
		t.Fatalf("unexpected replay outcome: %+v", outcome)
	}
	for _, call := range executor.rowCalls {
		if strings.Contains(call.sql, "WITH authority AS MATERIALIZED") {
			t.Fatal("identical durable replay reached the cancellation/insert linearization")
		}
	}
}

func TestConfigurationValidationProjectionNeverMislabelsProducerSequenceConflictAsCancellation(t *testing.T) {
	frame := testValidationFrame(t)
	record, _, err := validationOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	conflict := record
	conflict.EventID = "other-event"
	conflict.LogicalOutputID = "other-logical-output"
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{"claim-1"}},
		{values: []any{"", "claim-1", "CANCELLED", false, false, false}},
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: outputRecordScanValues(conflict)},
	}}
	repository, err := newConfigurationValidationResultsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ProjectConfigurationValidation(context.Background(), outputapp.ValidationProjection{
		Frame:       frame,
		BrowserData: []byte(`{"configuration_revision_id":"revision-1","valid":true,"issues":[]}`),
	})
	if !errors.Is(err, outputapp.ErrValidationOutputConflict) {
		t.Fatalf("expected producer sequence conflict, got %v", err)
	}
}

func TestSameDurableOutputIncludesAuthenticatedWorkloadIdentity(t *testing.T) {
	record, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	other := record
	other.WorkloadIdentity = "spiffe://elitea.test/worker/other"
	if sameDurableOutput(record, other) {
		t.Fatal("different authenticated workload identity was accepted as replay")
	}
}

func outputRecordScanValues(record outputRecord) []any {
	return []any{
		record.EventID,
		record.LogicalOutputID,
		record.ExecutionID,
		int64(record.Generation),
		record.WorkloadIdentity,
		record.WorkloadSessionID,
		record.ProducerID,
		int64(record.ClaimAttempt),
		int64(record.LeaseEpoch),
		record.FenceToken[:],
		record.StreamID,
		int64(record.Sequence),
		int64(record.ClaimHandoffWatermark),
		record.PayloadType,
		record.PayloadDigest[:],
		record.PayloadBytes,
		record.SettlementProposalID,
		string(record.SettlementOutcome),
		record.SettlementBytes,
		record.SettlementDigest[:],
		record.SettlementKey,
		record.OccurredAt,
	}
}

func testValidationFrame(t *testing.T) outputapp.ConfigurationValidationFrame {
	t.Helper()
	binding := configurationdomain.ValidationBinding{
		Command: configurationdomain.ValidationCommand{
			ConfigurationRevisionID: "revision-1",
			ConfigurationType:       "openapi",
			CatalogRevision:         "catalog-v1",
			CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
			SchemaID:                "openapi",
			SchemaRevision:          "schema-v1",
			SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
			SettingsEntryID:         "settings",
		},
		InputBundleID:         "bundle-1",
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		SettingsEntryVersion:  "revision-1",
		SettingsContentDigest: runtimedomain.SHA256([]byte("{}\n")),
	}
	payload := []byte("result-protobuf")
	frame := outputapp.ConfigurationValidationFrame{
		StreamID:            "execution-1:1",
		TenantID:            "tenant-1",
		ResourceProjectID:   "42",
		ProjectionProjectID: "42",
		WorkloadSessionID:   "session-1",
		ProducerID:          "producer-1",
		EventID:             "command-1:1",
		LogicalOutputID:     "configuration-validation:revision-1",
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
		Fence: runtimedomain.Fence{
			CommandID:         "command-1",
			ExecutionID:       "execution-1",
			Generation:        1,
			WorkloadIdentity:  "spiffe://elitea.test/worker/1",
			WorkloadSessionID: "session-1",
			ProducerID:        "producer-1",
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token"))),
		},
		PayloadDigest: runtimedomain.SHA256(payload),
		EncodedResult: payload,
		Result:        configurationdomain.ValidationResult{Binding: binding, Valid: true},
	}
	frame.Settlement = executionapp.SettlementProposal{
		Fence:                   frame.Fence,
		ProposalID:              "command-1:settlement",
		Outcome:                 executionapp.SettlementSucceeded,
		TerminalLogicalOutputID: frame.LogicalOutputID,
		TerminalEventID:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   frame.PayloadDigest,
		IdempotencyKey:          "command-1:prepare-settlement",
	}
	wire := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: frame.LogicalOutputID,
		TerminalEventId:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     frame.PayloadDigest[:],
		},
		PrepareIdempotencyKey: frame.Settlement.IdempotencyKey,
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	frame.EncodedSettlement = encoded
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(encoded)
	return frame
}

func testRuntimeFailureFrame(t *testing.T) outputapp.RuntimeFailureFrame {
	t.Helper()
	validation := testValidationFrame(t)
	failure := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY,
		SafeMessage: "Unsupported capability.",
		Retryable:   false,
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := runtimedomain.SHA256(payload)
	settlement := executionapp.SettlementProposal{
		Fence:                   validation.Fence,
		ProposalID:              validation.Settlement.ProposalID,
		Outcome:                 executionapp.SettlementFailed,
		TerminalLogicalOutputID: validation.LogicalOutputID,
		TerminalEventID:         validation.EventID,
		TerminalSequence:        validation.Sequence,
		TerminalPayloadDigest:   payloadDigest,
		IdempotencyKey:          validation.Settlement.IdempotencyKey,
	}
	wire := &runtimev1.SettlementProposalV1{
		ProposalId:              settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED,
		TerminalLogicalOutputId: settlement.TerminalLogicalOutputID,
		TerminalEventId:         settlement.TerminalEventID,
		TerminalSequence:        settlement.TerminalSequence,
		TerminalPayloadDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     payloadDigest[:],
		},
		PrepareIdempotencyKey: settlement.IdempotencyKey,
	}
	encodedSettlement, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	settlement.ProposalDigest = runtimedomain.SHA256(encodedSettlement)
	return outputapp.RuntimeFailureFrame{
		StreamID:              validation.StreamID,
		TenantID:              validation.TenantID,
		ResourceProjectID:     validation.ResourceProjectID,
		ProjectionProjectID:   validation.ProjectionProjectID,
		WorkloadSessionID:     validation.WorkloadSessionID,
		ProducerID:            validation.ProducerID,
		EventID:               validation.EventID,
		LogicalOutputID:       validation.LogicalOutputID,
		Sequence:              validation.Sequence,
		ClaimHandoffWatermark: validation.ClaimHandoffWatermark,
		OccurredAt:            validation.OccurredAt,
		Fence:                 validation.Fence,
		PayloadDigest:         payloadDigest,
		EncodedFailure:        payload,
		Settlement:            settlement,
		EncodedSettlement:     encodedSettlement,
		Failure: outputapp.RuntimeFailure{
			Code:        "UNSUPPORTED_CAPABILITY",
			SafeMessage: "Unsupported capability.",
			Retryable:   false,
		},
	}
}
