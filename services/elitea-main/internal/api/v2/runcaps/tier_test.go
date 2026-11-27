package runcaps_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/runcaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeTierCaps struct {
	project, application int64
	err                  error
}

func (f *fakeTierCaps) TierCaps(_ context.Context, projectID, applicationID int64) (agentlifecycle.Tier, platformconfig.TierRunCaps, error) {
	f.project, f.application = projectID, applicationID
	if f.err != nil {
		return "", platformconfig.TierRunCaps{}, f.err
	}
	tier := agentlifecycle.TierLow
	if applicationID == 42 {
		tier = agentlifecycle.TierHigh
	}
	return tier, platformconfig.TierRunCaps{MaxToolCalls: 200, MaxModelTurns: 50, MaxRecordsRead: 100000, MaxSpend: 5}, nil
}

func serveTierCaps(handler *runcaps.TierCapsHandler, path string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Get("/elitea_core"+runcaps.TierCapsPath, handler.Read)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestTierCapsReadAnswersTheTierAndEveryCap(t *testing.T) {
	reader := &fakeTierCaps{}
	recorder := serveTierCaps(runcaps.NewTierCapsHandler(reader), "/elitea_core/run_caps/prompt_lib/7/42")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["tier"] != "high" || reader.project != 7 || reader.application != 42 {
		t.Errorf("body = %v, reader saw %d/%d", body, reader.project, reader.application)
	}
	caps, _ := body["caps"].(map[string]any)
	for name, want := range map[string]float64{"max_tool_calls": 200, "max_model_turns": 50, "max_records_read": 100000, "max_spend": 5} {
		if caps[name] != want {
			t.Errorf("caps.%s = %v, want %v", name, caps[name], want)
		}
	}
}

func TestTierCapsReadOfANewAgentAsksForApplicationZero(t *testing.T) {
	reader := &fakeTierCaps{}
	recorder := serveTierCaps(runcaps.NewTierCapsHandler(reader), "/elitea_core/run_caps/prompt_lib/7/0")
	if recorder.Code != http.StatusOK || reader.application != 0 {
		t.Fatalf("status = %d, application = %d; want 200 and 0", recorder.Code, reader.application)
	}
}

func TestTierCapsReadRefusesABadIDAndReportsAFailedRead(t *testing.T) {
	for path, want := range map[string]int{
		"/elitea_core/run_caps/prompt_lib/x/1":  http.StatusBadRequest,
		"/elitea_core/run_caps/prompt_lib/7/-1": http.StatusBadRequest,
	} {
		if got := serveTierCaps(runcaps.NewTierCapsHandler(&fakeTierCaps{}), path).Code; got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	failing := &fakeTierCaps{err: errors.New("down")}
	if got := serveTierCaps(runcaps.NewTierCapsHandler(failing), "/elitea_core/run_caps/prompt_lib/7/1").Code; got != http.StatusInternalServerError {
		t.Errorf("failed read = %d, want 500", got)
	}
	if got := serveTierCaps(runcaps.NewTierCapsHandler(nil), "/elitea_core/run_caps/prompt_lib/7/1").Code; got != http.StatusServiceUnavailable {
		t.Errorf("no reader = %d, want 503", got)
	}
}
