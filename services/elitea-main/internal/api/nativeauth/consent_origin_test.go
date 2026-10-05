package nativeauth_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

var metaReferrer = regexp.MustCompile(`<meta name="referrer" content="([^"]*)">`)

// effectiveReferrerPolicy is the policy a browser applies to a request the
// page makes: the Referrer-Policy header, then every <meta name=referrer> in
// document order, the last one winning (HTML "Standard metadata names";
// Referrer Policy §4.1, §4.2).
func effectiveReferrerPolicy(response *httptest.ResponseRecorder) string {
	policy := ""
	for _, value := range strings.Split(response.Header().Get("Referrer-Policy"), ",") {
		if value = strings.TrimSpace(value); value != "" {
			policy = value
		}
	}
	for _, match := range metaReferrer.FindAllStringSubmatch(response.Body.String(), -1) {
		policy = match[1]
	}
	return policy
}

// browserOrigin is the Origin header a browser (WebKit, Chromium) sends for a
// same-origin form POST from the page: Fetch "serializing a request origin"
// replaces the origin with "null" when the request's referrer policy is
// no-referrer, whatever the target.
func browserOrigin(page *httptest.ResponseRecorder, pageOrigin string) string {
	if effectiveReferrerPolicy(page) == "no-referrer" {
		return "null"
	}
	return pageOrigin
}

// DEF-1 (Agent Zefir E2E): the consent page was served with
// Referrer-Policy: no-referrer, so Safari answered the decision form with
// "Origin: null" and the decision handler refused every real browser with
// 403. The page must leave the browser sending its own origin on the
// same-origin decision POST, and still send no referrer to anything else.
func TestConsentPageLetsTheBrowserSendItsOriginToTheDecision(t *testing.T) {
	page := httptest.NewRecorder()
	browserauth.NewNativePages(nil).RenderConsent(page, httptest.NewRequest(http.MethodGet, "/", nil), browserauth.NativeConsent{
		ClientName: "Agent", ClientID: testClientID, Email: "a@example.test", Platform: "ios",
		Origin: testOrigin, Action: nativeapi.DecisionPath, Request: "handle", UserID: "7",
		FormAction: domain.FormActionSource(testRedirect),
	})
	if got := effectiveReferrerPolicy(page); got != "same-origin" {
		t.Fatalf("effective referrer policy of the consent page = %q, want same-origin (header %q)",
			got, page.Header().Get("Referrer-Policy"))
	}

	// The decision handler accepts what that browser sends: the origin check
	// passes and the request goes on to the binder (no binder cookie here, so
	// the refusal is the binder's page, not the origin's 403).
	handler := nativeapi.New(nativeapi.Config{
		Registry:     domain.NewRegistry([]domain.Client{fileClient()}, nil),
		PublicOrigin: testOrigin,
		Pages:        browserauth.NewNativePages(nil),
	})
	router := chi.NewRouter()
	handler.Mount(router)
	request := httptest.NewRequest(http.MethodPost, nativeapi.DecisionPath,
		strings.NewReader("request=handle&uid=7&decision=allow"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", browserOrigin(page, testOrigin))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusForbidden {
		t.Fatalf("decision from the consent page's own browser = 403 %s", recorder.Body.String())
	}

	// The error page keeps no-referrer: it has nothing to post.
	errorPage := httptest.NewRecorder()
	browserauth.NewNativePages(nil).RenderError(errorPage, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusBadRequest, "x")
	if got := effectiveReferrerPolicy(errorPage); got != "no-referrer" {
		t.Fatalf("error page referrer policy = %q, want no-referrer", got)
	}
}

// The origin check itself is unchanged: a foreign origin and an opaque
// ("null") origin are still refused.
func TestDecisionStillRefusesForeignAndOpaqueOrigins(t *testing.T) {
	handler := nativeapi.New(nativeapi.Config{
		Registry:     domain.NewRegistry([]domain.Client{fileClient()}, nil),
		PublicOrigin: testOrigin,
		Pages:        browserauth.NewNativePages(nil),
	})
	router := chi.NewRouter()
	handler.Mount(router)
	for _, origin := range []string{"https://evil.example", "null"} {
		request := httptest.NewRequest(http.MethodPost, nativeapi.DecisionPath,
			strings.NewReader("request=handle&uid=7&decision=allow"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", origin)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("decision with Origin %q = %d, want 403", origin, recorder.Code)
		}
	}
}
