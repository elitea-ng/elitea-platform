package toolkits

// The projections against a real database, through the real route.
//
// The unit tests above drive the projection functions. This one drives the
// SERVED BODY: rows are written into the two tables an operator actually edits,
// and the assertion is on what `GET /elitea_core/toolkits/prompt_lib/{id}`
// answers. That is the endpoint the create page and the MCP page both read, so
// a projection that is correct in isolation and unreachable through the handler
// would still leave both screens empty — which is the state validation gap 1
// records.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// blockingGuardrails refuses the toolkit types it is given.
type blockingGuardrails struct{ blocked []string }

func (g blockingGuardrails) GuardrailPolicy(context.Context) (guardrails.Policy, error) {
	return guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: g.blocked}), nil
}

// projectionPool applies the SHARED migration corpus, which is what creates
// both `elitea_mcp.prebuilt_servers` (0094) and the `provider_hub` schema
// (0107/0109). Applying the real corpus rather than the two files by hand is
// what makes this test notice a migration that stops creating them.
func projectionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	// The baseline first: shared/0030 references `centry`, which the bootstrap
	// schema creates. This is the same order every other integration fixture in
	// this package uses, and the fresh-install bootstrap gap it records.
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	return pool
}

func servedCatalogue(t *testing.T, handler *Handler) map[string]map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ListTypeSchemas(recorder, httptest.NewRequest(http.MethodGet,
		"/elitea_core/toolkits/prompt_lib/1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ListTypeSchemas answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var catalogue map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalogue); err != nil {
		t.Fatalf("decode the served catalogue: %v", err)
	}
	return catalogue
}

func TestTheServedCatalogueCarriesTheRemoteMCPTypeOnAVirginDatabase(t *testing.T) {
	pool := projectionPool(t)
	catalogue := servedCatalogue(t, NewHandler(pool))

	schema, found := catalogue["mcp"]
	if !found {
		t.Fatalf("no `mcp` type is served; the MCP create page has nothing to offer: %v",
			keysOf(catalogue))
	}
	if schema["title"] != "mcp" {
		t.Errorf("title = %v; the web form keys the Remote MCP behaviour off this", schema["title"])
	}
	metadata, _ := schema["metadata"].(map[string]any)
	if metadata["label"] != "Remote MCP" {
		t.Errorf("label = %v", metadata["label"])
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, hasURL := properties["url"]; !hasURL {
		t.Errorf("the settings form has no URL field: %v", properties)
	}
	// Absence of an operator's rows is not a defect. An empty catalogue table
	// and an empty admission plane must both serve the built-in half plus this
	// one type, never an error and never a smaller catalogue.
	for _, builtIn := range []string{"github", "artifact", "openapi"} {
		if _, found := catalogue[builtIn]; !found {
			t.Errorf("the projection removed the built-in type %q", builtIn)
		}
	}
}

func TestAnEnabledCatalogueRowIsServedAsACreatableType(t *testing.T) {
	pool := projectionPool(t)
	ctx := context.Background()
	store := newPrebuiltStoreForTest(t, pool)

	if _, err := store.Upsert(ctx, prebuiltRow("context7", "Context7", "https://ctx7.example/mcp", true)); err != nil {
		t.Fatalf("seed the catalogue: %v", err)
	}
	if _, err := store.Upsert(ctx, prebuiltRow("switched_off", "Switched Off", "https://off.example/mcp", false)); err != nil {
		t.Fatalf("seed the catalogue: %v", err)
	}

	catalogue := servedCatalogue(t, NewHandler(pool))

	schema, found := catalogue["mcp_context7"]
	if !found {
		t.Fatalf("an enabled catalogue row is not offered: %v", keysOf(catalogue))
	}
	metadata, _ := schema["metadata"].(map[string]any)
	if metadata["label"] != "Context7" {
		t.Errorf("label = %v", metadata["label"])
	}
	if _, found := catalogue["mcp_switched_off"]; found {
		t.Error("a disabled catalogue row is offered as a creatable type")
	}
}

func TestAnAdmittedProvidersToolkitsAreServedAsCreatableTypes(t *testing.T) {
	pool := projectionPool(t)
	ctx := context.Background()

	manifest := []byte(`{
	  "name": "inventory",
	  "service_location_url": "https://elitea-inventory:8080",
	  "provided_toolkits": [{
	    "name": "Inventory",
	    "description": "Knowledge graph",
	    "toolkit_config": {
	      "type": "Config",
	      "fields_order": ["bucket"],
	      "parameters": {"bucket": {"type": "String", "required": true, "description": "the bucket"}}
	    },
	    "provided_tools": [{"name": "run_ingestion", "description": "build it",
	      "args_schema": {"toolkit_id": {"type": "Integer", "required": true}}}],
	    "toolkit_metadata": {"type_override": "inventory"}
	  }]
	}`)
	// publicproject.ID() defaults to 1 when no variable is set, which is the
	// project a facade registers under on a default deployment.
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 1, ProviderID: "inventory", Origin: "https://elitea-inventory:8080",
		Manifest: manifest, Actor: "facade:inventory",
	}); err != nil {
		t.Fatalf("register the provider: %v", err)
	}

	// `record` is the shipped posture and the one the standalone stack runs.
	t.Setenv("ELITEA_PROVIDER_ADMISSION", "record")
	catalogue := servedCatalogue(t, NewHandler(pool))

	schema, found := catalogue["inventory"]
	if !found {
		t.Fatalf("an admitted provider's toolkit is not offered: %v", keysOf(catalogue))
	}
	metadata, _ := schema["metadata"].(map[string]any)
	if metadata["admission_status"] != "inactive" {
		t.Errorf("a type offered on a recorded-not-in-force decision must say so: %v", metadata)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, found := properties["toolkit_configuration_bucket"]; !found {
		t.Errorf("the provider's own parameter is missing: %v", keysOfAny(properties))
	}
	selected, _ := properties["selected_tools"].(map[string]any)
	argsSchemas, _ := selected["args_schemas"].(map[string]any)
	if _, found := argsSchemas["run_ingestion"]; !found {
		t.Errorf("the provider's tool has no argument schema: %v", argsSchemas)
	}

	// Under `enforce` the same inactive revision must disappear, exactly as the
	// request-path gate refuses it.
	t.Setenv("ELITEA_PROVIDER_ADMISSION", "enforce")
	if _, found := servedCatalogue(t, NewHandler(pool))["inventory"]; found {
		t.Error("an inactive provider is offered under the `enforce` posture")
	}
}

// Guardrails run AFTER the merge, so a deny-list an operator wrote covers a
// projected type exactly as it covers a built-in one. A projection that
// composed after guardrails would be a way back in for a blocked type.
func TestGuardrailsStayTerminalOverAProjectedType(t *testing.T) {
	pool := projectionPool(t)
	ctx := context.Background()
	store := newPrebuiltStoreForTest(t, pool)
	if _, err := store.Upsert(ctx, prebuiltRow("context7", "Context7", "https://ctx7.example/mcp", true)); err != nil {
		t.Fatalf("seed the catalogue: %v", err)
	}

	handler := NewHandler(pool, WithGuardrails(blockingGuardrails{blocked: []string{"mcp", "mcp_context7"}}))
	catalogue := servedCatalogue(t, handler)

	for _, blocked := range []string{"mcp", "mcp_context7"} {
		if _, found := catalogue[blocked]; found {
			t.Errorf("the blocked type %q re-entered the catalogue through a projection", blocked)
		}
	}
	if _, found := catalogue["github"]; !found {
		t.Error("the deny-list removed a type nobody blocked")
	}
}

// newPrebuiltStoreForTest and prebuiltRow keep the seeding on the production
// writer: a fixture that INSERTed its own rows could satisfy a shape the admin
// save path never produces.
func newPrebuiltStoreForTest(t *testing.T, pool *pgxpool.Pool) *mcpregistry.PrebuiltStore {
	t.Helper()
	return mcpregistry.NewPrebuiltStore(pool)
}

func prebuiltRow(key, displayName, serverURL string, enabled bool) mcpregistry.PrebuiltServer {
	return mcpregistry.PrebuiltServer{
		Key: key, DisplayName: displayName, ServerURL: serverURL, Enabled: enabled,
	}
}
