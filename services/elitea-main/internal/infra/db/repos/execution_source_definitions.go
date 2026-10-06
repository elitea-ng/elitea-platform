package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// Same capture component and exact bytes/frame owner as saved descendants.
// The root row contains only a protected source-selection receipt, not a second
// definition cache or consumer grant. Main's producer never accepts Worker bytes.
func (repo *ExecutionChildScopesRepository) CaptureOriginalRootSource(ctx context.Context, request app.RootSourceCaptureRequest) (scope.SourceReference, error) {
	if repo == nil || repo.store == nil {
		return scope.SourceReference{}, scope.ErrUnavailable
	}
	var reference scope.SourceReference
	err := repo.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx sqlExecutor) error {
		var err error
		reference, err = captureMainSourceReference(ctx, tx, request.ProjectID, request.ActorID, request.ApplicationID, request.VersionID, request.VersionDetails)
		return err
	})
	return reference, err
}

func captureMainSourceReference(ctx context.Context, tx sqlExecutor, project, actor int64, application, version uint64, definition []byte) (scope.SourceReference, error) {
	wire, err := scope.NewSourceWire(project, actor, application, version, definition)
	if err != nil {
		return scope.SourceReference{}, err
	}
	if err = captureMainDefinition(ctx, tx, project, application, version, wire.SourceDefinitionSHA256, definition); err != nil {
		return scope.SourceReference{}, err
	}
	ref := wire.Reference()
	if _, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_definition_source_refs(source_id,revision,digest_sha256,resource_project_id,actor_id,application_id,version_id,definition_sha256,canonical_wire) VALUES($1,1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(source_id) DO NOTHING`, ref.SourceID, ref.DigestSHA256, project, actor, application, version, wire.SourceDefinitionSHA256, wire.CanonicalBytes()); err != nil {
		return scope.SourceReference{}, scope.ErrUnavailable
	}
	if _, err = readCapturedRootSource(ctx, tx, ref, project, actor); err != nil {
		return scope.SourceReference{}, err
	}
	return ref, nil
}

func readCapturedRootSource(ctx context.Context, tx sqlExecutor, ref scope.SourceReference, project, actor int64) (scope.SourceDefinition, error) {
	if tx == nil || ref.Validate() != nil {
		return scope.SourceDefinition{}, scope.ErrDenied
	}
	var raw, definition []byte
	if tx.QueryRow(ctx, `SELECT r.canonical_wire,d.definition_bytes FROM elitea_runtime.execution_definition_source_refs r JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) WHERE r.source_id=$1 AND r.revision=$2 AND r.digest_sha256=$3 AND r.resource_project_id=$4 AND r.actor_id=$5 AND r.definition_sha256=$6 AND r.application_id=$7 AND r.version_id=$8 FOR SHARE OF r,d`, ref.SourceID, ref.Revision, ref.DigestSHA256, project, actor, ref.SourceDefinitionSHA256, ref.ApplicationID, ref.VersionID).Scan(&raw, &definition) != nil {
		return scope.SourceDefinition{}, scope.ErrDenied
	}
	return scope.DecodeSourceWire(raw, definition, ref, project, actor)
}
func originalRootSourceReference(payload []byte) (scope.SourceReference, error) {
	var input runtimev1.AgentExecutionInputV1
	if len(payload) == 0 || len(payload) > 8388608 || proto.Unmarshal(payload, &input) != nil {
		return scope.SourceReference{}, scope.ErrDenied
	}
	var application struct {
		ID        uint64          `json:"id"`
		VersionID uint64          `json:"version_id"`
		Source    json.RawMessage `json:"source_definition"`
	}
	if json.Unmarshal(input.Application, &application) != nil || len(application.Source) == 0 || len(application.Source) > 8192 {
		return scope.SourceReference{}, scope.ErrDenied
	}
	var ref scope.SourceReference
	decoder := json.NewDecoder(bytes.NewReader(application.Source))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ref) != nil || decoder.Decode(new(any)) != io.EOF || ref.Validate() != nil || ref.ApplicationID != application.ID || ref.VersionID != application.VersionID {
		return scope.SourceReference{}, scope.ErrDenied
	}
	return ref, nil
}

// Called only inside the existing owner's current authenticated transaction.
// Actor/project and immutable input come from Main, never the content body.
func (repo *ExecutionChildScopesRepository) readOriginalRootSourceUnderOriginalAccess(ctx context.Context, tx sqlExecutor, original storage.HTTPActionInput) (scope.SourceDefinition, error) {
	if repo == nil || tx == nil {
		return scope.SourceDefinition{}, scope.ErrDenied
	}
	ref, err := originalRootSourceReference(original.Payload)
	if err != nil {
		return scope.SourceDefinition{}, err
	}
	return readCapturedRootSource(ctx, tx, ref, original.ProjectID, original.ActorID)
}

// Continue uses the exact original admitted input relation selected by the
// already authenticated continuation resolver; it never captures a current edit.
func (repo *ExecutionChildScopesRepository) RestoreOriginalRootSource(ctx context.Context, request app.RootSourceRestoreRequest) (scope.SourceDefinition, error) {
	original, err := repo.RestoreOriginalRootContinuation(ctx, request)
	return original.Source, err
}

func (repo *ExecutionChildScopesRepository) RestoreOriginalRootContinuation(ctx context.Context, request app.RootSourceRestoreRequest) (app.RootSourceContinuation, error) {
	if repo == nil || repo.store == nil || request.ProjectID <= 0 || request.ActorID <= 0 || request.ConversationID == "" || request.ResponseMessageID == "" || request.ExecutionGeneration == "" {
		return app.RootSourceContinuation{}, scope.ErrDenied
	}
	var result app.RootSourceContinuation
	err := repo.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx sqlExecutor) error {
		// Continuation commands have the same browser correlation, but are not
		// the original owner. Select chat_predict only and reject ANY second
		// original identity, even byte-identical. LIMIT 2 is a cardinality probe;
		// a third conflicting row can never hide behind two equal rows.
		rows, err := tx.Query(ctx, `SELECT e.content_bytes FROM elitea_runtime.agent_execution_jobs a JOIN elitea_runtime.execution_jobs j USING(execution_id,generation) JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=a.input_bundle_id AND e.entry_id=a.request_entry_id WHERE j.resource_project_id=$1::bigint::text AND j.actor_id=$2::bigint::text AND a.client_stream_id=$3 AND a.client_message_id=$4 AND a.client_execution_generation=$5 AND a.sio_event='chat_predict' AND j.capability_id=a.capability_id AND e.semantic_role='agent.execution_request' AND e.content_size<=8388608 ORDER BY j.admitted_at ASC LIMIT 2 FOR SHARE OF a,j,e`, request.ProjectID, request.ActorID, request.ConversationID, request.ResponseMessageID, request.ExecutionGeneration)
		if err != nil {
			return scope.ErrDenied
		}
		// Finish the bounded row cursor before another query on this transaction.
		// FOR SHARE locks remain held after Close; the source read stays atomic.
		var inputs [][]byte
		for rows.Next() {
			var input []byte
			if rows.Scan(&input) != nil {
				rows.Close()
				return scope.ErrDenied
			}
			inputs = append(inputs, bytes.Clone(input))
		}
		err = rows.Err()
		rows.Close()
		if len(inputs) != 1 || err != nil {
			return scope.ErrDenied
		}
		selected, err := originalRootSourceReference(inputs[0])
		if err != nil {
			return err
		}
		result.ApplicationTools, err = originalContinuationApplicationTools(inputs[0], selected, request.ExecutionGeneration)
		if err != nil {
			return err
		}
		result.Source, err = readCapturedRootSource(ctx, tx, selected, request.ProjectID, request.ActorID)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func originalContinuationApplicationTools(payload []byte, reference scope.SourceReference, executionGeneration string) (json.RawMessage, error) {
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(payload, &input) != nil || input.ShouldContinue || input.GetExecutionGeneration() != executionGeneration {
		return nil, scope.ErrDenied
	}
	tools := json.RawMessage(input.Tools)
	if reference.Kind == "saved_application" {
		var application struct {
			Version struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if json.Unmarshal(input.Application, &application) != nil {
			return nil, scope.ErrDenied
		}
		tools = application.Version.Tools
	}
	var entries []json.RawMessage
	if len(tools) == 0 || len(tools) > 8388608 || json.Unmarshal(tools, &entries) != nil || entries == nil {
		return nil, scope.ErrDenied
	}
	selected := make([]json.RawMessage, 0)
	for _, entry := range entries {
		var tool struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry, &tool) != nil || tool.Type == "" {
			return nil, scope.ErrDenied
		}
		if tool.Type == "application" {
			selected = append(selected, bytes.Clone(entry))
		}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, scope.ErrDenied
	}
	return encoded, nil
}
