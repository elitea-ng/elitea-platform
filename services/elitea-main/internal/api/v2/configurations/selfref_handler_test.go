package configurations_test

// selfref_handler_test.go — handler-level regression tests for circular-routing
// guard #1 (spec §2.6). selfref_test.go covers the pure validateNotSelfReferential
// function; these tests prove the Create and Update HTTP handlers actually CALL it.
// Deleting either guard call in handler.go makes these tests fail.
//
// Why this works without a database: in both handlers the guard runs on the
// decoded request body BEFORE any h.pool use, so a self-referential api_base is
// rejected with 400 + SELF_REFERENTIAL_CREDENTIAL while the pool is still nil.
// If the guard call is removed, execution falls through to the DB layer and the
// nil pool panics — serveRecovering turns that panic into a recorded failure
// instead of crashing the test binary.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

// selfOrigin is the platform's own /llm origin for these tests.
const selfOrigin = "https://self.elitea.test/llm/v1"

// TestMain sets ELITEA_SELF_LLM_ORIGINS before any test runs.
//
// The production helper selfLLMOrigins() memoises with sync.Once, so a per-test
// t.Setenv would be defeated by whichever test happened to warm the cache first.
// Setting the variable for the whole test binary here is deterministic and needs
// no production seam. Nothing else in this package reads the variable, so this
// does not affect other tests.
func TestMain(m *testing.M) {
	_ = os.Setenv("ELITEA_SELF_LLM_ORIGINS", selfOrigin)
	os.Exit(m.Run())
}

// serveRecovering serves the request and converts a handler panic (which is what
// a nil *pgxpool.Pool produces once execution reaches the DB layer) into a
// recorded marker, so an escaped guard is a normal test failure rather than a
// crashed binary.
func serveRecovering(r http.Handler, req *http.Request) (rec *httptest.ResponseRecorder, panicked bool) {
	rec = httptest.NewRecorder()
	defer func() {
		if p := recover(); p != nil {
			panicked = true
		}
	}()
	r.ServeHTTP(rec, req)
	return rec, false
}

// selfRefRouter carries an authenticated, entitled caller because since #496
// every route it exercises is gated. The guard under test runs INSIDE the
// handler, so the request has to pass the gate to reach it — and serveRecovering
// below is what proves the guard, not the gate, produced the 400.
func selfRefRouter() *chi.Mux {
	h := handler.NewHandler(nil, handler.WithPermissionResolver(entitledResolver()))
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Mount("/api/v2", h.Routes())
	return r
}

func assertSelfRefRejected(t *testing.T, r http.Handler, req *http.Request) {
	t.Helper()
	rec, panicked := serveRecovering(r, req)
	if panicked {
		t.Fatalf("handler reached the DB layer (nil pool panic) — the self-referential guard did not run")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from the self-referential guard, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), handler.SelfReferentialCredentialReason) {
		t.Fatalf("expected %q in response body, got: %s",
			handler.SelfReferentialCredentialReason, rec.Body.String())
	}
}

func mustJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return bytes.NewReader(b)
}

// ---- Create -----------------------------------------------------------------

func TestCreate_RejectsSelfReferentialAPIBase(t *testing.T) {
	r := selfRefRouter()

	// azure_open_ai passes validateConfigData when api_base is present, so the
	// next gate reached is the self-referential guard.
	// `label` is present because the create route now refuses a body that
	// omits a schema-required field before it reaches any other gate
	// (required_fields.go). The subject here is still api_base.
	body := mustJSON(t, map[string]any{
		"elitea_title": "loop-cred",
		"label":        "loop-cred",
		"type":         "azure_open_ai",
		"data": map[string]any{
			"api_base": selfOrigin,
			"api_key":  "sk-test",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/configurations/1", body)
	req.Header.Set("Content-Type", "application/json")

	assertSelfRefRejected(t, r, req)
}

func TestCreate_RejectsSelfReferentialAPIBaseSubPath(t *testing.T) {
	r := selfRefRouter()

	body := mustJSON(t, map[string]any{
		"elitea_title": "loop-cred",
		"label":        "loop-cred",
		"type":         "azure_open_ai",
		"data": map[string]any{
			// Segment-prefix match on the configured self origin.
			"api_base": "https://SELF.elitea.test/llm/v1/chat/",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/configurations/mode/1", body)
	req.Header.Set("Content-Type", "application/json")

	assertSelfRefRejected(t, r, req)
}

// TestCreate_NonSelfReferentialPassesGuard is the negative control: an upstream
// api_base must NOT be rejected by the guard. It falls through to the DB layer,
// which panics on the nil pool — proving the guard let it past rather than the
// assertions above passing for some unrelated reason.
func TestCreate_NonSelfReferentialPassesGuard(t *testing.T) {
	r := selfRefRouter()

	// Complete, so the control still reaches the guard: a body missing a
	// schema-required field is refused before it, and a refusal for the wrong
	// reason would make this control pass while proving nothing.
	body := mustJSON(t, map[string]any{
		"elitea_title": "upstream-cred",
		"label":        "upstream-cred",
		"type":         "azure_open_ai",
		"data": map[string]any{
			"api_base": "https://api.openai.com/v1",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/configurations/1", body)
	req.Header.Set("Content-Type", "application/json")

	rec, panicked := serveRecovering(r, req)
	if !panicked && strings.Contains(rec.Body.String(), handler.SelfReferentialCredentialReason) {
		t.Fatalf("upstream api_base was wrongly rejected as self-referential: %s", rec.Body.String())
	}
}

// ---- Update -----------------------------------------------------------------

func TestUpdate_RejectsSelfReferentialAPIBase(t *testing.T) {
	r := selfRefRouter()

	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"api_base": selfOrigin,
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/configuration/1/42", body)
	req.Header.Set("Content-Type", "application/json")

	assertSelfRefRejected(t, r, req)
}

func TestUpdate_RejectsSelfReferentialAPIBaseWithDefaultPort(t *testing.T) {
	r := selfRefRouter()

	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			// Default port + trailing slash normalise to the configured origin.
			"api_base": "https://self.elitea.test:443/llm/v1/",
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/configuration/mode/1/42", body)
	req.Header.Set("Content-Type", "application/json")

	assertSelfRefRejected(t, r, req)
}

// TestUpdate_NonSelfReferentialPassesGuard is the Update-side negative control.
func TestUpdate_NonSelfReferentialPassesGuard(t *testing.T) {
	r := selfRefRouter()

	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"api_base": "https://api.openai.com/v1",
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/configuration/1/42", body)
	req.Header.Set("Content-Type", "application/json")

	rec, panicked := serveRecovering(r, req)
	if !panicked && strings.Contains(rec.Body.String(), handler.SelfReferentialCredentialReason) {
		t.Fatalf("upstream api_base was wrongly rejected as self-referential: %s", rec.Body.String())
	}
}
