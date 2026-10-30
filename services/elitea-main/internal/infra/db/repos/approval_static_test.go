package repos

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStaticApprovalPausesReadEveryWait(t *testing.T) {
	message, err := decodeCurrentAgentFullMessage([]byte(`"paused"`), []byte(`[]`), []byte(staticMessageMetadataFixture()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := staticApprovalPauses(&message)
	if err != nil {
		t.Fatal(err)
	}
	pauses, order := decodeApprovalPauseList(raw)
	if len(order) != 1 || order[0] != "pipeline-static:sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("pauses = %s", raw)
	}
	pause := pauses[order[0]]
	if call := approvalCallOf(pause); call.GuardrailType != "pipeline_static" || call.ArgumentDigest != "" {
		t.Fatalf("call = %+v", call)
	}
	// A static wait offers only continue: an unattended skip finds no
	// refusal and falls back to fail.
	if refusal := skipRefusal(availableActions(pause)); refusal != "" {
		t.Fatalf("a static wait offers the refusal %q", refusal)
	}

	tools := currentAgentFullMessage{PipelineStaticTools: json.RawMessage(`{"revision":1,"pauses":[
		{"proof":{"pause_id":"pipeline-static:sha256:` + strings.Repeat("b", 64) + `"}},
		{"proof":{"pause_id":"pipeline-static:sha256:` + strings.Repeat("c", 64) + `"}}]}`)}
	raw, err = staticApprovalPauses(&tools)
	if err != nil {
		t.Fatal(err)
	}
	if _, order := decodeApprovalPauseList(raw); len(order) != 2 {
		t.Fatalf("tool pauses = %s", raw)
	}
	if raw, err := staticApprovalPauses(&currentAgentFullMessage{}); raw != nil || err != nil {
		t.Fatalf("an ordinary message has no static wait: %s %v", raw, err)
	}
	if _, err := staticApprovalPauses(&currentAgentFullMessage{PipelineStaticTools: json.RawMessage(`{"pauses":[{}]}`)}); err == nil {
		t.Fatal("a static wait without a pause id is accepted")
	}
}
