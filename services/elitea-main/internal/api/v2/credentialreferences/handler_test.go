package credentialreferences

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const refID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"

// fakeStore enforces the two-person rule the way the PostgreSQL store does
// and records every actor it is handed.
type fakeStore struct {
	stagedBy string
	state    credentialgrant.ReferenceState
	actors   []string
	attached repos.ToolkitAttachment
}

func (s *fakeStore) record() repos.CredentialReferenceRecord {
	return repos.CredentialReferenceRecord{
		CredentialReference: credentialgrant.CredentialReference{ID: refID, TenantProjectID: 7, State: s.state,
			Class: credentialgrant.ClassStaticSecret, Backend: "pgvault", Generation: 1},
		StagedBy: s.stagedBy,
	}
}

func (s *fakeStore) Get(_ context.Context, id string) (repos.CredentialReferenceRecord, error) {
	if id != refID {
		return repos.CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	return s.record(), nil
}

func (s *fakeStore) List(context.Context, repos.CredentialReferenceFilter) ([]repos.CredentialReferenceRecord, error) {
	return []repos.CredentialReferenceRecord{s.record()}, nil
}

func (s *fakeStore) Transition(_ context.Context, id string, action repos.CredentialReferenceAction, actor string) (repos.CredentialReferenceRecord, error) {
	s.actors = append(s.actors, actor)
	if id != refID {
		return repos.CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	switch {
	case action == repos.CredentialReferenceActivate && actor == s.stagedBy:
		return repos.CredentialReferenceRecord{}, repos.ErrCredentialReferenceSelfActive
	case action == repos.CredentialReferenceActivate && s.state != credentialgrant.ReferenceStaged:
		return repos.CredentialReferenceRecord{}, repos.ErrCredentialReferenceTransition
	case action == repos.CredentialReferenceActivate:
		s.state = credentialgrant.ReferenceActive
	default:
		s.state = credentialgrant.ReferenceRetired
	}
	return s.record(), nil
}

func (s *fakeStore) Attach(_ context.Context, id string, target repos.ToolkitAttachment, actor string) (repos.ToolkitAttachmentResult, error) {
	s.actors, s.attached = append(s.actors, actor), target
	return repos.ToolkitAttachmentResult{ProjectID: target.ProjectID, ToolkitID: target.ToolkitID, Field: "c", CredentialReferenceID: id}, nil
}

func (s *fakeStore) Detach(_ context.Context, _ string, target repos.ToolkitAttachment, actor string) (repos.ToolkitAttachmentResult, error) {
	s.actors = append(s.actors, actor)
	return repos.ToolkitAttachmentResult{}, repos.ErrToolkitNoConfigurationRef
}

func (s *fakeStore) PinToolkit(_ context.Context, binding string, target repos.ToolkitAttachment, actor string) (repos.AgentToolkitPin, error) {
	s.actors, s.attached = append(s.actors, actor), target
	return repos.AgentToolkitPin{AgentBindingID: binding, TenantProjectID: target.ProjectID, ToolkitID: target.ToolkitID}, nil
}

func (s *fakeStore) UnpinToolkit(_ context.Context, _ string, target repos.ToolkitAttachment, actor string) error {
	s.actors, s.attached = append(s.actors, actor), target
	return nil
}

func router(t *testing.T, store Store, userID string) http.Handler {
	t.Helper()
	h, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "token-1", UserID: userID})))
		})
	})
	r.Get("/credential_references", h.List)
	r.Get("/credential_references/{id}", h.Get)
	r.Post("/credential_references/{id}:activate", h.Activate)
	r.Post("/credential_references/{id}:retire", h.Retire)
	r.Post("/credential_references/{id}:attach", h.Attach)
	r.Post("/credential_references/{id}:detach", h.Detach)
	r.Put("/agent_identities/{id}/toolkits/{toolkitID}", h.PinToolkit)
	r.Delete("/agent_identities/{id}/toolkits/{toolkitID}", h.UnpinToolkit)
	return r
}

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// Two-person rule: the stager's own activation is 403; another user's
// succeeds; the actor always comes from the principal.
func TestActivateIsTwoPerson(t *testing.T) {
	store := &fakeStore{stagedBy: "100", state: credentialgrant.ReferenceStaged}
	if got := serve(t, router(t, store, "100"), http.MethodPost, "/credential_references/"+refID+":activate", "{}").Code; got != http.StatusForbidden {
		t.Fatalf("stager activates = %d, want 403", got)
	}
	rec := serve(t, router(t, store, "200"), http.MethodPost, "/credential_references/"+refID+":activate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second person activates = %d, want 200: %s", rec.Code, rec.Body)
	}
	var view referenceView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.State != "active" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	if got := serve(t, router(t, store, "300"), http.MethodPost, "/credential_references/"+refID+":activate", "{}").Code; got != http.StatusConflict {
		t.Fatalf("activating an active reference = %d, want 409", got)
	}
	if strings.Join(store.actors, ",") != "100,200,300" {
		t.Fatalf("actors = %v", store.actors)
	}
}

// No body field can name the actor or an approver.
func TestTransitionBodyCannotNameTheActor(t *testing.T) {
	store := &fakeStore{stagedBy: "100", state: credentialgrant.ReferenceStaged}
	if got := serve(t, router(t, store, "100"), http.MethodPost, "/credential_references/"+refID+":activate", `{"activated_by":"200"}`).Code; got != http.StatusBadRequest {
		t.Fatalf("a body naming an actor = %d, want 400", got)
	}
	if len(store.actors) != 0 {
		t.Fatalf("the store was called: %v", store.actors)
	}
}

func TestCallerWithoutResolvedUserIsRefused(t *testing.T) {
	store := &fakeStore{stagedBy: "100", state: credentialgrant.ReferenceStaged}
	if got := serve(t, router(t, store, ""), http.MethodPost, "/credential_references/"+refID+":retire", "{}").Code; got != http.StatusForbidden {
		t.Fatalf("no resolved user id = %d, want 403", got)
	}
}

func TestListNeedsAProject(t *testing.T) {
	h := router(t, &fakeStore{state: credentialgrant.ReferenceStaged}, "200")
	if got := serve(t, h, http.MethodGet, "/credential_references", "").Code; got != http.StatusBadRequest {
		t.Fatalf("list without project_id = %d, want 400", got)
	}
	if got := serve(t, h, http.MethodGet, "/credential_references?project_id=7&state=bogus", "").Code; got != http.StatusBadRequest {
		t.Fatalf("list with a bad state = %d, want 400", got)
	}
	rec := serve(t, h, http.MethodGet, "/credential_references?project_id=7", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rows"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if got := serve(t, h, http.MethodGet, "/credential_references/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f99", "").Code; got != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", got)
	}
}

func TestAttachAndPinReadTheirTargets(t *testing.T) {
	store := &fakeStore{}
	h := router(t, store, "200")
	if got := serve(t, h, http.MethodPost, "/credential_references/"+refID+":attach", `{"project_id":7}`).Code; got != http.StatusBadRequest {
		t.Fatalf("attach without toolkit_id = %d, want 400", got)
	}
	if got := serve(t, h, http.MethodPost, "/credential_references/"+refID+":attach", `{"project_id":7,"toolkit_id":11}`).Code; got != http.StatusOK {
		t.Fatalf("attach = %d, want 200", got)
	}
	if store.attached != (repos.ToolkitAttachment{ProjectID: 7, ToolkitID: 11}) {
		t.Fatalf("attached = %+v", store.attached)
	}
	if got := serve(t, h, http.MethodPost, "/credential_references/"+refID+":detach", `{"project_id":7,"toolkit_id":11}`).Code; got != http.StatusConflict {
		t.Fatalf("detach of an MCP toolkit = %d, want 409", got)
	}
	if got := serve(t, h, http.MethodPut, "/agent_identities/"+refID+"/toolkits/12", `{"project_id":8}`).Code; got != http.StatusOK {
		t.Fatalf("pin = %d, want 200", got)
	}
	if store.attached != (repos.ToolkitAttachment{ProjectID: 8, ToolkitID: 12}) {
		t.Fatalf("pinned = %+v", store.attached)
	}
	if got := serve(t, h, http.MethodDelete, "/agent_identities/"+refID+"/toolkits/13?project_id=9", "").Code; got != http.StatusNoContent {
		t.Fatalf("unpin = %d, want 204", got)
	}
	if store.attached != (repos.ToolkitAttachment{ProjectID: 9, ToolkitID: 13}) {
		t.Fatalf("unpinned = %+v", store.attached)
	}
	if got := serve(t, h, http.MethodPut, "/agent_identities/"+refID+"/toolkits/x", `{"project_id":8}`).Code; got != http.StatusBadRequest {
		t.Fatalf("pin with a bad toolkit id = %d, want 400", got)
	}
}
