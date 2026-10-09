package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The desktop's resolved definition (ADR-0029 decision 5a) must not carry a
// secret, end to end: the real tenant-schema read, the real shared freeze with
// the real Configurations graph (reference mode, a configuration reference
// expanded), the real permission gate, the real route.
//
// THE CANARIES. A plaintext value in a toolkit setting the schema does not mark
// secret (the freeze lets it through — the worker needs it), a `{{secret.*}}`
// reference inside the credential the toolkit references, one in the
// instructions and one in an authored variable value. The test proves each is
// in what the database holds and absent from every byte of the response.
const (
	resolvedCanaryPlaintext   = "sk-resolved-canary-plaintext-58c2"
	resolvedCanaryVaultEntry  = "resolved_canary_vault_entry"
	resolvedCanaryInstruction = "resolved_canary_instruction"
	resolvedCanaryVariable    = "resolved_canary_variable"
)

func TestResolvedVersionCarriesNoSecretOverTheRealFreeze(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)
	seedResolvedVersionFixture(t, pool)

	configurations, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, 1, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(configurations.Destroy)
	service, err := runtimecomposition.NewClientApplicationVersionService(pool, configurations)
	if err != nil {
		t.Fatalf("compose the resolved version service: %v", err)
	}
	route, err := desktopopsapi.NewResolvedVersionRoute(service, apimw.AuthConfig{
		Validator:          apimw.TokenValidator(credentialJourneyValidator{}),
		PrincipalValidator: testPrincipalValidator{},
	}, legacyrbac.NewPostgresResolver(pool))
	if err != nil {
		t.Fatalf("compose the route: %v", err)
	}
	get := func(path string) (int, string) {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, testAuthHeader(request))
		return recorder.Code, recorder.Body.String()
	}

	status, body := get("/api/v2/elitea_core/resolved_version/prompt_lib/1/71/81")
	if status != http.StatusOK {
		t.Fatalf("resolving the seeded version answered %d, want 200. Body: %s", status, body)
	}
	for _, canary := range []string{
		resolvedCanaryPlaintext, resolvedCanaryVaultEntry, resolvedCanaryInstruction, resolvedCanaryVariable,
		"{{secret.", "__elitea_frozen_configuration_v1", "github_configuration",
	} {
		if strings.Contains(body, canary) {
			t.Fatalf("the resolved version leaks %q:\n%s", canary, body)
		}
	}
	var document storage.ClientApplicationVersion
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var details struct {
		Instructions string           `json:"instructions"`
		Variables    []map[string]any `json:"variables"`
		LLMSettings  map[string]any   `json:"llm_settings"`
		Tools        []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(document.VersionDetails, &details); err != nil {
		t.Fatalf("decode version_details: %v", err)
	}
	if details.Instructions != "Use [secret withheld] for the repo." {
		t.Fatalf("instructions = %q", details.Instructions)
	}
	if len(details.Variables) != 1 || details.Variables[0]["value"] != "[secret withheld]" {
		t.Fatalf("variables = %v", details.Variables)
	}
	if details.LLMSettings["model_name"] != "resolved-canary-model" {
		t.Fatalf("the freeze did not resolve the model: %v", details.LLMSettings)
	}
	var toolkit map[string]any
	for _, tool := range details.Tools {
		if tool["kind"] == storage.ClientToolKindRemoteToolkit {
			toolkit = tool
		}
	}
	if toolkit == nil {
		// Without the toolkit in the response the canary assertions above are
		// vacuous: the freeze would have dropped it rather than this
		// projection having stripped it.
		t.Fatalf("the seeded toolkit is not in the resolved version: %s", body)
	}
	ref, _ := toolkit["toolkit_ref"].(map[string]any)
	if ref["toolkit_id"] != float64(61) || ref["project_id"] != float64(1) ||
		ref["ref"] != storage.ClientToolkitRef(storage.ClientVersionIdentity{ProjectID: 1, ApplicationID: 71, VersionID: 81}, 61, "github") {
		t.Fatalf("toolkit_ref = %v", ref)
	}

	// The tenant schema scopes the read: project 2's copy of the ids is
	// another project, and the caller is not a member there.
	if status, body := get("/api/v2/elitea_core/resolved_version/prompt_lib/2/71/81"); status != http.StatusForbidden {
		t.Fatalf("a project the caller is not a member of answered %d. Body: %s", status, body)
	}
	if status, body := get("/api/v2/elitea_core/resolved_version/prompt_lib/1/71/999"); status != http.StatusNotFound {
		t.Fatalf("an absent version answered %d. Body: %s", status, body)
	}
}

func seedResolvedVersionFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	if _, err := pool.Exec(ctx, strings.ReplaceAll(`
INSERT INTO p_1.configuration (project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source)
VALUES
  (1, 'Resolved canary model', 'resolved_canary_model', 'llm_model', 'llm',
   '{"name":"resolved-canary-model"}'::jsonb, '{}'::jsonb, true, true, 'test'),
  (1, 'Resolved canary credential', 'resolved_canary_credential', 'github', 'credentials',
   '{"base_url":"https://api.github.com","access_token":"{{secret.`+resolvedCanaryVaultEntry+`}}"}'::jsonb,
   '{}'::jsonb, false, true, 'test');
INSERT INTO p_1.applications (id, name, description, owner_id)
VALUES (71, 'Resolved canary agent', 'canary', 1);
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES (
    81, 71, 'base', 'draft', $1, '80000000-0000-4000-8000-000000000081',
    '{"model_name":"resolved-canary-model"}'::jsonb,
    'Use {{secret.`+resolvedCanaryInstruction+`}} for the repo.',
    '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb
);
INSERT INTO p_1.application_variables (application_version_id, name, value)
VALUES (81, 'token', '{{secret.`+resolvedCanaryVariable+`}}');
INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, owner_id)
VALUES (61, 'github', 'canary-gh', 'GitHub',
  '{"repository":"`+resolvedCanaryPlaintext+`",
    "github_configuration":{"elitea_title":"resolved_canary_credential","private":false},
    "selected_tools":["read_file"]}'::jsonb, $1, '{}'::jsonb, 1);
INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (61, 71, 81, 'agent', '["read_file"]'::jsonb);`, "$1", strconv.Itoa(credentialJourneyUserID))); err != nil {
		t.Fatalf("seed the resolved version fixture: %v", err)
	}
	// The fixture check: every canary is in what the database holds.
	var stored string
	if err := pool.QueryRow(ctx, `
SELECT (SELECT settings::text FROM p_1.elitea_tools WHERE id = 61)
    || (SELECT data::text FROM p_1.configuration WHERE elitea_title = 'resolved_canary_credential')
    || (SELECT instructions FROM p_1.application_versions WHERE id = 81)
    || (SELECT value FROM p_1.application_variables WHERE application_version_id = 81)`).Scan(&stored); err != nil {
		t.Fatalf("read the fixture back: %v", err)
	}
	for _, canary := range []string{resolvedCanaryPlaintext, resolvedCanaryVaultEntry, resolvedCanaryInstruction, resolvedCanaryVariable} {
		if !strings.Contains(stored, canary) {
			t.Fatalf("the fixture does not hold %q", canary)
		}
	}
}
