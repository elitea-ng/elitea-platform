package projects_test

// The project-create route, driven against a REAL database (#376).
//
// WHY THIS FILE EXISTS. create_test.go pins the HTTP contract of the same
// route, but it does so against a stub provisioner: it proves who may reach the
// route and what the handler passes down, and nothing about the database. The
// E2E stack does not use the route either — it builds its projects with raw SQL
// and its own create_tenant_schema call. So the route had no test that ran it
// end to end, and three onboarding defects shipped through that gap:
//
//   #373  provisioning wrote no secrets vault, so the model picker of a new
//         project could never populate;
//   #374  delete dropped the tenant schema before the project row;
//   #375  create with no project_admin_email made a project no person could
//         open.
//
// The pattern this repository keeps hitting is a passing test that does not
// discriminate. A test that asserts 201 does not discriminate. A test that then
// USES the new project does. So every case below creates a project through the
// route and then does the work a newly onboarded customer does: it reads the
// model catalogue, it opens the project, and it makes an agent in it.
//
// This file covers #373 and #375. It does NOT cover the delete ordering of
// #374, which owns that fix; TestCreateThenDeleteLeavesTheDeploymentMigratable
// only asserts that the deployment still migrates afterwards.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2apps "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	onboardingDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	// Relative to this package. The same bootstrap file
	// provisioner_postgres_integration_test.go reads.
	onboardingBootstrapSchema = "../../../infra/db/migrations/001_initial.sql"

	// The account 001_initial.sql seeds. newOnboardingPool gives it the
	// administration-mode `admin` role, which migration 0069 grants
	// `projects.projects.project.create`. The role is granted by the fixture
	// rather than inherited from the bootstrap seed: shared/0111 revokes the
	// seed's copy, because an account nobody can sign in as must not carry a
	// standing global-admin grant that an e-mail match could adopt.
	onboardingMakerID    = 1
	onboardingMakerEmail = "dev@elitea.ai"

	// centry's public project. Its vault holds the deployment-wide model
	// defaults that a project with none of its own falls back to.
	onboardingPublicProjectID = 1

	// The deployment default model the fixture puts in the public vault. The
	// model picker of a new project must resolve to this value.
	onboardingDefaultModelName = "gpt-4o"
)

/* ── #373: the secrets vault ───────────────────────────────────────────── */

// TestCreateProjectRouteGivesTheNewProjectAUsableSecretsVault is the #373 case.
//
// What #373 cost: storage.PostgresSecretVaultLoader.load joins
// centry.secrets_key and centry.secrets_data and reported pgx.ErrNoRows as the
// generic ErrContentUnavailable; storage.CurrentModelDefaultsReader.Load failed
// the WHOLE read on that error; and the configurations route turned it into a
// 500. The model picker asks that route for its catalogue, so a project with no
// vault rows presented to its owner as "the product has no models".
//
// An absent vault is now a distinct answer (storage.ErrVaultAbsent) that those
// readers treat as "never set", so a project with no vault no longer 500s and
// this test is no longer what stands between a new project and an empty model
// picker. It still pins the vault the route must create: the secrets API reads
// it, and pylon provisions one with every project.
func TestCreateProjectRouteGivesTheNewProjectAUsableSecretsVault(t *testing.T) {
	pool := newOnboardingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	environment := newOnboardingEnvironment(ctx, t, pool)
	projectID := environment.createProject(ctx, t, map[string]any{
		"name":                "Vault Onboarding",
		"project_admin_email": onboardingMakerEmail,
	})

	// PREMISE. Without this the vault assertions below could pass vacuously
	// against a project the route never really built.
	if tables := onboardingSchemaTables(ctx, t, pool, fmt.Sprintf("p_%d", projectID)); len(tables) == 0 {
		t.Fatal("the route created no tenant schema, so this test cannot prove anything about its vault")
	}

	// ── the rows ─────────────────────────────────────────────────────────
	vaultID := fmt.Sprintf("project-%d", projectID)
	for _, table := range []string{"centry.secrets_key", "centry.secrets_data"} {
		var rows int
		if err := pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1`, table), vaultID,
		).Scan(&rows); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if rows != 1 {
			t.Errorf("%s has %d rows for %q, want 1 — the project's model picker cannot populate without it",
				table, rows, vaultID)
		}
	}

	// ── the vault opens ──────────────────────────────────────────────────
	//
	// Counting rows is not enough: a key row that does not decrypt its data row
	// is worse than an absent pair, because the secrets handler never overwrites
	// an unreadable vault and the project would 500 for ever. So the real reader
	// has to open it.
	vault, err := environment.vaults.LoadProjectVault(ctx, projectID)
	if err != nil {
		t.Fatalf("the new project's vault does not open: %v", err)
	}
	if vault == nil {
		t.Fatal("the new project's vault loaded as nil")
	}
	// A freshly provisioned vault is EMPTY, not broken. An absent name must read
	// as "no such secret" rather than as a failure.
	if _, err := vault.LookupRegular("default_llm_model_name"); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		t.Errorf("empty vault lookup err = %v, want ErrSecretNotFound", err)
	}

	// ── the model picker path ────────────────────────────────────────────
	//
	// This is the assertion that ties the vault row to the customer-visible
	// effect. The project has no defaults of its own, so the reader falls back
	// to the public project's vault and the admin vault — and that fallback is
	// only reached when the PROJECT vault opened first.
	defaults, err := environment.modelDefaults.Load(
		ctx, int32(projectID), onboardingPublicProjectID, configurationapp.CurrentModelSectionLLM)
	if err != nil {
		t.Fatalf("the model defaults of the new project do not resolve: %v", err)
	}
	if defaults.Model.Public.Name != onboardingDefaultModelName {
		t.Errorf("resolved default model = %q, want %q — the new project cannot see the deployment default",
			defaults.Model.Public.Name, onboardingDefaultModelName)
	}

	// And the catalogue the model picker actually calls.
	catalogue, err := environment.models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section:         configurationapp.CurrentModelSectionLLM,
		ProjectID:       int32(projectID),
		PublicProjectID: onboardingPublicProjectID,
		IncludeShared:   true,
	})
	if err != nil {
		t.Fatalf("the model catalogue of the new project does not read: %v", err)
	}
	if catalogue.Items == nil {
		t.Error("the model catalogue carries no item list")
	}
}

/* ── #375: the project nobody could open ───────────────────────────────── */

// TestCreateProjectRouteWithoutAnAdministratorOnboardsItsMaker is the #375 case.
//
// THE RULE THIS PINS. A create request that names no administrator gives the
// MAKER of the project the `admin` project role. See createProjectAdmin for why
// that rule was chosen over refusing the request.
//
// Membership is asserted three ways, because each one is a different failure a
// customer would report: the row itself, the project list the maker's own
// sidebar reads, and a real write in the new project.
func TestCreateProjectRouteWithoutAnAdministratorOnboardsItsMaker(t *testing.T) {
	pool := newOnboardingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	environment := newOnboardingEnvironment(ctx, t, pool)
	projectID := environment.createProject(ctx, t, map[string]any{
		"name": "Orphan Candidate",
	})

	// PREMISE.
	if tables := onboardingSchemaTables(ctx, t, pool, fmt.Sprintf("p_%d", projectID)); len(tables) == 0 {
		t.Fatal("the route created no tenant schema, so this test cannot prove anything about its membership")
	}

	// ── the membership row ───────────────────────────────────────────────
	var member bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
    WHERE assignment.project_id = $1 AND assignment.user_id = $2 AND role.name = 'admin'
)`, projectID, onboardingMakerID).Scan(&member); err != nil {
		t.Fatalf("check maker membership: %v", err)
	}
	if !member {
		t.Fatal("the maker of the project is not a member of it: " +
			"the create answered 201 for a project nobody can open")
	}

	// ── the maker's own project list ─────────────────────────────────────
	//
	// ListCurrentUserProjects inner-joins membership, so a project with no
	// member is absent from the list its maker sees. Asserted through the route
	// rather than the query, because the sidebar reads the route.
	listed := environment.listProjects(ctx, t)
	if !onboardingContainsID(listed, projectID) {
		t.Errorf("project %d is absent from its maker's project list %v", projectID, listed)
	}

	// ── real work in the new project ─────────────────────────────────────
	//
	// legacyrbac.projectPermissions returns nothing without a role assignment,
	// so every default-mode route on a memberless project answers 403. Creating
	// an agent and reading it back is the shortest path that proves the whole
	// chain: membership, the central grant fallback, and the tenant schema.
	applicationID := environment.createAgent(ctx, t, projectID, "First Agent")
	environment.readAgentBack(ctx, t, projectID, applicationID, "First Agent")
}

// TestCreateProjectRouteKeepsANamedAdministrator pins the half that already
// worked, so the #375 fix cannot be mistaken for a rewrite of it. A request
// that NAMES administrators grants them and nobody else — the maker does not
// silently join a project it created for somebody else.
func TestCreateProjectRouteKeepsANamedAdministrator(t *testing.T) {
	pool := newOnboardingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	environment := newOnboardingEnvironment(ctx, t, pool)
	namedID := onboardingSeedUser(ctx, t, pool, "customer.admin@example.test")

	projectID := environment.createProject(ctx, t, map[string]any{
		"name": "Customer Project",
		// Upper case on purpose: the assignment is matched case-insensitively.
		"project_admin_email": "CUSTOMER.ADMIN@example.test",
	})

	members := onboardingProjectMembers(ctx, t, pool, projectID)
	if _, ok := members[namedID]; !ok {
		t.Errorf("the named administrator %d is not a member (members %v)", namedID, members)
	}
	if _, ok := members[onboardingMakerID]; ok {
		t.Errorf("the maker joined a project it created for somebody else (members %v)", members)
	}
}

/* ── #376: the route builds the reference tenant, and delete leaves the
      deployment migratable ─────────────────────────────────────────────── */

// TestCreateProjectRouteBuildsTheReferenceTenant compares the tenant the ROUTE
// built against p_1, which the bootstrap and the migration corpus build between
// them. A literal table list would have to be edited every time the corpus
// grows, and an out-of-date list silently stops discriminating.
func TestCreateProjectRouteBuildsTheReferenceTenant(t *testing.T) {
	pool := newOnboardingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	environment := newOnboardingEnvironment(ctx, t, pool)
	projectID := environment.createProject(ctx, t, map[string]any{"name": "Reference Tenant"})

	reference := onboardingSchemaTables(ctx, t, pool, "p_1")
	if len(reference) == 0 {
		t.Fatal("reference schema p_1 has no tables; the fixture is broken, not the code")
	}
	provisioned := onboardingSchemaTables(ctx, t, pool, fmt.Sprintf("p_%d", projectID))
	if !onboardingEqualStrings(reference, provisioned) {
		t.Fatalf("the route's tenant does not match the reference p_1\n missing: %v\n extra:   %v",
			onboardingDifference(reference, provisioned), onboardingDifference(provisioned, reference))
	}
}

// TestCreateThenDeleteLeavesTheDeploymentMigratable drives both halves of the
// route and then asserts the state cmd/elitea-migrate refuses to run against.
//
// The preflight there hard-errors when ANY project with create_success = TRUE
// has no p_<id> schema, and that error fails migration for the WHOLE
// deployment rather than for the one project. So a create or a delete that
// leaves the two out of step is not a local defect.
func TestCreateThenDeleteLeavesTheDeploymentMigratable(t *testing.T) {
	pool := newOnboardingPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	environment := newOnboardingEnvironment(ctx, t, pool)
	projectID := environment.createProject(ctx, t, map[string]any{"name": "Short Lived"})

	// PREMISE: something really was created, so "it is gone" means something.
	if tables := onboardingSchemaTables(ctx, t, pool, fmt.Sprintf("p_%d", projectID)); len(tables) == 0 {
		t.Fatal("nothing was provisioned, so this test cannot prove anything was deleted")
	}
	onboardingAssertDeploymentMigrates(ctx, t, pool)

	environment.deleteProject(ctx, t, projectID)

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&rows); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	if rows != 0 {
		t.Error("the deleted project row survived")
	}
	onboardingAssertDeploymentMigrates(ctx, t, pool)
}

/* ── the composed environment ──────────────────────────────────────────── */

// onboardingEnvironment is the production composition of everything a customer
// touches on the onboarding path: the real projects router with the real
// permission gate and the real provisioner, the real applications routes, and
// the real model catalogue chain.
type onboardingEnvironment struct {
	router        *chi.Mux
	vaults        *storage.PostgresSecretVaultLoader
	modelDefaults *storage.CurrentModelDefaultsReader
	models        *configurationapp.CurrentModelCatalogService
}

func newOnboardingEnvironment(ctx context.Context, t *testing.T, pool *pgxpool.Pool) *onboardingEnvironment {
	t.Helper()

	// The real resolver, reading the real grant tables. A stub here would prove
	// nothing about whether a new project's roles resolve.
	resolver := legacyrbac.NewPostgresResolver(pool)
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))

	router := chi.NewRouter()
	// Every request below is the bootstrap operator. The auth middleware is not
	// under test here; create_test.go covers who may reach the route.
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, auth.User{
				ID:       fmt.Sprintf("%d", onboardingMakerID),
				UserID:   fmt.Sprintf("%d", onboardingMakerID),
				Email:    onboardingMakerEmail,
				AuthType: "user",
			}))
		})
	})
	// The REAL Routes(), which is what internal/api/router.go mounts, so the
	// RequireCentralPermissions gate under test is the one production wires.
	router.Mount("/projects", handler.NewHandler(pool,
		handler.WithPermissionResolver(resolver),
		handler.WithProvisioner(provisioner),
	).Routes())
	// The applications routes, registered flat under /elitea_core exactly as
	// internal/api/router.go registers them, with the same default-mode gate.
	applications := v2apps.NewHandler(repos.NewApplicationsRepo(pool), pool)
	projectPermission := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireResolvedPermissions(resolver, auth.PermissionModeDefault, permission)
	}
	router.Route("/elitea_core", func(r chi.Router) {
		r.With(projectPermission("models.applications.applications.create")).
			Post("/applications/prompt_lib/{projectID}", applications.Create)
		r.With(projectPermission("models.applications.application.details")).
			Get("/application/prompt_lib/{projectID}/{applicationID}", applications.Get)
	})

	environment := &onboardingEnvironment{router: router}
	onboardingSeedDeploymentVaults(ctx, t, pool)

	vaults, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatalf("compose vault loader: %v", err)
	}
	t.Cleanup(vaults.Destroy)
	environment.vaults = vaults

	modelDefaults, err := storage.NewCurrentModelDefaultsReader(vaults)
	if err != nil {
		t.Fatalf("compose model defaults reader: %v", err)
	}
	environment.modelDefaults = modelDefaults

	modelRows, err := repos.NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatalf("compose model repository: %v", err)
	}
	models, err := configurationapp.NewCurrentModelCatalogService(modelRows, modelDefaults)
	if err != nil {
		t.Fatalf("compose model catalogue service: %v", err)
	}
	environment.models = models
	return environment
}

// createProject drives POST /projects/project/administration and returns the
// new project id. It fails the test on any status other than 201, and reports
// the per-step body, which is where a provisioning failure names its step.
func (e *onboardingEnvironment) createProject(ctx context.Context, t *testing.T, body map[string]any) int64 {
	t.Helper()
	recorder := e.do(ctx, t, http.MethodPost, "/projects/project/administration", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		ID    *int64 `json:"id"`
		Steps []struct {
			Step string `json:"step"`
			OK   *bool  `json:"ok"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode create response %s: %v", recorder.Body.String(), err)
	}
	if decoded.ID == nil || *decoded.ID <= 1 {
		t.Fatalf("create returned no newly allocated project id: %s", recorder.Body.String())
	}
	for _, status := range decoded.Steps {
		if status.OK == nil || !*status.OK {
			t.Errorf("provisioning step %q did not report success: %s", status.Step, recorder.Body.String())
		}
	}
	return *decoded.ID
}

func (e *onboardingEnvironment) deleteProject(ctx context.Context, t *testing.T, projectID int64) {
	t.Helper()
	path := fmt.Sprintf("/projects/project/administration/%d", projectID)
	recorder := e.do(ctx, t, http.MethodDelete, path, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
}

// listProjects reads the maker's own project list through the route the web
// client's project switcher calls.
func (e *onboardingEnvironment) listProjects(ctx context.Context, t *testing.T) []int64 {
	t.Helper()
	recorder := e.do(ctx, t, http.MethodGet, "/projects/project/default/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("project list status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode project list %s: %v", recorder.Body.String(), err)
	}
	ids := make([]int64, 0, len(decoded))
	for _, project := range decoded {
		ids = append(ids, project.ID)
	}
	return ids
}

// createAgent makes an agent in the new project through the same route the
// create-agent form posts to. The body is the shape apps/elitea-web sends.
func (e *onboardingEnvironment) createAgent(ctx context.Context, t *testing.T, projectID int64, name string) string {
	t.Helper()
	path := fmt.Sprintf("/elitea_core/applications/prompt_lib/%d", projectID)
	recorder := e.do(ctx, t, http.MethodPost, path, map[string]any{
		"name":        name,
		"description": "created by the onboarding test",
		"type":        "agent",
		"versions": []any{map[string]any{
			"name":                  "base",
			"agent_type":            "openai",
			"instructions":          "Follow the brief.",
			"conversation_starters": []any{"hello"},
		}},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create agent status = %d, want 201; body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode agent %s: %v", recorder.Body.String(), err)
	}
	if decoded.ID == "" {
		t.Fatalf("the created agent carries no id: %s", recorder.Body.String())
	}
	return decoded.ID
}

// readAgentBack proves the agent is durable rather than merely accepted.
func (e *onboardingEnvironment) readAgentBack(
	ctx context.Context, t *testing.T, projectID int64, applicationID, want string,
) {
	t.Helper()
	path := fmt.Sprintf("/elitea_core/application/prompt_lib/%d/%s", projectID, applicationID)
	recorder := e.do(ctx, t, http.MethodGet, path, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("read agent status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode agent read %s: %v", recorder.Body.String(), err)
	}
	if decoded.ID != applicationID || decoded.Name != want {
		t.Errorf("read back agent (%q, %q), want (%q, %q)", decoded.ID, decoded.Name, applicationID, want)
	}
}

func (e *onboardingEnvironment) do(
	ctx context.Context, t *testing.T, method, path string, body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.router.ServeHTTP(recorder, request)
	return recorder
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// newOnboardingPool builds an isolated database holding exactly what a real
// deployment holds: the bootstrap schema plus the embedded corpus. Same recipe
// as provisioner_postgres_integration_test.go's newProvisioningPool, and for
// the same reason — the shared corpus carries the grant migrations, so a
// fixture that skipped it would answer 403 for reasons unrelated to the code.
func newOnboardingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(onboardingDatabaseURLEnv)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skipf("set %s to run the project onboarding integration test", onboardingDatabaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_onboarding_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", onboardingDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})

	bootstrap, err := os.ReadFile(onboardingBootstrapSchema)
	if err != nil {
		t.Fatalf("read bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply bootstrap schema: %v", err)
	}

	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}

	// THE MAKER'S ROLE IS STATED, NOT INHERITED.
	//
	// 001_initial.sql grants user 1 the administration-mode `admin` role, and
	// shared/0111 takes it back: that account has no identity-provider link and
	// cannot sign in, yet the OIDC path adopts an existing account BY E-MAIL, so
	// the standing grant made anyone who obtained dev@elitea.ai a global
	// administrator of a fresh install.
	//
	// This fixture needs an operator who can create a project, which is a
	// different requirement from "the seed happens to leave one lying around".
	// Granting it here says which privilege the test depends on, and leaves the
	// bootstrap seed free to stop handing that privilege out.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id
FROM public.auth_core__role AS role
WHERE role.mode = 'administration' AND role.name = 'admin'
ON CONFLICT (user_id, role_id) DO NOTHING`, onboardingMakerID); err != nil {
		t.Fatalf("grant the project maker its administration role: %v", err)
	}
	return pool
}

// onboardingSeedDeploymentVaults puts the deployment-wide vaults in place.
//
// A real deployment has them: the public project's vault holds the default
// model every project without one of its own falls back to, and the admin vault
// is the last source in that fallback chain. Neither is created by project
// provisioning, and neither is in scope for #373 — provisioning owns the
// per-project vault only.
//
// They are written through the secrets handler's own programmatic path, so the
// blobs are the shape production writes rather than a fixture invention. The
// admin vault is then COPIED from the public one, which is what
// apps/elitea-web/scripts/e2e-stack.sh does for the same reason: the blobs are
// Fernet, so no hand-written SQL can produce a valid pair.
func onboardingSeedDeploymentVaults(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	secrets := v2secrets.NewHandler(pool)
	publicProject := fmt.Sprintf("%d", onboardingPublicProjectID)
	for name, value := range map[string]string{
		"default_llm_model_name":       onboardingDefaultModelName,
		"default_llm_model_project_id": publicProject,
	} {
		if err := secrets.StoreSecret(ctx, nil, publicProject, name, value); err != nil {
			t.Fatalf("seed public vault secret %s: %v", name, err)
		}
	}
	for _, table := range []string{"centry.secrets_key", "centry.secrets_data"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (id, data)
SELECT 'admin', data FROM %s WHERE id = $1
ON CONFLICT (id) DO NOTHING`, table, table),
			"project-"+publicProject,
		); err != nil {
			t.Fatalf("seed admin vault from %s: %v", table, err)
		}
	}

	// PREMISE: the fixture really wrote a readable public vault. Without this a
	// later "the defaults do not resolve" failure could be the fixture's fault
	// rather than the code's.
	var publicRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.secrets_key WHERE id IN ($1, 'admin')`,
		"project-"+publicProject,
	).Scan(&publicRows); err != nil {
		t.Fatalf("verify seeded vaults: %v", err)
	}
	if publicRows != 2 {
		t.Fatalf("the fixture seeded %d deployment vaults, want 2", publicRows)
	}
}

// onboardingAssertDeploymentMigrates reproduces cmd/elitea-migrate's
// -all-tenants preflight, then applies the corpus to every tenant it names.
// The preflight is the check that fails the WHOLE deployment when one project
// row and its schema are out of step.
func onboardingAssertDeploymentMigrates(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT
    project.id,
    EXISTS (
        SELECT 1
        FROM pg_catalog.pg_namespace
        WHERE nspname = 'p_' || project.id::text
    ) AS schema_exists
FROM centry.project AS project
WHERE project.create_success = TRUE
ORDER BY project.id`)
	if err != nil {
		t.Fatalf("preflight tenant projects: %v", err)
	}
	defer rows.Close()

	projectIDs := make([]int64, 0)
	for rows.Next() {
		var (
			projectID    int64
			schemaExists bool
		)
		if err := rows.Scan(&projectID, &schemaExists); err != nil {
			t.Fatalf("scan preflight row: %v", err)
		}
		if !schemaExists {
			t.Fatalf("project %d is marked created and has no p_%d schema: "+
				"elitea-migrate refuses to run for the whole deployment in this state", projectID, projectID)
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate preflight rows: %v", err)
	}
	if len(projectIDs) == 0 {
		t.Fatal("the preflight found no created project at all; the fixture is broken, not the code")
	}

	runner := migrate.New(pool, platformmigrations.Files)
	for _, projectID := range projectIDs {
		if err := runner.ApplyTenant(ctx, projectID); err != nil {
			t.Fatalf("elitea-migrate cannot apply the corpus to project %d: %v", projectID, err)
		}
	}
}

func onboardingSeedUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email,
	).Scan(&userID); err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
	return userID
}

// onboardingProjectMembers maps every member of a project to its role names.
func onboardingProjectMembers(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64,
) map[int64][]string {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT assignment.user_id, role.name
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
WHERE assignment.project_id = $1
ORDER BY assignment.user_id, role.name`, projectID)
	if err != nil {
		t.Fatalf("list project members: %v", err)
	}
	defer rows.Close()
	members := make(map[int64][]string)
	for rows.Next() {
		var (
			userID int64
			role   string
		)
		if err := rows.Scan(&userID, &role); err != nil {
			t.Fatalf("scan project member: %v", err)
		}
		members[userID] = append(members[userID], role)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate project members: %v", err)
	}
	return members
}

func onboardingSchemaTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT table_name FROM information_schema.tables
WHERE table_schema = $1 AND table_type = 'BASE TABLE'
ORDER BY table_name`, schema)
	if err != nil {
		t.Fatalf("list tables in %s: %v", schema, err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables in %s: %v", schema, err)
	}
	return names
}

func onboardingContainsID(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func onboardingEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func onboardingDifference(from, remove []string) []string {
	present := make(map[string]struct{}, len(remove))
	for _, value := range remove {
		present[value] = struct{}{}
	}
	missing := make([]string, 0)
	for _, value := range from {
		if _, ok := present[value]; !ok {
			missing = append(missing, value)
		}
	}
	return missing
}
