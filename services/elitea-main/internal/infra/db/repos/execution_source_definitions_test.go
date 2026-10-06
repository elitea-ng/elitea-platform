package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func rootCaptureFixture(t *testing.T) (app.RootSourceCaptureRequest, scope.SourceWire) {
	t.Helper()
	request := app.RootSourceCaptureRequest{ProjectID: 7, ActorID: 11, ApplicationID: 31, VersionID: 41, VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"pipeline","instructions":"Original YAML","meta":{"debug":false},"request":184467440737095516170000000000000001}`)}
	wire, err := scope.NewSourceWire(7, 11, 31, 41, request.VersionDetails)
	if err != nil {
		t.Fatal(err)
	}
	return request, wire
}
func protectedRootInput(t *testing.T, ref scope.SourceReference) []byte {
	t.Helper()
	application, _ := json.Marshal(map[string]any{"id": ref.ApplicationID, "version_id": ref.VersionID, "source_definition": ref, "version_details": map[string]any{"instructions": "runtime additions", "tools": []any{}}})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: application, ExecutionGeneration: stringPointerSourceTest("9fba0a08-5049-42bb-9019-c2f3df686010")})
	if err != nil {
		t.Fatal(err)
	}
	return input
}
func TestOriginalRootCaptureUsesSingleMainDefinitionStoreAndImmutableReceipt(t *testing.T) {
	request, wire := rootCaptureFixture(t)
	e := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{[]byte(request.VersionDetails)}}, {values: []any{wire.CanonicalBytes(), []byte(request.VersionDetails)}}}}
	store := &scriptedStore{scriptedExecutor: e}
	repo := &ExecutionChildScopesRepository{store: store}
	ref, err := repo.CaptureOriginalRootSource(t.Context(), request)
	if err != nil || ref != wire.Reference() || store.txCalls != 1 || len(e.execCalls) != 2 {
		t.Fatalf("ref=%v err=%v writes=%d", ref, err, len(e.execCalls))
	}
	if !strings.Contains(e.execCalls[0].sql, "execution_captured_definitions") || !strings.Contains(e.execCalls[1].sql, "execution_definition_source_refs") || strings.Contains(e.execCalls[1].sql, "definition_bytes") {
		t.Fatal("second bytes registry or nonowning producer")
	}
	for _, call := range e.execCalls {
		if !strings.Contains(call.sql, "ON CONFLICT") || strings.Contains(call.sql, "DO UPDATE") {
			t.Fatal("source capture can overwrite immutable bytes")
		}
	}
}
func TestOriginalRootCaptureRejectsStoredByteDrift(t *testing.T) {
	request, _ := rootCaptureFixture(t)
	e := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{[]byte(`{"instructions":"changed"}`)}}}}
	repo := &ExecutionChildScopesRepository{store: &scriptedStore{scriptedExecutor: e}}
	if _, err := repo.CaptureOriginalRootSource(t.Context(), request); !errors.Is(err, scope.ErrDenied) || len(e.execCalls) != 1 {
		t.Fatalf("err=%v writes=%d", err, len(e.execCalls))
	}
}
func TestOriginalRootSourceReadUsesProtectedOriginalInputAndTrustedOwnerAccess(t *testing.T) {
	request, wire := rootCaptureFixture(t)
	payload := protectedRootInput(t, wire.Reference())
	for _, name := range []string{"valid", "foreign actor", "foreign project", "wrong ref", "missing marker", "editable version marker"} {
		t.Run(name, func(t *testing.T) {
			original := storage.HTTPActionInput{ProjectID: 7, ActorID: 11, Payload: bytes.Clone(payload)}
			switch name {
			case "foreign actor":
				original.ActorID = 12
			case "foreign project":
				original.ProjectID = 8
			case "wrong ref":
				ref := wire.Reference()
				ref.SourceID = scope.Digest([]byte("different"))
				original.Payload = protectedRootInput(t, ref)
			case "missing marker", "editable version marker":
				raw, _ := json.Marshal(map[string]any{"id": 31, "version_id": 41, "version_details": map[string]any{"source_definition": wire.Reference(), "instructions": "edited"}})
				original.Payload, _ = proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: raw})
			}
			e := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{wire.CanonicalBytes(), []byte(request.VersionDetails)}}}}
			repo := &ExecutionChildScopesRepository{}
			source, err := repo.readOriginalRootSourceUnderOriginalAccess(t.Context(), e, original)
			if name == "valid" {
				if err != nil || source.Reference != wire.Reference() || !bytes.Equal(source.PreRedemptionVersion, request.VersionDetails) {
					t.Fatal(source, err)
				}
			} else if err == nil {
				t.Fatal("untrusted source accepted")
			}
			if len(e.execCalls) != 0 {
				t.Fatal("read path performed a write")
			}
		})
	}
}

type sourceTrackedRows struct {
	sqlRows
	closed bool
}

func (rows *sourceTrackedRows) Close() { rows.closed = true; rows.sqlRows.Close() }

type sourceTrackedExecutor struct {
	*scriptedExecutor
	active *sourceTrackedRows
}

func (e *sourceTrackedExecutor) Query(ctx context.Context, sql string, args ...any) (sqlRows, error) {
	rows, err := e.scriptedExecutor.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	e.active = &sourceTrackedRows{sqlRows: rows}
	return e.active, nil
}
func (e *sourceTrackedExecutor) QueryRow(ctx context.Context, sql string, args ...any) sqlRow {
	if e.active != nil && !e.active.closed {
		return scriptedRow{err: errors.New("connection busy before cursor close")}
	}
	return e.scriptedExecutor.QueryRow(ctx, sql, args...)
}

type sourceTrackedStore struct {
	*sourceTrackedExecutor
	options   pgx.TxOptions
	committed bool
}

func (s *sourceTrackedStore) WithinTx(ctx context.Context, options pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.options = options
	err := fn(s.sourceTrackedExecutor)
	s.committed = err == nil
	return err
}
func TestOriginalRootContinueLookupClosesLockedRowsAndRefusesRelationDrift(t *testing.T) {
	request, wire := rootCaptureFixture(t)
	payload := protectedRootInput(t, wire.Reference())
	for _, name := range []string{"exact original", "duplicate identical originals", "third conflicting original", "changed relation", "missing input", "continuation payload", "wrong input generation"} {
		t.Run(name, func(t *testing.T) {
			inputs := []scriptedRow{{values: []any{payload}}}
			if name == "duplicate identical originals" || name == "third conflicting original" || name == "changed relation" {
				inputs = append(inputs, scriptedRow{values: []any{payload}})
			}
			if name == "third conflicting original" {
				ref := wire.Reference()
				ref.SourceID = scope.Digest([]byte("third"))
				inputs = append(inputs, scriptedRow{values: []any{protectedRootInput(t, ref)}})
				// SQL LIMIT 2 hides this third conflicting row. Refuse the
				// first two equal originals by cardinality, before source read.
				inputs = inputs[:2]
			}
			if name == "continuation payload" || name == "wrong input generation" {
				var input runtimev1.AgentExecutionInputV1
				_ = proto.Unmarshal(payload, &input)
				if name == "continuation payload" {
					input.ShouldContinue = true
				} else {
					input.ExecutionGeneration = stringPointerSourceTest("different")
				}
				changed, _ := proto.Marshal(&input)
				inputs[0].values = []any{changed}
			}
			if name == "changed relation" {
				ref := wire.Reference()
				ref.SourceID = scope.Digest([]byte("new source"))
				inputs[1].values = []any{protectedRootInput(t, ref)}
			}
			if name == "missing input" {
				inputs = nil
			}
			e := &sourceTrackedExecutor{scriptedExecutor: &scriptedExecutor{rowsResult: &scriptedRows{rows: inputs}, rowResults: []scriptedRow{{values: []any{wire.CanonicalBytes(), []byte(request.VersionDetails)}}}}}
			store := &sourceTrackedStore{sourceTrackedExecutor: e}
			repo := &ExecutionChildScopesRepository{store: store}
			result, err := repo.RestoreOriginalRootSource(t.Context(), app.RootSourceRestoreRequest{ProjectID: 7, ActorID: 11, ConversationID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010"})
			if name == "exact original" {
				if err != nil || result.Reference != wire.Reference() || !store.committed || !e.active.closed || store.options.IsoLevel != pgx.Serializable {
					t.Fatal(result, err)
				}
			} else if err == nil || store.committed || len(e.rowCalls) != 0 {
				t.Fatal("drift created or read another source")
			}
			if len(e.execCalls) != 0 || len(e.queryCalls) != 1 || !strings.Contains(e.queryCalls[0].sql, "client_execution_generation=$5") || !strings.Contains(e.queryCalls[0].sql, "FOR SHARE OF a,j,e") || !strings.Contains(e.queryCalls[0].sql, "a.sio_event='chat_predict'") {
				t.Fatal("original actor/project/run input relation not locked")
			}
		})
	}
}

func stringPointerSourceTest(value string) *string { return &value }

func TestOriginalRootContinuationOwnerProjectsOnlyOriginalApplicationHandles(t *testing.T) {
	request, wire := rootCaptureFixture(t)
	handle := json.RawMessage(`{"type":"application","id":44,"settings":{"application_id":3,"application_version_id":4},"nested_skill_registry":[{"application_id":3,"application_version_id":4,"application_name":"child","skills":[{"skill_id":7,"name":"Original skill","icon_meta":null}]}]}`)
	application, _ := json.Marshal(map[string]any{"id": 31, "version_id": 41, "source_definition": wire.Reference(), "version_details": map[string]any{"instructions": "runtime-added memories", "tools": []any{handle, json.RawMessage(`{"type":"mcp","settings":{"redeemed":"never-copy"}}`)}}})
	payload, _ := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: application, ExecutionGeneration: stringPointerSourceTest("9fba0a08-5049-42bb-9019-c2f3df686010")})
	e := &sourceTrackedExecutor{scriptedExecutor: &scriptedExecutor{rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{payload}}}}, rowResults: []scriptedRow{{values: []any{wire.CanonicalBytes(), []byte(request.VersionDetails)}}}}}
	repo := &ExecutionChildScopesRepository{store: &sourceTrackedStore{sourceTrackedExecutor: e}}
	original, err := repo.RestoreOriginalRootContinuation(t.Context(), app.RootSourceRestoreRequest{ProjectID: 7, ActorID: 11, ConversationID: "conversation", ResponseMessageID: "response", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010"})
	if err != nil || !bytes.Contains(original.ApplicationTools, []byte("Original skill")) || bytes.Contains(original.ApplicationTools, []byte("never-copy")) || bytes.Contains(original.ApplicationTools, []byte("runtime-added")) || !bytes.Equal(original.Source.PreRedemptionVersion, request.VersionDetails) || !e.active.closed {
		t.Fatalf("original=%+v err=%v", original, err)
	}
}
