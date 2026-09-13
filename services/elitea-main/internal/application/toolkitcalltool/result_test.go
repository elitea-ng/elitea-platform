package toolkitcalltool

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type resultStore struct {
	stubSettlements
	state  string
	denied bool
}

func (s *resultStore) ReadToolkitCallToolResultBinding(context.Context, ResultRequest) (executiondomain.ToolkitCallToolBinding, string, error) {
	if s.denied {
		return executiondomain.ToolkitCallToolBinding{}, "", ErrToolRunNotFound
	}
	return executiondomain.ToolkitCallToolBinding{ToolkitID: 19, ToolkitType: "github"}, s.state, nil
}
func TestReadToolRunDoesNotAdmitOrDispatch(t *testing.T) {
	request := ResultRequest{ProjectID: 1, ActorUserID: 7, ToolkitID: 19, ExecutionID: "execution"}
	for _, state := range []string{"PENDING", "DISPATCHED", "CLAIMED", "RUNNING", "SETTLING"} {
		t.Run(state, func(t *testing.T) {
			store := &resultStore{state: state}
			service := &RunService{settlements: store}
			_, pending, err := service.ReadToolRun(context.Background(), request)
			if err != nil || !pending {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			store.found = true
			store.settlement = settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{"answer":42}`, "")
			result, pending, err := service.ReadToolRun(context.Background(), request)
			if err != nil || pending || result.ResultJSON != `{"answer":42}` {
				t.Fatalf("result=%+v pending=%v err=%v", result, pending, err)
			}
		})
	}
	store := &resultStore{denied: true}
	service := &RunService{settlements: store}
	if _, _, err := service.ReadToolRun(context.Background(), request); !errors.Is(err, ErrToolRunNotFound) || store.calls != 0 {
		t.Fatalf("unauthorized read: calls=%d err=%v", store.calls, err)
	}
	store.denied = false
	store.state = "FAILED"
	result, pending, err := service.ReadToolRun(context.Background(), request)
	if err != nil || pending || result.Status != RunStatusRuntimeFailure {
		t.Fatalf("terminal=%+v pending=%v err=%v", result, pending, err)
	}
}
