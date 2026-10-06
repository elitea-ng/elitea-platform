package toolkitcalltool

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCodeToolkitGuardRequiresApprovalBeforeNativeAdmission(t *testing.T) {
	inputs := testInputs()
	inputs.RuntimeContext = json.RawMessage(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{"*":["list_issues"]},"sensitive_action_company_name":"Company","sensitive_action_message_template":"Approval required"}}`)
	resolver := &stubResolver{inputs: inputs}
	admissions := &stubAdmissions{}
	dispatcher := &stubDispatcher{}
	service := newTestService(t, resolver, stubVerdict{supported: true}, admissions, dispatcher, &stubSettlements{}, time.Second)
	request := RunRequest{ProjectID: 7, ActorUserID: 11, ToolkitID: 19, ToolName: "list_issues", Arguments: json.RawMessage(`{}`)}
	revision := CodeToolkitRevision(inputs)
	if err := service.CheckCodeTool(t.Context(), request, hex.EncodeToString(revision[:])); !errors.Is(err, ErrCodeToolApprovalRequired) {
		t.Fatalf("sensitive guard=%v", err)
	}
	if admissions.calls != 0 || dispatcher.calls != 0 {
		t.Fatal("sensitive call reached admission or dispatch before approval")
	}
}
func TestCodeToolkitExactRevisionRejectsSameNameDifferentIdentity(t *testing.T) {
	original := testInputs()
	selected := CodeToolkitRevision(original)
	foreign := original
	foreign.ToolkitID++
	foreign.Settings = json.RawMessage(`{"id":20,"type":"github","toolkit_name":"gh","settings":{}}`)
	resolver := &stubResolver{inputs: foreign}
	admissions := &stubAdmissions{}
	dispatcher := &stubDispatcher{}
	service := newTestService(t, resolver, stubVerdict{supported: true}, admissions, dispatcher, &stubSettlements{}, time.Second)
	request := RunRequest{ProjectID: 7, ActorUserID: 11, ToolkitID: 19, ToolName: "list_issues", Arguments: json.RawMessage(`{}`)}
	if err := service.CheckCodeTool(t.Context(), request, hex.EncodeToString(selected[:])); !errors.Is(err, ErrCodeToolkitRevisionConflict) {
		t.Fatal("same name substituted for original ID/revision", err)
	}
	if admissions.calls != 0 || dispatcher.calls != 0 {
		t.Fatal("foreign identity reached an effect")
	}
}
