package llmproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// This file holds ONE test, and it is the only place the gateway's budget
// refusal is read the way elitea-sdk reads it.
//
// Every other budget test asserts the two wire fields directly. That proves the
// gateway writes what this repository decided to write. It does not prove the
// SDK can act on it, and for four months it could not: the member refusal put
// the scope in error.type, the SDK matches error.type against "budget_exceeded"
// alone, and so `budget_exceeded_from` returned None for every member-cap
// refusal. Nothing failed. The gateway wrote a correct-looking 402, the SDK
// raised no typed exception, and the refusal reached the model as content.
//
// So this test does not assert field values. It ports the SDK's reader and asks
// it the question the SDK asks: is this a budget refusal, and whose budget?
//
// Keep sdkBudgetScopeFrom a TRANSCRIPTION of
// elitea-sdk/elitea_sdk/runtime/exceptions.py::budget_exceeded_from. When that
// function changes, change this one, and let the test say what broke.

// sdkBudgetScopes mirrors elitea-sdk's BUDGET_SCOPES tuple.
var sdkBudgetScopes = map[string]bool{
	"project_budget_exceeded": true,
	"member_budget_exceeded":  true,
}

// sdkBudgetScopeFrom is the Go transcription of the SDK's reader. It returns
// the scope the SDK would attach to its BudgetExceededError, or "" when the SDK
// would NOT classify the body as a budget refusal at all.
//
// The two SDK behaviours that matter, both reproduced here:
//
//   - the match is on error.type and on nothing else. The SDK's dict branch
//     returns None when the type does not match; it does not fall through to
//     the message-text path;
//   - an unrecognised error.code resolves to DEFAULT_BUDGET_SCOPE, which is the
//     project scope.
func sdkBudgetScopeFrom(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode refusal body %q: %v", body, err)
	}
	if payload.Error == nil {
		t.Fatalf("refusal body %q has no error object; the SDK reads body[\"error\"]", body)
	}
	if payload.Error.Type != "budget_exceeded" {
		return ""
	}
	if sdkBudgetScopes[payload.Error.Code] {
		return payload.Error.Code
	}
	return "project_budget_exceeded" // DEFAULT_BUDGET_SCOPE
}

// TestBudgetRefusalMatchesSDKContract asserts that each of the gateway's two
// budget refusals reaches the SDK as a budget refusal, carrying the scope that
// names the ceiling that actually blocked the request.
//
// The project case is the one to read carefully. Its code is
// "insufficient_quota", not "project_budget_exceeded": spec §2.5 and the
// cutover gate both require the OpenAI canonical code there, and the SDK
// resolves an unrecognised code to the project scope, which is the right
// answer. That is a RELIANCE on a default, so it is pinned here rather than
// left to be rediscovered.
func TestBudgetRefusalMatchesSDKContract(t *testing.T) {
	cases := []struct {
		name           string
		projectVerdict failmode.Decision
		memberVerdict  failmode.Decision
		wantScope      string
	}{
		{
			name:           "project ceiling",
			projectVerdict: block402(),
			memberVerdict:  allow(),
			wantScope:      "project_budget_exceeded",
		},
		{
			name:           "member ceiling",
			projectVerdict: allow(),
			memberVerdict:  block402(),
			wantScope:      "member_budget_exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := newScopedChecker(tc.projectVerdict, tc.memberVerdict)
			router := &trackingRouter{}
			router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
			h := newHandlerWithScopedGate(router, gate, 500_000)

			rec := httptest.NewRecorder()
			h.Chat(rec, memberChatRequest(t, "42", "7"))

			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402; body %s", rec.Code, rec.Body.String())
			}
			if router.called.Load() {
				t.Fatal("the provider was called for a refused request")
			}

			scope := sdkBudgetScopeFrom(t, rec.Body.Bytes())
			if scope == "" {
				t.Fatalf("the SDK would not classify this as a budget refusal at all; body %s",
					rec.Body.String())
			}
			if scope != tc.wantScope {
				t.Fatalf("SDK scope = %q, want %q; body %s",
					scope, tc.wantScope, rec.Body.String())
			}
		})
	}
}
