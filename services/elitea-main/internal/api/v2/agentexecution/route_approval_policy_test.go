package agentexecution

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
)

// A pipeline the approval policy refuses (ADR-0041 decision 5) answers 422
// with the rule's code and the message that names the node and the path,
// not the generic unsupported answer.
func TestWriteStartErrorNamesTheApprovalPolicyRefusal(t *testing.T) {
	refusal := approvalpolicy.CheckPipelineHITL(`
entry_point: post
nodes:
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`, []approvalpolicy.PipelineToolkit{{Name: "chat", Type: "slack"}}, agentlifecycle.TierCritical)
	response := httptest.NewRecorder()
	writeStartError(response, fmt.Errorf("%w: %w", agentexecutionapp.ErrUnsupportedCurrentAgentStart, refusal))
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusUnprocessableEntity || body["error"] != approvalpolicy.CodeHITLRequired ||
		!strings.Contains(body["message"], `Node "post"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
