package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	toolkitsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

func TestInternalToolkitLifecyclePersistsAndChangesAgentAttachment(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, applicationVersionID := seedInternalApplicationVersion(t, pool)
	executor := newHandlerInternalToolkitExecutor(toolkitsapi.NewHandler(pool))
	ctx := context.Background()

	created, err := executor.Execute(ctx, 1, 73, internalCreateToolkit, map[string]any{
		"type":        "custom",
		"name":        "durable_tools",
		"description": "Internal MCP toolkit fixture.",
		"settings": map[string]any{
			"selected_tools": []any{"first", "second"},
		},
	})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create toolkit: status=%d error=%v body=%s", created.status, err, created.body)
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.body, &createdBody); err != nil {
		t.Fatalf("decode created toolkit: %v", err)
	}
	toolkitID := scalarArgument(createdBody["id"])
	if toolkitID == "" {
		t.Fatalf("created toolkit identity = %v", createdBody)
	}
	var storedAuthor int64
	if err := pool.QueryRow(ctx, `SELECT author_id FROM p_1.elitea_tools WHERE id = $1`, toolkitID).
		Scan(&storedAuthor); err != nil {
		t.Fatalf("read toolkit author: %v", err)
	}
	if storedAuthor != 73 {
		t.Fatalf("stored author = %d, want 73", storedAuthor)
	}

	updated, err := executor.Execute(ctx, 1, 73, internalUpdateToolkit, map[string]any{
		"toolkit_id": toolkitID,
		"name":       "durable_tools_renamed",
	})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("update toolkit: status=%d error=%v body=%s", updated.status, err, updated.body)
	}
	var storedName string
	if err := pool.QueryRow(ctx, `SELECT name FROM p_1.elitea_tools WHERE id = $1`, toolkitID).
		Scan(&storedName); err != nil {
		t.Fatalf("read toolkit name: %v", err)
	}
	if storedName != "durable_tools_renamed" {
		t.Fatalf("stored name = %q", storedName)
	}

	attached, err := executor.Execute(ctx, 1, 73, internalUpdateToolRelation, map[string]any{
		"toolkit_id":        toolkitID,
		"entity_id":         fmt.Sprintf("%d", applicationID),
		"entity_version_id": fmt.Sprintf("%d", applicationVersionID),
		"has_relation":      true,
	})
	if err != nil || attached.status != http.StatusCreated {
		t.Fatalf("attach toolkit: status=%d error=%v body=%s", attached.status, err, attached.body)
	}
	var selectedBefore string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(selected_tools, 'null'::jsonb)::text FROM p_1.entity_tool_mapping
		WHERE entity_version_id = $1 AND tool_id = $2`, applicationVersionID, toolkitID).
		Scan(&selectedBefore); err != nil {
		t.Fatalf("read attached toolkit selection: %v", err)
	}
	if selectedBefore != "[]" && selectedBefore != "null" {
		t.Fatalf("new relation selection = %s, want database default without an invented selection", selectedBefore)
	}

	selectedEmpty, err := executor.Execute(ctx, 1, 73, internalUpdateToolRelation, map[string]any{
		"toolkit_id":        toolkitID,
		"entity_id":         fmt.Sprintf("%d", applicationID),
		"entity_version_id": fmt.Sprintf("%d", applicationVersionID),
		"has_relation":      true,
		"selected_tools":    []any{},
	})
	if err != nil || selectedEmpty.status != http.StatusCreated {
		t.Fatalf("replace toolkit selection: status=%d error=%v body=%s", selectedEmpty.status, err, selectedEmpty.body)
	}
	var selectedAfter string
	if err := pool.QueryRow(ctx, `
		SELECT selected_tools::text FROM p_1.entity_tool_mapping
		WHERE entity_version_id = $1 AND tool_id = $2`, applicationVersionID, toolkitID).
		Scan(&selectedAfter); err != nil {
		t.Fatalf("read replaced toolkit selection: %v", err)
	}
	if selectedAfter != "[]" {
		t.Fatalf("replaced relation selection = %s, want []", selectedAfter)
	}

	detached, err := executor.Execute(ctx, 1, 73, internalUpdateToolRelation, map[string]any{
		"toolkit_id":        toolkitID,
		"entity_id":         fmt.Sprintf("%d", applicationID),
		"entity_version_id": fmt.Sprintf("%d", applicationVersionID),
		"has_relation":      false,
	})
	if err != nil || detached.status != http.StatusCreated {
		t.Fatalf("detach toolkit: status=%d error=%v body=%s", detached.status, err, detached.body)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM p_1.entity_tool_mapping
		WHERE entity_version_id = $1 AND tool_id = $2`, applicationVersionID, toolkitID).
		Scan(&remaining); err != nil {
		t.Fatalf("count detached toolkit relation: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("detached toolkit relation count = %d", remaining)
	}

	listed, err := executor.Execute(ctx, 1, 73, internalListToolkits, map[string]any{
		"limit": json.Number("100"), "offset": json.Number("0"),
	})
	if err != nil || listed.status != http.StatusOK {
		t.Fatalf("list toolkit: status=%d error=%v body=%s", listed.status, err, listed.body)
	}
	if !json.Valid(listed.body) {
		t.Fatalf("list toolkit returned invalid JSON: %s", listed.body)
	}
}
