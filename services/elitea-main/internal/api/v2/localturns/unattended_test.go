package localturns

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

func TestUnattendedErrorsKeepTheContractShape(t *testing.T) {
	cases := []struct {
		err    error
		status int
		want   map[string]any
	}{
		{fmt.Errorf("wrapped: %w", &unattended.ChainLimitError{Limit: unattended.LimitSpend}), http.StatusConflict,
			map[string]any{"error": "chain_limit_reached", "limit": "spend"}},
		{unattended.ErrChainEnded, http.StatusConflict, map[string]any{"error": "chain_ended"}},
		{localturn.ErrConfirmationGrantRefused, http.StatusForbidden, map[string]any{"error": "confirmation_grant_refused"}},
		{unattended.Refused("sensitive_tools", "", unattended.RefusedInvalid), http.StatusUnprocessableEntity,
			map[string]any{"error": "unattended_grant_refused", "reason": "invalid", "field": "sensitive_tools"}},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		if !writeUnattendedError(recorder, tc.err) {
			t.Fatalf("%v was not answered", tc.err)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != tc.status {
			t.Fatalf("%v: status %d, want %d", tc.err, recorder.Code, tc.status)
		}
		for key, value := range tc.want {
			if body[key] != value {
				t.Fatalf("%v: body %v, want %s=%v", tc.err, body, key, value)
			}
		}
	}
	if writeUnattendedError(httptest.NewRecorder(), localturn.ErrInvalid) {
		t.Fatal("an ordinary error was answered as an unattended one")
	}
}

func TestUnattendedStartBodyAndAnswer(t *testing.T) {
	var body startBody
	if err := json.Unmarshal([]byte(`{"question_id":"q","user_input":"u","unattended":{
		"grant_id":"0b7d1f6a-3c2e-4f8a-9d10-2a3b4c5d6e7f","chain_root_execution_id":"0123456789abcdef0123456789abcdef",
		"continue_step_limit":true,"continue_tool_call_cap":true,"hook_asks":true,"remote_confirmations":true,
		"browser_submits":["shop.example.com"],
		"chain":{"max_continuations":3,"max_spend":1.5,"deadline_seconds":600}}}`), &body); err != nil {
		t.Fatal(err)
	}
	request := body.Unattended.request()
	if request == nil || len(request.BrowserSubmits) != 1 || request.BrowserSubmits[0] != "shop.example.com" {
		t.Fatalf("browser_submits = %+v", request)
	}
	if !request.ContinueStepLimit || !request.ContinueToolCallCap || !request.HookAsks ||
		!request.RemoteConfirmations || *request.Chain.MaxContinuations != 3 || *request.Chain.MaxSpendUSD != 1.5 ||
		*request.Chain.DeadlineSeconds != 600 || request.ChainRootExecutionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("request = %+v", request)
	}
	var none *unattendedBody
	if none.request() != nil {
		t.Fatal("an absent body made a grant")
	}
	answer := unattendedStarted(localturn.UnattendedStarted{
		GrantID: "g", ChainRootExecutionID: "r", ContinuationIndex: 2,
		Limits: unattended.Limits{MaxContinuations: 3, MaxSpendUSD: 1.5, Deadline: 10 * time.Minute},
	})
	chain, _ := answer["chain"].(map[string]any)
	if answer["continuation_index"] != 2 || chain["deadline_seconds"] != int64(600) || chain["max_spend"] != 1.5 {
		t.Fatalf("answer = %v", answer)
	}
}
