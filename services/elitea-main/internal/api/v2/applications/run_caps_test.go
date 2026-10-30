package applications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type runCapsSettingsStub struct{ caps platformconfig.RunCaps }

func (s runCapsSettingsStub) Get(context.Context) (platformconfig.RunCaps, error) { return s.caps, nil }

type runCapsTierStub string

func (s runCapsTierStub) AgentRiskTier(context.Context, int64, int64) (string, error) {
	return string(s), nil
}

// ADR-0041 decision 3: an author may lower a run cap, never raise it; the
// save answers a typed 422 that names the cap, the tier and both values.
func TestVersionSaveRefusesARunCapAboveTheTier(t *testing.T) {
	t.Parallel()
	checker, err := workerruncaps.NewChecker(runCapsSettingsStub{caps: platformconfig.RunCapsFromValues(
		platformconfig.Values{"high_max_tool_calls": float64(80)})}, runCapsTierStub("high"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil).WithRunCapsChecker(checker)
	request := httptest.NewRequest(http.MethodPut, "/", nil)

	recorder := httptest.NewRecorder()
	if h.checkRunCaps(recorder, request, "17", "5", map[string]any{
		"meta": map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(81)}},
	}) {
		t.Fatal("a raise was accepted")
	}
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != workerruncaps.CodeCapAboveTier || body["cap"] != "max_tool_calls" ||
		body["tier"] != "high" || body["tier_cap"] != float64(80) || body["requested"] != float64(81) || body["error"] == "" {
		t.Fatalf("body = %v", body)
	}

	recorder = httptest.NewRecorder()
	if !h.checkRunCaps(recorder, request, "17", "5", map[string]any{
		"meta": map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(10)}},
	}) {
		t.Fatalf("a lower cap was refused: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	if h.checkRunCaps(recorder, request, "17", "5", map[string]any{
		"meta": map[string]any{"run_caps": map[string]any{"max_spend": "lots"}},
	}) || recorder.Code != http.StatusUnprocessableEntity {
		t.Fatal("an invalid cap was accepted")
	}
	// No checker (a composition without the section) accepts.
	if !NewHandler(nil).checkRunCaps(httptest.NewRecorder(), request, "17", "5", map[string]any{
		"meta": map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(1e9)}},
	}) {
		t.Fatal("a handler without a checker refused")
	}
}
