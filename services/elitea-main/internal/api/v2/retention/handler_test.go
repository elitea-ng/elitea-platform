package retention

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type fakeStore struct {
	platform retention.Policy
	actor    int64
	hold     retention.Hold
}

func (f *fakeStore) ListPolicies(context.Context) ([]retention.Policy, error) {
	return []retention.Policy{{Class: retention.ClassConversation}}, nil
}

func (f *fakeStore) SetPlatformPolicy(_ context.Context, p retention.Policy, actor int64) (retention.Policy, error) {
	if err := retention.ValidatePlatform(p); err != nil {
		return retention.Policy{}, apierr.BadRequest(err.Error())
	}
	f.platform, f.actor = p, actor
	return p, nil
}

func (f *fakeStore) SetProjectOverride(_ context.Context, p retention.Policy, _ int64) (retention.Policy, error) {
	return p, nil
}

func (f *fakeStore) DeleteProjectOverride(context.Context, retention.DataClass, int64) error {
	return nil
}

func (f *fakeStore) ListHolds(context.Context, bool) ([]retention.Hold, error) { return nil, nil }

func (f *fakeStore) CreateHold(_ context.Context, h retention.Hold) (retention.Hold, error) {
	if err := retention.ValidateHold(h); err != nil {
		return retention.Hold{}, apierr.BadRequest(err.Error())
	}
	h.ID = 9
	f.hold = h
	return h, nil
}

func (f *fakeStore) ReleaseHold(_ context.Context, id, actor int64, _ string) (retention.Hold, error) {
	return retention.Hold{ID: id, Scope: retention.HoldUser, CreatedBy: actor}, nil
}

func serve(t *testing.T, h *Handler, method, pattern, target, body string) (*httptest.ResponseRecorder, audit.Annotation) {
	t.Helper()
	router := chi.NewRouter()
	var slot *audit.AnnotationSlot
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, s := audit.ContextWithAnnotationSlot(r.Context())
			slot = s
			ctx = auth.ContextWithUser(ctx, auth.User{ID: "5", UserID: "5"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	switch method {
	case http.MethodPut:
		router.Put(pattern, h.SetPlatformPolicy)
	case http.MethodPost:
		router.Post(pattern, h.CreateHold)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	annotation, _ := slot.Read()
	return rec, annotation
}

func TestPolicyUpdateIsStoredAndAnnotated(t *testing.T) {
	store := &fakeStore{}
	rec, annotation := serve(t, NewHandler(store), http.MethodPut, "/retention/policies/{dataClass}",
		"/retention/policies/conversation", `{"min_age_days": 30, "max_age_days": 365, "allow_project_override": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.actor != 5 || store.platform.MaxAgeDays == nil || *store.platform.MaxAgeDays != 365 ||
		store.platform.MinAgeDays != 30 || !store.platform.AllowProjectOverride {
		t.Fatalf("stored %+v by %d", store.platform, store.actor)
	}
	if annotation.Action != "retention.policy.update" || !strings.Contains(annotation.EntityName, "365") {
		t.Fatalf("annotation %+v", annotation)
	}
}

func TestInvalidPolicyIsRefused(t *testing.T) {
	rec, _ := serve(t, NewHandler(&fakeStore{}), http.MethodPut, "/retention/policies/{dataClass}",
		"/retention/policies/gateway_request_log",
		`{"min_age_days": 0, "max_age_days": null, "allow_project_override": false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

// The PUT replaces the row, so every field is required: an omitted field
// used to reset min_age_days to 0 and allow_project_override to false
// silently. null max_age_days is a value (keep forever), not an omission.
func TestPolicyUpdateRequiresEveryField(t *testing.T) {
	for _, body := range []string{
		`{"max_age_days": 365, "allow_project_override": true}`,
		`{"min_age_days": 30, "allow_project_override": true}`,
		`{"min_age_days": 30, "max_age_days": 365}`,
		`{"min_age_days": null, "max_age_days": 365, "allow_project_override": true}`,
		`{"min_age_days": 30, "max_age_days": 365, "allow_project_override": null}`,
		`{}`,
	} {
		store := &fakeStore{}
		rec, _ := serve(t, NewHandler(store), http.MethodPut, "/retention/policies/{dataClass}",
			"/retention/policies/conversation", body)
		if rec.Code != http.StatusBadRequest || store.actor != 0 {
			t.Errorf("%s: status %d, stored by %d; want 400 and nothing stored", body, rec.Code, store.actor)
		}
	}
	store := &fakeStore{}
	rec, _ := serve(t, NewHandler(store), http.MethodPut, "/retention/policies/{dataClass}",
		"/retention/policies/conversation", `{"min_age_days": 0, "max_age_days": null, "allow_project_override": false}`)
	if rec.Code != http.StatusOK || store.platform.MaxAgeDays != nil {
		t.Fatalf("keep forever: status %d, stored %+v", rec.Code, store.platform)
	}
}

func TestHoldCreateIsAnnotatedWithItsID(t *testing.T) {
	store := &fakeStore{}
	rec, annotation := serve(t, NewHandler(store), http.MethodPost, "/legal_holds", "/legal_holds",
		`{"scope": "user", "user_id": 12, "reason": "matter 7"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.hold.CreatedBy != 5 || annotation.Action != "legal_hold.create" ||
		annotation.EntityID == nil || *annotation.EntityID != 9 || annotation.EntityName != "user 12" {
		t.Fatalf("hold %+v annotation %+v", store.hold, annotation)
	}
}

func TestZeroHandlerAnswers503(t *testing.T) {
	rec, _ := serve(t, &Handler{}, http.MethodPost, "/legal_holds", "/legal_holds", `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}
