package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// WHAT A PATH NOBODY REGISTERED ANSWERS (F3).
//
// A route that moves, or one that was retired, keeps being called by a client
// that has not been rebuilt. The status that answers it decides what the
// client does next, and the two answers are not interchangeable:
//
//	401 → "your session died"  → apps/elitea-web opens a fresh OIDC round-trip
//	404 → "that route is gone" → the caller degrades and carries on
//
// The web client is 401-only about this on purpose (`needsReauth` in
// apps/elitea-web/src/shared/api/http.ts; a past over-eager escalation on 403
// hung the UI), so a 401 for a path that simply does not exist is read as a
// dead session and re-authenticates on every visit to the page that calls it.
//
// The ordering that produces the right answer is: authenticate, THEN route.
// apimw.Auth is mounted on the group that wraps the whole `/api/v2` subrouter,
// so it runs before chi looks the path up — a caller with a credential reaches
// the subrouter's own fallback and a caller without one is refused before it.
// These tests pin that ordering in both directions; they fail if Auth is ever
// moved below routing, or if the group loses its fallback.
func newUnknownPathRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

// unknownAPIPaths are paths under /api/v2 that no registration claims. The
// first is the retired legacy read from #126 that was still being requested in
// production; the others cover a plausible typo and an unclaimed plugin
// prefix, so the assertion is about the GROUP and not about one string.
var unknownAPIPaths = []string{
	"/api/v2/chat_config/prompt_lib/1",
	"/api/v2/elitea_core/no_such_resource/prompt_lib/1",
	"/api/v2/no_such_plugin/anything",
}

func TestUnknownAPIPathAnswers404ToAnAuthenticatedCaller(t *testing.T) {
	router := newUnknownPathRouter(t)

	for _, path := range unknownAPIPaths {
		recorder := serveResponse(t, router, http.MethodGet, path)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404.\n"+
				"  A 401 here is read by the SPA as an expired session and it "+
				"re-authenticates on every visit to the page that still calls "+
				"this path.\n  Body: %s", path, recorder.Code, recorder.Body.String())
			continue
		}
		// The BODY matters as much as the status. chi's default fallback
		// writes `404 page not found` as text/plain, so the one answer a
		// client cannot parse is the answer it gets for a route that moved.
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Errorf("GET %s body %q is not JSON: %v", path, recorder.Body.String(), err)
		}
	}
}

func TestUnknownAPIPathAnswers401WithoutACredential(t *testing.T) {
	router := newUnknownPathRouter(t)

	for _, path := range unknownAPIPaths {
		recorder := httptest.NewRecorder()
		// No testAuthHeader: this request presents nothing at all.
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a credential status = %d, want 401.\n"+
				"  Routing must not answer before authentication does; a 404 "+
				"here would tell an anonymous caller which paths exist.\n"+
				"  Body: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// The control. Both assertions above are satisfied by a router that stopped
// serving: every path would 404 with a credential and 401 without one. This
// pins that a REGISTERED path under the same group still answers, so the two
// tests measure the fallback and not a dead mount.
func TestUnknownPathAssertionsAreNotVacuous(t *testing.T) {
	router := newUnknownPathRouter(t)

	const registered = "/api/v2/admin/system_info/prompt_lib"
	if status := serveStatus(t, router, http.MethodGet, registered); status == http.StatusNotFound {
		t.Fatalf("GET %s answered 404; the registered surface is gone, so the "+
			"unknown-path assertions above prove nothing", registered)
	}
}
