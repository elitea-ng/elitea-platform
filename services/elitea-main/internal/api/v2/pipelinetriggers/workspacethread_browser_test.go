package pipelinetriggers

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// ADR-0049 decision 7: a thread target's grant keeps the browser submit
// hosts it was saved with, and a stored grant gives them to each
// occurrence's start.
func TestThreadGrantKeepsBrowserSubmits(t *testing.T) {
	requested, err := parseThreadTarget([]byte(`{"target_kind":"workspace_thread",
		"conversation_uuid":"0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60","prompt":"go",
		"grant":{"continue_step_limit":true,"browser_submits":["forms.example.com","forms.example.com"]}}`))
	if err != nil || requested.grant == nil {
		t.Fatalf("requested=%+v err=%v", requested, err)
	}
	if err := unattended.ValidateBrowserSubmits(requested.grant.BrowserSubmits); err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(threadGrantBody{ContinueStepLimit: true, GoalCheck: true,
		BrowserSubmits: unattended.NormalizeBrowserSubmits(requested.grant.BrowserSubmits)})
	if err != nil {
		t.Fatal(err)
	}
	request := storedGrant(stored).request()
	if len(request.BrowserSubmits) != 1 || request.BrowserSubmits[0] != "forms.example.com" {
		t.Fatalf("request=%+v", request)
	}
	// A grant stored before the field has none.
	if got := storedGrant([]byte(`{"continue_step_limit":true}`)).request().BrowserSubmits; len(got) != 0 {
		t.Fatalf("browser_submits=%v", got)
	}
}

func TestAdmitThreadGrantRefusesABadBrowserSubmitHost(t *testing.T) {
	_, err := (&Handler{}).admitThreadGrant(t.Context(), &threadGrantWire{BrowserSubmits: []string{"https://example.com"}})
	refusal, ok := unattended.AsRefusal(err)
	if !ok || refusal.Field != "browser_submits" {
		t.Fatalf("err=%v", err)
	}
}
