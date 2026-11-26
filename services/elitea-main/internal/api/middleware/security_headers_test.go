package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

// The header must reach a plain JSON response. This is the case the eight
// per-handler copies did not cover, and it is the one that matters for
// go/reflected-xss: nosniff is what stops a browser reading a JSON body that
// carries caller text as a document.
func TestSecurityHeadersSetsNosniffOnAJSONResponse(t *testing.T) {
	t.Parallel()

	handler := apimw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A body that starts with markup and carries caller text. Without
		// nosniff a browser may render this rather than read it as JSON.
		_, _ = w.Write([]byte(`{"error":"<script>alert(1)</script>"}`))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// A refusal is written by many paths in this service, and several write no
// Content-Type at all. Those bodies are the sniffable ones, so the header has
// to be there even when the handler sets nothing.
func TestSecurityHeadersSetsNosniffWhenTheHandlerSetsNoContentType(t *testing.T) {
	t.Parallel()

	handler := apimw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil))

	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// The middleware must not take a header away from a handler that set its own.
// A handler that must be framed, or that owns a stricter policy, keeps its
// value. Without this the middleware could not go in front of every route.
func TestSecurityHeadersKeepsAHeaderTheHandlerSetItself(t *testing.T) {
	t.Parallel()

	handler := apimw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if got := recorder.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want the handler's SAMEORIGIN", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want the handler's value", got)
	}
	// The one the handler did not set still arrives.
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// The headers have to be written before the handler's first write. A header
// set after that never reaches the wire, so a middleware that wrote them on
// the way OUT would set them where nothing reads them.
func TestSecurityHeadersReachAResponseTheHandlerWroteImmediately(t *testing.T) {
	t.Parallel()

	handler := apimw.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No WriteHeader call: the first Write commits 200 and the header map.
		_, _ = w.Write([]byte("ok"))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := recorder.Result().Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff on a committed response", got)
	}
}

// HSTS is sent only when the public origin is https: a plain-HTTP stack must
// never pin a browser to TLS.
func TestStrictTransportSecurityFollowsThePublicOriginScheme(t *testing.T) {
	t.Parallel()

	for origin, want := range map[string]string{
		"https://elitea.example.com": apimw.StrictTransportSecurityValue,
		"HTTPS://Elitea.Example.com": apimw.StrictTransportSecurityValue,
		"http://localhost:8080":      "",
		"":                           "",
	} {
		handler := apimw.StrictTransportSecurity(origin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/health", nil))
		if got := recorder.Result().Header.Get("Strict-Transport-Security"); got != want {
			t.Errorf("origin %q: Strict-Transport-Security = %q, want %q", origin, got, want)
		}
	}
	if want := "max-age=63072000; includeSubDomains"; apimw.StrictTransportSecurityValue != want {
		t.Errorf("policy = %q, want %q", apimw.StrictTransportSecurityValue, want)
	}
}
