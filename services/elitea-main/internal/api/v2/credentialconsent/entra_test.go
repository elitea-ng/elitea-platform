package credentialconsent

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
)

const entraRef = "car_reference-of-user-11"

// entraStore holds one requirement of user 11 in project 7.
type entraStore struct {
	reads     int
	delegates int
	noSignIn  bool
}

func (s *entraStore) RequirementFor(_ context.Context, reference string, project, user int64) (delegationstore.Requirement, error) {
	s.reads++
	if reference != entraRef || project != 7 || user != 11 {
		return delegationstore.Requirement{}, delegationstore.ErrRequirementNotFound
	}
	return delegationstore.Requirement{Reference: entraRef, TenantProjectID: 7, ActorUserID: 11,
		DelegationID: "d-1", Reason: delegationstore.ReasonInteractionRequired, CreatedAt: time.Unix(100, 0)}, nil
}

func (s *entraStore) Delegation(context.Context, string) (mandate.Delegation, error) {
	return mandate.Delegation{ID: "d-1", TenantProjectID: 7, AgentBindingID: "b-1", Audiences: []string{"api://middle"}}, nil
}

func (s *entraStore) Delegate(context.Context, delegationstore.DelegateRequest) (delegationstore.Delegation, error) {
	s.delegates++
	if s.noSignIn {
		return delegationstore.Delegation{}, delegationstore.ErrNoSignInAssertion
	}
	return delegationstore.Delegation{ID: "d-2"}, nil
}

type entraResumer struct{ resumes int }

func (r *entraResumer) Resume(context.Context, agentexecutionapp.CredentialAuthorizationResume) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	r.resumes++
	return agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "e-2"}, nil
}

func entraHandler(t *testing.T, store *entraStore, resumer *entraResumer) *Handler {
	t.Helper()
	h, err := New(nil, slog.New(slog.DiscardHandler), WithEntraConsent(store, resumer, false))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func entraServe(h *Handler, resolver auth.PermissionResolver, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.With(DecodeStart).Post("/credential_consent/{provider}:start", h.Start)
	r.Get("/credential_consent/{provider}:callback", h.Callback(resolver, "default"))
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "11", UserID: "11"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestEntraNeedsItsDependencies(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("a handler with neither flow was built")
	}
	if _, err := New(nil, nil, WithEntraConsent(nil, &entraResumer{}, false)); err == nil {
		t.Fatal("an Entra handler without custody was built")
	}
	// A provider flow alone answers 404 for Entra; Entra alone answers 404
	// for a provider.
	h, err := New(&fakeFlow{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := entraServe(h, fakeResolver{}, http.MethodPost, "/credential_consent/entra:start", `{"project_id":7,"authorization_reference":"car_x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("entra without Entra: %d", rec.Code)
	}
	h = entraHandler(t, &entraStore{}, &entraResumer{})
	if rec := entraServe(h, fakeResolver{}, http.MethodPost, "/credential_consent/jira:start", `{"project_id":7,"agent_binding_id":"a","target_binding_id":"b"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("jira without a flow: %d", rec.Code)
	}
}

// The callback checks the permission in the project before it creates a
// delegation, and a missing new sign-in creates and resumes nothing.
func TestEntraCallbackRefusals(t *testing.T) {
	session := &http.Cookie{Name: browsersession.CookieName,
		Value: browsersession.CookieValue(base64.RawURLEncoding.EncodeToString(make([]byte, browsersession.IDRandomBytes)))}
	start := func(h *Handler) (*http.Cookie, string) {
		rec := entraServe(h, fakeResolver{}, http.MethodPost, "/credential_consent/entra:start", `{"project_id":7,"authorization_reference":"`+entraRef+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("start: %d %s", rec.Code, rec.Body)
		}
		cookie := rec.Result().Cookies()[0]
		state := strings.SplitN(cookie.Value, "|", 2)[0]
		return cookie, "/credential_consent/entra:callback?state=" + state
	}

	store, resumer := &entraStore{}, &entraResumer{}
	h := entraHandler(t, store, resumer)
	cookie, callback := start(h)
	if rec := entraServe(h, fakeResolver{}, http.MethodGet, callback, "", cookie, session); rec.Code != http.StatusForbidden || store.delegates != 0 {
		t.Fatalf("no permission: %d, delegates %d", rec.Code, store.delegates)
	}

	store, resumer = &entraStore{noSignIn: true}, &entraResumer{}
	h = entraHandler(t, store, resumer)
	cookie, callback = start(h)
	use := fakeResolver{perms: []string{credentialgrant.PermissionUse}}
	if rec := entraServe(h, use, http.MethodGet, callback, "", cookie, session); rec.Code != http.StatusConflict || resumer.resumes != 0 {
		t.Fatalf("no new sign-in: %d, resumes %d", rec.Code, resumer.resumes)
	}

	store, resumer = &entraStore{}, &entraResumer{}
	h = entraHandler(t, store, resumer)
	cookie, callback = start(h)
	if rec := entraServe(h, use, http.MethodGet, callback, "", cookie); rec.Code != http.StatusConflict || store.delegates != 0 {
		t.Fatalf("no browser session: %d, delegates %d", rec.Code, store.delegates)
	}
	if rec := entraServe(h, use, http.MethodGet, callback, "", cookie, session); rec.Code != http.StatusOK || resumer.resumes != 1 {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
}
