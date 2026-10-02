package mcp

import "testing"

func TestInternalConfigurationIDsForwarding(t *testing.T) {
	for _, raw := range []string{"", "4,2,4"} {
		query, failure := internalConfigurationListQuery(map[string]any{"ids": raw, "limit": 1})
		if failure != nil || query.Get("ids") != raw || query.Get("limit") != "1" {
			t.Fatalf("query %v failure %v", query, failure)
		}
	}
	for _, raw := range []any{nil, []any{1}, 1, "0", "2,invalid"} {
		if _, failure := internalConfigurationListQuery(map[string]any{"ids": raw}); failure == nil {
			t.Fatalf("accepted %#v", raw)
		}
	}
}
