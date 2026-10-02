package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	eliteacoreapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

type projectContextResponse struct {
	ID                    *int64  `json:"id"`
	Content               string  `json:"content"`
	Enabled               bool    `json:"enabled"`
	ActivationDescription *string `json:"activation_description"`
	UpdatedAt             *string `json:"updated_at"`
}

func TestInternalProjectContextLifecycleMatchesCurrentBuilderContract(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	executor := newHandlerInternalProjectContextExecutor(eliteacoreapi.NewHandler(pool))
	ctx := context.Background()

	absent := executeProjectContext(t, executor, ctx, internalGetProjectContext, nil, http.StatusOK)
	if absent.ID != nil || absent.Content != "" || !absent.Enabled ||
		absent.ActivationDescription != nil || absent.UpdatedAt != nil {
		t.Fatalf("absent project context = %#v", absent)
	}

	created := executeProjectContext(t, executor, ctx, internalUpdateProjectContext, map[string]any{
		"content":                "Use project-specific billing rules.",
		"enabled":                false,
		"activation_description": "  billing\n incidents  ",
	}, http.StatusOK)
	if created.ID == nil || created.Content != "Use project-specific billing rules." || created.Enabled ||
		created.ActivationDescription == nil || *created.ActivationDescription != "billing incidents" ||
		created.UpdatedAt != nil {
		t.Fatalf("created project context = %#v", created)
	}

	var (
		storedLabel    *string
		storedTitle    string
		storedType     string
		storedSection  string
		storedData     []byte
		storedShared   bool
		storedStatusOK bool
		storedSource   string
		storedAuthorID *int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT label, elitea_title, type, section, data, shared, status_ok, source, author_id
		FROM p_1.configuration WHERE id = $1`, *created.ID).Scan(
		&storedLabel, &storedTitle, &storedType, &storedSection, &storedData,
		&storedShared, &storedStatusOK, &storedSource, &storedAuthorID,
	); err != nil {
		t.Fatalf("read stored project context: %v", err)
	}
	if storedLabel == nil || *storedLabel != "Project Context" || storedTitle != "project_context_1" ||
		storedType != "project_context" || storedSection != "project_settings" || storedShared ||
		!storedStatusOK || storedSource != "system" || storedAuthorID != nil {
		t.Fatalf("stored project context identity = label:%v title:%q type:%q section:%q shared:%v status:%v source:%q author:%v",
			storedLabel, storedTitle, storedType, storedSection, storedShared, storedStatusOK, storedSource, storedAuthorID)
	}
	var stored map[string]any
	if err := json.Unmarshal(storedData, &stored); err != nil || stored["activation_description"] != "billing incidents" {
		t.Fatalf("stored project context data = %s, error=%v", storedData, err)
	}

	updated := executeProjectContext(t, executor, ctx, internalUpdateProjectContext, map[string]any{
		"content": "Updated rules.",
	}, http.StatusOK)
	if updated.ID == nil || *updated.ID != *created.ID || updated.Content != "Updated rules." ||
		!updated.Enabled || updated.ActivationDescription == nil ||
		*updated.ActivationDescription != "billing incidents" || updated.UpdatedAt == nil {
		t.Fatalf("updated project context = %#v", updated)
	}

	removedActivation := executeProjectContext(t, executor, ctx, internalUpdateProjectContext, map[string]any{
		"content":                "Updated rules.",
		"enabled":                true,
		"activation_description": "  ",
	}, http.StatusOK)
	if removedActivation.ActivationDescription != nil || removedActivation.UpdatedAt == nil {
		t.Fatalf("activation removal = %#v", removedActivation)
	}

	read := executeProjectContext(t, executor, ctx, internalGetProjectContext, nil, http.StatusOK)
	if read.ID == nil || *read.ID != *created.ID || read.Content != "Updated rules." ||
		!read.Enabled || read.ActivationDescription != nil || read.UpdatedAt == nil {
		t.Fatalf("read project context = %#v", read)
	}

	deleted, err := executor.Execute(ctx, 1, 73, internalDeleteProjectContext, nil)
	if err != nil || deleted.status != http.StatusNoContent || len(deleted.body) != 0 {
		t.Fatalf("delete project context: status=%d error=%v body=%s", deleted.status, err, deleted.body)
	}
	missing, err := executor.Execute(ctx, 1, 73, internalDeleteProjectContext, nil)
	if err != nil || missing.status != http.StatusNotFound {
		t.Fatalf("repeat delete: status=%d error=%v body=%s", missing.status, err, missing.body)
	}
	afterDelete := executeProjectContext(t, executor, ctx, internalGetProjectContext, nil, http.StatusOK)
	if afterDelete.ID != nil || afterDelete.Content != "" || !afterDelete.Enabled {
		t.Fatalf("project context after delete = %#v", afterDelete)
	}
}

func executeProjectContext(
	t *testing.T,
	executor internalProjectContextExecutor,
	ctx context.Context,
	operation internalProjectContextOperation,
	arguments map[string]any,
	wantStatus int,
) projectContextResponse {
	t.Helper()
	result, err := executor.Execute(ctx, 1, 73, operation, arguments)
	if err != nil || result.status != wantStatus {
		t.Fatalf("execute %s: status=%d want=%d error=%v body=%s",
			operation, result.status, wantStatus, err, result.body)
	}
	var response projectContextResponse
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode %s response: %v; body=%s", operation, err, result.body)
	}
	return response
}
