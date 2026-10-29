package workeregress

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
)

type storeStub struct {
	queries []app.HostsQuery
	report  app.HostsReport
	err     error
}

func (s *storeStub) Hosts(_ context.Context, query app.HostsQuery) (app.HostsReport, error) {
	s.queries = append(s.queries, query)
	return s.report, s.err
}

var fixedNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func serve(t *testing.T, handler *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.Hosts(recorder, httptest.NewRequest(http.MethodGet, "/api/v2"+HostsPath+target, nil))
	return recorder
}

func TestHostsDefaultsAndShape(t *testing.T) {
	agent := int64(7)
	seen := time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)
	store := &storeStub{report: app.HostsReport{
		Reached:          []app.ReachedHost{{ProjectID: 3, AgentID: &agent, Host: "api.github.com", Port: 443, Connections: 12, Executions: 4, LastSeen: seen}},
		Refused:          []app.RefusedHost{{ProjectID: 3, Host: "evil.example", Port: 443, Reason: "egress_host_not_allowed", Count: 2, Executions: 1, LastSeen: seen}},
		RefusedTruncated: true,
	}}
	handler := NewHandler(store)
	handler.now = func() time.Time { return fixedNow }
	response := serve(t, handler, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
	want := app.HostsQuery{Since: fixedNow.Add(-30 * 24 * time.Hour), Source: app.SourceWorker, Limit: app.MaxHostsRows}
	if !reflect.DeepEqual(store.queries, []app.HostsQuery{want}) {
		t.Fatalf("queries=%+v", store.queries)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantBody := map[string]any{
		"days": float64(30), "since": "2026-09-28T12:00:00Z", "source": "worker", "project_id": nil,
		"reached": []any{map[string]any{"project_id": float64(3), "agent_id": float64(7), "host": "api.github.com", "port": float64(443),
			"connections": float64(12), "executions": float64(4), "last_seen": "2026-10-27T09:30:00Z"}},
		"refused": []any{map[string]any{"project_id": float64(3), "agent_id": nil, "host": "evil.example", "port": float64(443),
			"reason": "egress_host_not_allowed", "count": float64(2), "executions": float64(1), "last_seen": "2026-10-27T09:30:00Z"}},
		"reached_truncated": false, "refused_truncated": true,
	}
	if !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("body=%v", body)
	}
}

func TestHostsParameters(t *testing.T) {
	store := &storeStub{}
	handler := NewHandler(store)
	handler.now = func() time.Time { return fixedNow }
	response := serve(t, handler, "?days=90&source=all&project_id=42")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	want := app.HostsQuery{Since: fixedNow.Add(-90 * 24 * time.Hour), Source: app.SourceAll, ProjectID: 42, Limit: app.MaxHostsRows}
	if !reflect.DeepEqual(store.queries, []app.HostsQuery{want}) {
		t.Fatalf("queries=%+v", store.queries)
	}
	var body struct {
		ProjectID *int64 `json:"project_id"`
		Reached   []any  `json:"reached"`
		Refused   []any  `json:"refused"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ProjectID == nil || *body.ProjectID != 42 || body.Reached == nil || body.Refused == nil {
		t.Fatalf("body=%s", response.Body)
	}
}

func TestHostsRefusals(t *testing.T) {
	for _, target := range []string{
		"?days=0", "?days=91", "?days=x", "?days=1.5",
		"?source=sandbox", "?project_id=0", "?project_id=-3", "?project_id=a",
	} {
		store := &storeStub{}
		response := serve(t, NewHandler(store), target)
		if response.Code != http.StatusBadRequest || len(store.queries) != 0 {
			t.Fatalf("%s: status=%d queries=%d", target, response.Code, len(store.queries))
		}
	}
	if response := serve(t, NewHandler(nil), ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: status=%d", response.Code)
	}
	if response := serve(t, NewHandler(&storeStub{err: errors.New("database down")}), ""); response.Code != http.StatusInternalServerError {
		t.Fatalf("store error: status=%d", response.Code)
	}
}
