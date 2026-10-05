package eliteacore

// The admin audit routes refuse a reversed date window (legacy issue 6738), with
// the same code the analytics routes use. The handler has no pool here: the
// window is checked before any read, so a nil pool proves the database is never
// reached.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestAuditRoutesAnswerInvalidDateRange(t *testing.T) {
	handler := NewHandler(nil)
	router := chi.NewRouter()
	router.Get("/elitea_core/audit/{mode}", handler.AuditTrail)
	router.Get("/elitea_core/audit_traces/{mode}", handler.AuditTraces)
	router.Get("/elitea_core/audit_heatmap/{mode}", handler.AuditHeatmap)
	router.Get("/elitea_core/audit_trace_heatmap/{mode}", handler.AuditTraceHeatmap)
	router.Get("/elitea_core/project_user_activity/{mode}", handler.ProjectUserActivity)

	const reversed = "date_from=2026-09-26T19:10:00.000Z&date_to=2026-09-24T19:07:00.000Z"
	for _, target := range []string{
		"/elitea_core/audit/administration?" + reversed,
		"/elitea_core/audit_traces/administration?" + reversed,
		"/elitea_core/audit_heatmap/administration?" + reversed,
		"/elitea_core/audit_trace_heatmap/administration?" + reversed,
		"/elitea_core/project_user_activity/administration?project_id=3&" + reversed,
	} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", recorder.Body.String())
			}
			if body["code"] != "invalid_date_range" {
				t.Fatalf("code = %v, want invalid_date_range", body["code"])
			}
		})
	}
}

func TestReversedDateRangeOnlyFlagsAReversedPair(t *testing.T) {
	early := optionalTime("2026-09-24T19:07:00Z")
	late := optionalTime("2026-09-26T19:10:00Z")
	switch {
	case !reversedDateRange(late, early):
		t.Error("a later start is not flagged")
	case reversedDateRange(early, late):
		t.Error("an ordered window is flagged")
	case reversedDateRange(early, early):
		t.Error("an equal start and end is flagged; it is an empty window, not an invalid one")
	case reversedDateRange(nil, early), reversedDateRange(late, nil):
		t.Error("an open window is flagged")
	}
}
