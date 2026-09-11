package mcp

import (
	"context"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"net/http"
	"strings"
	"testing"
)

func TestInternalDiscoveryCatalogHasOnlyCurrentReadOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(context.Background(), `"p_1"`, scope{kind: scopeCategory, category: internalDiscoveryCategory})
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	for _, tool := range tools {
		if tool.permission == "" || tool.internalDiscoveryOperation == "" {
			t.Fatalf("missing authority: %+v", tool)
		}
	}
	if tools[0].Name != "get_prompt_lib_tags" || tools[1].Name != "get_prompt_lib_search_options" {
		t.Fatalf("names=%v", tools)
	}
}
func TestInternalDiscoveryAuthorityPrecedesDataAccess(t *testing.T) {
	tool := internalDiscoveryTools()[0]
	for _, test := range []struct {
		name        string
		project     any
		permissions []string
		wantCalls   int
	}{
		{"foreign project", float64(2), []string{tool.permission}, 0},
		{"no permission", float64(1), nil, 1},
		{"authorized unavailable", float64(1), []string{tool.permission}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: test.permissions}}
			h := NewHandler(nil, nil, nil, resolver)
			req, _ := http.NewRequest(http.MethodPost, "/app/1/mcp/elitea_core/discovery", nil)
			req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "41", UserID: "41"}))
			result := h.callInternalDiscoveryTool(req, 1, tool, map[string]any{"project_id": test.project})
			if result["isError"] != true || resolver.calls != test.wantCalls {
				t.Fatalf("result=%v resolver=%+v", result, resolver)
			}
			if text := textOf(t, result); strings.Contains(text, "database") || strings.Contains(text, "password") {
				t.Fatalf("unsafe error=%s", text)
			}
		})
	}
}
func TestInternalDiscoveryRefusesUnknownAndOversizedArguments(t *testing.T) {
	tool := internalDiscoveryTools()[0]
	for _, args := range []map[string]any{{"schema": "p_2"}, {"query": strings.Repeat("x", 1025)}, {"tags": make([]any, 101)}, {"tags": "1"}, {"query": true}, {"my_liked": "true"}} {
		if _, err := internalDiscoveryQuery(tool, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestInternalDiscoveryArraysKeepRESTQueryNames(t *testing.T) {
	values, err := internalDiscoveryQuery(internalDiscoveryTools()[1], map[string]any{
		"entities": []any{"application", "skill"},
		"statuses": []any{"published"},
		"tags":     []any{float64(7), float64(11)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("entities[]") != "application" || values.Get("statuses[]") != "published" || len(values["tags[]"]) != 2 {
		t.Fatalf("REST filters changed: %v", values)
	}
	for _, key := range []string{"entities", "statuses", "tags"} {
		if values.Has(key) {
			t.Fatalf("model field leaked into REST query: %s", key)
		}
	}
}
