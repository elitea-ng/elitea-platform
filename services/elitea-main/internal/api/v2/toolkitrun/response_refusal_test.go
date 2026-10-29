package toolkitrun

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// A command Main refused to prepare (a worker egress allowlist that cannot be
// assembled) answers 422 with the refusal's own message at once, not a 500
// and not a timeout.
func TestWriteErrorAnswersADispatchRefusal(t *testing.T) {
	const message = `The platform's worker egress allowlist entry "https://x" is not a valid host. A platform administrator must correct it.`
	recorder := httptest.NewRecorder()
	WriteError(recorder, fmt.Errorf("dispatch tool run: %w", &executionapp.DispatchRefusal{Code: "EGRESS_POLICY_INVALID", Message: message}))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || body["reason"] != "egress_policy_invalid" ||
		body["error"] != message || body["ok"] != false {
		t.Fatalf("%d %v", recorder.Code, body)
	}
}
