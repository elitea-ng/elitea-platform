package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestPostgresToolkitResultOwnership(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := toolRunSubmitRequest("result-ownership", `{}`)
	input.Identity.TenantID = "1"
	admitted, err := newToolRunAdmissionService(t, repo, "result-owner").Submit(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	request := toolkitcalltoolapp.ResultRequest{ProjectID: 1, ActorUserID: 7, ToolkitID: 19, ExecutionID: admitted.Outcome.ExecutionID}
	binding, state, err := repo.ReadToolkitCallToolResultBinding(ctx, request)
	if err != nil || binding.ToolkitID != 19 || binding.ToolkitType != "github" || state != "PENDING" {
		t.Fatalf("binding=%+v state=%s err=%v", binding, state, err)
	}
	command, err := proto.Marshal(&runtimev1.WorkerCommandV1{ExecutionId: request.ExecutionID, Generation: 1, CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{ToolkitId: "19", ToolName: "list_issues"}}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := proto.Marshal(&runtimev1.SignedWorkerCommandEnvelopeV1{WorkerCommandBytes: command})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(envelope)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.command_outbox SET prepared_signed_envelope_bytes=$1,prepared_signed_envelope_digest=$2,prepared_signature_profile=1,prepared_key_id='fixture',prepared_at=clock_timestamp() WHERE execution_id=$3`, envelope, digest[:], request.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := repo.ReadToolkitCallToolAuthorizationRequest(ctx, request)
	if err != nil || retry.ToolName != "list_issues" || string(retry.ToolParams) != `{}` {
		t.Fatalf("original arguments recovery: %v", err)
	}
	for _, field := range []string{"project", "actor", "toolkit", "execution"} {
		t.Run(field, func(t *testing.T) {
			changed := request
			switch field {
			case "project":
				changed.ProjectID = 2
			case "actor":
				changed.ActorUserID = 8
			case "toolkit":
				changed.ToolkitID = 20
			case "execution":
				changed.ExecutionID = "missing"
			}
			if _, err := repo.ReadToolkitCallToolAuthorizationRequest(ctx, changed); !errors.Is(err, toolkitcalltoolapp.ErrToolRunNotFound) {
				t.Fatalf("foreign arguments: %v", err)
			}
			if _, _, err := repo.ReadToolkitCallToolResultBinding(ctx, changed); !errors.Is(err, toolkitcalltoolapp.ErrToolRunNotFound) {
				t.Fatalf("expected not found, got %v", err)
			}
		})
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
}
