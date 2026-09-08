package configurations_test

// The two platform CREATE routes, end to end against a real PostgreSQL.
//
// ## What this file proves that the unit tests cannot
//
// `global_row_label_test.go` proves the rewrite puts a label in the body. This
// file proves the whole path a browser takes: the admin panel's own body,
// through `/admin/gateway/providers`, past the schema-driven required-field
// rule (required_fields.go), into a row that is stored, labelled, filed under
// `ai_credentials` and shared — and then a platform MODEL naming that
// credential, through the second surface, for the same reasons.
//
// The regression it pins answered 400 for both. `label` is required by every
// type the pinned registry carries, and neither panel has a label field, so
// the required-field rule refused a body no control on those screens can fix.
// Only a request against a real database can show that the accepted body then
// really becomes the row the panel lists.
//
// It uses the CREATE harness (`newCreateEchoPool`), not this package's
// projection-backed one: the projection declares `uuid NOT NULL` with no
// default, so no create can run against it at all.
//
// No body here carries a schema-declared password. This harness composes no
// project vault, and the route answers 503 for a plaintext secret
// (secret_sealing.go) — the panel omits an empty key for the same reason it
// matters here, and a 503 in a file about required fields would read as this
// rule firing when it did not.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

// platformSurfaceRouter mounts both platform surfaces at the paths production
// uses. No permission middleware: the gate is applied at the mount in router.go
// and covered there.
func platformSurfaceRouter(pool *pgxpool.Pool) chi.Router {
	handler := configurations.NewHandler(pool,
		configurations.WithPublicProjectID(globalScopeProject))
	r := chi.NewRouter()
	r.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
		r.Mount("/platform_models", handler.GlobalModelRoutes())
	})
	return r
}

func platformSurfacePost(
	t *testing.T, router chi.Router, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// TestThePlatformPanelsBodiesAreCreated is the regression, in the order the
// operator meets it: publish a provider, then adopt a model that uses it.
func TestThePlatformPanelsBodiesAreCreated(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformSurfaceRouter(pool)

	const providerTitle = "autotest_platform_provider"
	// EXACTLY what the provider dialog sends: one name, the type, and the
	// provider's own data fields. No label, no section, no `shared`.
	provider := platformSurfacePost(t, router, "/gateway/providers", map[string]any{
		"elitea_title": providerTitle,
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if provider.Code != http.StatusCreated {
		t.Fatalf("POST /gateway/providers = %d, want 201; body = %s",
			provider.Code, provider.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(provider.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created provider %q: %v", provider.Body.String(), err)
	}
	// The label the surface completed is the title the operator typed — which
	// is the field this panel's own listing renders.
	if created["label"] != providerTitle {
		t.Errorf("label = %#v, want %q", created["label"], providerTitle)
	}
	if created["section"] != "ai_credentials" {
		t.Errorf("section = %#v, want %q", created["section"], "ai_credentials")
	}
	if created["shared"] != true {
		t.Errorf("shared = %#v, want true (a platform credential is always shared)", created["shared"])
	}
	// Stored, not merely echoed: an empty label column is the state the
	// required-field rule exists to prevent, and the echo is computed in the
	// same handler that writes the row.
	if stored := storedLabel(t, pool, providerTitle); stored != providerTitle {
		t.Errorf("the stored label = %q, want %q", stored, providerTitle)
	}

	const modelTitle = "autotest_platform_model"
	// EXACTLY what the platform-model dialog sends. The credential link names
	// the provider published above, which is the only kind of link this surface
	// admits.
	model := platformSurfacePost(t, router, "/gateway/platform_models", map[string]any{
		"elitea_title": modelTitle,
		"type":         "llm_model",
		"data": map[string]any{
			"name":           "autotest-model",
			"ai_credentials": map[string]any{"elitea_title": providerTitle},
		},
	})
	if model.Code != http.StatusCreated {
		t.Fatalf("POST /gateway/platform_models = %d, want 201; body = %s",
			model.Code, model.Body.String())
	}
	if stored := storedLabel(t, pool, modelTitle); stored != modelTitle {
		t.Errorf("the stored platform model label = %q, want %q", stored, modelTitle)
	}
}

// TestTheProjectPlaneStillRefusesAnUnlabelledCreate is the other half, and the
// one a fix at the wrong layer would have broken: the rule was NOT narrowed.
// A body naming neither a label nor anything to derive one from is still
// refused, on the route every API client and the reference implementation use.
func TestTheProjectPlaneStillRefusesAnUnlabelledCreate(t *testing.T) {
	pool := newCreateEchoPool(t)
	recorder := createEchoDo(t, createEchoRouter(pool), map[string]any{
		"elitea_title": "autotest_unlabelled_credential",
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "label") {
		t.Errorf("the refusal must name the missing field: %s", recorder.Body.String())
	}
	assertNoConfigurationRow(t, pool, "autotest_unlabelled_credential")
}

// storedLabel reads the label column of one row, by title.
func storedLabel(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var label *string
	statement := fmt.Sprintf(
		"SELECT label FROM p_%d.configuration WHERE elitea_title = $1", globalScopeProject)
	if err := pool.QueryRow(context.Background(), statement, title).Scan(&label); err != nil {
		t.Fatalf("read the stored label for %q: %v", title, err)
	}
	if label == nil {
		return ""
	}
	return *label
}
