package applications

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckCitationsAcceptsOffRequiredAndNull(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"no meta":  {},
		"absent":   {"meta": map[string]any{"step_limit": 5}},
		"null":     {"meta": map[string]any{"citations": nil}},
		"off":      {"meta": map[string]any{"citations": "off"}},
		"required": {"meta": map[string]any{"citations": "required"}},
	} {
		recorder := httptest.NewRecorder()
		if !checkCitations(recorder, body) {
			t.Errorf("%s: refused with %d", name, recorder.Code)
		}
	}
}

func TestCheckCitationsRefusesOtherValues(t *testing.T) {
	for _, value := range []any{"Required", "always", true, 1, map[string]any{}} {
		recorder := httptest.NewRecorder()
		body := map[string]any{"meta": map[string]any{"citations": value}}
		if checkCitations(recorder, body) || recorder.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d, want 400", value, recorder.Code)
		}
		// The save path refuses it before it checks the output contract.
		recorder = httptest.NewRecorder()
		if checkOutputContracts(recorder, body, "agent") || recorder.Code != http.StatusBadRequest {
			t.Errorf("%v through the save check: got %d, want 400", value, recorder.Code)
		}
	}
}
