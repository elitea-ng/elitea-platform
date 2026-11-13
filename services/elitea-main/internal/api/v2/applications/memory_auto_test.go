package applications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

type memoryAutoGateStub struct {
	limit agentlifecycle.Tier
	tier  agentlifecycle.Tier
}

func (s memoryAutoGateStub) MaxTier(context.Context) (agentlifecycle.Tier, error) {
	return s.limit, nil
}

func (s memoryAutoGateStub) AgentTier(context.Context, int64, int64) (agentlifecycle.Tier, error) {
	return s.tier, nil
}

func memoryBody(write string) map[string]any {
	return map[string]any{"meta": map[string]any{"memory": map[string]any{"enabled": true, "write": write}}}
}

// Issue 1660: write auto is refused for an agent above the configured tier
// and accepted at or below it; the attended modes are never refused.
func TestMemoryAutoIsLimitedToTheAgentsRiskTier(t *testing.T) {
	cases := []struct {
		name    string
		tier    agentlifecycle.Tier
		write   string
		app     string
		refused bool
	}{
		{"high tier, auto", agentlifecycle.TierHigh, "auto", "9", true},
		{"critical tier, auto", agentlifecycle.TierCritical, "auto", "9", true},
		{"medium tier, auto", agentlifecycle.TierMedium, "auto", "9", false},
		{"low tier, auto", agentlifecycle.TierLow, "auto", "9", false},
		{"high tier, attended", agentlifecycle.TierHigh, "auto_attended", "9", false},
		{"high tier, review", agentlifecycle.TierHigh, "review", "9", false},
		{"a create is low, auto", agentlifecycle.TierCritical, "auto", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := (&Handler{}).WithMemoryAutoGate(memoryAutoGateStub{limit: agentlifecycle.TierMedium, tier: tc.tier})
			rec := httptest.NewRecorder()
			ok := h.checkMemoryAuto(rec, httptest.NewRequest(http.MethodPut, "/", nil), "7", tc.app, memoryBody(tc.write))
			if ok == tc.refused {
				t.Fatalf("ok = %v, want %v", ok, !tc.refused)
			}
			if !tc.refused {
				return
			}
			var body struct {
				Code    string `json:"code"`
				Tier    string `json:"tier"`
				MaxTier string `json:"max_tier"`
			}
			if rec.Code != http.StatusUnprocessableEntity ||
				json.Unmarshal(rec.Body.Bytes(), &body) != nil ||
				body.Code != CodeMemoryAutoAboveTier || body.Tier != string(tc.tier) || body.MaxTier != "medium" {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMemoryAutoWithoutAGateAcceptsAnyTier(t *testing.T) {
	rec := httptest.NewRecorder()
	if !(&Handler{}).checkMemoryAuto(rec, httptest.NewRequest(http.MethodPut, "/", nil), "7", "9", memoryBody("auto")) {
		t.Fatalf("a handler without a gate refused: %s", rec.Body.String())
	}
}
