package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestInternalApplicationListFiltersReachSharedRepository(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, _ := seedInternalApplicationVersion(t, pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.social_likes(entity_name,entity_id,user_id,created_at) VALUES('application',$1,41,'2026-01-15')`, appID); err != nil {
		t.Fatal(err)
	}
	permission := "models.applications.applications.list"
	var tool Tool
	for _, candidate := range internalApplicationTools() {
		if candidate.internalApplicationOperation == internalListApplications {
			tool = candidate
		}
	}
	permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}}}
	router := internalApplicationRouter(t, tool, newPostgresInternalApplicationExecutor(pool), permissions)
	arguments := map[string]any{"ids": strconv.FormatInt(appID, 10), "tags": "old-tag", "author_id": 7, "statuses": "draft", "agents_type": "pipeline", "my_liked": true, "trend_start_period": "2026-01-01T00:00:00", "trend_end_period": "2026-02-01T00:00:00", "sort_by": "name", "sort_order": "asc", "limit": 1, "offset": 0}
	invoke := func() map[string]any {
		wire, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool.Name, "arguments": arguments}})
		return resultOf(t, post(t, router, "/app/1/mcp/elitea_core/applications", string(wire)))
	}
	result := invoke()
	if result["isError"] == true {
		t.Fatalf("list=%v", result)
	}
	var page struct {
		Total int              `json:"total"`
		Rows  []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(textOf(t, result)), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0]["id"] != strconv.FormatInt(appID, 10) {
		t.Fatalf("filtered list=%+v", page)
	}
	arguments["tags"] = "missing-tag"
	result = invoke()
	if err := json.Unmarshal([]byte(textOf(t, result)), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Fatalf("tag filter ignored: %+v", page)
	}
	arguments["tags"] = "old-tag"
	arguments["offset"] = 1
	result = invoke()
	if err := json.Unmarshal([]byte(textOf(t, result)), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 0 {
		t.Fatalf("offset changed total: %+v", page)
	}
	arguments["ids"] = "0"
	result = invoke()
	if result["isError"] != true {
		t.Fatalf("invalid IDs accepted: %v", result)
	}
}
