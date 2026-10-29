package agentlifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// fakeStore answers the calls these tests make; the embedded interface
// panics on any other.
type fakeStore struct {
	Store
	approval   repos.Approval
	approveErr error
	rows       []agentlifecycle.Registry
	filter     agentlifecycle.RegistryFilter
}

func (f *fakeStore) ApproveChangeRequest(_ context.Context, approval repos.Approval) (repos.ApprovalOutcome, error) {
	f.approval = approval
	if f.approveErr != nil {
		return repos.ApprovalOutcome{}, f.approveErr
	}
	return repos.ApprovalOutcome{Change: agentlifecycle.ChangeRequest{ID: approval.ChangeID}, Complete: true}, nil
}

func (f *fakeStore) ListRegistry(_ context.Context, filter agentlifecycle.RegistryFilter) ([]agentlifecycle.Registry, int, error) {
	f.filter = filter
	return f.rows, len(f.rows), nil
}

func serve(t *testing.T, method, pattern, target, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Method(method, pattern, handler)
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "8", UserID: "8"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestApproveResolvesGovernanceAndAnswersRefusals(t *testing.T) {
	store := &fakeStore{}
	handler := NewHandler(store, func(*http.Request) (bool, error) { return true, nil })
	recorder := serve(t, http.MethodPost, "/api"+ApprovePath, "/api/change_requests/prompt_lib/1/31/5/approve",
		`{"note":"ok"}`, handler.ApproveChangeRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if store.approval.ApproverID != 8 || !store.approval.Governance || store.approval.ChangeID != 5 || store.approval.Note != "ok" {
		t.Fatalf("approval = %+v", store.approval)
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{agentlifecycle.Refuse(agentlifecycle.SelfApprovalCode, "x"), http.StatusForbidden, agentlifecycle.SelfApprovalCode},
		{agentlifecycle.RefuseWith(agentlifecycle.GateFailedCode, "x", map[string]any{"failures": []string{"gate_regression"}}),
			http.StatusConflict, agentlifecycle.GateFailedCode},
		{&agentlifecycle.PublishRefusal{Status: http.StatusConflict, Body: map[string]any{"code": "version_name_taken"}},
			http.StatusConflict, "version_name_taken"},
	} {
		store.approveErr = tc.err
		recorder = serve(t, http.MethodPost, "/api"+ApprovePath, "/api/change_requests/prompt_lib/1/31/5/approve", "",
			handler.ApproveChangeRequest)
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != tc.status || body["code"] != tc.code {
			t.Fatalf("%v: status %d body %s", tc.err, recorder.Code, recorder.Body)
		}
	}

	// A cause that is not a refusal is never echoed.
	store.approveErr = errors.New("pq: secret detail")
	recorder = serve(t, http.MethodPost, "/api"+ApprovePath, "/api/change_requests/prompt_lib/1/31/5/approve", "",
		handler.ApproveChangeRequest)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
}

func TestApproveRefusesUnknownFields(t *testing.T) {
	handler := NewHandler(&fakeStore{}, nil)
	recorder := serve(t, http.MethodPost, "/api"+ApprovePath, "/api/change_requests/prompt_lib/1/31/5/approve",
		`{"governance":true}`, handler.ApproveChangeRequest)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d", recorder.Code)
	}
}

func TestAdminRegistryCSVDefusesFormulas(t *testing.T) {
	store := &fakeStore{rows: []agentlifecycle.Registry{{
		ProjectID: 1, ApplicationID: 31, ApplicationName: "=HYPERLINK(\"x\")", Tier: agentlifecycle.TierHigh,
		State: agentlifecycle.StateCertified, DataClasses: []string{"customer_pii", "finance"},
	}}}
	handler := NewHandler(store, nil)
	recorder := serve(t, http.MethodGet, "/admin"+AdminRegistryPath, "/admin/agent_registry?format=csv&tier=high", "",
		handler.AdminRegistry)
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("status %d type %s", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	if !strings.Contains(recorder.Body.String(), `'=HYPERLINK`) || !strings.Contains(recorder.Body.String(), "customer_pii;finance") {
		t.Fatalf("csv = %s", recorder.Body)
	}
	if store.filter.Tier != agentlifecycle.TierHigh || store.filter.Limit != agentlifecycle.MaxRegistryExportRows {
		t.Fatalf("filter = %+v", store.filter)
	}

	recorder = serve(t, http.MethodGet, "/admin"+AdminRegistryPath, "/admin/agent_registry?tier=extreme", "", handler.AdminRegistry)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d", recorder.Code)
	}
}
