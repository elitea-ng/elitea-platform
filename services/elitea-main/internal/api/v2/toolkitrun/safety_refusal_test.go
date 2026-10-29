package toolkitrun

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// A direct toolkit run the kill switch refused (ADR-0041 decision 1) answers
// 409 execution_suspended, and 503 when the suspension state could not be
// read — not the 500 of an unknown failure. The toolkit routes, the tool
// test route and the index route's synchronous tool run all write through
// WriteError.
func TestWriteErrorMapsSafetyRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("admit: %w", &safety.SuspendedError{SuspensionID: 3, Scope: safety.ScopeToolkitType, Key: "github"}),
			http.StatusConflict, safety.ErrorCode},
		{fmt.Errorf("admit: %w: db down", safety.ErrStateUnavailable),
			http.StatusServiceUnavailable, "suspension_state_unavailable"},
	} {
		rec := httptest.NewRecorder()
		WriteError(rec, tc.err)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tc.status || body["code"] != tc.code || body["ok"] != false {
			t.Errorf("%v: status %d body %v; want %d %s", tc.err, rec.Code, body, tc.status, tc.code)
		}
	}
}
