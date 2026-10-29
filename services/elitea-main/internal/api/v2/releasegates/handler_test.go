package releasegates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
)

type fakeGates struct {
	put     []releasegate.SuiteConfig
	deleted int
	putErr  error
	gate    releasegate.GateResult
}

func (f *fakeGates) ListSuites(context.Context, int64, int64) ([]releasegate.SuiteConfig, error) {
	return f.put, nil
}
func (f *fakeGates) PutSuite(_ context.Context, _ int64, suite releasegate.SuiteConfig) (releasegate.SuiteConfig, error) {
	if f.putErr != nil {
		return releasegate.SuiteConfig{}, f.putErr
	}
	if err := suite.Validate(); err != nil {
		return releasegate.SuiteConfig{}, err
	}
	f.put = append(f.put, suite)
	return suite, nil
}
func (f *fakeGates) DeleteSuite(context.Context, int64, int64, int64) (bool, error) {
	f.deleted++
	return true, nil
}
func (f *fakeGates) ListGates(context.Context, int64, int64, int) ([]releasegate.GateResult, error) {
	return []releasegate.GateResult{f.gate}, nil
}
func (f *fakeGates) Result(_ context.Context, _, gateID int64) (releasegate.GateResult, error) {
	if gateID != f.gate.ID {
		return releasegate.GateResult{}, releasegate.ErrGateNotFound
	}
	return f.gate, nil
}

func router(gates Gates) http.Handler {
	handler := NewHandler(gates)
	r := chi.NewRouter()
	r.Get(SuitesPath, handler.ListSuites)
	r.Put(SuitePath, handler.PutSuite)
	r.Get(GatesPath, handler.ListGates)
	r.Get(GatePath, handler.GetGate)
	return r
}

func call(t *testing.T, handler http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func TestPutSuite(t *testing.T) {
	gates := &fakeGates{}
	handler := router(gates)

	status, body := call(t, handler, http.MethodPut, "/release_gate_suites/prompt_lib/1/3/4",
		`{"release_gate":true,"regression":true,"criteria":[{"dimension_id":"7","kind":"threshold","min_score":70}],"reevaluate_every_days":30}`)
	if status != http.StatusOK || body["release_gate"] != true || body["dataset_id"] != float64(4) {
		t.Fatalf("put = %d %v", status, body)
	}
	if len(gates.put) != 1 || !gates.put[0].Regression || *gates.put[0].ReevaluateEveryDays != 30 {
		t.Fatalf("stored = %+v", gates.put)
	}

	status, body = call(t, handler, http.MethodPut, "/release_gate_suites/prompt_lib/1/3/4",
		`{"release_gate":true,"criteria":[{"dimension_id":"7","kind":"threshold"}]}`)
	if status != http.StatusBadRequest || body["code"] != "release_gate_invalid_config" {
		t.Fatalf("invalid criterion = %d %v", status, body)
	}

	status, _ = call(t, handler, http.MethodPut, "/release_gate_suites/prompt_lib/1/3/4", `{"release_gate":true,"extra":1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", status)
	}

	status, body = call(t, handler, http.MethodPut, "/release_gate_suites/prompt_lib/1/3/4", `{"release_gate":false}`)
	if status != http.StatusOK || body["release_gate"] != false || gates.deleted != 1 {
		t.Fatalf("unmark = %d %v", status, body)
	}

	gates.putErr = releasegate.ErrRegressionTaken
	status, body = call(t, handler, http.MethodPut, "/release_gate_suites/prompt_lib/1/3/5",
		`{"release_gate":true,"regression":true,"criteria":[{"dimension_id":"7","kind":"no_worse_than","max_drop":5}]}`)
	if status != http.StatusConflict || body["code"] != "release_gate_regression_taken" {
		t.Fatalf("regression taken = %d %v", status, body)
	}
}

func TestGetGate(t *testing.T) {
	gates := &fakeGates{gate: releasegate.GateResult{
		ID: 9, ApplicationID: 3, State: releasegate.StateFailed,
		Failures: []releasegate.GateFailure{{Code: releasegate.FailureRegression, Cases: []releasegate.RegressedCase{{CaseID: "8"}}}},
	}}
	handler := router(gates)

	status, body := call(t, handler, http.MethodGet, "/release_gates/prompt_lib/1/3/9", "")
	if status != http.StatusOK || body["state"] != "failed" || body["blocks"] != true {
		t.Fatalf("get = %d %v", status, body)
	}
	failures := body["failures"].([]any)
	if fmt.Sprint(failures[0].(map[string]any)["cases"]) == "" {
		t.Fatalf("failures = %v", failures)
	}

	// A gate of another agent of the project is not found under this one.
	status, body = call(t, handler, http.MethodGet, "/release_gates/prompt_lib/1/4/9", "")
	if status != http.StatusNotFound || body["code"] != "release_gate_not_found" {
		t.Fatalf("other agent = %d %v", status, body)
	}

	status, body = call(t, handler, http.MethodGet, "/release_gates/prompt_lib/1/3", "")
	if status != http.StatusOK || body["total"] != float64(1) {
		t.Fatalf("list = %d %v", status, body)
	}
}

func TestNoGateAnswers503(t *testing.T) {
	var gate *releasegate.Gate
	status, _ := call(t, router(gate), http.MethodGet, "/release_gates/prompt_lib/1/3", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", status)
	}
}

type fakeGovernor struct{ err error }

func (f fakeGovernor) CheckDirectPublish(context.Context, int64, int64) error { return f.err }

// TestPutSuiteLooseningNeedsGovernance proves the author of an agent whose
// tier needs approval cannot loosen its gate (403 governance_role_required),
// may tighten it, and that a governance role holder may loosen it.
func TestPutSuiteLooseningNeedsGovernance(t *testing.T) {
	governed := fakeGovernor{err: agentlifecycle.Refuse(agentlifecycle.ChangeRequiredCode, "needs approval")}
	marked := func() *fakeGates {
		min := 70.0
		return &fakeGates{put: []releasegate.SuiteConfig{{ApplicationID: 3, DatasetID: 4, Regression: true,
			Criteria: []releasegate.Criterion{{DimensionID: "7", Kind: releasegate.CriterionThreshold, MinScore: &min}}}}}
	}
	serveWith := func(gates *fakeGates, governor Governor, governance bool) http.Handler {
		handler := NewHandler(gates, WithGovernance(governor, func(*http.Request) (bool, error) { return governance, nil }))
		r := chi.NewRouter()
		r.Put(SuitePath, handler.PutSuite)
		return r
	}
	const path = "/release_gate_suites/prompt_lib/1/3/4"
	for _, c := range []struct {
		name       string
		body       string
		governor   fakeGovernor
		governance bool
		want       int
	}{
		{"unmark", `{"release_gate":false}`, governed, false, http.StatusForbidden},
		{"unset regression", `{"release_gate":true,"regression":false,"criteria":[{"dimension_id":"7","kind":"threshold","min_score":70}]}`, governed, false, http.StatusForbidden},
		{"lower min_score", `{"release_gate":true,"regression":true,"criteria":[{"dimension_id":"7","kind":"threshold","min_score":50}]}`, governed, false, http.StatusForbidden},
		{"tighten", `{"release_gate":true,"regression":true,"criteria":[{"dimension_id":"7","kind":"threshold","min_score":90}]}`, governed, false, http.StatusOK},
		{"unmark by governance", `{"release_gate":false}`, governed, true, http.StatusOK},
		{"unmark at low tier", `{"release_gate":false}`, fakeGovernor{}, false, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			gates := marked()
			status, body := call(t, serveWith(gates, c.governor, c.governance), http.MethodPut, path, c.body)
			if status != c.want {
				t.Fatalf("status %d, want %d: %v", status, c.want, body)
			}
			if c.want == http.StatusForbidden {
				if body["code"] != agentlifecycle.GovernanceRequiredCode || gates.deleted != 0 || len(gates.put) != 1 {
					t.Fatalf("refused edit changed the gate or answered %v", body)
				}
			}
		})
	}
}
