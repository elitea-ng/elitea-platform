package credentialgrants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// memoryStore is a credentialgrant.Store over a map, so the handler runs the
// real service and its two-person rule.
type memoryStore struct {
	grants map[string]credentialgrant.CredentialUseGrant
}

func (m *memoryStore) CreateGrant(_ context.Context, g credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
	m.grants[g.ID] = g
	return g, nil
}

func (m *memoryStore) ListGrants(_ context.Context, f credentialgrant.ListFilter) ([]credentialgrant.CredentialUseGrant, error) {
	var out []credentialgrant.CredentialUseGrant
	for _, g := range m.grants {
		if g.TenantProjectID == f.TenantProjectID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *memoryStore) TransitionGrant(_ context.Context, tenant int64, id string, apply func(credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error)) (credentialgrant.CredentialUseGrant, error) {
	g, ok := m.grants[id]
	if !ok || g.TenantProjectID != tenant {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
	}
	next, err := apply(g)
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	m.grants[id] = next
	return next, nil
}

func newRouter(t *testing.T) chi.Router {
	t.Helper()
	service, err := credentialgrant.NewService(credentialgrant.ServiceConfig{
		Store: &memoryStore{grants: map[string]credentialgrant.CredentialUseGrant{}},
		Now:   func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(service)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Get("/projects/{projectID}/credential_grants", h.List)
	r.Post("/projects/{projectID}/credential_grants", h.Propose)
	r.Post("/projects/{projectID}/credential_grants/{id}:approve", h.Approve)
	r.Post("/projects/{projectID}/credential_grants/{id}:suspend", h.Suspend)
	r.Post("/projects/{projectID}/credential_grants/{id}:revoke", h.Revoke)
	return r
}

func call(t *testing.T, r http.Handler, method, path, userID, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if userID != "" {
		req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: userID, UserID: userID}))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

const proposal = `{"agent_binding_id":"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f01","toolkit_id":11,
"credential_reference_id":"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f03","target_binding_id":"0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f04",
"capabilities":["use"],"operation_scopes":["jira.issue.read"]}`

func TestProposeApproveAndTwoPersonRule(t *testing.T) {
	r := newRouter(t)
	status, created := call(t, r, http.MethodPost, "/projects/7/credential_grants", "100", proposal)
	if status != http.StatusCreated || created["state"] != "proposed" || created["tenant_project_id"] != float64(7) {
		t.Fatalf("propose = %d %v", status, created)
	}
	id := created["id"].(string)

	if status, _ := call(t, r, http.MethodPost, "/projects/7/credential_grants/"+id+":approve", "100", ""); status != http.StatusForbidden {
		t.Fatalf("approval by the proposer = %d, want 403", status)
	}
	if status, _ := call(t, r, http.MethodPost, "/projects/8/credential_grants/"+id+":approve", "200", ""); status != http.StatusNotFound {
		t.Fatalf("approval through another project = %d, want 404", status)
	}
	status, approved := call(t, r, http.MethodPost, "/projects/7/credential_grants/"+id+":approve", "200", "")
	if status != http.StatusOK || approved["state"] != "active" || approved["approved_by"] != "200" || approved["epoch"] != float64(1) {
		t.Fatalf("approve = %d %v", status, approved)
	}
	if status, _ := call(t, r, http.MethodPost, "/projects/7/credential_grants/"+id+":approve", "300", ""); status != http.StatusConflict {
		t.Fatalf("second approval = %d, want 409", status)
	}
	if status, s := call(t, r, http.MethodPost, "/projects/7/credential_grants/"+id+":suspend", "100", ""); status != http.StatusOK || s["state"] != "suspended" {
		t.Fatalf("suspend = %d %v", status, s)
	}
	if status, s := call(t, r, http.MethodPost, "/projects/7/credential_grants/"+id+":revoke", "100", ""); status != http.StatusOK || s["state"] != "revoked" {
		t.Fatalf("revoke = %d %v", status, s)
	}
	status, list := call(t, r, http.MethodGet, "/projects/7/credential_grants", "100", "")
	if rows, _ := list["rows"].([]any); status != http.StatusOK || len(rows) != 1 {
		t.Fatalf("list = %d %v", status, list)
	}
	status, list = call(t, r, http.MethodGet, "/projects/8/credential_grants", "100", "")
	if rows, _ := list["rows"].([]any); status != http.StatusOK || len(rows) != 0 {
		t.Fatalf("list of another project = %d %v", status, list)
	}
}

func TestProposeRefusesBadInput(t *testing.T) {
	r := newRouter(t)
	for name, body := range map[string]string{
		"unknown field":      `{"tenant_project_id":8}`,
		"unknown capability": strings.Replace(proposal, `["use"]`, `["admin"]`, 1),
		"no operation":       strings.Replace(proposal, `["jira.issue.read"]`, `[]`, 1),
		"not json":           `{`,
	} {
		if status, _ := call(t, r, http.MethodPost, "/projects/7/credential_grants", "100", body); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
	if status, _ := call(t, r, http.MethodPost, "/projects/7/credential_grants", "", proposal); status != http.StatusUnauthorized {
		t.Errorf("no user: status %d, want 401", status)
	}
	if status, _ := call(t, r, http.MethodGet, "/projects/7/credential_grants?state=gone", "100", ""); status != http.StatusBadRequest {
		t.Errorf("bad state: status %d, want 400", status)
	}
	if status, _ := call(t, r, http.MethodGet, "/projects/x/credential_grants", "100", ""); status != http.StatusBadRequest {
		t.Errorf("bad project: status %d, want 400", status)
	}
}
