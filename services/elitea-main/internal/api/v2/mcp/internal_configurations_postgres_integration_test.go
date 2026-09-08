package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	configurationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInternalConfigurationLifecyclePersistsWithActorAndPartialUpdate(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	executor := newHandlerInternalConfigurationExecutor(configurationsapi.NewHandler(pool), nil)
	ctx := context.Background()

	created, err := executor.Execute(ctx, 1, 73, internalCreateConfiguration, map[string]any{
		"elitea_title": "internal_prompt",
		"label":        "Internal prompt",
		"type":         "service_prompt",
		"shared":       false,
		"data": map[string]any{
			"key": "router_assistant", "prompt": "Route this request.",
		},
	})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create configuration: status=%d error=%v body=%s", created.status, err, created.body)
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.body, &createdBody); err != nil {
		t.Fatalf("decode created configuration: %v", err)
	}
	configurationID := scalarArgument(createdBody["id"])
	if configurationID == "" {
		t.Fatalf("created configuration identity = %#v", createdBody)
	}

	var storedAuthor int64
	var storedSection string
	if err := pool.QueryRow(ctx, `
		SELECT author_id, section FROM p_1.configuration WHERE id = $1`, configurationID).
		Scan(&storedAuthor, &storedSection); err != nil {
		t.Fatalf("read created configuration: %v", err)
	}
	if storedAuthor != 73 || storedSection != "service_prompts" {
		t.Fatalf("stored actor/section = %d/%q, want 73/service_prompts", storedAuthor, storedSection)
	}

	updated, err := executor.Execute(ctx, 1, 73, internalUpdateConfiguration, map[string]any{
		"config_id": configurationID,
		"label":     "Renamed prompt",
		"shared":    true,
	})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("update configuration: status=%d error=%v body=%s", updated.status, err, updated.body)
	}
	var storedLabel string
	var storedShared bool
	var storedData []byte
	if err := pool.QueryRow(ctx, `
		SELECT label, shared, data FROM p_1.configuration WHERE id = $1`, configurationID).
		Scan(&storedLabel, &storedShared, &storedData); err != nil {
		t.Fatalf("read updated configuration: %v", err)
	}
	if storedLabel != "Renamed prompt" || !storedShared ||
		!json.Valid(storedData) || !bytes.Contains(storedData, []byte("Route this request.")) {
		t.Fatalf("partial update changed unexpected data: label=%q shared=%v data=%s",
			storedLabel, storedShared, storedData)
	}

	read, err := executor.Execute(ctx, 1, 73, internalGetConfiguration, map[string]any{
		"config_id": configurationID,
	})
	if err != nil || read.status != http.StatusOK || !json.Valid(read.body) {
		t.Fatalf("get configuration: status=%d error=%v body=%s", read.status, err, read.body)
	}

	listed, err := executor.Execute(ctx, 1, 73, internalListConfigurations, map[string]any{
		"type": []any{"service_prompt"}, "limit": json.Number("10"),
	})
	if err != nil || listed.status != http.StatusOK || !json.Valid(listed.body) ||
		!bytes.Contains(listed.body, []byte("internal_prompt")) {
		t.Fatalf("list configuration: status=%d error=%v body=%s", listed.status, err, listed.body)
	}
}

func TestInternalConfigurationTracingContainmentMatchesCurrentPlatform(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.auth_core__user (id, email, name)
		VALUES (73, 'tracing-member@example.test', 'Tracing member')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed tracing actor: %v", err)
	}
	assignConfigurationRole(t, pool, 73, "editor")

	executor := newHandlerInternalConfigurationExecutor(configurationsapi.NewHandler(pool), nil)
	createArguments := map[string]any{
		"elitea_title": "internal_tracing",
		"label":        "Internal tracing",
		"type":         "langfuse",
		"data":         map[string]any{},
	}
	refused, err := executor.Execute(ctx, 1, 73, internalCreateConfiguration, createArguments)
	if err != nil || refused.status != http.StatusForbidden {
		t.Fatalf("editor tracing create: status=%d error=%v body=%s", refused.status, err, refused.body)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.configuration WHERE elitea_title = 'internal_tracing'`).
		Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused create persisted rows=%d error=%v", rows, err)
	}

	assignConfigurationRole(t, pool, 73, "admin")
	created, err := executor.Execute(ctx, 1, 73, internalCreateConfiguration, createArguments)
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("admin tracing create: status=%d error=%v body=%s", created.status, err, created.body)
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.body, &createdBody); err != nil {
		t.Fatalf("decode tracing configuration: %v", err)
	}
	configurationID := scalarArgument(createdBody["id"])

	assignConfigurationRole(t, pool, 73, "editor")
	read, err := executor.Execute(ctx, 1, 73, internalGetConfiguration, map[string]any{
		"config_id": configurationID,
	})
	if err != nil || read.status != http.StatusNotFound {
		t.Fatalf("editor tracing read: status=%d error=%v body=%s", read.status, err, read.body)
	}
	updated, err := executor.Execute(ctx, 1, 73, internalUpdateConfiguration, map[string]any{
		"config_id": configurationID, "label": "must not update",
	})
	if err != nil || updated.status != http.StatusForbidden {
		t.Fatalf("editor tracing update: status=%d error=%v body=%s", updated.status, err, updated.body)
	}

	if _, err := pool.Exec(ctx, `UPDATE centry.project SET name = 'project_user_73' WHERE id = 1`); err != nil {
		t.Fatalf("mark personal project: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 73`); err != nil {
		t.Fatalf("remove personal-project role assignment: %v", err)
	}
	read, err = executor.Execute(ctx, 1, 73, internalGetConfiguration, map[string]any{
		"config_id": configurationID,
	})
	if err != nil || read.status != http.StatusOK {
		t.Fatalf("personal-project tracing read: status=%d error=%v body=%s", read.status, err, read.body)
	}
	updated, err = executor.Execute(ctx, 1, 73, internalUpdateConfiguration, map[string]any{
		"config_id": configurationID, "label": "Personal tracing",
	})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("personal-project tracing update: status=%d error=%v body=%s", updated.status, err, updated.body)
	}
}

func assignConfigurationRole(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, userID int64, role string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.auth_core__project_role (project_id, name)
		VALUES (1, 'admin'), (1, 'editor')
		ON CONFLICT (project_id, name) DO NOTHING`); err != nil {
		t.Fatalf("seed project roles: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = $1`, userID); err != nil {
		t.Fatalf("clear actor roles: %v", err)
	}
	command, err := pool.Exec(ctx, `
		INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
		SELECT 1, $1, id
		FROM public.auth_core__project_role
		WHERE project_id = 1 AND name = $2`, userID, role)
	if err != nil {
		t.Fatalf("assign %s role: %v", role, err)
	}
	if command.RowsAffected() != 1 {
		t.Fatalf("assign %s role affected %d rows", role, command.RowsAffected())
	}
}
