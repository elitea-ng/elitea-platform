package applications_test

// The body-less PATCH that reads a version's expanded detail (#336).
//
// The SDK's `get_app_version_details`
// (elitea_sdk/runtime/clients/client.py:681-688) runs
// `requests.patch(url, headers=..., verify=False)` with NO body, so this
// handler is a READ. Before this change the Go router served only GET, PUT and
// DELETE on the path, and the hybrid edge sent the PATCH to pylon
// (deploy/centry-hybrid/traefik/index-routes.yml). `GetVersionExpanded`
// existed but no router registered it, and it authenticated against an
// `APPLICATION_SECRET_KEY` process environment variable that no deployment
// sets and that pylon never reads.
//
// Every assertion below reads the SERVED body, because a status code cannot
// tell a correct payload from an empty one — and the SDK indexes this payload
// directly (`data['llm_settings']['model_name']`, `var['name']`), so a missing
// key raises there rather than degrading.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// expandedFixture is one project holding one agent version that exercises every
// field this endpoint adds: two variables, three attached-skill mappings (one
// of which must be filtered out), and a `meta` carrying both the fork markers
// and an icon.
type expandedFixture struct {
	pool          *pgxpool.Pool
	router        *chi.Mux
	applicationID int64
	versionID     int64
}

const (
	expandedProjectID   = "1"
	expandedHeaderValue = "fixture-header-secret"
)

func newExpandedFixture(t *testing.T, agentType string) *expandedFixture {
	t.Helper()
	return newExpandedFixtureWithHeaderValue(t, agentType, expandedHeaderValue)
}

// newExpandedFixtureWithHeaderValue builds the same project, and decides what
// the project vault holds under `secrets_header_value`.
//
// An EMPTY headerValue seals an unrelated secret instead, which leaves the
// vault readable and the header value absent. That is the exact narrow gap
// issue #408 names: the vault opens, so `ResolveSecretValue` answers
// ErrSecretNotFound rather than a decryption error, and pylon answers that
// state with the literal "secret".
func newExpandedFixtureWithHeaderValue(t *testing.T, agentType, headerValue string) *expandedFixture {
	t.Helper()
	pool := newHandlerTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	// The ledgered corpus on top of the bootstrap schema, so the fixture runs
	// against the DDL production has rather than a hand-written subset.
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}
	seedHandlerUser(t, pool, 7, "expanded@example.com")

	// `applications.owner_id` is the owning PROJECT and this row lives in p_1,
	// so it holds 1 (#533). User 7 is the version AUTHOR, below. The seed used
	// to put the user in both columns, which tenant/0131 refuses with a foreign
	// key to centry.project.
	var applicationID, versionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ('expanded-fixture', '', 1) RETURNING id`).Scan(&applicationID); err != nil {
		t.Fatalf("insert fixture application: %v", err)
	}
	// meta carries BOTH fork markers (is_forked must be true) and an icon.
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions
			(application_id, name, status, author_id, agent_type, instructions, llm_settings, meta)
		VALUES ($1, 'base', 'draft', 7, $2, 'Use ~Alpha when the caller asks.',
			'{"model_name":"gpt-4o","temperature":0.5}'::jsonb,
			'{"parent_entity_id": 4, "parent_project_id": 9, "icon_meta": {"color": "teal"}}'::jsonb)
		RETURNING id`, applicationID, agentType).Scan(&versionID); err != nil {
		t.Fatalf("insert fixture version: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.application_variables (application_version_id, name, value)
		VALUES ($1, 'region', 'eu-west-1'), ($1, 'retries', '3')`, versionID); err != nil {
		t.Fatalf("insert fixture variables: %v", err)
	}

	seedExpandedSkill(t, pool, versionID, "Alpha", "The first skill.", "Alpha body.")
	// A skill whose selected version carries only whitespace. Pylon's
	// resolve_runtime_skills drops it, because `load_skill` could not serve it.
	seedExpandedSkill(t, pool, versionID, "Blank", "No body.", "   ")

	// The project vault holds the value pylon's check_secret_header compares.
	storedName, storedValue := "secrets_header_value", headerValue
	if headerValue == "" {
		storedName, storedValue = "unrelated", "value"
	}
	if err := secrets.NewHandler(pool).StoreSecret(
		ctx, nil, expandedProjectID, storedName, storedValue,
	); err != nil {
		t.Fatalf("store the project secret %q: %v", storedName, err)
	}

	router := newHandlerTestServer(t, pool, auth.User{ID: "7"})
	return &expandedFixture{pool: pool, router: router, applicationID: applicationID, versionID: versionID}
}

func seedExpandedSkill(t *testing.T, pool *pgxpool.Pool, versionID int64, name, description, instructions string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var skillID, skillVersionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.skills (name, description, owner_id, author_id)
		VALUES ($1, $2, 1, 7) RETURNING id`, name, description).Scan(&skillID); err != nil {
		t.Fatalf("insert skill %q: %v", name, err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, meta)
		VALUES ($1, 'base', $2, 7, '{"icon_meta": {"glyph": "star"}}'::jsonb)
		RETURNING id`, skillID, instructions).Scan(&skillVersionID); err != nil {
		t.Fatalf("insert skill version for %q: %v", name, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES ($1, 'agent', $2, $3)`, versionID, skillID, skillVersionID); err != nil {
		t.Fatalf("insert skill mapping for %q: %v", name, err)
	}
}

// patchVersion issues the body-less PATCH the SDK issues. `body` stays nil on
// purpose: a handler that decoded the request would meet io.EOF here.
func (f *expandedFixture) patchVersion(t *testing.T, secretHeader string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/version/prompt_lib/" + expandedProjectID + "/" +
		strconv.FormatInt(f.applicationID, 10) + "/" + strconv.FormatInt(f.versionID, 10)
	request := httptest.NewRequest(http.MethodPatch, path, nil)
	if secretHeader != "" {
		request.Header.Set("X-SECRET", secretHeader)
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the response body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// ── The route exists and serves the expanded detail ──────────────────────────

func TestVersionPatchServesTheExpandedDetail(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")

	recorder := fixture.patchVersion(t, expandedHeaderValue)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 from the body-less PATCH, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)

	// The keys the SDK indexes without a presence check.
	llm, ok := body["llm_settings"].(map[string]any)
	if !ok {
		t.Fatalf("llm_settings must be an object, got %T", body["llm_settings"])
	}
	if llm["model_name"] != "gpt-4o" {
		t.Errorf("expected llm_settings.model_name gpt-4o, got %v", llm["model_name"])
	}

	// variables — a list of dicts each carrying a `name`.
	variables, ok := body["variables"].([]any)
	if !ok {
		t.Fatalf("variables must be an array, got %T (%s)", body["variables"], recorder.Body.String())
	}
	if len(variables) != 2 {
		t.Fatalf("expected the 2 seeded variables, got %d: %v", len(variables), variables)
	}
	first, _ := variables[0].(map[string]any)
	if first["name"] != "region" || first["value"] != "eu-west-1" {
		t.Errorf("expected the first variable region=eu-west-1, got %v", first)
	}

	// icon_meta — lifted out of meta.
	iconMeta, ok := body["icon_meta"].(map[string]any)
	if !ok {
		t.Fatalf("icon_meta must be an object, got %T (%s)", body["icon_meta"], recorder.Body.String())
	}
	if iconMeta["color"] != "teal" {
		t.Errorf("expected icon_meta.color teal, got %v", iconMeta)
	}

	// is_forked — both parent markers are present in meta.
	if forked, _ := body["is_forked"].(bool); !forked {
		t.Errorf("expected is_forked true when meta carries both parent markers, got %v", body["is_forked"])
	}

	// attached_skills — the registry the SDK binds load_skill against.
	assertAlphaOnly(t, body, recorder)
}

// assertAlphaOnly checks the registry holds the usable skill and NOT the one
// whose body is blank.
func assertAlphaOnly(t *testing.T, body map[string]any, recorder *httptest.ResponseRecorder) {
	t.Helper()
	attached, ok := body["attached_skills"].([]any)
	if !ok {
		t.Fatalf("attached_skills must be an array, got %T (%s)", body["attached_skills"], recorder.Body.String())
	}
	if len(attached) != 1 {
		t.Fatalf("expected only the usable skill, got %d entries: %v", len(attached), attached)
	}
	skill, _ := attached[0].(map[string]any)
	if skill["name"] != "Alpha" {
		t.Errorf("expected the Alpha skill, got %v", skill["name"])
	}
	if skill["instructions"] != "Alpha body." {
		t.Errorf("expected the skill body to reach the caller, got %v", skill["instructions"])
	}
	icon, _ := skill["icon_meta"].(map[string]any)
	if icon["glyph"] != "star" {
		t.Errorf("expected the skill icon_meta, got %v", skill["icon_meta"])
	}
	// The projection pylon serves carries neither of these.
	if _, present := skill["skill_version_id"]; present {
		t.Errorf("attached_skills must not carry skill_version_id, got %v", skill)
	}
	if _, present := skill["version_name"]; present {
		t.Errorf("attached_skills must not carry version_name, got %v", skill)
	}
	// `skills` is the pre-resolution key pylon POPS. Serving it would publish a
	// key no consumer reads.
	if _, present := body["skills"]; present {
		t.Errorf("the response must not carry `skills`; the SDK reads attached_skills")
	}
}

// A pipeline contributes no skills at all, which is pylon's rule.
func TestVersionPatchServesNoSkillsForAPipeline(t *testing.T) {
	fixture := newExpandedFixture(t, "pipeline")

	recorder := fixture.patchVersion(t, expandedHeaderValue)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)

	attached, ok := body["attached_skills"].([]any)
	if !ok {
		t.Fatalf("attached_skills must be an array, got %T", body["attached_skills"])
	}
	if len(attached) != 0 {
		t.Errorf("a pipeline must contribute no skills, got %v", attached)
	}
}

// ── Authentication ───────────────────────────────────────────────────────────

func TestVersionPatchRefusesAWrongSecretHeader(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")

	recorder := fixture.patchVersion(t, "not-the-vault-value")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a wrong X-SECRET, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	if body["error"] != "Invalid secret header" {
		t.Errorf("expected the pylon error body, got %s", recorder.Body.String())
	}
	// A refused call must disclose nothing about the version.
	if _, present := body["llm_settings"]; present {
		t.Errorf("a refused call must not carry version data, got %s", recorder.Body.String())
	}
}

func TestVersionPatchRefusesAMissingSecretHeader(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")

	recorder := fixture.patchVersion(t, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing X-SECRET, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The retired environment variable must grant nothing. This is the assertion
// that the authentication actually MOVED rather than gained a second branch:
// the old code accepted any caller presenting APPLICATION_SECRET_KEY's value.
func TestVersionPatchIgnoresTheRetiredEnvironmentSecret(t *testing.T) {
	t.Setenv("APPLICATION_SECRET_KEY", "retired-env-secret")
	fixture := newExpandedFixture(t, "openai")

	recorder := fixture.patchVersion(t, "retired-env-secret")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("APPLICATION_SECRET_KEY must not authenticate this route, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
}

// A provisioned project refuses the pylon literal (#408).
//
// This is the acceptance criterion "prove a request carrying the old literal
// `secret` is refused on a newly provisioned project". The fixture's vault
// holds a value that is not the literal, exactly as
// EnsureProjectSecretsHeaderValue writes one, so the literal is simply the
// wrong value and gets pylon's 400.
func TestVersionPatchRefusesThePylonLiteralOnAProvisionedProject(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")

	recorder := fixture.patchVersion(t, "secret")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("the pylon literal must be refused, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
	if _, present := decodeBody(t, recorder)["llm_settings"]; present {
		t.Errorf("a refused call must not carry version data, got %s", recorder.Body.String())
	}
}

// A READABLE vault with no `secrets_header_value` refuses every caller (#408).
//
// This test replaces TestVersionPatchFallsBackToThePylonDefaultSecret, which
// required 200 for the literal "secret" and so pinned the defect. The literal
// is what the SDK sends when nothing supplies XSECRET
// (elitea-sdk/elitea_sdk/runtime/clients/client.py:94), and pylon's
// check_secret_header accepts it on any project that never set the key.
//
// It is safe to refuse now because the worker carries the project's own value:
// the runtime-context token response gained `secrets_header_value`
// (internal/infra/storage/index_runtime_context.go) and the worker puts it in
// the SDK client's api_extra_headers
// (services/elitea-worker-python/src/elitea_worker/agents/client_context.py).
func TestVersionPatchRefusesAProjectWithNoHeaderValue(t *testing.T) {
	fixture := newExpandedFixtureWithHeaderValue(t, "openai", "")

	// The vault opens, so this is the narrow ErrSecretNotFound gap and not an
	// unreadable vault.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := secrets.NewHandler(fixture.pool).ResolveSecretValue(
		ctx, expandedProjectID, "unrelated",
	); err != nil {
		t.Fatalf("the fixture vault must be readable: %v", err)
	}

	for _, received := range []string{"secret", "", expandedHeaderValue} {
		recorder := fixture.patchVersion(t, received)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("a project with no header value must refuse %q with 403, got %d: %s",
				received, recorder.Code, recorder.Body.String())
		}
		body := decodeBody(t, recorder)
		if body["error"] != "This project has no secrets_header_value secret. "+
			"The version details route cannot authenticate a caller until the project has one." {
			t.Errorf("expected the repair message, got %s", recorder.Body.String())
		}
		if _, present := body["llm_settings"]; present {
			t.Errorf("a refused call must not carry version data, got %s", recorder.Body.String())
		}
	}
}

// A vault that will not open refuses too, and it says so differently: an
// operator repairs a missing value and a broken key in different ways.
func TestVersionPatchRefusesAnUnreadableVault(t *testing.T) {
	fixture := newExpandedFixture(t, "openai")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Keep the key row and destroy the sealed data, which is what a vault
	// written under a master key this deployment no longer sets looks like.
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE centry.secrets_data SET data = $2 WHERE id = $1`,
		"project-"+expandedProjectID, []byte("not-a-fernet-token")); err != nil {
		t.Fatalf("corrupt the project vault data: %v", err)
	}

	recorder := fixture.patchVersion(t, expandedHeaderValue)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("an unreadable vault must refuse with 403, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
}
