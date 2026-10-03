package api

// The SCIM tree is mounted OUTSIDE the session Auth group (shared migration
// 0134). These tests drive the production router, so they fail if the tree
// moves back under that group: a personal access token would then reach the
// general Auth middleware, and its refusal would not carry the SCIM sentence.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	scimapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/scim"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

type discardAuditRecorder struct{}

func (discardAuditRecorder) Record(context.Context, audit.Event) {}

type countingAuditRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *countingAuditRecorder) Record(_ context.Context, event audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *countingAuditRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// A caller with no credential must not be able to write audit rows. Both SCIM
// routes are reachable without a session, so an audit middleware above the
// credential check would turn every refused request into a row.
func TestRefusedSCIMRequestsWriteNoAuditRow(t *testing.T) {
	recorder := &countingAuditRecorder{}
	router := NewRouter(RouterConfig{Pool: &pgxpool.Pool{}, AuditRecorder: recorder})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, scimapi.BasePath+"/Users", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodDelete, scimapi.BasePath+"/Users/1", nil),
		httptest.NewRequest(http.MethodGet, scimapi.BasePath+"/Users", nil),
		func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, scimapi.TokenPath, strings.NewReader("grant_type=client_credentials"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}(),
		func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, scimapi.TokenPath, strings.NewReader("grant_type=password"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}(),
	} {
		request.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatalf("%s %s = %d, want a refusal", request.Method, request.URL.Path, response.Code)
		}
	}
	if got := recorder.count(); got != 0 {
		t.Fatalf("refused SCIM requests wrote %d audit rows, want 0", got)
	}
}

func TestSCIMRefusesPersonalAccessTokensAndSessionsThroughTheRouter(t *testing.T) {
	router := NewRouter(RouterConfig{Pool: &pgxpool.Pool{}, AuditRecorder: discardAuditRecorder{}})

	for name, prepare := range map[string]func(*http.Request){
		"personal access token": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln")
		},
		"browser session": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "elitea_session", Value: "opaque-session-id"})
		},
		"no credential": func(*http.Request) {},
	} {
		for _, path := range []string{"/Users", "/Groups", "/ServiceProviderConfig"} {
			t.Run(name+" "+path, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, scimapi.BasePath+path, nil)
				prepare(request)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				if response.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body %s", response.Code, response.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v", err)
				}
				if body["detail"] != scimapi.RefusalDetail {
					t.Fatalf("detail = %v, want the SCIM refusal; the tree is behind another authenticator", body["detail"])
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
				}
			})
		}
	}
}

// The token endpoint answers a client with no session. A request with no
// client credential is refused by the endpoint itself, with an RFC 6749 body,
// and never redirected to a login form.
func TestSCIMTokenEndpointIsReachableWithoutASession(t *testing.T) {
	router := NewRouter(RouterConfig{Pool: &pgxpool.Pool{}, AuditRecorder: discardAuditRecorder{}})

	request := httptest.NewRequest(http.MethodPost, scimapi.TokenPath, strings.NewReader("grant_type=client_credentials"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", response.Code, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != "invalid_client" {
		t.Fatalf("error = %q, want invalid_client", body["error"])
	}
}
