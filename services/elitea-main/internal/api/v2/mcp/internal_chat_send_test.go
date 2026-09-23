package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	agentapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type chatStartProbe struct {
	app   *agentapp.CurrentApplicationStartRequest
	adhoc *agentapp.CurrentAdhocStartRequest
	err   error
}

func (p *chatStartProbe) StartCurrentApplication(_ context.Context, r agentapp.CurrentApplicationStartRequest) (agentapp.CurrentApplicationStartOutcome, error) {
	p.app = &r
	return agentapp.CurrentApplicationStartOutcome{ExecutionID: "execution", ResponseMessageID: "response"}, p.err
}
func (p *chatStartProbe) StartCurrentAdhoc(_ context.Context, r agentapp.CurrentAdhocStartRequest) (agentapp.CurrentApplicationStartOutcome, error) {
	p.adhoc = &r
	return agentapp.CurrentApplicationStartOutcome{ExecutionID: "execution", ResponseMessageID: "response"}, p.err
}
func chatSendFixture(p *chatStartProbe) *internalChatSendRuntime {
	return &internalChatSendRuntime{start: p, conversationGet: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"11111111-1111-4111-8111-111111111111","project_id":"2","participants":[{"id":7,"entity_name":"application"},{"id":8,"entity_name":"dummy"}]}`))
	}, read: func(context.Context, string, string) (turnState, error) {
		return turnState{settled: true, text: "answer"}, nil
	}}
}
func chatSendArguments() map[string]any {
	return map[string]any{"project_id": 2, "conversation_uuid": "11111111-1111-4111-8111-111111111111", "question_id": "22222222-2222-4222-8222-222222222222", "user_input": "hello", "participant_id": 7}
}
func chatSendContext() context.Context {
	return auth.ContextWithUser(context.Background(), auth.User{ID: "token", UserID: "3"})
}

func TestInternalChatSendUsesOneSharedAdmissionAndBoundResponse(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		p := &chatStartProbe{}
		runtime := chatSendFixture(p)
		args := chatSendArguments()
		if ordinary {
			delete(args, "participant_id")
			args["llm_settings"] = map[string]any{"model_name": "chosen"}
		}
		reads := 0
		runtime.read = func(_ context.Context, schema, response string) (turnState, error) {
			reads++
			if schema != "p_2" || response != "response" {
				t.Fatal("read lost scope")
			}
			return turnState{settled: true, text: "answer"}, nil
		}
		result, err := runtime.execute(chatSendContext(), 2, 3, args)
		if err != nil || result.status != 200 || reads != 1 {
			t.Fatalf("%+v %v", result, err)
		}
		if ordinary {
			if p.adhoc == nil || p.app != nil || p.adhoc.TargetParticipantID != 8 {
				t.Fatal("wrong ordinary admission")
			}
		} else {
			if p.app == nil || p.adhoc != nil || p.app.TargetParticipantID != 7 {
				t.Fatal("wrong application admission")
			}
		}
		var body map[string]any
		_ = json.Unmarshal(result.body, &body)
		if body["status"] != "completed" || body["task_id"] != "execution" || body["question_id"] != args["question_id"] {
			t.Fatal(string(result.body))
		}
	}
}
func TestInternalChatSendPauseIsNotSuccessAndAsyncDoesNotObserve(t *testing.T) {
	for _, wait := range []int{0, 30} {
		p := &chatStartProbe{}
		runtime := chatSendFixture(p)
		args := chatSendArguments()
		args["await_task_timeout"] = wait
		runtime.read = func(context.Context, string, string) (turnState, error) {
			if wait == 0 {
				t.Fatal("async observed")
			}
			return turnState{settled: true, hitlPause: true, authorizationPause: true, text: "partial"}, nil
		}
		result, err := runtime.execute(chatSendContext(), 2, 3, args)
		if err != nil {
			t.Fatal(err)
		}
		expected := http.StatusConflict
		if wait == 0 {
			expected = http.StatusAccepted
		}
		if result.status != expected {
			t.Fatalf("status=%d", result.status)
		}
	}
}
func TestInternalChatSendFailureNeverTriesAnotherAdmission(t *testing.T) {
	p := &chatStartProbe{err: errors.New("uncertain")}
	runtime := chatSendFixture(p)
	result, err := runtime.execute(chatSendContext(), 2, 3, chatSendArguments())
	if err != nil || result.status != 400 || p.app == nil || p.adhoc != nil {
		t.Fatalf("%+v %v", result, err)
	}
}
func TestInternalChatSendRejectsDecisionAndAuthorityArguments(t *testing.T) {
	for _, field := range []string{"hitl_action", "mcp_auth_resume", "actor_id", "execution_id"} {
		p := &chatStartProbe{}
		runtime := chatSendFixture(p)
		args := chatSendArguments()
		args[field] = "forged"
		result, err := runtime.execute(chatSendContext(), 2, 3, args)
		if err != nil || result.status != 400 || p.app != nil || p.adhoc != nil {
			t.Fatalf("accepted %s", field)
		}
	}
}
