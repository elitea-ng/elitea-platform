package unattendedwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

func TestGrantDecodesEmbedded(t *testing.T) {
	var body struct {
		Grant
		Extra bool `json:"extra"`
	}
	if err := json.Unmarshal([]byte(`{"grant_id":"g","continue_step_limit":true,"hook_asks":true,"extra":true,
		"chain":{"max_continuations":3,"max_spend":1.5,"deadline_seconds":600}}`), &body); err != nil {
		t.Fatal(err)
	}
	request := body.Request()
	if !body.Extra || request.GrantID != "g" || !request.ContinueStepLimit || !request.HookAsks ||
		*request.Chain.MaxContinuations != 3 || *request.Chain.MaxSpendUSD != 1.5 || *request.Chain.DeadlineSeconds != 600 {
		t.Fatalf("request = %+v", request)
	}
	if (Grant{}).Request().Chain != (unattended.ChainRequest{}) {
		t.Fatal("an absent chain is not all defaults")
	}
}

func TestWriteRefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := fmt.Errorf("wrapped: %w", unattended.Refused("chain.max_spend", "", unattended.RefusedOverMaximum))
	if !WriteRefusal(recorder, err) {
		t.Fatal("a refusal was not answered")
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || body["error"] != "unattended_grant_refused" ||
		body["reason"] != "over_maximum" || body["field"] != "chain.max_spend" || body["tool"] != nil {
		t.Fatalf("answer = %d %v", recorder.Code, body)
	}
	if WriteRefusal(httptest.NewRecorder(), errors.New("other")) {
		t.Fatal("another error was answered")
	}
}
