package repos

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const (
	continuationStream     = "be5ca461-fdee-4d71-bf93-b6ae43e87dc8"
	continuationMessage    = "bf7bacc5-998e-5380-a039-9cbd9db195bf"
	continuationGeneration = "a1547ea4-499d-4fb9-bab1-94a5f52f1b24"
	continuationExecution  = "95069a39ff955e391027b9152d202ed5"
)

// seedOriginalRootTurn admits one chat_predict the way Main does: the saved
// pipeline definition is captured once, and the original input names it by a
// protected receipt. Everything below runs on a real, migrated PostgreSQL: the
// scripted-store tests cannot see a column-type error in the lookup SQL.
func seedOriginalRootTurn(t *testing.T, pool *pgxpool.Pool, project, actor int64) (*ExecutionChildScopesRepository, scope.SourceReference) {
	t.Helper()
	repo, err := NewExecutionChildScopesRepository(pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	version := json.RawMessage(`{"id":145,"application_id":138,"agent_type":"pipeline","instructions":"entry_point: a\nnodes: []\n","tools":[{"type":"application","id":97,"agent_type":"agent","settings":{"application_id":137,"application_version_id":144}}]}`)
	ref, err := repo.CaptureOriginalRootSource(t.Context(), app.RootSourceCaptureRequest{ProjectID: project, ActorID: actor, ApplicationID: 138, VersionID: 145, VersionDetails: version})
	if err != nil {
		t.Fatal(err)
	}
	application, _ := json.Marshal(map[string]any{"id": 138, "version_id": 145, "source_definition": ref, "version_details": json.RawMessage(version)})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: application, ExecutionGeneration: stringPointerSourceTest(continuationGeneration)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	steps := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO elitea_runtime.input_bundles (input_bundle_id, immutable_version, media_type, resource_project_id, manifest_digest, manifest_size, manifest_bytes, created_by, created_at) VALUES ('bundle-cont', '1', 'application/x-protobuf', $1, decode(repeat('ab', 32), 'hex'), 2, '{}'::bytea, 'tests', clock_timestamp())`, []any{int32(project)}},
		{`INSERT INTO elitea_runtime.input_bundle_entries (input_bundle_id, entry_id, entry_version, semantic_role, media_type, content_digest, content_size, content_reference, classification, required_grant_audience, content_bytes) VALUES ('bundle-cont', 'request', '1', 'agent.execution_request', 'application/vnd.elitea.agent-execution-input.v1+protobuf', decode(repeat('cd', 32), 'hex'), $1, 'inline://request', 'internal', 'worker', $2)`, []any{int64(len(input)), input}},
		{`INSERT INTO elitea_runtime.execution_jobs (execution_id, generation, command_id, tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key, state, desired_state, admitted_at) VALUES ($1, 1, 'command-cont', ($2::integer)::text, $2::integer, $2::integer, $3::bigint::text, 'user:' || $3::bigint::text, $4, '1', 'bundle-cont', decode(repeat('ef', 32), 'hex'), 'continuation-source', 'idem-cont', 'RUNNING', 'RUNNING', clock_timestamp())`, []any{continuationExecution, int32(project), actor, executiondomain.AgentApplicationCapability}},
		{`INSERT INTO elitea_runtime.agent_execution_jobs (execution_id, generation, capability_id, input_bundle_id, request_entry_id, client_stream_id, client_message_id, client_execution_generation, sio_event) VALUES ($1, 1, $2, 'bundle-cont', 'request', $3, $4, $5, 'chat_predict')`, []any{continuationExecution, executiondomain.AgentApplicationCapability, continuationStream, continuationMessage, continuationGeneration}},
	}
	for _, step := range steps {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("seed %.60s: %v", step.sql, err)
		}
	}
	return repo, ref
}

func continuationRequest(project, actor int64) app.RootSourceRestoreRequest {
	return app.RootSourceRestoreRequest{ProjectID: project, ActorID: actor, ConversationID: continuationStream, ResponseMessageID: continuationMessage, ExecutionGeneration: continuationGeneration}
}

// The original lookup compared the INTEGER resource_project_id with a text
// parameter. PostgreSQL refuses that statement ("operator does not exist"),
// and the owner turned the failure into "saved child scope is not authorized"
// on every real continuation of a saved pipeline.
func TestPostgresOriginalRootContinuationRestoresTheAdmittedSavedPipeline(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, ref := seedOriginalRootTurn(t, pool, 1, 11)

	restored, err := repo.RestoreOriginalRootContinuation(t.Context(), continuationRequest(1, 11))
	if err != nil {
		t.Fatalf("restore the admitted original: %v", err)
	}
	if restored.Source.Reference != ref || restored.Source.ResourceProjectID != 1 || restored.Source.ActorID != 11 {
		t.Fatalf("restored source=%+v", restored.Source)
	}
	var handles []map[string]any
	if json.Unmarshal(restored.ApplicationTools, &handles) != nil || len(handles) != 1 || handles[0]["type"] != "application" {
		t.Fatalf("admitted application handles=%s", restored.ApplicationTools)
	}
}

// Authorization stays strict: only the admitting actor in the admitting
// project, on the exact stream, message and generation, reads the original.
// A refusal is a denial; only an infrastructure failure is "unavailable".
func TestPostgresOriginalRootContinuationRefusesEveryOtherRelation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, _ := seedOriginalRootTurn(t, pool, 1, 11)
	cases := map[string]app.RootSourceRestoreRequest{
		"another actor":      continuationRequest(1, 12),
		"another project":    continuationRequest(2, 11),
		"another stream":     {ProjectID: 1, ActorID: 11, ConversationID: "11111111-1111-4111-8111-111111111111", ResponseMessageID: continuationMessage, ExecutionGeneration: continuationGeneration},
		"another message":    {ProjectID: 1, ActorID: 11, ConversationID: continuationStream, ResponseMessageID: "22222222-2222-4222-8222-222222222222", ExecutionGeneration: continuationGeneration},
		"another generation": {ProjectID: 1, ActorID: 11, ConversationID: continuationStream, ResponseMessageID: continuationMessage, ExecutionGeneration: "different"},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			restored, err := repo.RestoreOriginalRootContinuation(t.Context(), request)
			if !errors.Is(err, scope.ErrDenied) || len(restored.ApplicationTools) != 0 || restored.Source.Reference.SourceID != "" {
				t.Fatalf("restored=%+v err=%v", restored, err)
			}
		})
	}
}

// A lookup that cannot run is an outage, not a refusal, and its cause is not
// part of the error the caller sees.
func TestPostgresOriginalRootContinuationReportsABrokenLookupAsUnavailable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, _ := seedOriginalRootTurn(t, pool, 1, 11)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE elitea_runtime.agent_execution_jobs RENAME COLUMN client_stream_id TO client_stream_id_moved`); err != nil {
		t.Fatal(err)
	}
	_, err := repo.RestoreOriginalRootContinuation(context.Background(), continuationRequest(1, 11))
	if !errors.Is(err, scope.ErrUnavailable) || errors.Is(err, scope.ErrDenied) || strings.Contains(err.Error(), "client_stream_id") {
		t.Fatalf("err=%v", err)
	}
}
