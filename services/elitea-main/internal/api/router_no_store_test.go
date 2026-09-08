package api_test

// Router-level wiring for the API group's cache directive.
//
// The middleware's own unit tests say what `apimw.NoStore` does. This says it
// is MOUNTED — the failure this closes was not a middleware that did the wrong
// thing, it was an API that said nothing about caching at all, and a browser
// that decided for itself.
//
// Reuses stubTokenValidator/buildMinimalRouterConfig from router_llm_test.go
// (same package): the directive is written INSIDE the /api/v2 group, which
// sits below Auth, so an unauthenticated request never reaches it.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The group's own "no such route" answer carries it, which is the cheapest
// authenticated request that reaches the group without a database.
func TestAPIGroupAnswersWithNoStore(t *testing.T) {
	validator := &stubTokenValidator{user: auth.User{ID: "100", UserID: "100", Name: "reader", AuthType: "token"}}
	r := api.NewRouter(buildMinimalRouterConfig(t, validator, &stubProjectResolver{id: 5}, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/no-such-route", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown path inside the API group: got %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want %q — the API group's no-store middleware is not mounted", got, "no-store")
	}
}

// …and the branding bootstrap, which is registered OUTSIDE that group so a
// browser with no session can load it, keeps the revalidating value it chose.
// A directive applied one level higher would have made every brand pack
// uncacheable.
func TestBrandingBootstrapKeepsItsOwnCacheControl(t *testing.T) {
	r := api.NewRouter(buildMinimalRouterConfig(t, nil, nil, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET bootstrap.js: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q, want the branding handler's own %q", got, "no-cache")
	}
}
