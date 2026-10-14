package toolkits

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemorySettingsAreValidatedOnSave(t *testing.T) {
	t.Parallel()
	valid := []map[string]any{
		{},
		{"namespace": "team-notes", "memory_scope": "both"},
		{"namespace": nil, "memory_scope": nil},
		{"namespace": strings.Repeat("n", 256), "memory_scope": "project"},
		{"memory_scope": "personal", "pgvector_configuration": map[string]any{}},
	}
	for _, settings := range valid {
		if problems := memorySettingsProblems(settings); len(problems) != 0 {
			t.Errorf("%v: unexpected problems %v", settings, problems)
		}
	}
	invalid := map[string]map[string]any{
		"namespace":    {"namespace": "   "},
		"namespace ":   {"namespace": 5},
		"namespace  ":  {"namespace": strings.Repeat("n", 257)},
		"namespace   ": {"namespace": "a\nb"},
		"memory_scope": {"memory_scope": "everyone"},
	}
	for field, settings := range invalid {
		problems := memorySettingsProblems(settings)
		if len(problems) != 1 {
			t.Errorf("%v: problems = %v", settings, problems)
			continue
		}
		loc, _ := problems[0]["loc"].([]string)
		if len(loc) != 2 || loc[0] != "settings" || loc[1] != strings.TrimSpace(field) {
			t.Errorf("%v: loc = %v", settings, loc)
		}
	}
	if problems := memorySettingsProblems(map[string]any{"namespace": 1, "memory_scope": 2}); len(problems) != 2 {
		t.Errorf("two bad settings gave %v", problems)
	}

	// The refusal is a 400 with the form's field errors, for the memory type
	// only, and does not need the configuration validator.
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	if !handler.refuseMalformedMemorySettings(recorder, "create_toolkit", "memory", map[string]any{"memory_scope": "x"}) {
		t.Fatal("a malformed memory scope was not refused")
	}
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "settings_errors") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	other := httptest.NewRecorder()
	if handler.refuseMalformedMemorySettings(other, "create_toolkit", "github", map[string]any{"memory_scope": "x"}) {
		t.Fatal("another toolkit type was judged by the memory rules")
	}
	if handler.refuseMalformedMemorySettings(httptest.NewRecorder(), "create_toolkit", "memory", map[string]any{"namespace": "ok"}) {
		t.Fatal("a valid memory toolkit was refused")
	}
}
