package toolkits

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue 1968: toolkit name 128 / description 2000 characters.
func TestRefuseOverlongText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    map[string]any
		refused bool
		field   string
	}{
		{"name 128", map[string]any{"name": strings.Repeat("a", 128)}, false, ""},
		{"name 129", map[string]any{"name": strings.Repeat("a", 129)}, true, "name"},
		{"description 2000 multibyte", map[string]any{"description": strings.Repeat("字", 2000)}, false, ""},
		{"description 2001", map[string]any{"description": strings.Repeat("a", 2001)}, true, "description"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if got := refuseOverlongText(rec, tc.body); got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
			if !tc.refused {
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", rec.Code)
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["field"] != tc.field {
				t.Fatalf("field = %v, want %s", out["field"], tc.field)
			}
		})
	}
}
