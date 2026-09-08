package configurations_test

// A platform model's PROVIDER LINK, end to end against a real PostgreSQL.
//
// ## The state these tests close
//
// A platform model is a row in the public project's schema with `shared = true`
// and a `data.ai_credentials` link naming a published platform credential. A
// row with no link is not a lenient variant of that: `ai_credentials` is
// required on all five model types, so provider admission refuses it and stores
// `status_ok = false`, and every reader — the model catalogue, the tier
// defaults and the LLM gateway — selects on `status_ok = true`. The row is
// therefore written, listed by the admin panel alone, and served to nobody.
//
// The create already refused that body for the schema-required field. What it
// did not refuse was reaching the same row in two steps: publish a linked model,
// then send an update whose `data` omits the link. The `data` column is replaced
// whole, so the second write removed it and answered 200.
//
// ## Why against a real database
//
// Each of these answers depends on a row the request itself does not carry: the
// link is checked against the platform's published credentials, the refusal
// names them, and the "nothing was written" half is only visible in the table.
// A handler test with a fake pool would state the branch and not the outcome.

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

// platformLinkRouter mounts the two platform surfaces AND the generic project
// update route, because the two refusals under test live on opposite sides of
// that boundary: the platform surface names the providers an operator can pick,
// and the generic route applies the schema's own required-field rule to the
// `data` object of any model row.
func platformLinkRouter(pool *pgxpool.Pool) chi.Router {
	handler := configurations.NewHandler(pool,
		configurations.WithPublicProjectID(globalScopeProject))
	r := chi.NewRouter()
	r.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
		r.Mount("/platform_models", handler.GlobalModelRoutes())
	})
	r.Put("/configurations/configuration/{projectID}/{configID}", handler.Update)
	return r
}

// platformLinkSend runs one request with a JSON body.
func platformLinkSend(
	t *testing.T, router chi.Router, method, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// platformLinkProvider publishes one platform credential and returns its title.
func platformLinkProvider(t *testing.T, router chi.Router, title string) string {
	t.Helper()
	recorder := platformLinkSend(t, router, http.MethodPost, "/gateway/providers", map[string]any{
		"elitea_title": title,
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("publishing the platform provider = %d, want 201; body = %s",
			recorder.Code, recorder.Body.String())
	}
	return title
}

// listedPlatformModel is the one row of the admin listing a test asserts on.
type listedPlatformModel struct {
	ID                 int    `json:"id"`
	Name               string `json:"elitea_title"`
	CredentialName     string `json:"credential_name"`
	CredentialResolves bool   `json:"credential_resolves"`
	LowTier            bool   `json:"low_tier"`
	HighTier           bool   `json:"high_tier"`
}

// readPlatformModel reads the listing and returns the row with this title.
func readPlatformModel(t *testing.T, router chi.Router, title string) listedPlatformModel {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/gateway/platform_models", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /gateway/platform_models = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []listedPlatformModel `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the listing %q: %v", recorder.Body.String(), err)
	}
	for _, item := range body.Items {
		if item.Name == title {
			return item
		}
	}
	t.Fatalf("the listing carries no model called %q: %s", title, recorder.Body.String())
	return listedPlatformModel{}
}

// storedModelData reads one row's `data` column, by title.
func storedModelData(t *testing.T, pool *pgxpool.Pool, title string) map[string]any {
	t.Helper()
	var raw []byte
	statement := fmt.Sprintf(
		"SELECT data FROM p_%d.configuration WHERE elitea_title = $1", globalScopeProject)
	if err := pool.QueryRow(context.Background(), statement, title).Scan(&raw); err != nil {
		t.Fatalf("read the stored data for %q: %v", title, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the stored data for %q: %v", title, err)
	}
	return decoded
}

// TestAPlatformModelMustNameAPublishedProvider.
//
// The create half. The refusal has to name the providers that WOULD have
// worked: an operator who reaches it is looking at a select they left alone,
// and "ai_credentials is required" says which field without saying what to put
// in it.
func TestAPlatformModelMustNameAPublishedProvider(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_link_provider")

	const unlinked = "autotest_unlinked_model"
	refusal := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": unlinked,
			"type":         "llm_model",
			"data":         map[string]any{"name": "autotest-unlinked"},
		})
	if refusal.Code != http.StatusBadRequest {
		t.Fatalf("a platform model naming no provider = %d, want 400; body = %s",
			refusal.Code, refusal.Body.String())
	}
	if !strings.Contains(refusal.Body.String(), provider) {
		t.Errorf("the refusal does not name the provider that would have worked: %s",
			refusal.Body.String())
	}
	// Refused, not stored-and-flagged. A row here would be advertised to the
	// admin panel and to nothing else.
	assertNoConfigurationRow(t, pool, unlinked)
}

// TestAPlatformModelsTierFlagsAreStoredAndReadBack.
//
// `low_tier` and `high_tier` decide whether a project's AI configuration offers
// the model as a tier default. The edit dialog rewrites `data` whole, so the
// listing has to report them or every save would clear them — the same shape as
// the link this file's other tests pin.
func TestAPlatformModelsTierFlagsAreStoredAndReadBack(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_tier_provider")

	const title = "autotest_high_tier_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": title,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-high-tier",
				"ai_credentials": map[string]any{"elitea_title": provider},
				"high_tier":      true,
				"low_tier":       false,
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /gateway/platform_models = %d, want 201; body = %s",
			created.Code, created.Body.String())
	}

	listed := readPlatformModel(t, router, title)
	if listed.CredentialName != provider || !listed.CredentialResolves {
		t.Errorf("credential_name = %q, credential_resolves = %v; want the published provider, resolving",
			listed.CredentialName, listed.CredentialResolves)
	}
	if !listed.HighTier || listed.LowTier {
		t.Errorf("high_tier = %v, low_tier = %v; want the flags the create sent",
			listed.HighTier, listed.LowTier)
	}

	// Stored, not merely echoed by the listing's own reader.
	stored := storedModelData(t, pool, title)
	if stored["high_tier"] != true {
		t.Errorf("the stored data holds high_tier = %#v, want true", stored["high_tier"])
	}
}

// TestAnUnlinkedModelRowIsListedAsUnresolved.
//
// A row that predates the write-time refusal, or that a seed wrote straight
// into the table, still exists. The panel is the only place it is visible, so
// the listing has to call it unresolved rather than reporting the pre-#451
// prefix fallback as a working configuration.
func TestAnUnlinkedModelRowIsListedAsUnresolved(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)

	const title = "autotest_legacy_unlinked_model"
	statement := fmt.Sprintf(`
		INSERT INTO p_%d.configuration
			(uuid, project_id, elitea_title, label, type, section, data, meta, shared,
			 status_ok, source, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $2, 'llm_model', 'llm',
			 '{"name":"autotest-legacy"}'::jsonb, '{}'::jsonb, true, true, 'user', now(), now())`,
		globalScopeProject)
	if _, err := pool.Exec(context.Background(), statement, globalScopeProject, title); err != nil {
		t.Fatalf("seed the unlinked model row: %v", err)
	}

	listed := readPlatformModel(t, router, title)
	if listed.CredentialName != "" {
		t.Errorf("credential_name = %q, want empty for a row that names none", listed.CredentialName)
	}
	if listed.CredentialResolves {
		t.Error("a row naming no provider at all was listed as resolving")
	}
}

// TestClearingAPlatformModelsProviderIsRefused.
//
// The two-step route to the same undispatchable row: publish a linked model,
// then update it with a `data` that omits the link. It answered 200 and removed
// the link, because the `data` column is replaced whole.
//
// Both entrances are asserted. The platform surface answers first and names the
// providers; the generic project route — which any API client can call with the
// same row id — applies the schema's required-field rule to the `data` the body
// carries. Fixing one and reading the other as "the same rule, so it must be
// fine" is how the second half of a paired defect survives a repair.
func TestClearingAPlatformModelsProviderIsRefused(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_clear_provider")

	const title = "autotest_clearable_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": title,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-clearable",
				"ai_credentials": map[string]any{"elitea_title": provider},
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /gateway/platform_models = %d, want 201; body = %s",
			created.Code, created.Body.String())
	}
	var stored struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode the created model %q: %v", created.Body.String(), err)
	}

	platformRefusal := platformLinkSend(t, router, http.MethodPut,
		fmt.Sprintf("/gateway/platform_models/%d", stored.ID), map[string]any{
			"elitea_title": title,
			"data":         map[string]any{"name": "autotest-clearable"},
		})
	if platformRefusal.Code != http.StatusBadRequest {
		t.Fatalf("clearing the link through the admin surface = %d, want 400; body = %s",
			platformRefusal.Code, platformRefusal.Body.String())
	}
	if !strings.Contains(platformRefusal.Body.String(), provider) {
		t.Errorf("the refusal does not name the provider that would have worked: %s",
			platformRefusal.Body.String())
	}

	genericRefusal := platformLinkSend(t, router, http.MethodPut,
		fmt.Sprintf("/configurations/configuration/%d/%d", globalScopeProject, stored.ID),
		map[string]any{"data": map[string]any{"name": "autotest-clearable"}})
	if genericRefusal.Code != http.StatusBadRequest {
		t.Fatalf("clearing the link through the project route = %d, want 400; body = %s",
			genericRefusal.Code, genericRefusal.Body.String())
	}
	if !strings.Contains(genericRefusal.Body.String(), "ai_credentials") {
		t.Errorf("the refusal does not name the field the update would remove: %s",
			genericRefusal.Body.String())
	}

	// The row still holds what it held. A refusal that wrote first and refused
	// afterwards would leave exactly the state it was refusing.
	link, _ := storedModelData(t, pool, title)["ai_credentials"].(map[string]any)
	if link == nil || link["elitea_title"] != provider {
		t.Errorf("the stored link = %#v, want the provider the create named", link)
	}
}

// TestARenameStillNeedsNoProviderRestated.
//
// The partial-update contract, which the rule above must not have taken away.
// An update that carries no `data` writes no `data` column, so there is nothing
// it can remove — and requiring the link there would mean an operator could not
// rename a model without restating its provider.
func TestARenameStillNeedsNoProviderRestated(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_rename_provider")

	const title = "autotest_renameable_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": title,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-renameable",
				"ai_credentials": map[string]any{"elitea_title": provider},
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /gateway/platform_models = %d, want 201; body = %s",
			created.Code, created.Body.String())
	}
	var stored struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode the created model %q: %v", created.Body.String(), err)
	}

	const renamed = "autotest_renamed_model"
	update := platformLinkSend(t, router, http.MethodPut,
		fmt.Sprintf("/gateway/platform_models/%d", stored.ID),
		map[string]any{"elitea_title": renamed})
	if update.Code != http.StatusOK {
		t.Fatalf("renaming a platform model = %d, want 200; body = %s",
			update.Code, update.Body.String())
	}
	link, _ := storedModelData(t, pool, renamed)["ai_credentials"].(map[string]any)
	if link == nil || link["elitea_title"] != provider {
		t.Errorf("the rename changed the stored link to %#v", link)
	}
}
