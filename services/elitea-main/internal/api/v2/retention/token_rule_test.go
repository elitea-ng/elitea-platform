package retention

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

// The token rule of the action table on the retention writes (ADR-0059
// R1.2b, issue 2993): the middleware leaves the decision to the handler
// (adminaccess.TokenRuleInHandler), which refuses an inherit token the
// shorten outcome only, and the store writes nothing.

func days(n int) *int { return &n }

// serveAsToken routes the three policy writes, with an inherit token caller
// on the context, and answers the response and the admin audit annotation.
func serveAsToken(t *testing.T, store *fakeStore, method, target, body string) (*httptest.ResponseRecorder, audit.Annotation) {
	t.Helper()
	h := NewHandler(store)
	router := chi.NewRouter()
	var slot *audit.AnnotationSlot
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, s := audit.ContextWithAnnotationSlot(r.Context())
			slot = s
			ctx = auth.ContextWithUser(ctx, auth.User{ID: "5", UserID: "5"})
			ctx = adminaccess.ContextWithTokenCaller(ctx,
				adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	router.Put("/retention/policies/{dataClass}", h.SetPlatformPolicy)
	router.Put("/retention/policies/{dataClass}/projects/{projectID}", h.SetProjectOverride)
	router.Delete("/retention/policies/{dataClass}/projects/{projectID}", h.DeleteProjectOverride)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body)))
	annotation, _ := slot.Read()
	return rec, annotation
}

func refusedByTheTokenRule(rec *httptest.ResponseRecorder) bool {
	if rec.Code != http.StatusForbidden {
		return false
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code == adminaccess.CodeTokenCriticalAction
}

// platformStore holds a conversation platform row of 30 to 365 days that
// allows overrides, and optionally a project override.
func platformStore(override *int) *fakeStore {
	store := &fakeStore{
		platform: retention.Policy{Class: retention.ClassConversation, MinAgeDays: 30, MaxAgeDays: days(365),
			AllowProjectOverride: true},
		hasPlatform: true,
	}
	if override != nil {
		projectID := int64(1)
		store.override = &retention.Policy{Class: retention.ClassConversation, ProjectID: &projectID, MaxAgeDays: override}
	}
	return store
}

func TestAnInheritTokenIsRefusedAShorterRetentionOnly(t *testing.T) {
	const (
		platformPath = "/retention/policies/conversation"
		overridePath = "/retention/policies/conversation/projects/1"
	)
	for _, tc := range []struct {
		name, method, target, body string
		override                   *int
		refused                    bool
		overridesOff               bool
	}{
		{"platform maximum down", http.MethodPut, platformPath,
			`{"min_age_days": 30, "max_age_days": 90, "allow_project_override": true}`, nil, true, false},
		{"platform minimum down", http.MethodPut, platformPath,
			`{"min_age_days": 7, "max_age_days": 365, "allow_project_override": true}`, nil, true, false},
		{"platform maximum up", http.MethodPut, platformPath,
			`{"min_age_days": 30, "max_age_days": 730, "allow_project_override": true}`, nil, false, false},
		{"platform kept forever", http.MethodPut, platformPath,
			`{"min_age_days": 30, "max_age_days": null, "allow_project_override": true}`, nil, false, false},
		{"override below the platform maximum", http.MethodPut, overridePath, `{"max_age_days": 90}`, nil, true, false},
		{"override raised", http.MethodPut, overridePath, `{"max_age_days": 200}`, days(90), false, false},
		{"override lowered", http.MethodPut, overridePath, `{"max_age_days": 60}`, days(90), true, false},
		{"override deleted, back to the longer platform maximum", http.MethodDelete, overridePath, "", days(90), false, false},
		// A stored override below the platform maximum takes effect once
		// overrides apply, so turning them on shortens (review of #2997).
		{"overrides turned on", http.MethodPut, platformPath,
			`{"min_age_days": 30, "max_age_days": 365, "allow_project_override": true}`, days(60), true, true},
		{"overrides turned off", http.MethodPut, platformPath,
			`{"min_age_days": 30, "max_age_days": 365, "allow_project_override": false}`, days(60), false, false},
	} {
		store := platformStore(tc.override)
		store.platform.AllowProjectOverride = !tc.overridesOff
		before := *store
		rec, annotation := serveAsToken(t, store, tc.method, tc.target, tc.body)
		if refused := refusedByTheTokenRule(rec); refused != tc.refused {
			t.Errorf("%s: %d %s, refused %v, want %v", tc.name, rec.Code, rec.Body, refused, tc.refused)
			continue
		}
		if !tc.refused {
			if rec.Code >= 400 {
				t.Errorf("%s: %d %s, want the write", tc.name, rec.Code, rec.Body)
			}
			continue
		}
		if annotation.Action != adminaccess.TokenRefusedAuditAction {
			t.Errorf("%s: audit action %q, want %s", tc.name, annotation.Action, adminaccess.TokenRefusedAuditAction)
		}
		if store.actor != 0 || store.override != before.override ||
			*store.platform.MaxAgeDays != 365 || store.platform.MinAgeDays != 30 ||
			store.platform.AllowProjectOverride != !tc.overridesOff {
			t.Errorf("%s: the store was written: %+v", tc.name, store)
		}
	}
}

// A browser session is not a token: the same shortening is stored.
func TestASessionShortensRetention(t *testing.T) {
	store := platformStore(nil)
	rec, _ := serve(t, NewHandler(store), http.MethodPut, "/retention/policies/{dataClass}",
		"/retention/policies/conversation", `{"min_age_days": 30, "max_age_days": 90, "allow_project_override": true}`)
	if rec.Code != http.StatusOK || *store.platform.MaxAgeDays != 90 {
		t.Fatalf("session shorten = %d, stored %+v", rec.Code, store.platform)
	}
}
