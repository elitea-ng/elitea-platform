package middleware

// The sealed break-glass session in apimw.Auth (ADR-0059 R2.7a, issue 2521):
// read on the administration surface only, before every other cookie, and
// cleared when it is refused. browsersession's own suite proves which answer
// each session state produces; this file proves what the middleware does with
// each answer and where it asks at all.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// sealedStore is a session store that reads both cookies.
type sealedStore struct {
	stubSessionStore
	sealed      browsersession.Session
	sealedErr   error
	sealedCalls int
}

func (s *sealedStore) ValidateSealed(context.Context, string) (browsersession.Session, error) {
	s.sealedCalls++
	if s.sealedErr != nil {
		return browsersession.Session{}, s.sealedErr
	}
	return s.sealed, nil
}

const (
	sealedValue   = "s1.sealed-session-identifier-0123456789abcdefghij"
	ordinaryValue = "s1.ordinary-session-identifier-0123456789abcdefgh"
)

// Routes of each kind. The surface ones are read from the embedded list, so
// the test follows it.
const (
	surfaceRoute  = "/api/v2/auth/permissions/administration"
	modeRoute     = "/api/v2/elitea_core/audit/{mode}"
	offSurface    = "/api/v2/elitea_core/applications/prompt_lib/{projectID}"
	sealedUserRef = int64(77)
)

func init() {
	for _, route := range []string{"GET " + surfaceRoute, "GET " + modeRoute} {
		found := false
		for _, listed := range adminaccess.AdministrationSurface() {
			found = found || listed == route
		}
		if !found {
			panic(route + " is not on the embedded administration surface; pick another route for this test")
		}
	}
}

func sealedStoreFixture() *sealedStore {
	return &sealedStore{
		stubSessionStore: stubSessionStore{session: browsersession.Session{
			ID: "ordinary", UserID: 41, Provider: browsersession.ProviderOIDC, Ref: 12,
		}},
		sealed: browsersession.Session{
			ID: "sealed", UserID: 42, Provider: browsersession.ProviderSealed, Ref: sealedUserRef,
			AuthenticatedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
		},
	}
}

type sealedRequest struct {
	method, path     string
	sealed, ordinary bool
	authorization    string
}

// serveSealed routes the request through a chi router that mounts the three
// routes behind Auth, as the production router does, and answers the
// principal the route saw.
func serveSealed(t *testing.T, store *sealedStore, req sealedRequest) (*httptest.ResponseRecorder, *auth.User) {
	t.Helper()
	var seen *auth.User
	record := func(_ http.ResponseWriter, r *http.Request) {
		if user, ok := auth.UserFromContext(r.Context()); ok {
			seen = &user
		}
	}
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(Auth(AuthConfig{PrincipalValidator: &recordingPrincipals{}, SessionStore: store}))
		r.Get(surfaceRoute, record)
		r.Get(modeRoute, record)
		r.Get(offSurface, record)
	})
	method := req.method
	if method == "" {
		method = http.MethodGet
	}
	request := httptest.NewRequest(method, req.path, nil)
	if req.sealed {
		request.AddCookie(&http.Cookie{Name: browsersession.SealedCookieName, Value: sealedValue})
	}
	if req.ordinary {
		request.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: ordinaryValue})
	}
	if req.authorization != "" {
		request.Header.Set("Authorization", req.authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder, seen
}

func TestSealedSessionAuthenticatesOnTheSurface(t *testing.T) {
	for name, path := range map[string]string{
		"an administration route":        surfaceRoute,
		"a {mode} route, administration": "/api/v2/elitea_core/audit/administration",
	} {
		t.Run(name, func(t *testing.T) {
			store := sealedStoreFixture()
			recorder, user := serveSealed(t, store, sealedRequest{path: path, sealed: true})
			if recorder.Code != http.StatusOK || user == nil {
				t.Fatalf("status %d, principal %v; want the sealed session served", recorder.Code, user)
			}
			if user.CredentialKind != auth.CredentialKindSealed || user.BrowserSessionProvider != browsersession.ProviderSealed ||
				user.BrowserSessionRef != sealedUserRef || user.UserID != "42" {
				t.Errorf("principal %+v is not the sealed session's", *user)
			}
			if user.BrowserSessionAuthenticationProven {
				t.Error("a sealed session vouches for a recent authentication")
			}
			if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
				t.Errorf("a served sealed session wrote cookies %v", cookies)
			}
		})
	}
}

func TestSealedSessionWinsOverTheOrdinaryCookieOnTheSurface(t *testing.T) {
	store := sealedStoreFixture()
	_, user := serveSealed(t, store, sealedRequest{path: surfaceRoute, sealed: true, ordinary: true})
	if user == nil || user.CredentialKind != auth.CredentialKindSealed {
		t.Fatalf("principal %v; want the sealed session", user)
	}
	if store.calls != 0 {
		t.Errorf("the ordinary session was read %d times on the surface beside a sealed cookie", store.calls)
	}

	// An invalid sealed cookie is not rescued by the ordinary one.
	store = sealedStoreFixture()
	store.sealedErr = browsersession.ErrRevoked
	recorder, user := serveSealed(t, store, sealedRequest{path: surfaceRoute, sealed: true, ordinary: true})
	if recorder.Code != http.StatusUnauthorized || user != nil {
		t.Fatalf("status %d, principal %v; want 401 for a revoked sealed session", recorder.Code, user)
	}
}

// Off the surface the cookie is no credential: it is never read, the
// ordinary cookie decides, and alone it is unauthenticated.
func TestSealedSessionIsRefusedOffTheSurface(t *testing.T) {
	for name, path := range map[string]string{
		"a user route":                 "/api/v2/elitea_core/applications/prompt_lib/1",
		"a {mode} route, default mode": "/api/v2/elitea_core/audit/prompt_lib",
		"a route nobody mounts":        "/api/v2/admin/not_mounted",
	} {
		t.Run(name, func(t *testing.T) {
			store := sealedStoreFixture()
			recorder, user := serveSealed(t, store, sealedRequest{path: path, sealed: true})
			if user != nil || (recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusNotFound) {
				t.Fatalf("status %d, principal %v; want no principal", recorder.Code, user)
			}
			if recorder.Code == http.StatusUnauthorized && !strings.Contains(recorder.Body.String(), reasonNoCredential) {
				t.Errorf("body %s; want %s: the sealed cookie must read as no credential", recorder.Body.String(), reasonNoCredential)
			}
			if store.sealedCalls != 0 {
				t.Errorf("the sealed session was read %d times off the surface", store.sealedCalls)
			}

			store = sealedStoreFixture()
			_, user = serveSealed(t, store, sealedRequest{path: path, sealed: true, ordinary: true})
			if path != "/api/v2/admin/not_mounted" && (user == nil || user.CredentialKind != auth.CredentialKindSession) {
				t.Errorf("principal %v; want the ordinary session to decide off the surface", user)
			}
			if store.sealedCalls != 0 {
				t.Errorf("the sealed session was read %d times off the surface", store.sealedCalls)
			}
		})
	}
}

// An explicit credential decides before any cookie, the sealed one included.
func TestSealedSessionIsNotReadBesideAnExplicitCredential(t *testing.T) {
	store := sealedStoreFixture()
	recorder, user := serveSealed(t, store, sealedRequest{path: surfaceRoute, sealed: true, authorization: "Bearer nope"})
	if user != nil || recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, principal %v; want the bearer refused, no sealed fallback", recorder.Code, user)
	}
	if store.sealedCalls != 0 {
		t.Errorf("the sealed session was read beside an Authorization header")
	}
}

func TestRefusedSealedSessionClearsItsCookie(t *testing.T) {
	for _, refusal := range []error{
		browsersession.ErrNotFound, browsersession.ErrMalformedCookie, browsersession.ErrRevoked,
		browsersession.ErrExpired, browsersession.ErrIdle, browsersession.ErrSealedUnavailable,
	} {
		t.Run(refusal.Error(), func(t *testing.T) {
			store := sealedStoreFixture()
			store.sealedErr = refusal
			recorder, user := serveSealed(t, store, sealedRequest{path: surfaceRoute, sealed: true})
			if recorder.Code != http.StatusUnauthorized || user != nil {
				t.Fatalf("status %d, principal %v; want 401", recorder.Code, user)
			}
			assertSealedCookieCleared(t, recorder)
		})
	}

	// A store that did not answer says nothing about the session: 503, and
	// the cookie stays.
	store := sealedStoreFixture()
	store.sealedErr = errors.New("connection refused")
	recorder, _ := serveSealed(t, store, sealedRequest{path: surfaceRoute, sealed: true})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d; want 503 when the store cannot answer", recorder.Code)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("an unanswered store cleared cookies %v", cookies)
	}
}

// A store that cannot read sealed sessions refuses the cookie on the surface.
func TestSealedCookieWithoutASealedStore(t *testing.T) {
	router := chi.NewRouter()
	store := &stubSessionStore{}
	router.With(Auth(AuthConfig{PrincipalValidator: &recordingPrincipals{}, SessionStore: store})).
		Get(surfaceRoute, func(http.ResponseWriter, *http.Request) { t.Error("served") })
	request := httptest.NewRequest(http.MethodGet, surfaceRoute, nil)
	request.AddCookie(&http.Cookie{Name: browsersession.SealedCookieName, Value: sealedValue})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d; want 401", recorder.Code)
	}
	assertSealedCookieCleared(t, recorder)
}

func assertSealedCookieCleared(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == browsersession.SealedCookieName {
			if cookie.MaxAge >= 0 || cookie.Value != "" || !cookie.Secure || !cookie.HttpOnly ||
				cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
				t.Errorf("the clearing cookie %+v does not replace the sealed cookie", cookie)
			}
			return
		}
	}
	t.Errorf("the refused sealed cookie was not cleared: %v", recorder.Result().Cookies())
}

// The cookie the sealed sign-in writes, and the one Auth clears: the four
// attributes of ADR-0059 R2.7a (Secure, HttpOnly, SameSite=Strict, Path=/
// with no Domain), on a name distinct from the ordinary session's.
func TestSealedCookieHasTheFourAttributes(t *testing.T) {
	recorder := httptest.NewRecorder()
	browsersession.SetSealedCookie(recorder, sealedValue, time.Hour)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != browsersession.SealedCookieName || cookie.Name == browsersession.CookieName ||
		!strings.HasPrefix(cookie.Name, "__Host-") {
		t.Fatalf("cookie name %q", cookie.Name)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("sealed cookie %+v lacks an attribute", cookie)
	}
}
