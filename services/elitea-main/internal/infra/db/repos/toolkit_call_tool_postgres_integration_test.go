package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func toolRunDispatchPolicy() ToolkitCallToolDispatchPolicy {
	return ToolkitCallToolDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "tool-run-limits-v1",
		MaxOutstanding:    2,
	}
}

func newToolRunAdmissionService(
	t *testing.T,
	repository *ToolkitCallToolJobsRepository,
	prefix string,
) *toolkitcalltoolapp.AdmissionService {
	t.Helper()
	factory, err := toolkitcalltoolapp.NewInputBundleFactory(
		toolkitcalltoolapp.InputProfile{
			Classification:        "project-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
		},
		postgresIndexIDs(prefix+"-bundle", prefix+"-settings-content", prefix+"-arguments-content", prefix+"-runtime-context-content"),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := toolkitcalltoolapp.NewAdmissionService(
		repository, factory, time.Now,
		postgresIndexIDs(prefix+"-execution", prefix+"-command", prefix+"-outbox"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func toolRunSubmitRequest(idempotencyKey, arguments string) toolkitcalltoolapp.SubmitRequest {
	return toolkitcalltoolapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-postgres",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "7",
		},
		IdempotencyKey: idempotencyKey,
		Inputs: toolkitcalltoolapp.AuthoritativeInputs{
			ToolkitType: "github",
			ToolkitID:   19,
			ToolName:    "list_issues",
			Settings: json.RawMessage(
				`{"id":19,"type":"github","toolkit_name":"gh","settings":{"token":"secret-ref://toolkit/19"}}`),
			Arguments:      json.RawMessage(arguments),
			RuntimeContext: json.RawMessage(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}}}`),
		},
	}
}

// TestPostgresToolkitCallToolAdmission is a real PostgreSQL 16-18
// service-integration gate for the tool-run producer. It crosses the typed
// application use case, the SQLC queries, migration 0115's two CHECK
// constraints and one database transaction.
func TestPostgresToolkitCallToolAdmission(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := toolRunDispatchPolicy()
	repository, err := NewToolkitCallToolJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	service := newToolRunAdmissionService(t, repository, "first")
	admitted, err := service.Submit(ctx, toolRunSubmitRequest("run-1", `{"repo":"a"}`))
	if err != nil || !admitted.Outcome.Created {
		t.Fatalf("admit tool run: outcome=%+v err=%v", admitted.Outcome, err)
	}

	// The kernel row exists and names the capability migration 0115 admits.
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE capability_id = 'toolkit.call_tool.v1'`)
	// NO per-capability binding table: the two entry ids come from the roles.
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.input_bundles`)
	assertPostgresCount(t, ctx, pool, 3, `SELECT count(*) FROM elitea_runtime.input_bundle_entries`)
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.input_bundle_entries WHERE semantic_role = 'toolkit.call_tool.settings'`)
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.input_bundle_entries WHERE semantic_role = 'toolkit.call_tool.arguments'`)
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.command_outbox WHERE stream_name = $1`, policy.StreamName)

	// The capability CHECK migration 0115 widened requires every
	// configuration-validation column to be NULL for this capability. A row
	// that carried one would have been rejected at INSERT.
	var validationColumns int
	var state, desiredState, capabilityVersion string
	if err := pool.QueryRow(ctx, `
SELECT num_nonnulls(configuration_revision_id, configuration_type, catalog_revision,
                    catalog_digest, schema_id, schema_revision, schema_digest, settings_entry_id),
       state, desired_state, capability_version
FROM elitea_runtime.execution_jobs
WHERE capability_id = 'toolkit.call_tool.v1'`).Scan(
		&validationColumns, &state, &desiredState, &capabilityVersion); err != nil {
		t.Fatalf("read the admitted tool-run job: %v", err)
	}
	if validationColumns != 0 {
		t.Fatalf("the tool-run job carries %d configuration-validation columns", validationColumns)
	}
	if state != string(executiondomain.JobPending) || desiredState != "RUNNING" ||
		capabilityVersion != policy.CapabilityVersion {
		t.Fatalf("job state=%q desired=%q version=%q", state, desiredState, capabilityVersion)
	}

	// IDEMPOTENT REPLAY. The same request under the same key returns the first
	// admission and writes nothing new — #616's criterion, at the storage tier.
	replayed, err := newToolRunAdmissionService(t, repository, "second").
		Submit(ctx, toolRunSubmitRequest("run-1", `{"repo":"a"}`))
	if err != nil {
		t.Fatalf("replay tool run: %v", err)
	}
	if replayed.Outcome.Created {
		t.Fatal("a replay reported a new admission")
	}
	if replayed.Outcome.ExecutionID != admitted.Outcome.ExecutionID {
		t.Fatalf("a replay returned execution %q, not %q",
			replayed.Outcome.ExecutionID, admitted.Outcome.ExecutionID)
	}
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE capability_id = 'toolkit.call_tool.v1'`)

	// A CHANGED request under the SAME key is a conflict, never a silent
	// re-answer of the first run.
	_, err = newToolRunAdmissionService(t, repository, "third").
		Submit(ctx, toolRunSubmitRequest("run-1", `{"repo":"b"}`))
	if !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

// The bounded wait's read sees nothing until a terminal row exists, and never
// reports "not settled" as a failure.
func TestPostgresToolkitCallToolSettlementIsAbsentUntilTheRunSettles(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admitted, err := newToolRunAdmissionService(t, repository, "first").
		Submit(ctx, toolRunSubmitRequest("run-1", `{}`))
	if err != nil {
		t.Fatalf("admit tool run: %v", err)
	}
	settlement, found, err := repository.ReadToolkitCallToolSettlement(
		ctx, admitted.Outcome.ExecutionID, 1)
	if err != nil {
		t.Fatalf("read settlement: %v", err)
	}
	if found {
		t.Fatalf("an unsettled run reported a settlement: %+v", settlement)
	}
}

// Admission capacity is enforced per capability, and it is the tool run's own
// bound rather than the index path's.
func TestPostgresToolkitCallToolAdmissionEnforcesCapacity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := toolRunDispatchPolicy()
	policy.MaxOutstanding = 1
	repository, err := NewToolkitCallToolJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := newToolRunAdmissionService(t, repository, "first").
		Submit(ctx, toolRunSubmitRequest("run-1", `{}`)); err != nil {
		t.Fatalf("admit the first tool run: %v", err)
	}
	_, err = newToolRunAdmissionService(t, repository, "second").
		Submit(ctx, toolRunSubmitRequest("run-2", `{}`))
	var capacity *executionapp.AdmissionCapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("expected an AdmissionCapacityError, got %v", err)
	}
	if capacity.CapabilityID != executiondomain.ToolkitCallToolCapability {
		t.Fatalf("capacity error names capability %q", capacity.CapabilityID)
	}
}

// The output plane's admitted binding is rebuilt from the roles alone, which is
// the claim that lets this capability own no binding table.
func TestPostgresToolkitCallToolExpectedBindingComesFromTheRoles(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewToolkitCallToolResultsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admitted, err := newToolRunAdmissionService(t, repository, "first").
		Submit(ctx, toolRunSubmitRequest("run-1", `{"repo":"a"}`))
	if err != nil {
		t.Fatalf("admit tool run: %v", err)
	}
	expected, err := results.ExpectedToolkitCallTool(ctx, admitted.Outcome.ExecutionID, 1)
	if err != nil {
		t.Fatalf("load the expected binding: %v", err)
	}
	if expected.Settings.EntryID != toolkitcalltoolapp.SettingsEntryID ||
		expected.Arguments.EntryID != toolkitcalltoolapp.ArgumentsEntryID {
		t.Fatalf("the binding named %q/%q", expected.Settings.EntryID, expected.Arguments.EntryID)
	}
	if expected.Settings.ContentDigest == expected.Arguments.ContentDigest {
		t.Fatal("the two entries share a content digest, so they are not two entries")
	}
	if expected.LogicalOutputID != "toolkit-call-tool:"+admitted.Outcome.ExecutionID {
		t.Fatalf("logical output id %q", expected.LogicalOutputID)
	}
	if expected.CapabilityID != executiondomain.ToolkitCallToolCapability {
		t.Fatalf("capability %q", expected.CapabilityID)
	}
	if err := expected.Validate(); err != nil {
		t.Fatalf("the rebuilt binding does not validate: %v", err)
	}
}

func TestPostgresToolkitCallToolAuthorizationIdentityComesFromFrozenSettings(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewToolkitCallToolResultsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	request := toolRunSubmitRequest("authorization", `{}`)
	request.Inputs.ToolkitType = "mcp"
	request.Inputs.Settings = json.RawMessage(`{"id":19,"type":"mcp","toolkit_name":"saved","settings":{"url":"https://mcp.example.test/tools"}}`)
	admitted, err := newToolRunAdmissionService(t, repository, "authorization").Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedToolkitCallTool(context.Background(), admitted.Outcome.ExecutionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if expected.ToolkitID != "19" || expected.ToolkitType != "mcp" || expected.ToolkitName != "saved" || expected.ServerURL != "https://mcp.example.test/tools" {
		t.Fatal("authorization subject differs from frozen saved toolkit")
	}
}
