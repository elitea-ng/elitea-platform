package llmproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// TestBudgetRefusalScopeFieldNamesOnlyTheGate pins error.scope (#6732).
//
// A provider's own quota refusal and the gate's project refusal share
// type=budget_exceeded and code=insufficient_quota. The workers name the
// refusing budget from error.scope only, so the gate must set it and a
// provider-origin refusal must not.
func TestBudgetRefusalScopeFieldNamesOnlyTheGate(t *testing.T) {
	type refusal struct {
		Error struct {
			Type  string  `json:"type"`
			Code  string  `json:"code"`
			Scope *string `json:"scope"`
		} `json:"error"`
	}
	cases := []struct {
		name           string
		projectVerdict failmode.Decision
		memberVerdict  failmode.Decision
		wantCode       string
		wantScope      string
	}{
		{"project ceiling", block402(), allow(), "insufficient_quota", "project"},
		{"member ceiling", allow(), block402(), "member_budget_exceeded", "member"},
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
			var got refusal
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if got.Error.Type != "budget_exceeded" || got.Error.Code != tc.wantCode {
				t.Fatalf("type/code = %q/%q, want budget_exceeded/%q", got.Error.Type, got.Error.Code, tc.wantCode)
			}
			if got.Error.Scope == nil || *got.Error.Scope != tc.wantScope {
				t.Fatalf("error.scope = %v, want %q; body %s", got.Error.Scope, tc.wantScope, rec.Body.String())
			}
		})
	}

	t.Run("provider quota refusal carries no scope", func(t *testing.T) {
		raw, err := json.Marshal(openAIErrorBody(bErr(http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota",
			"You exceeded your current quota, please check your plan and billing details.")))
		if err != nil {
			t.Fatal(err)
		}
		var got refusal
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Type != "budget_exceeded" || got.Error.Code != "insufficient_quota" {
			t.Fatalf("type/code = %q/%q, want the shared budget contract", got.Error.Type, got.Error.Code)
		}
		if got.Error.Scope != nil {
			t.Fatalf("a provider refusal carries error.scope=%q; body %s", *got.Error.Scope, raw)
		}
	})

	t.Run("realtime refusal frame carries the gate scope", func(t *testing.T) {
		frame := realtimeRefusalFrameScoped("budget_exceeded", "member_budget_exceeded", "m", "member")
		var got struct {
			Error struct {
				Scope string `json:"scope"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame, &got); err != nil || got.Error.Scope != "member" {
			t.Fatalf("frame %s: scope %q err %v", frame, got.Error.Scope, err)
		}
	})
}
