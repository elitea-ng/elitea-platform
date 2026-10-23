package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestExpectedRuntimeFailureUsesExactCapabilitySpecificDurableBindings(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		logical    string
	}{
		{
			name:       "configuration validation",
			capability: executiondomain.ConfigurationValidationCapability,
			logical:    "configuration-validation:revision-1",
		},
		{
			name:       "index ingest",
			capability: executiondomain.IndexIngestCapability,
			logical:    "index-ingest:execution-1",
		},
		{
			name:       "application agent",
			capability: executiondomain.AgentApplicationCapability,
			logical:    "agent-execution:execution-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
				"tenant-1", "42", "42", test.capability, "command-1",
				"execution-1", int64(1), test.logical,
			}}}}
			repository := newOutputInboxRepository(store)
			expected, err := repository.ExpectedRuntimeFailure(context.Background(), "execution-1", 1)
			if err != nil {
				t.Fatal(err)
			}
			if expected.CapabilityID != test.capability || expected.LogicalOutputID != test.logical || expected.Generation != 1 {
				t.Fatalf("unexpected runtime failure binding: %+v", expected)
			}
			if len(store.rowCalls) != 1 || len(store.rowCalls[0].args) != 2 || store.rowCalls[0].args[0] != "execution-1" || store.rowCalls[0].args[1] != int64(1) {
				t.Fatalf("runtime failure lookup is not execution/generation bound: %+v", store.rowCalls)
			}
			query := store.rowCalls[0].sql
			for _, fragment := range []string{
				"j.capability_id = 'configuration.validate.v1'",
				"j.capability_id = 'index.ingest.v1'",
				"e.entry_id = j.settings_entry_id",
				"i.capability_id = j.capability_id",
				"i.input_bundle_id = j.input_bundle_id",
				"'agent.execute.application.v1'",
				"a.input_bundle_id = j.input_bundle_id",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatalf("runtime failure binding SQL is missing %q", fragment)
				}
			}
		})
	}
}

func TestExpectedRuntimeFailureRejectsUnsupportedDurableCapability(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"tenant-1", "42", "42", "provider.execute.v1", "command-1",
		"execution-1", int64(1), "provider:execution-1",
	}}}}
	repository := newOutputInboxRepository(store)
	if _, err := repository.ExpectedRuntimeFailure(context.Background(), "execution-1", 1); !errors.Is(err, outputapp.ErrInvalidValidationOutput) {
		t.Fatalf("unsupported capability error = %v", err)
	}
	emptyStore := &scriptedExecutor{}
	if _, err := newOutputInboxRepository(emptyStore).ExpectedRuntimeFailure(context.Background(), "execution-1", ^uint64(0)); !errors.Is(err, outputapp.ErrInvalidValidationOutput) {
		t.Fatalf("overflowing generation error = %v", err)
	}
	if len(emptyStore.rowCalls) != 0 {
		t.Fatal("overflowing generation reached PostgreSQL")
	}
}

func TestOutputRecordCouplesPayloadTypeToTerminalOutcome(t *testing.T) {
	validation, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	runtimeFailure := validation
	runtimeFailure.PayloadType = payloadTypeRuntimeFailure
	runtimeFailure.SettlementOutcome = executionapp.SettlementFailed
	indexResult := validation
	indexResult.PayloadType = payloadTypeIndexIngestResult

	tests := []struct {
		name    string
		record  outputRecord
		wantErr bool
	}{
		{name: "validation success", record: validation},
		{name: "index success", record: indexResult},
		{name: "index failure", record: withSettlementOutcome(indexResult, executionapp.SettlementFailed)},
		// A Rust run stopped after it touched the store reports the store and
		// settles cancelled (ADR-0030 I1); an unknown outcome stays refused.
		{name: "cancelled index", record: withSettlementOutcome(indexResult, executionapp.SettlementCancelled)},
		{name: "outcome unknown index", record: withSettlementOutcome(indexResult, executionapp.SettlementOutcomeUnknown), wantErr: true},
		{name: "runtime failure", record: runtimeFailure},
		{name: "cancelled runtime failure", record: withSettlementOutcome(runtimeFailure, executionapp.SettlementCancelled)},
		{name: "cancelled validation", record: withSettlementOutcome(validation, executionapp.SettlementCancelled), wantErr: true},
		{name: "successful runtime failure", record: withSettlementOutcome(runtimeFailure, executionapp.SettlementSucceeded), wantErr: true},
		{name: "outcome unknown runtime failure", record: withSettlementOutcome(runtimeFailure, executionapp.SettlementOutcomeUnknown), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.record.validate()
			if test.wantErr && !errors.Is(err, outputapp.ErrInvalidValidationOutput) {
				t.Fatalf("expected invalid output, got %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("expected valid output, got %v", err)
			}
		})
	}
}

func TestInsertOutputInboxCancellationPredicateIsNarrow(t *testing.T) {
	validation, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	cancelledFailure := validation
	cancelledFailure.PayloadType = payloadTypeRuntimeFailure
	cancelledFailure.SettlementOutcome = executionapp.SettlementCancelled

	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"claim-1"}},
		{values: []any{"claim-1", "claim-1", "CANCELLED", false, false, false}},
	}}
	result, err := insertOutputInbox(context.Background(), executor, cancelledFailure)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Inserted || result.CancellationRejected {
		t.Fatal("correctly fenced cancelled runtime failure was not inserted")
	}
	if len(executor.rowCalls) != 2 {
		t.Fatalf("unexpected insert query count: %d", len(executor.rowCalls))
	}
	lockCall := executor.rowCalls[0]
	for _, predicate := range []string{"c.initial_output_watermark = $13", "FOR UPDATE OF j, o, c"} {
		if !strings.Contains(lockCall.sql, predicate) {
			t.Fatalf("output authority lock SQL is missing %q", predicate)
		}
	}
	call := executor.rowCalls[1]
	for _, predicate := range []string{
		"authority.desired_state = 'RUNNING'",
		"authority.desired_state = 'CANCELLED'",
		"$14 = 'RUNTIME_FAILURE'",
		"$18 = 'CANCELLED'",
	} {
		if !strings.Contains(call.sql, predicate) {
			t.Fatalf("output admission SQL is missing %q", predicate)
		}
	}
	if strings.Contains(call.sql, "FOR UPDATE") {
		t.Fatal("output state query shares the authority-lock statement snapshot")
	}
	if len(call.args) != 27 {
		t.Fatalf("unexpected output insert argument count: %d", len(call.args))
	}
	if call.args[13] != payloadTypeRuntimeFailure || call.args[17] != string(executionapp.SettlementCancelled) {
		t.Fatalf("cancellation predicate is not bound to runtime failure/cancelled: payload=%v outcome=%v", call.args[13], call.args[17])
	}
}

func TestInsertOutputInboxValidationSuccessHasNoCancellationAdmissionPath(t *testing.T) {
	record, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"claim-1"}},
		{values: []any{"", "", "", false, false, false}},
	}}
	result, err := insertOutputInbox(context.Background(), executor, record)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted || result.CancellationRejected {
		t.Fatal("validation success was inserted without a matching running claim")
	}
	call := executor.rowCalls[1]
	if call.args[13] != payloadTypeConfigurationValidation || call.args[17] != string(executionapp.SettlementSucceeded) {
		t.Fatalf("ordinary validation unexpectedly matched the cancellation pair: payload=%v outcome=%v", call.args[13], call.args[17])
	}
	if !strings.Contains(call.sql, "authority.desired_state = 'CANCELLED'") || !strings.Contains(call.sql, "$14 = 'RUNTIME_FAILURE'") || !strings.Contains(call.sql, "$18 = 'CANCELLED'") {
		t.Fatal("validation success rejection is not enforced by the cancellation predicate")
	}
}

func TestInsertOutputInboxRequiresPriorNodeEventForLaterTerminalSequence(t *testing.T) {
	record, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	record.Sequence = 2
	record.EventID = record.CommandID + ":2"
	record.SettlementProposalID = record.CommandID + ":settlement"

	accepted := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"claim-1"}},
		{values: []any{"claim-1", "claim-1", "RUNNING", false, false, false}},
	}}
	result, err := insertOutputInbox(context.Background(), accepted, record)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Inserted || !strings.Contains(accepted.rowCalls[1].sql, "previous_sequence") || !strings.Contains(accepted.rowCalls[1].sql, "execution_replay_state") || !strings.Contains(accepted.rowCalls[1].sql, "last_node_sequence") {
		t.Fatalf("later terminal sequence was not bound to durable node progress: result=%+v", result)
	}

	rejected := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"claim-1"}},
		{values: []any{"", "claim-1", "RUNNING", false, false, true}},
	}}
	if _, err := insertOutputInbox(context.Background(), rejected, record); !errors.Is(err, outputapp.ErrValidationOutputConflict) {
		t.Fatalf("terminal sequence gap was not rejected: %v", err)
	}
}

func TestInsertOutputInboxReturnsTypedCancellationLinearizationOnlyForExactAuthority(t *testing.T) {
	record, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name                 string
		lock                 scriptedRow
		row                  []any
		cancelled            bool
		cancelledIndexResult bool
	}{
		{name: "exact cancelled authority", lock: scriptedRow{values: []any{"claim-1"}}, row: []any{"", "claim-1", "CANCELLED", false, false, false}, cancelled: true},
		// An index result a stopped run reported with the store's record IS the
		// cancellation (ADR-0030 I1): it is not replaced by the canonical one.
		{name: "cancelled index result under cancelled authority", lock: scriptedRow{values: []any{"claim-1"}}, row: []any{"", "claim-1", "CANCELLED", false, false, false}, cancelledIndexResult: true},
		{name: "stale authority", lock: scriptedRow{err: pgx.ErrNoRows}},
		{name: "draining authority", lock: scriptedRow{values: []any{"claim-1"}}, row: []any{"", "claim-1", "DRAINING", false, false, false}},
		{name: "existing conflict", lock: scriptedRow{values: []any{"claim-1"}}, row: []any{"", "claim-1", "CANCELLED", true, false, false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows := []scriptedRow{test.lock}
			if test.row != nil {
				rows = append(rows, scriptedRow{values: test.row})
			}
			executor := &scriptedExecutor{rowResults: rows}
			record := record
			if test.cancelledIndexResult {
				record.PayloadType = payloadTypeIndexIngestResult
				record.SettlementOutcome = executionapp.SettlementCancelled
			}
			result, err := insertOutputInbox(context.Background(), executor, record)
			if err != nil {
				t.Fatal(err)
			}
			if result.Inserted || result.CancellationRejected != test.cancelled {
				t.Fatalf("unexpected insert result: %+v", result)
			}
			if !strings.Contains(executor.rowCalls[0].sql, "FOR UPDATE OF j, o, c") {
				t.Fatal("output/cancellation decision is not serialized on the job and claim")
			}
			if len(executor.rowCalls) > 1 && strings.Contains(executor.rowCalls[1].sql, "FOR UPDATE") {
				t.Fatal("output state query did not use a fresh READ COMMITTED statement snapshot")
			}
		})
	}
}

func TestInsertOutputInboxDatabaseDeadlineAllowsOnlyCanonicalFirstFailure(t *testing.T) {
	nonDeadline, _, err := validationOutputRecord(testValidationFrame(t))
	if err != nil {
		t.Fatal(err)
	}
	deadline := nonDeadline
	deadline.PayloadType = payloadTypeRuntimeFailure
	deadline.SettlementOutcome = executionapp.SettlementFailed
	deadlineMessage := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
		SafeMessage: outputapp.DeadlineExceededSafeMessage,
		Retryable:   true,
	}
	deadline.PayloadBytes, err = proto.MarshalOptions{Deterministic: true}.Marshal(deadlineMessage)
	if err != nil {
		t.Fatal(err)
	}
	deadline.PayloadDigest = runtimedomain.SHA256(deadline.PayloadBytes)

	tests := []struct {
		name             string
		record           outputRecord
		row              []any
		wantInserted     bool
		wantDeadlineLost bool
	}{
		{
			name:             "late success loses",
			record:           nonDeadline,
			row:              []any{"", "claim-1", "RUNNING", false, true, false},
			wantDeadlineLost: true,
		},
		{
			name:         "canonical deadline is admitted",
			record:       deadline,
			row:          []any{"claim-1", "claim-1", "RUNNING", false, true, false},
			wantInserted: true,
		},
		{
			name:   "existing output identity is not replaced",
			record: nonDeadline,
			row:    []any{"", "claim-1", "RUNNING", true, true, false},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &scriptedExecutor{rowResults: []scriptedRow{
				{values: []any{"claim-1"}},
				{values: test.row},
			}}
			result, err := insertOutputInbox(context.Background(), executor, test.record)
			if err != nil {
				t.Fatal(err)
			}
			if result.Inserted != test.wantInserted || result.DeadlineRejected != test.wantDeadlineLost {
				t.Fatalf("unexpected deadline linearization: %+v", result)
			}
			lockCall := executor.rowCalls[0]
			for _, fragment := range []string{
				"JOIN elitea_runtime.command_outbox AS o",
				"o.authority_granted_at IS NOT NULL",
				"FOR UPDATE OF j, o, c",
			} {
				if !strings.Contains(lockCall.sql, fragment) {
					t.Fatalf("output authority lock SQL is missing %q", fragment)
				}
			}
			call := executor.rowCalls[1]
			for _, fragment := range []string{
				"JOIN elitea_runtime.command_outbox AS o",
				"o.authority_granted_at IS NOT NULL",
				"o.deadline <= clock_timestamp() AS deadline_expired",
				"NOT authority.deadline_expired OR $27",
			} {
				if !strings.Contains(call.sql, fragment) {
					t.Fatalf("output deadline SQL is missing %q", fragment)
				}
			}
			if strings.Contains(call.sql, "FOR UPDATE") {
				t.Fatal("output deadline state query shares the authority-lock statement snapshot")
			}
			if got := call.args[26]; got != isCanonicalDeadlineOutput(test.record) {
				t.Fatalf("canonical deadline binding changed: got=%v", got)
			}
		})
	}
}

func withSettlementOutcome(record outputRecord, outcome executionapp.SettlementOutcome) outputRecord {
	record.SettlementOutcome = outcome
	return record
}
