package agentexecution

import (
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// ADR-0050 decision 2 (#1618): a heartbeat act binds only the tools the
// agent may use unattended, whatever floor the conversation had.
func TestHeartbeatFloorBindsOnlyUnattendedTools(t *testing.T) {
	if got := heartbeatFloor(nil, false); got != nil {
		t.Fatalf("not a heartbeat: %+v", got)
	}
	got := heartbeatFloor(nil, true)
	if got.GetCause() != turntaking.CauseHeartbeat || !got.GetUnattendedToolsOnly() {
		t.Fatalf("heartbeat floor = %+v", got)
	}
	existing := &runtimev1.AgentFloorV1{Cause: turntaking.CauseHumanMessage}
	if got := heartbeatFloor(existing, true); got.GetCause() != turntaking.CauseHeartbeat || !got.GetUnattendedToolsOnly() {
		t.Fatalf("existing floor = %+v", got)
	}
}

func TestHeartbeatStartIsAScheduleStartOnly(t *testing.T) {
	request := CurrentApplicationStartRequest{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ConversationUUID: "10000000-0000-4000-8000-000000000031", QuestionID: "20000000-0000-4000-8000-000000000031",
		UserInput: "act", TriggerOrigin: executiondomain.TriggerOriginSchedule, Heartbeat: true,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("a schedule heartbeat start: %v", err)
	}
	request.TriggerOrigin = executiondomain.TriggerOriginManual
	if err := request.Validate(); err == nil {
		t.Fatal("a manual heartbeat start was accepted")
	}
	request.TriggerOrigin = executiondomain.TriggerOriginSchedule
	request.AddressedTo = []int64{21}
	if err := request.Validate(); err == nil {
		t.Fatal("a heartbeat start with addressees was accepted")
	}
}
