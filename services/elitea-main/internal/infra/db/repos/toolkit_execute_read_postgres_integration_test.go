package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// This is an upgrade-path test as much as a repository test. Migrations 0054
// and 0056 installed input and output type bounds. Migration 0112 must extend
// both constraints for the direct toolkit contract.
func TestPostgresToolkitExecuteReadMigrationAdmitsInputAndOutput(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := ToolkitExecuteReadDispatchPolicy{
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Minute,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
	repository, err := NewToolkitExecuteReadJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}

	ids := []string{"bundle-toolkit-read", "content-toolkit-read", "execution-toolkit-read", "command-toolkit-read", "outbox-toolkit-read"}
	nextID := func() (string, error) {
		value := ids[0]
		ids = ids[1:]
		return value, nil
	}
	factory, err := toolkitexecutionapp.NewInputBundleFactory(
		toolkitexecutionapp.InputProfile{
			Classification:        "confidential",
			RequiredGrantAudience: "runtime-worker",
		},
		nextID,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := toolkitexecutionapp.NewAdmissionService(repository, factory, nil, nextID)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := service.Submit(context.Background(), toolkitexecutionapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "7",
		},
		IdempotencyKey: "toolkit-read-upgrade-proof",
		Frozen: toolkitexecutionapp.FrozenCurrentReadTool{
			ToolkitType: "openapi", ToolkitName: "echo", ToolName: "echo_marker",
			ToolkitJSON:    []byte(`{"id":20,"type":"openapi","toolkit_name":"echo","settings":{"selected_tools":["echo_marker"]}}`),
			ArgumentsJSON:  []byte(`{"marker":"safe-proof"}`),
			GuardrailsJSON: []byte(`{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{},"sensitive_action_company_name":"","sensitive_action_message_template":""}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExecutionID != "execution-toolkit-read" || !outcome.Created {
		t.Fatalf("admission outcome = %#v", outcome)
	}

	var capabilityID, mediaType, requestEntryID string
	var contentSize int64
	err = pool.QueryRow(context.Background(), `
SELECT j.capability_id, e.media_type, e.content_size, t.request_entry_id
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.toolkit_execute_read_jobs AS t
  ON t.execution_id = j.execution_id AND t.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = t.input_bundle_id AND e.entry_id = t.request_entry_id
WHERE j.execution_id = $1`, outcome.ExecutionID).Scan(
		&capabilityID, &mediaType, &contentSize, &requestEntryID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if capabilityID != executiondomain.ToolkitExecuteReadCapability ||
		mediaType != executiondomain.ToolkitExecuteReadInputMediaType ||
		contentSize <= 0 || requestEntryID != "toolkit-read-request" {
		t.Fatalf("stored toolkit admission = capability %q media %q size %d entry %q",
			capabilityID, mediaType, contentSize, requestEntryID)
	}

	var outputConstraint string
	err = pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(oid)
FROM pg_constraint
WHERE conrelid = 'elitea_runtime.output_inbox'::regclass
  AND conname = 'output_inbox_payload_type'`).Scan(&outputConstraint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outputConstraint, "'TOOLKIT_EXECUTE_READ_RESULT'::text") {
		t.Fatalf("output inbox constraint does not admit direct toolkit results: %s", outputConstraint)
	}
}
