package indexing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// An index start the kill switch refused (ADR-0041 decision 1) answers 409
// execution_suspended, or 503 when the suspension state could not be read,
// not 500 "Failed to start index_data".
func TestIndexStartMapsSafetyRefusals(t *testing.T) {
	t.Parallel()
	h := &StartHandler{}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("submit: %w", &safety.SuspendedError{SuspensionID: 4, Scope: safety.ScopeProject, ProjectID: 7, Key: "7"}),
			http.StatusConflict, safety.ErrorCode},
		{fmt.Errorf("submit: %w: db down", safety.ErrStateUnavailable),
			http.StatusServiceUnavailable, "suspension_state_unavailable"},
	} {
		rec := httptest.NewRecorder()
		h.writeStartError(rec, tc.err)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tc.status || body["code"] != tc.code {
			t.Errorf("%v: status %d body %v; want %d %s", tc.err, rec.Code, body, tc.status, tc.code)
		}
	}
}
