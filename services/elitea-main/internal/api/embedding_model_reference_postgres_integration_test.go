package api

// A toolkit may name an embedding model, and the save-time gate refuses a name
// the project cannot see. This file proves the WHOLE chain that makes the
// accepted half possible, because every link in it is written in a different
// place and each one looks right on its own:
//
//	create the credential ──► create the embedding model ──► admission decides
//	    status_ok ──► the model catalogue lists the row (it selects
//	    status_ok = true) ──► the toolkit gate finds the NAME and admits the save
//
// The name the toolkit carries is `data.name`, NOT the row's `elitea_title`
// (infra/db/repos/models.go maps the catalogue item's Name from the data
// object for every section except vectorstorage). A fixture that passes the
// title is refused for a model that plainly exists, which is the specific way
// this chain fails silently.
//
// # WHY IT IS HERE AND NOT IN A HANDLER TEST
//
// Three separate compositions have to be present at once — the provider
// admission that writes status_ok, the toolkit settings validator, and the
// Configurations runtime both are built from — and a handler test can prove
// none of them. This is also the shape the browser suite's own fixture uses
// (apps/elitea-web/e2e/fixtures/configurations.ts), so a change that breaks
// the fixture fails here first, with the failing link named.

import (
	"context"
	"net/http"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

const (
	embeddingReferenceCredential = "autotest_embed_credential"
	embeddingReferenceTitle      = "autotest_embed_model"
	// The name the catalogue publishes, deliberately DIFFERENT from the row's
	// title: a gate that matched on the title would pass with them equal.
	embeddingReferenceModelName = "autotest-embed-name"
	// The toolkit's own credential. It is created through the route as well,
	// so this file depends on no other file's seed.
	embeddingReferenceGithub = "autotest_embed_github"
)

func TestAToolkitCanNameAnEmbeddingModelTheProjectCanSee(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	// The vault the Configurations runtime opens for project 1. Absence, not
	// emptiness, is what fails a resolve (internal/infra/storage).
	if err := v2secrets.NewHandler(pool).EnsureProjectVault(ctx, "1"); err != nil {
		t.Fatalf("provision the project 1 vault: %v", err)
	}

	configurations, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, 1, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(configurations.Destroy)
	admission, err := runtimecomposition.NewCurrentProviderAdmission(configurations, true)
	if err != nil {
		t.Fatalf("compose the provider admission: %v", err)
	}
	validator, err := configurations.NewToolkitSettingsValidator(pool)
	if err != nil {
		t.Fatalf("compose the toolkit settings validator: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:                     pool,
		AuthValidator:            apimw.TokenValidator(credentialJourneyValidator{}),
		PrincipalValidator:       testPrincipalValidator{},
		WebhookRepo:              emptyWebhookRepo{},
		EventSource:              newEventSource(),
		ConfigProviderAdmission:  admission,
		ToolkitSettingsValidator: validator,
	})

	// ── the credential the model links ─────────────────────────────────────
	status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/configurations/configurations/1", map[string]any{
			"elitea_title": embeddingReferenceCredential,
			"label":        embeddingReferenceCredential,
			"type":         "open_ai",
			"data":         map[string]any{"api_base": "https://autotest.invalid/v1"},
		})
	if status != http.StatusCreated {
		t.Fatalf("creating the credential answered %d, want 201. Body: %s", status, body)
	}

	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/configurations/configurations/1", map[string]any{
			"elitea_title": embeddingReferenceGithub,
			"label":        embeddingReferenceGithub,
			"type":         "github",
			"data":         map[string]any{"base_url": "https://autotest.invalid/api"},
		})
	if status != http.StatusCreated {
		t.Fatalf("creating the toolkit credential answered %d, want 201. Body: %s", status, body)
	}

	// ── the embedding model ────────────────────────────────────────────────
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/configurations/configurations/1", map[string]any{
			"elitea_title": embeddingReferenceTitle,
			"label":        embeddingReferenceTitle,
			"type":         "embedding_model",
			"data": map[string]any{
				"name":           embeddingReferenceModelName,
				"ai_credentials": map[string]any{"elitea_title": embeddingReferenceCredential, "private": false},
			},
		})
	if status != http.StatusCreated {
		t.Fatalf("creating the embedding model answered %d, want 201. Body: %s", status, body)
	}
	created := decodeJourneyJSON(t, body)
	if created["section"] != "embedding" {
		t.Fatalf("the embedding model was filed under %v, want \"embedding\"; "+
			"the model catalogue selects on the section and finds nothing else", created["section"])
	}
	if created["status_ok"] != true {
		t.Fatalf("the embedding model was stored with status_ok = %v.\n"+
			"  The catalogue selects status_ok = true, so the toolkit gate below cannot see it.\n"+
			"  Body: %s", created["status_ok"], body)
	}

	// ── the toolkit that names it ──────────────────────────────────────────
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", embeddingToolkitBody(
			"autotest_embed_toolkit", embeddingReferenceModelName))
	if status != http.StatusCreated {
		t.Fatalf("a toolkit naming a visible embedding model answered %d, want 201.\n"+
			"  Body: %s", status, body)
	}

	// ── the control: a name the project cannot see ─────────────────────────
	// Without it the acceptance above is satisfied by a gate that admits
	// everything, which is exactly the state this platform shipped in before
	// the validator was composed.
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", embeddingToolkitBody(
			"autotest_embed_toolkit_unknown", "autotest-no-such-model"))
	if status != http.StatusBadRequest {
		t.Fatalf("a toolkit naming a model the project cannot see answered %d, want 400. Body: %s",
			status, body)
	}
	assertAdmissionFieldError(t, body, "embedding_model", "configuration_model_not_found")

	// ── the trap this file exists for ──────────────────────────────────────
	// The row's TITLE is not the name the catalogue publishes. A fixture that
	// sends it is refused for a model that plainly exists.
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", embeddingToolkitBody(
			"autotest_embed_toolkit_by_title", embeddingReferenceTitle))
	if status != http.StatusBadRequest {
		t.Fatalf("a toolkit naming the model by its ROW TITLE answered %d, want 400: "+
			"the catalogue publishes data.name. Body: %s", status, body)
	}
}

func embeddingToolkitBody(name, embeddingModel string) map[string]any {
	return map[string]any{
		"name": name,
		"type": "github",
		"settings": map[string]any{
			"repository":           "octocat/hello-world",
			"github_configuration": map[string]any{"elitea_title": embeddingReferenceGithub, "private": false},
			// A plain STRING, not a configuration reference: the schema
			// declares this field `configuration_model`, and the two shapes are
			// not interchangeable.
			"embedding_model": embeddingModel,
		},
	}
}
