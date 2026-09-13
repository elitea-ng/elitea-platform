package repos

import (
	"context"
	"errors"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
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
			if _, _, err := repo.ReadToolkitCallToolResultBinding(ctx, changed); !errors.Is(err, toolkitcalltoolapp.ErrToolRunNotFound) {
				t.Fatalf("expected not found, got %v", err)
			}
		})
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
}
