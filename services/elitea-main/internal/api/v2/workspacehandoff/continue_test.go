package workspacehandoff

import (
	"net/http"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	handoffapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const completeURL = "/api/v2/elitea_core/workspace_handoff_complete/prompt_lib/1/0b7e4c1a-2d3f-4a5b-9c6d-7e8f9a0b1c2d"

// Contract 1.33 (ADR-0045 decision 8): the complete's take_back_when_done
// and continue reach the service, and the answer names the cloud turn.
func TestCompleteWithContinueReachesTheServiceAndAnswersTheTurn(t *testing.T) {
	handoff := &fakeHandoff{handoff: handoffapp.Handoff{State: workspacestore.StateCloudRunning, Epoch: 4,
		Continued: &handoffapp.ContinuedTurn{ExecutionID: "3e9a1b7c5d2f4e6a8b0c1d2e3f4a5b6c",
			QuestionID: "7a9c1e3f-5b7d-4f9a-9b1c-3d5e7f9a1b2c", ResponseMessageID: "2c4e6a8b-0d1f-4a3b-8c5d-7e9f1a2b3c4d"}}}
	h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
	body := `{"epoch":3,"handoff_ref":"refs/elitea/handoff/1","sha":"` + sha1Value + `","take_back_when_done":true,
"continue":{"prompt":"Finish the migration.","mode":"plan","grant":{"grant_id":"1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
"chain_root_execution_id":"9f8e7d6c5b4a39281706f5e4d3c2b1a0","continue_step_limit":true,"hook_asks":true,
"chain":{"max_continuations":5},"goal_check":false}}}`
	response := serve(t, h.complete, nativeUser(), []string{SendPermission}, http.MethodPost, CompletePath, completeURL, body)
	answer := decode(t, response)
	continued, _ := answer["continued"].(map[string]any)
	if response.Code != http.StatusOK || answer["state"] != "CLOUD_RUNNING" || continued == nil ||
		continued["execution_id"] != "3e9a1b7c5d2f4e6a8b0c1d2e3f4a5b6c" ||
		continued["message_id"] != "2c4e6a8b-0d1f-4a3b-8c5d-7e9f1a2b3c4d" ||
		continued["question_id"] != "7a9c1e3f-5b7d-4f9a-9b1c-3d5e7f9a1b2c" {
		t.Fatalf("complete = %d %v", response.Code, answer)
	}
	request := handoff.completed
	if !request.TakeBackWhenDone || request.Continue == nil || request.Continue.Prompt != "Finish the migration." ||
		request.Continue.Mode != "plan" || request.Continue.Grant == nil {
		t.Fatalf("complete request = %+v", request)
	}
	grant := request.Continue.Grant
	if grant.GrantID != "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f" || grant.ChainRootExecutionID != "9f8e7d6c5b4a39281706f5e4d3c2b1a0" ||
		!grant.ContinueStepLimit || !grant.HookAsks || grant.GoalCheck || grant.Chain.MaxContinuations == nil ||
		*grant.Chain.MaxContinuations != 5 {
		t.Fatalf("grant = %+v", grant)
	}

	// A plain complete answers no continued turn.
	handoff.handoff = handoffapp.Handoff{State: workspacestore.StateCloudIdle, Epoch: 4}
	response = serve(t, h.complete, nativeUser(), []string{SendPermission}, http.MethodPost, CompletePath, completeURL,
		`{"epoch":3,"handoff_ref":"refs/elitea/handoff/1","sha":"`+sha1Value+`"}`)
	answer = decode(t, response)
	if _, present := answer["continued"]; response.Code != http.StatusOK || present || handoff.completed.Continue != nil ||
		handoff.completed.TakeBackWhenDone {
		t.Fatalf("plain complete = %d %v (%+v)", response.Code, answer, handoff.completed)
	}
}

func TestCompleteWithContinueRefusals(t *testing.T) {
	body := `{"epoch":3,"handoff_ref":"refs/elitea/handoff/1","sha":"` + sha1Value + `","continue":{"prompt":"go","mode":"build"}}`
	for _, test := range []struct {
		err    error
		status int
		code   string
		field  string
		value  any
	}{
		{unattended.Refused("sensitive_tools", "github/merge", unattended.RefusedTierRequiresHITL), http.StatusUnprocessableEntity,
			"unattended_grant_refused", "reason", unattended.RefusedTierRequiresHITL},
		{&unattended.ChainLimitError{Limit: unattended.LimitDeadline}, http.StatusConflict, "chain_limit_reached", "limit", "deadline"},
		{unattended.ErrChainEnded, http.StatusConflict, "chain_ended", "", nil},
		{handoffapp.ErrStaleEpoch, http.StatusConflict, "stale_epoch", "", nil},
		{handoffapp.ErrInvalid, http.StatusUnprocessableEntity, "invalid_handoff", "", nil},
		{handoffapp.ErrUnavailable, http.StatusServiceUnavailable, "handoff_unavailable", "", nil},
	} {
		h := &handler{handoff: &fakeHandoff{err: test.err}, store: &fakeStore{}, conversations: fakeConversations{}}
		response := serve(t, h.complete, nativeUser(), []string{SendPermission}, http.MethodPost, CompletePath, completeURL, body)
		answer := decode(t, response)
		if response.Code != test.status || answer["error"] != test.code || (test.field != "" && answer[test.field] != test.value) {
			t.Errorf("%v = %d %v, want %d %s", test.err, response.Code, answer, test.status, test.code)
		}
	}
}

func TestStateAnswersTakeBackWhenDoneAndTheLastCloudRun(t *testing.T) {
	finished := time.Date(2026, 11, 4, 10, 15, 2, 0, time.UTC)
	for _, run := range []*handoffapp.CloudRun{nil, {Outcome: handoffapp.CloudRunLimitReached, ExecutionID: "e1", FinishedAt: finished}} {
		h := &handler{handoff: &fakeHandoff{view: handoffapp.View{Present: true, LastCloudRun: run, Handoff: handoffapp.Handoff{
			State: workspacestore.StateCloudIdle, Epoch: 4, TakeBackWhenDone: true, UpdatedAt: time.Unix(0, 0),
		}}}, store: &fakeStore{}, conversations: fakeConversations{}}
		response := serve(t, h.state, &auth.User{ID: "7", UserID: "7", AuthType: "session"}, []string{ReadPermission},
			http.MethodGet, HandoffPath, handoffURL, "")
		answer := decode(t, response)
		value, present := answer["last_cloud_run"]
		if response.Code != http.StatusOK || answer["take_back_when_done"] != true || !present {
			t.Fatalf("state = %d %v", response.Code, answer)
		}
		if run == nil {
			if value != nil {
				t.Fatalf("last_cloud_run with none = %v", value)
			}
			continue
		}
		got, _ := value.(map[string]any)
		if got["outcome"] != "limit_reached" || got["execution_id"] != "e1" || got["finished_at"] != "2026-11-04T10:15:02Z" {
			t.Fatalf("last_cloud_run = %v", got)
		}
	}
}
