package agentexecution

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// ADR-0050 phase 0 (issue 1602): a start with no target in a conversation
// holding several agents answers its own code, so the composer can ask the
// person to pick an agent instead of showing a generic refusal.
func TestWriteStartErrorNamesTheMissingAgentTarget(t *testing.T) {
	for _, err := range []error{
		agentexecutionapp.ErrAgentTargetRequired,
		fmt.Errorf("resolve current ad-hoc target: %w", agentexecutionapp.ErrAgentTargetRequired),
	} {
		response := httptest.NewRecorder()
		writeStartError(response, err)
		if response.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(response.Body.String(), `"error":"agent_target_required"`) {
			t.Fatalf("%v: status=%d body=%s", err, response.Code, response.Body.String())
		}
	}
}
