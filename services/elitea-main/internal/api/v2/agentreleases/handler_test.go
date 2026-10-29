package agentreleases

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakeStore struct {
	rollbackErr    error
	rollbackReason string
	rollbackActor  int64
	getErr         error
}

func (f *fakeStore) ListReleases(context.Context, int64, int64) ([]agentrelease.Release, error) {
	return []agentrelease.Release{{ID: 2, ApplicationID: 31, State: agentrelease.StatePublished, IsHead: true}}, nil
}

func (f *fakeStore) GetRelease(_ context.Context, _, _, releaseID int64) (repos.ReleaseDetail, error) {
	if f.getErr != nil {
		return repos.ReleaseDetail{}, f.getErr
	}
	return repos.ReleaseDetail{Release: agentrelease.Release{ID: releaseID, Definition: json.RawMessage(`{"instructions":"x"}`)}}, nil
}

func (f *fakeStore) Rollback(_ context.Context, _, _, releaseID, actorID int64, reason string) (repos.RollbackOutcome, error) {
	f.rollbackReason, f.rollbackActor = reason, actorID
	if f.rollbackErr != nil {
		return repos.RollbackOutcome{}, f.rollbackErr
	}
	return repos.RollbackOutcome{From: agentrelease.Release{ID: 9}, To: agentrelease.Release{ID: releaseID, IsHead: true}}, nil
}

func (f *fakeStore) ListRevisions(context.Context, int64, int64, int64) ([]agentrelease.Revision, error) {
	return nil, nil
}

func serve(t *testing.T, handler *Handler, method, target, body string, signedIn bool) (*httptest.ResponseRecorder, *audit.AnnotationSlot) {
	t.Helper()
	router := chi.NewRouter()
	router.Get(ReleasesPath, handler.List)
	router.Get(ReleasePath, handler.Get)
	router.Post(RollbackPath, handler.Rollback)
	router.Get(RevisionsPath, handler.Revisions)
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	if signedIn {
		ctx = auth.ContextWithUser(ctx, auth.User{ID: "4", UserID: "4"})
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request.WithContext(ctx))
	return recorder, slot
}

func TestRollbackAnnotatesTheAuditRecord(t *testing.T) {
	store := &fakeStore{}
	recorder, slot := serve(t, NewHandler(store), http.MethodPost,
		"/application_releases/prompt_lib/7/31/5/rollback", `{"reason":"  v3 is wrong "}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if store.rollbackReason != "v3 is wrong" || store.rollbackActor != 4 {
		t.Fatalf("rollback got reason %q actor %d", store.rollbackReason, store.rollbackActor)
	}
	annotation, ok := slot.Read()
	if !ok || annotation.Action != AuditActionRolledBack || annotation.EntityType != "application_release" ||
		annotation.EntityID == nil || *annotation.EntityID != 5 || annotation.ProjectID == nil || *annotation.ProjectID != 7 {
		t.Fatalf("annotation = %+v %v", annotation, ok)
	}
	var body struct {
		From, To ReleaseSummary
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.From.ID != 9 || body.To.ID != 5 || !body.To.IsPublishedRelease {
		t.Fatalf("body %s (%v)", recorder.Body, err)
	}
}

// A reason at the 200-character limit is accepted and survives whole in
// the audit entity name, which the audit store clamps at 256 bytes.
func TestRollbackReasonAtTheLimitFitsTheAuditName(t *testing.T) {
	reason := strings.Repeat("r", maxReasonLength)
	store := &fakeStore{}
	recorder, slot := serve(t, NewHandler(store), http.MethodPost,
		"/application_releases/prompt_lib/7/2147483647/5/rollback", `{"reason":"`+reason+`"}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	annotation, ok := slot.Read()
	if !ok || !strings.HasSuffix(annotation.EntityName, " reason: "+reason) || len(annotation.EntityName) > 256 {
		t.Fatalf("entity name %q (%d bytes)", annotation.EntityName, len(annotation.EntityName))
	}
}

func TestRollbackRefusals(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		body   string
		signed bool
		status int
		code   string
	}{
		{"drift", &agentrelease.DriftError{ToolkitID: 3, Reason: agentrelease.DriftToolkitMissing}, "", true, http.StatusConflict, agentrelease.DriftCode},
		{"not found", agentrelease.ErrNotFound, "", true, http.StatusNotFound, agentrelease.NotFoundCode},
		{"no head", agentrelease.ErrNotPublished, "", true, http.StatusConflict, "application_not_published"},
		{"already head", agentrelease.ErrAlreadyHead, "", true, http.StatusConflict, "release_already_published"},
		{"withdrawn", agentrelease.ErrWithdrawn, "", true, http.StatusConflict, "release_withdrawn"},
		{"unknown field", nil, `{"why":"x"}`, true, http.StatusBadRequest, ""},
		{"reason too long", nil, `{"reason":"` + strings.Repeat("é", 201) + `"}`, true, http.StatusBadRequest, ""},
		{"no user", nil, "", false, http.StatusUnauthorized, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder, slot := serve(t, NewHandler(&fakeStore{rollbackErr: c.err}), http.MethodPost,
				"/application_releases/prompt_lib/7/31/5/rollback", c.body, c.signed)
			if recorder.Code != c.status {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
			}
			if c.code != "" && !strings.Contains(recorder.Body.String(), `"code":"`+c.code+`"`) {
				t.Fatalf("body %s, want code %s", recorder.Body, c.code)
			}
			if _, annotated := slot.Read(); annotated {
				t.Fatal("a refused rollback annotated the audit record")
			}
		})
	}
}

func TestReadsAndUnavailableStore(t *testing.T) {
	recorder, _ := serve(t, NewHandler(&fakeStore{}), http.MethodGet, "/application_releases/prompt_lib/7/31", "", true)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"total":1`) {
		t.Fatalf("list %d %s", recorder.Code, recorder.Body)
	}
	recorder, _ = serve(t, NewHandler(&fakeStore{}), http.MethodGet, "/application_releases/prompt_lib/7/31/5", "", true)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"toolkit_pins":[]`) ||
		!strings.Contains(recorder.Body.String(), `"changes":[]`) || !strings.Contains(recorder.Body.String(), `"previous_release_id":null`) {
		t.Fatalf("get %d %s", recorder.Code, recorder.Body)
	}
	recorder, _ = serve(t, NewHandler(&fakeStore{getErr: agentrelease.ErrNotFound}), http.MethodGet, "/application_releases/prompt_lib/7/31/5", "", true)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing release %d", recorder.Code)
	}
	recorder, _ = serve(t, NewHandler(&fakeStore{}), http.MethodGet, "/version_revisions/prompt_lib/7/31/41", "", true)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"rows":[]`) {
		t.Fatalf("revisions %d %s", recorder.Code, recorder.Body)
	}
	recorder, _ = serve(t, NewHandler(&fakeStore{}), http.MethodGet, "/application_releases/prompt_lib/7/0", "", true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad id %d", recorder.Code)
	}
	var none *repos.AgentReleases
	recorder, _ = serve(t, NewHandler(none), http.MethodGet, "/application_releases/prompt_lib/7/31", "", true)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store %d", recorder.Code)
	}
}

func TestRollbackEntityNameCarriesTheReason(t *testing.T) {
	if got := rollbackEntityName(31, 5, ""); got != "application:31 from release:5" {
		t.Fatalf("no reason: %q", got)
	}
	if got := rollbackEntityName(31, 5, "regression in v6"); got != "application:31 from release:5 reason: regression in v6" {
		t.Fatalf("with reason: %q", got)
	}
}

type fakeGovernor struct{ err error }

func (f fakeGovernor) CheckDirectPublish(context.Context, int64, int64) error { return f.err }

// TestRollbackOfGovernedAgentNeedsTheGovernanceRole proves a rollback of an
// agent whose tier needs approval answers 409 change_request_required
// without the governance role, and goes through (audited) with it.
func TestRollbackOfGovernedAgentNeedsTheGovernanceRole(t *testing.T) {
	governed := fakeGovernor{err: agentlifecycle.Refuse(agentlifecycle.ChangeRequiredCode, "needs a change request")}
	for _, c := range []struct {
		name       string
		governor   fakeGovernor
		governance bool
		want       int
	}{
		{"governed, author", governed, false, http.StatusConflict},
		{"governed, governance role", governed, true, http.StatusOK},
		{"low tier", fakeGovernor{}, false, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{}
			holds := func(*http.Request) (bool, error) { return c.governance, nil }
			recorder, slot := serve(t, NewHandler(store, WithGovernance(c.governor, holds)), http.MethodPost,
				"/application_releases/prompt_lib/7/31/5/rollback", `{"reason":"bad"}`, true)
			if recorder.Code != c.want {
				t.Fatalf("status %d, want %d: %s", recorder.Code, c.want, recorder.Body)
			}
			_, annotated := slot.Read()
			if c.want == http.StatusConflict {
				if !strings.Contains(recorder.Body.String(), agentlifecycle.ChangeRequiredCode) || store.rollbackActor != 0 || annotated {
					t.Fatalf("refused rollback: body %s, store actor %d, annotated %v", recorder.Body, store.rollbackActor, annotated)
				}
				return
			}
			if store.rollbackActor != 4 || !annotated {
				t.Fatalf("rollback not done or not audited: actor %d, annotated %v", store.rollbackActor, annotated)
			}
		})
	}
}
