package configurations

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue 1968: a configuration's label and display name are at most 128 characters.
func TestRefuseOverlongConfigurationName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    map[string]any
		refused bool
	}{
		{"label 128", map[string]any{"label": strings.Repeat("a", 128)}, false},
		{"label 129", map[string]any{"label": strings.Repeat("a", 129)}, true},
		{"title 129 multibyte", map[string]any{"elitea_title": strings.Repeat("я", 129)}, true},
		{"name alias 129", map[string]any{"name": strings.Repeat("a", 129)}, true},
		{"absent", map[string]any{"type": "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if got := refuseOverlongConfigurationName(rec, tc.body); got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
			if tc.refused && rec.Code != 400 {
				t.Fatalf("status = %d", rec.Code)
			}
		})
	}
}
