package configurations

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAgentConfigurationDiscoveryPreservesSelectedSchema(t *testing.T) {
	h := NewHandler(nil)
	read := func(path string, discovery bool) []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		if discovery {
			h.DiscoverAvailable(w, r)
		} else {
			h.Available(w, r)
		}
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var result []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	full := read("/?section=credentials", false)
	summary := read("/?section=credentials", true)
	if len(summary) != len(full) {
		t.Fatal("summary loses types")
	}
	for _, item := range summary {
		if _, ok := item["config_schema"]; ok {
			t.Fatal("summary includes schemas")
		}
	}
	selected := read("/?section=credentials&type=github", true)
	if len(selected) != 1 || !reflect.DeepEqual(selected[0], findCurrentAvailableDTO(t, full, "github")) {
		t.Fatal("selected schema differs from REST catalog")
	}
	if len(read("/?section=credentials&type=unknown", true)) != 0 {
		t.Fatal("unknown type returns unrelated schemas")
	}
}
