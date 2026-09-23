package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"testing"
	"time"
)

func TestPostgresToolkitDiscoveryAdmissionBindsScopeInputsAndReplay(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo, err := NewToolkitAvailableToolsJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	newID := func() (string, error) { n++; return fmt.Sprintf("discovery-%d", n), nil }
	factory, err := discovery.NewInputBundleFactory(discovery.InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1"}, newID)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := discovery.NewAdmissionService(repo, factory, time.Now, newID)
	if err != nil {
		t.Fatal(err)
	}
	request := discovery.SubmitRequest{Identity: executionapp.AdmissionIdentity{TenantID: "tenant-postgres", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "7"}, IdempotencyKey: "discovery-replay", Inputs: discovery.AuthoritativeInputs{ToolkitType: "github", ToolkitID: 19, Settings: json.RawMessage(`{"selected_tools":["list_issues"]}`), RuntimeContext: json.RawMessage(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}}}`)}}
	admitted, err := admission.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := admission.Submit(ctx, request)
	if err != nil || replay.Outcome.Created || admitted.Outcome.ExecutionID != replay.Outcome.ExecutionID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var count int
	var capability, actor string
	err = pool.QueryRow(ctx, `SELECT j.capability_id,j.actor_id,(SELECT count(*) FROM elitea_runtime.input_bundle_entries e WHERE e.input_bundle_id=j.input_bundle_id) FROM elitea_runtime.execution_jobs j WHERE j.execution_id=$1`, admitted.Outcome.ExecutionID).Scan(&capability, &actor, &count)
	if err != nil || capability != "toolkit.available_tools.v1" || actor != "7" || count != 2 {
		t.Fatalf("durable binding mismatch: %v", err)
	}
	request.Inputs.ToolkitType = "openapi"
	if _, err = admission.Submit(ctx, request); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("type change reused admission: %v", err)
	}
	request.Inputs.ToolkitType = "github"

	request.Inputs.RuntimeContext = json.RawMessage(`{"toolkit_security":{"blocked_toolkits":["github"],"blocked_tools":{},"sensitive_tools":{}}}`)
	if _, err = admission.Submit(ctx, request); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("policy change reused admission: %v", err)
	}
	var rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE capability_id='toolkit.available_tools.v1'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("replay leaked jobs rows=%d err=%v", rows, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE elitea_runtime.command_outbox SET deadline=clock_timestamp()-interval '1 minute' WHERE outbox_id=$1`, admitted.OutboxID); err != nil {
		t.Fatal(err)
	}
	retired, err := repo.ReclaimExpiredToolkitAvailableToolsRuns(ctx, 32)
	if err != nil || retired != 1 {
		t.Fatalf("retirement count=%d err=%v", retired, err)
	}
	retired, err = repo.ReclaimExpiredToolkitAvailableToolsRuns(ctx, 32)
	if err != nil || retired != 0 {
		t.Fatalf("retirement replay count=%d err=%v", retired, err)
	}
	var state string
	if err = pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id=$1`, admitted.Outcome.ExecutionID).Scan(&state); err != nil || state != "FAILED" {
		t.Fatalf("expired admission state=%s err=%v", state, err)
	}

}
