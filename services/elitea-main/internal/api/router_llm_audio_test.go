package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// The /llm/v1/audio routes through NewRouter, with the REAL edge proxy
// (llmproxy.Proxy, which applies the audio body ceiling) in front of a fake
// gateway. The audio_limits tests in internal/llmproxy call that proxy
// directly with a project already on the context, so they cannot see whether
// the /llm middleware chain runs first. These tests can.

const audioTestSessionSecret = "audio-test-session-secret"

// audioSessionCookie mints an elitea_session cookie the Auth middleware's
// legacy HMAC reader accepts (middleware/auth.go verifySessionCookie).
func audioSessionCookie(t *testing.T, uid int) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"uid": uid, "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("encode the session payload: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(audioTestSessionSecret))
	mac.Write([]byte(body))
	return &http.Cookie{Name: "elitea_session", Value: body + "." + hex.EncodeToString(mac.Sum(nil))}
}

type fixedPersonalProject struct {
	id  int
	err error
}

func (f fixedPersonalProject) PersonalProjectID(context.Context, string) (int, error) {
	return f.id, f.err
}

// audioGateway is the fake elitea-llm-gateway. It counts the requests that
// reached it and records the project the edge signed for the last one.
type audioGateway struct {
	server  *httptest.Server
	calls   atomic.Int64
	project atomic.Value
}

func newAudioGateway(t *testing.T) *audioGateway {
	t.Helper()
	g := &audioGateway{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls.Add(1)
		g.project.Store(r.Header.Get(llmproxy.HeaderProjectID))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	t.Cleanup(g.server.Close)
	return g
}

func audioRouter(t *testing.T, gateway *audioGateway, resolver fixedPersonalProject) http.Handler {
	t.Helper()
	proxy, err := llmproxy.New(llmproxy.Config{TargetURL: gateway.server.URL, Transport: http.DefaultTransport, IdentitySecret: "edge-secret"})
	if err != nil {
		t.Fatalf("build the edge proxy: %v", err)
	}
	return api.NewRouter(api.RouterConfig{
		PrincipalValidator:     testPrincipalValidator{},
		SessionSecret:          audioTestSessionSecret,
		GatewayProxy:           proxy,
		GatewayProjectResolver: resolver,
	})
}

func audioPost(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://app.example.com"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

var audioPaths = []string{"/llm/v1/audio/speech", "/llm/v1/audio/transcriptions", "/llm/v1/audio/translations"}

// (a) No credential: 401, and the gateway is never called.
func TestLLMAudio_NoCredentialIsRefusedBeforeTheProxy(t *testing.T) {
	gateway := newAudioGateway(t)
	router := audioRouter(t, gateway, fixedPersonalProject{id: 5})
	for _, path := range audioPaths {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, audioPost(path, `{"model":"m","input":"hi"}`))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401; body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if n := gateway.calls.Load(); n != 0 {
		t.Fatalf("the gateway was called %d times for unauthenticated audio requests", n)
	}
}

// (b) A valid session that names a project it does not belong to. The edge
// never bills the named project: the selector falls back to the caller's own
// project (spec-llm-project-scope §6, admitProjectSelector), and when the caller
// has no own project the request is refused before the proxy.
func TestLLMAudio_NonMemberSelectorNeverReachesTheNamedProject(t *testing.T) {
	t.Run("falls back to the caller's own project", func(t *testing.T) {
		gateway := newAudioGateway(t)
		router := audioRouter(t, gateway, fixedPersonalProject{id: 5})
		for _, path := range audioPaths {
			req := audioPost(path, `{"model":"m","input":"hi"}`)
			req.AddCookie(audioSessionCookie(t, 11))
			req.Header.Set("X-Project-Id", "4242")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
			}
			if got, _ := gateway.project.Load().(string); got != strconv.Itoa(5) {
				t.Fatalf("%s: the gateway was told project %q, want the caller's own project 5 (never 4242)", path, got)
			}
		}
	})
	t.Run("refused when the caller has no project of its own", func(t *testing.T) {
		gateway := newAudioGateway(t)
		router := audioRouter(t, gateway, fixedPersonalProject{err: errors.New("no personal project")})
		for _, path := range audioPaths {
			req := audioPost(path, `{"model":"m","input":"hi"}`)
			req.AddCookie(audioSessionCookie(t, 11))
			req.Header.Set("X-Project-Id", "4242")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("%s: status = %d, want a 4xx refusal; body=%s", path, rec.Code, rec.Body.String())
			}
		}
		if n := gateway.calls.Load(); n != 0 {
			t.Fatalf("the gateway was called %d times for a caller with no resolvable project", n)
		}
	})
}

// (c) An oversize Content-Length from an unauthenticated caller is a 401, not
// a 413: authentication runs before the edge's audio body ceiling.
func TestLLMAudio_AuthRunsBeforeTheBodyCeiling(t *testing.T) {
	gateway := newAudioGateway(t)
	router := audioRouter(t, gateway, fixedPersonalProject{id: 5})
	req := audioPost("/llm/v1/audio/transcriptions", "x")
	req.ContentLength = 1 << 30
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth before the 413 ceiling); body=%s", rec.Code, rec.Body.String())
	}

	// The discriminating half: an AUTHENTICATED oversize request gets the 413,
	// so the ceiling is really behind the auth check and not absent.
	req = audioPost("/llm/v1/audio/transcriptions", "x")
	req.AddCookie(audioSessionCookie(t, 11))
	req.Header.Set("X-Project-Id", "5")
	req.ContentLength = 1 << 30
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for an authenticated oversize upload; body=%s", rec.Code, rec.Body.String())
	}
	if n := gateway.calls.Load(); n != 0 {
		t.Fatalf("the gateway was called %d times", n)
	}
}

// A cookie-authenticated POST from another origin is refused before the proxy
// (CSRF from a same-site sibling). The same request with the SPA's project
// selector header, which a form cannot set, passes.
func TestLLMAudio_CookiePostFromAForeignOriginIsRefused(t *testing.T) {
	gateway := newAudioGateway(t)
	router := audioRouter(t, gateway, fixedPersonalProject{id: 5})
	for _, path := range audioPaths {
		req := audioPost(path, `{"model":"m","input":"hi"}`)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		req.AddCookie(audioSessionCookie(t, 11))
		req.Header.Set("Origin", "https://user-content.example.com")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403; body=%s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "cross_origin_request") {
			t.Fatalf("%s: the refusal does not name the cause: %s", path, rec.Body.String())
		}
	}
	if n := gateway.calls.Load(); n != 0 {
		t.Fatalf("the gateway was called %d times for cross-origin cookie POSTs", n)
	}

	req := audioPost("/llm/v1/audio/speech", `{"model":"m","input":"hi"}`)
	req.AddCookie(audioSessionCookie(t, 11))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Project-Id", "5")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the SPA's own request: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
