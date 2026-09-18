package eliteacore_test

// Real-PostgreSQL coverage for the deterministic publish-validation rules
// added by the X3-agents-publish fix wave (onetest issues #909, #910, #912,
// #914, #936). Each rule is read out of `runPublishValidation`
// (internal/api/v2/eliteacore/handler.go) and driven end to end through
// `POST /publish_validate/prompt_lib/{project}/{version}`, the same route the
// publish wizard's Validation step calls.
//
// Two issues in the same package — #908 (missing model_project_id) and #913
// (zero tags as Critical) — are NOT implemented here: both, as literally
// specified, break the "well-formed agent" contract several OTHER e2e suites
// already rely on (`api.publish-validation.spec.ts`'s `createQualityAgent`
// carries no tags and no model and asserts an exact
// `{critical: 0, warnings: 0}` PASS; `api.publish-subagents.spec.ts`'s
// three-tier sub-agent chain test asserts an exact
// `{critical: 1, warnings: 0, suggestions: 0}` for an untagged, modelless
// chain). See ledger-X3.tsv for the DEFERRED rows.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pubValRulesInstructions = "You are a meeting preparation assistant. Turn the notes, transcripts and " +
	"agendas the user gives you into a short briefing that names the people involved and the decisions still open."

type pubValFixture struct {
	appID     int64
	versionID int64
}

// seedPubValAgent creates one agent + draft version with the given name and
// description, and a passing instructions body (so only the rule under test
// fires).
func seedPubValAgent(t *testing.T, pool *pgxpool.Pool, name, description string) pubValFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fixture pubValFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ($1, $2, 1) RETURNING id`,
		name, description).Scan(&fixture.appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'base', 'draft', 1, $2, 'openai') RETURNING id`,
		fixture.appID, pubValRulesInstructions).Scan(&fixture.versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	return fixture
}

func attachPubValSubAgent(t *testing.T, pool *pgxpool.Pool, parentAppID, parentVersionID, childAppID, childVersionID int64, toolName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	settings := fmt.Sprintf(`{"application_id":%d,"application_version_id":%d}`, childAppID, childVersionID)
	var toolID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
VALUES ($1, 'application', '', $2::jsonb, '{}'::jsonb, 1, 1) RETURNING id`,
		toolName, settings).Scan(&toolID); err != nil {
		t.Fatalf("seed the sub-agent tool row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id)
VALUES ($1, 'agent', $2, $3)`, parentVersionID, toolID, parentAppID); err != nil {
		t.Fatalf("seed the sub-agent mapping: %v", err)
	}
}

func pubValRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/publish_validate/prompt_lib/{projectID}/{versionID}", handler.PublishValidate)
	return router
}

type pubValFinding struct {
	Field   string `json:"field"`
	Issue   string `json:"issue"`
	Context string `json:"context"`
}

type pubValResult struct {
	Status          string          `json:"status"`
	CriticalIssues  []pubValFinding `json:"critical_issues"`
	Warnings        []pubValFinding `json:"warnings"`
	Recommendations []struct {
		Field      string `json:"field"`
		Suggestion string `json:"suggestion"`
	} `json:"recommendations"`
}

func doPubValValidate(t *testing.T, router chi.Router, versionID int64, versionName string) (*httptest.ResponseRecorder, pubValResult) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"version_name": versionName, "category": "Development"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/elitea_core/publish_validate/prompt_lib/1/%d", versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var result pubValResult
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	return recorder, result
}

// TestPublishValidationFlagsSecretShapedVariable — #909: a variable whose
// value looks like an API key/secret is flagged Critical. The function used
// to never read application_variables at all.
func TestPublishValidationFlagsSecretShapedVariable(t *testing.T) {
	pool := newPubValRulesPool(t)
	fixture := seedPubValAgent(t, pool, "secretvar agent", "a fixture agent long enough to pass the floor")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_variables (application_version_id, name, value) VALUES ($1, 'api_key', 'sk-abcdefghijklmnopqrstuv')`,
		fixture.versionID); err != nil {
		t.Fatalf("seed secret variable: %v", err)
	}

	router := pubValRouter(eliteacore.NewHandler(pool))
	_, result := doPubValValidate(t, router, fixture.versionID, "rel-secretvar")

	found := false
	for _, finding := range result.CriticalIssues {
		if containsFold(finding.Issue, "secret") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a secret-in-variable critical issue, got: %+v", result.CriticalIssues)
	}
}

// TestPublishValidationSubAgentNameFloor — #910: a sub-agent name under 3
// characters is flagged.
func TestPublishValidationSubAgentNameFloor(t *testing.T) {
	pool := newPubValRulesPool(t)
	parent := seedPubValAgent(t, pool, "shortname parent", "a fixture agent long enough to pass the floor")
	child := seedPubValAgent(t, pool, "AB", "a fixture agent long enough to pass the floor")
	attachPubValSubAgent(t, pool, parent.appID, parent.versionID, child.appID, child.versionID, "sub-shortname")

	router := pubValRouter(eliteacore.NewHandler(pool))
	_, result := doPubValValidate(t, router, parent.versionID, "rel-shortname")

	found := false
	for _, finding := range result.Warnings {
		if containsFold(finding.Issue, "3 character") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a sub-agent name length warning, got: %+v", result.Warnings)
	}
}

// TestPublishValidationSubAgentPlaceholderDescription — #912: a placeholder
// sub-agent description is a Critical worded "Sub-agent '[name]': ...",
// replacing the undocumented 20-char-only Warning.
func TestPublishValidationSubAgentPlaceholderDescription(t *testing.T) {
	pool := newPubValRulesPool(t)
	parent := seedPubValAgent(t, pool, "descparent", "a fixture agent long enough to pass the floor")
	child := seedPubValAgent(t, pool, "descchild", "TODO: describe")
	attachPubValSubAgent(t, pool, parent.appID, parent.versionID, child.appID, child.versionID, "sub-desc")

	router := pubValRouter(eliteacore.NewHandler(pool))
	_, result := doPubValValidate(t, router, parent.versionID, "rel-descchild")

	found := false
	for _, finding := range result.CriticalIssues {
		if finding.Field == "description" && containsFold(finding.Issue, "Sub-agent '") && containsFold(finding.Issue, "placeholder") {
			found = true
		}
	}
	if !found {
		t.Fatalf(`expected a Critical placeholder-description issue worded "Sub-agent '[name]': ...", got: %+v`, result.CriticalIssues)
	}
}

// TestPublishValidationMainAgentNameAndDescription — #911: the main agent's
// own placeholder Name and too-short Description are validated; previously
// neither column was ever read.
func TestPublishValidationMainAgentNameAndDescription(t *testing.T) {
	pool := newPubValRulesPool(t)
	fixture := seedPubValAgent(t, pool, "TODO: My Agent", "x")

	router := pubValRouter(eliteacore.NewHandler(pool))
	_, result := doPubValValidate(t, router, fixture.versionID, "rel-namedesc")

	nameFound, descFound := false, false
	for _, finding := range result.CriticalIssues {
		if finding.Field == "name" {
			nameFound = true
		}
	}
	for _, finding := range append(append([]pubValFinding{}, result.CriticalIssues...), result.Warnings...) {
		if finding.Field == "description" && finding.Context == "" {
			descFound = true
		}
	}
	if !nameFound {
		t.Fatalf("expected a placeholder-name issue, got: %+v", result.CriticalIssues)
	}
	if !descFound {
		t.Fatalf("expected a too-short main-agent description issue, got critical=%+v warnings=%+v", result.CriticalIssues, result.Warnings)
	}
}

// TestPublishValidationSemverSuggestion — #914: a valid, non-generic version
// name that is not semver-shaped gets a Suggestion recommending one.
func TestPublishValidationSemverSuggestion(t *testing.T) {
	pool := newPubValRulesPool(t)
	fixture := seedPubValAgent(t, pool, "semver agent", "a fixture agent long enough to pass the floor")

	router := pubValRouter(eliteacore.NewHandler(pool))
	_, result := doPubValValidate(t, router, fixture.versionID, "release-jan")

	found := false
	for _, rec := range result.Recommendations {
		if rec.Field == "version_name" && containsFold(rec.Suggestion, "semantic version") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a semantic-versioning suggestion, got: %+v", result.Recommendations)
	}

	// A real semver name raises nothing of the kind.
	_, semverResult := doPubValValidate(t, router, fixture.versionID, "1.2.3")
	for _, rec := range semverResult.Recommendations {
		if rec.Field == "version_name" {
			t.Fatalf("a semver-shaped name should not get the suggestion, got: %+v", rec)
		}
	}
}

// TestPublishValidationSubAgentPrivateModel — #936: a sub-agent on a
// project-specific (non-public) model is blocked with the same finding the
// main agent already gets for the identical condition. `validateSubAgents`
// used to never read a sub-agent's own llm_settings at all.
func TestPublishValidationSubAgentPrivateModel(t *testing.T) {
	pool := newPubValRulesPool(t)
	parent := seedPubValAgent(t, pool, "privmodel parent", "a fixture agent long enough to pass the floor")
	child := seedPubValAgent(t, pool, "privmodel child", "a fixture agent long enough to pass the floor")
	attachPubValSubAgent(t, pool, parent.appID, parent.versionID, child.appID, child.versionID, "sub-privmodel")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE p_1.application_versions SET llm_settings = '{"model_name":"private-model","model_project_id":"2"}'::jsonb
WHERE id = $1`, child.versionID); err != nil {
		t.Fatalf("seed sub-agent private model: %v", err)
	}

	router := pubValRouter(eliteacore.NewHandler(pool))
	recorder, result := doPubValValidate(t, router, parent.versionID, "rel-privmodel")

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (a sub-agent private model must block publish): body=%s", recorder.Code, recorder.Body.String())
	}
	found := false
	for _, finding := range result.CriticalIssues {
		if finding.Field == "llm_settings" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an llm_settings critical issue for the sub-agent's private model, got: %+v", result.CriticalIssues)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// newPubValRulesPool opens an isolated database on the server named by
// ELITEA_TEST_DATABASE_URL and applies the production migration chain to it.
func newPubValRulesPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL publish-validation-rules integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_pubvalrules_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply bootstrap migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply embedded tenant migrations: %v", err)
	}
	return pool
}
