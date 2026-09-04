package mcp

import (
	"encoding/json"
	"testing"
)

func TestInternalConfigurationListBuildsOnlyBoundedMainFilters(t *testing.T) {
	query, failure := internalConfigurationListQuery(map[string]any{
		"section":        []any{"ai_credentials", "llm"},
		"type":           []any{"open_ai", "llm_model"},
		"query":          "shared",
		"offset":         json.Number("30"),
		"limit":          json.Number("25"),
		"include_shared": true,
		"shared_offset":  json.Number("5"),
		"shared_limit":   json.Number("15"),
		"sort_by":        "elitea_title",
		"sort_order":     "asc",
		"ids":            []any{json.Number("999")},
	})
	if failure != nil {
		t.Fatalf("build list query: %s", failure.body)
	}
	if got := query["section"]; len(got) != 2 || got[0] != "ai_credentials" || got[1] != "llm" {
		t.Fatalf("section query = %#v", got)
	}
	if got := query["type"]; len(got) != 2 || got[0] != "open_ai" || got[1] != "llm_model" {
		t.Fatalf("type query = %#v", got)
	}
	for key, want := range map[string]string{
		"query": "shared", "offset": "30", "limit": "25", "include_shared": "true",
		"shared_offset": "5", "shared_limit": "15", "sort_by": "elitea_title", "sort_order": "asc",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q", key, got, want)
		}
	}
	if _, crossed := query["ids"]; crossed {
		t.Fatalf("unsupported ids crossed into Main query: %#v", query)
	}
}

func TestInternalConfigurationCreateUsesAllowlistAndNormalizesTitle(t *testing.T) {
	body, failure := internalConfigurationWriteBody(map[string]any{
		"project_id":   json.Number("9"),
		"elitea_title": "Build_Creds",
		"label":        "Build credentials",
		"type":         "open_ai",
		"shared":       true,
		"data":         map[string]any{"api_base": "https://example.invalid"},
		"meta":         map[string]any{"purpose": "must-not-cross"},
		"author_id":    json.Number("999"),
		"source":       "system",
		"status_ok":    true,
		"section":      "administration",
	}, true)
	if failure != nil {
		t.Fatalf("create body: %s", failure.body)
	}
	if body["elitea_title"] != "build_creds" || body["type"] != "open_ai" || body["shared"] != true {
		t.Fatalf("create body = %#v", body)
	}
	for _, forbidden := range []string{"project_id", "author_id", "source", "status_ok", "section", "meta"} {
		if _, crossed := body[forbidden]; crossed {
			t.Fatalf("control field %q crossed into create body: %#v", forbidden, body)
		}
	}
}

func TestInternalConfigurationUpdateIsPartialAndCannotReplaceIdentityOrType(t *testing.T) {
	body, failure := internalConfigurationWriteBody(map[string]any{
		"config_id":    json.Number("17"),
		"elitea_title": "renamed",
		"shared":       false,
		"type":         "other",
		"section":      "other",
		"project_id":   json.Number("999"),
		"author_id":    json.Number("999"),
	}, false)
	if failure != nil {
		t.Fatalf("update body: %s", failure.body)
	}
	if body["elitea_title"] != "renamed" || body["shared"] != false {
		t.Fatalf("update body = %#v", body)
	}
	for _, forbidden := range []string{"config_id", "type", "section", "project_id", "author_id"} {
		if _, crossed := body[forbidden]; crossed {
			t.Fatalf("identity field %q crossed into update body: %#v", forbidden, body)
		}
	}
}

func TestInternalConfigurationValidationStopsBeforeHandlerInvocation(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
		create    bool
	}{
		{"missing title", map[string]any{"label": "Label", "type": "open_ai", "data": map[string]any{}}, true},
		{"invalid title", map[string]any{"elitea_title": "spaces fail", "label": "Label", "type": "open_ai", "data": map[string]any{}}, true},
		{"missing label", map[string]any{"elitea_title": "ok", "type": "open_ai", "data": map[string]any{}}, true},
		{"missing data", map[string]any{"elitea_title": "ok", "label": "Label", "type": "open_ai"}, true},
		{"empty update", map[string]any{"config_id": json.Number("17")}, false},
		{"wrong shared", map[string]any{"shared": "false"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, failure := internalConfigurationWriteBody(test.arguments, test.create)
			if failure == nil || failure.status != 400 {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}

	if _, failure := internalConfigurationListQuery(map[string]any{
		"section": make([]any, internalConfigurationMaxFilterValues+1),
	}); failure == nil || failure.status != 400 {
		t.Fatalf("oversized list failure = %#v", failure)
	}
}
