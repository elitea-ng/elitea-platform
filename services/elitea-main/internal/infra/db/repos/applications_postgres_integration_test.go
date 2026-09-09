package repos

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// These tests run against the real PostgreSQL service the Test job in
// .github/workflows/ci-go.yml provisions (postgres:16-alpine, exported as
// ELITEA_TEST_DATABASE_URL), on the baseline tenant schema p_1 the 001_initial
// migration creates. Every statement in internal/infra/db/repos/applications.go
// was previously unexecuted in any environment; these exercise all twelve
// Repository methods against the actual DDL.

const testProjectID = "1"

func newApplicationsTestRepo(t *testing.T) (*ApplicationsRepo, *pgxpool.Pool) {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	return NewApplicationsRepo(pool), pool
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return auth.ContextWithUser(ctx, auth.User{ID: "1", UserID: "1"})
}

// apiStatus returns the HTTP status an error carries, or 0 when it is not a
// typed API error (which is itself a finding — an untyped error becomes a 500).
func apiStatus(err error) int {
	var apiErr *apierr.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

func seedUser(t *testing.T, pool *pgxpool.Pool, id int64, email string) {
	t.Helper()
	if _, err := pool.Exec(testContext(t),
		`INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name`,
		id, email, "Seeded "+email); err != nil {
		t.Fatalf("seed user %d: %v", id, err)
	}
}

func createTestApplication(t *testing.T, repo *ApplicationsRepo, name string, authorID int64, version *applications.Version) applications.Application {
	t.Helper()
	app, err := repo.Create(testContext(t), applications.CreateRequest{
		ProjectID:      testProjectID,
		Name:           name,
		AuthorID:       ownership.UserID(authorID),
		InitialVersion: version,
	})
	if err != nil {
		t.Fatalf("create application %q: %v", name, err)
	}
	return app
}

// ─────────────────────────────────────────────────────────────────────────────
// Create — owner threading, atomic initial version, returned identifiers
// ─────────────────────────────────────────────────────────────────────────────

// The two columns hold two different kinds of number, and this test reads both
// back from one row (#533).
//
// `applications.owner_id` is the owning PROJECT. This repository wrote the
// authenticated principal into it, so the row said that project 7 owned an
// agent that lives in schema p_1. Every legacy read filters
// `Application.owner_id == project_id`, so the agent was invisible to the
// legacy runtime and to any join written on the project meaning.
//
// `application_versions.author_id` is the USER, and it keeps the principal.
func TestApplicationsRepoPostgres_CreateStoresTheProjectAsOwnerAndThePrincipalAsAuthor(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 7, "seven@elitea.ai")

	app := createTestApplication(t, repo, "authored-by-seven", 7, &applications.Version{Name: "base"})

	var ownerID, authorID int64
	if err := pool.QueryRow(ctx, `
		SELECT a.owner_id, v.author_id
		FROM p_1.applications a JOIN p_1.application_versions v ON v.application_id = a.id
		WHERE a.id = $1`, app.ID).Scan(&ownerID, &authorID); err != nil {
		t.Fatalf("read back owner: %v", err)
	}
	if ownerID != 1 {
		t.Errorf("applications.owner_id = %d, want the project of schema p_1, which is 1", ownerID)
	}
	if authorID != 7 {
		t.Errorf("application_versions.author_id = %d, want the authenticated principal 7", authorID)
	}
	if app.OwnerID != "1" {
		t.Errorf("response owner_id = %q, want the project \"1\"", app.OwnerID)
	}
	if app.CreatedBy != "" {
		t.Errorf("response created_by = %q, want it empty: the table has no creator column", app.CreatedBy)
	}
}

func TestApplicationsRepoPostgres_CreateRefusesAnUnauthenticatedOwner(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)

	_, err := repo.Create(ctx, applications.CreateRequest{ProjectID: testProjectID, Name: "orphan"})
	if err == nil {
		t.Fatal("create without an owner succeeded; it must not fall back to user 1")
	}
	if got := apiStatus(err); got != 401 {
		t.Errorf("status = %d, want 401 (err=%v)", got, err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.applications`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d application rows written by a rejected create, want 0", count)
	}
}

func TestApplicationsRepoPostgres_CreateReturnsTheSerialIDAndTheUUIDSeparately(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "identifiers", 1, nil)

	// app.ID must be the SERIAL primary key: it is what the UI navigates to
	// (/agents/$tab/$agentId) and what every other route looks the row up by.
	var serialID int64
	var uuid string
	if err := pool.QueryRow(ctx,
		`SELECT id, uuid::text FROM p_1.applications WHERE name = 'identifiers'`).Scan(&serialID, &uuid); err != nil {
		t.Fatalf("read back row: %v", err)
	}
	if app.ID != fmt.Sprint(serialID) {
		t.Errorf("app.ID = %q, want the SERIAL id %d", app.ID, serialID)
	}
	if app.UUID != uuid {
		t.Errorf("app.UUID = %q, want the generated uuid %q", app.UUID, uuid)
	}
	if app.UUID == "" {
		t.Error("app.UUID is empty: the RETURNING uuid is being discarded again")
	}
	// The generated uuid must never land in ProjectID, which is what the
	// previous implementation scanned it into before overwriting it.
	if app.ProjectID != testProjectID {
		t.Errorf("app.ProjectID = %q, want %q", app.ProjectID, testProjectID)
	}

	// Get addresses the row by app.ID, so a wrong identifier does not round-trip.
	fetched, err := repo.Get(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("get by returned id: %v", err)
	}
	if fetched.Name != "identifiers" || fetched.UUID != uuid {
		t.Errorf("fetched = %+v, want name=identifiers uuid=%s", fetched, uuid)
	}
}

func TestApplicationsRepoPostgres_CreateCommitsApplicationAndVersionTogether(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "with-version", 1, &applications.Version{
		Name:                 "base",
		Instructions:         "Be precise.",
		AgentType:            "openai",
		WelcomeMessage:       "hi",
		LLMSettings:          map[string]any{"model_name": "gpt-4o", "temperature": 0.25, "max_tokens": 512},
		ConversationStarters: []any{"one", "two"},
		Meta:                 map[string]any{"step_limit": float64(25)},
	})
	if len(app.Versions) != 1 {
		t.Fatalf("Create returned %d versions, want the initial one", len(app.Versions))
	}

	// An application with no version row is invisible to List, so the initial
	// version is part of what "created" means.
	list, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list.Total != 1 || len(list.Rows) != 1 || list.Rows[0].ID != app.ID {
		t.Fatalf("list = %+v, want the created application", list)
	}

	got, err := repo.GetVersion(ctx, testProjectID, app.ID, app.Versions[0].ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if got.Instructions != "Be precise." || got.WelcomeMessage != "hi" || got.AgentType != "openai" {
		t.Errorf("version columns did not round-trip: %+v", got)
	}
	if got.LLMSettings["model_name"] != "gpt-4o" {
		t.Errorf("llm_settings did not round-trip: %+v", got.LLMSettings)
	}
	if !reflect.DeepEqual(got.ConversationStarters, []any{"one", "two"}) {
		t.Errorf("conversation_starters = %+v", got.ConversationStarters)
	}
	if got.Meta["step_limit"] != float64(25) {
		t.Errorf("meta = %+v", got.Meta)
	}
	// Config is the derived read projection over llm_settings + instructions.
	if got.Config.Model != "gpt-4o" || got.Config.Temperature != 0.25 ||
		got.Config.MaxTokens != 512 || got.Config.SystemPrompt != "Be precise." {
		t.Errorf("derived Config = %+v", got.Config)
	}
}

func TestApplicationsRepoPostgres_CreateRollsBackWhenTheInitialVersionFails(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	// application_versions.name is VARCHAR(128); a longer name fails the
	// INSERT after the applications row is already in the transaction.
	_, err := repo.Create(ctx, applications.CreateRequest{
		ProjectID:      testProjectID,
		Name:           "half-created",
		AuthorID:       1,
		InitialVersion: &applications.Version{Name: strings.Repeat("v", 129)},
	})
	if err == nil {
		t.Fatal("create with an invalid initial version succeeded")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.applications WHERE name = 'half-created'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d orphaned application rows left behind, want 0", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Project-id validation — hostile input must never reach SQL text
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_NonNumericProjectIDNeverReachesSQL(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE p_1.injection_canary (id int)`); err != nil {
		t.Fatalf("create canary: %v", err)
	}

	hostile := []string{
		`1"."applications"; DROP TABLE p_1.injection_canary; SELECT * FROM "p_1"."applications`,
		`1; DROP TABLE p_1.injection_canary; --`,
		`1 OR 1=1`,
		`p_1`,
		``,
		`  1`,
		`1.0`,
		`-1`,
		`٩`, // non-ASCII digit
		strings.Repeat("9", 40),
	}

	for _, projectID := range hostile {
		t.Run(fmt.Sprintf("%q", projectID), func(t *testing.T) {
			calls := map[string]error{
				"List": func() error {
					_, err := repo.List(ctx, applications.ListRequest{ProjectID: projectID, Page: 1, PageSize: 10})
					return err
				}(),
				"Get": func() error { _, err := repo.Get(ctx, projectID, "1"); return err }(),
				"Create": func() error {
					_, err := repo.Create(ctx, applications.CreateRequest{ProjectID: projectID, Name: "x", AuthorID: 1})
					return err
				}(),
				"Update": func() error {
					_, err := repo.Update(ctx, applications.UpdateRequest{ProjectID: projectID, ApplicationID: "1"})
					return err
				}(),
				"Delete":     repo.Delete(ctx, projectID, "1"),
				"GetVersion": func() error { _, err := repo.GetVersion(ctx, projectID, "1", "1"); return err }(),
				"ListVersions": func() error {
					_, err := repo.ListVersions(ctx, projectID, "1")
					return err
				}(),
				"CreateVersion": func() error {
					_, err := repo.CreateVersion(ctx, projectID, "1", applications.Version{Name: "v", AuthorID: 1})
					return err
				}(),
				"UpdateVersion": func() error {
					_, err := repo.UpdateVersion(ctx, projectID, "1", "1", applications.Version{Name: "v"})
					return err
				}(),
				"DeleteVersion":       repo.DeleteVersion(ctx, projectID, "1", "1"),
				"SetDefaultVersion":   repo.SetDefaultVersion(ctx, projectID, "1", "1"),
				"BatchReplaceVersion": repo.BatchReplaceVersion(ctx, projectID, "1", "2", false),
			}

			for method, err := range calls {
				if err == nil {
					t.Errorf("%s accepted project id %q", method, projectID)
					continue
				}
				if got := apiStatus(err); got != 400 {
					t.Errorf("%s: status = %d, want 400 (err=%v)", method, got, err)
				}
				// The rejection must not be a database error carrying the
				// caller's own text back out.
				if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), projectID) && projectID != "" {
					t.Errorf("%s: error leaks the input or a SQL error: %v", method, err)
				}
			}
		})
	}

	var canaryAlive bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema='p_1' AND table_name='injection_canary')`,
	).Scan(&canaryAlive); err != nil {
		t.Fatal(err)
	}
	if !canaryAlive {
		t.Fatal("injected SQL executed: the canary table was dropped")
	}
}

func TestApplicationsRepoPostgres_ValidProjectIDWithNoTenantSchemaIsAnErrorNotAnEmptyPage(t *testing.T) {
	repo, _ := newApplicationsTestRepo(t)
	ctx := testContext(t)

	// p_4242 does not exist. The previous implementation swallowed the
	// failure and reported an empty, successful page.
	if _, err := repo.List(ctx, applications.ListRequest{ProjectID: "4242", Page: 1, PageSize: 10}); err == nil {
		t.Fatal("List against a missing tenant schema reported success")
	}
	if _, err := repo.ListVersions(ctx, "4242", "1"); err == nil {
		t.Fatal("ListVersions against a missing tenant schema reported success")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_ListFiltersPaginatesAndAttributes(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 5, "five@elitea.ai")

	for _, name := range []string{"alpha agent", "beta agent", "gamma widget"} {
		createTestApplication(t, repo, name, 5, &applications.Version{Name: "base"})
	}
	// A pipeline-typed application must not appear in the classic listing.
	createTestApplication(t, repo, "delta pipeline", 5,
		&applications.Version{Name: "base", AgentType: "pipeline"})
	// An application with no version row is not a listable agent.
	createTestApplication(t, repo, "versionless", 5, nil)

	classic, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 1, PageSize: 10, AgentsType: "classic"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if classic.Total != 3 {
		t.Errorf("classic total = %d, want 3 (pipeline and versionless excluded)", classic.Total)
	}
	for _, row := range classic.Rows {
		if row.Name == "delta pipeline" || row.Name == "versionless" {
			t.Errorf("classic listing contains %q", row.Name)
		}
	}

	pipelines, err := repo.List(ctx, applications.ListRequest{
		ProjectID: testProjectID, Page: 1, PageSize: 10, AgentsType: "pipeline"})
	if err != nil {
		t.Fatalf("list pipelines: %v", err)
	}
	if pipelines.Total != 1 || len(pipelines.Rows) != 1 || pipelines.Rows[0].Name != "delta pipeline" {
		t.Errorf("pipeline listing = %+v", pipelines)
	}

	searched, err := repo.List(ctx, applications.ListRequest{
		ProjectID: testProjectID, Page: 1, PageSize: 10, Search: "agent"})
	if err != nil {
		t.Fatalf("list search: %v", err)
	}
	if searched.Total != 2 {
		t.Errorf("search total = %d, want 2", searched.Total)
	}

	paged, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 2, PageSize: 2, AgentsType: "classic"})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(paged.Rows) != 1 {
		t.Errorf("page 2 of 3 rows at size 2 returned %d rows, want 1", len(paged.Rows))
	}
	if paged.TotalPages != 2 {
		t.Errorf("total_pages = %d, want 2", paged.TotalPages)
	}
	if paged.Page != 2 || paged.PageSize != 2 {
		t.Errorf("page=%d page_size=%d, want 2/2", paged.Page, paged.PageSize)
	}

	// Author attribution joins public.auth_core__user on the VERSION author.
	// `owner_id` is the owning project, so the row's owner_id is the project
	// and the author is the principal that wrote the version (#533).
	if len(classic.Rows) == 0 || len(classic.Rows[0].Authors) != 1 {
		t.Fatalf("rows[0] = %+v, want exactly one author", classic.Rows)
	}
	if classic.Rows[0].Authors[0].Email != "five@elitea.ai" || classic.Rows[0].Authors[0].ID != "5" {
		t.Errorf("author = %+v, want id 5 five@elitea.ai", classic.Rows[0].Authors[0])
	}
	if classic.Rows[0].OwnerID != "1" {
		t.Errorf("row owner_id = %q, want the project \"1\"", classic.Rows[0].OwnerID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Get / Update / Delete
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_UpdateWritesOnlySuppliedFields(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app, err := repo.Create(ctx, applications.CreateRequest{
		ProjectID: testProjectID, Name: "before", Description: "keep me", Icon: "icon-a", AuthorID: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	renamed := "after"
	updated, err := repo.Update(ctx, applications.UpdateRequest{
		ProjectID: testProjectID, ApplicationID: app.ID, Name: &renamed,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "after" || updated.Description != "keep me" || updated.Icon != "icon-a" {
		t.Errorf("update clobbered unsupplied fields: %+v", updated)
	}
	if updated.UUID != app.UUID {
		t.Errorf("update returned uuid %q, want the row's %q", updated.UUID, app.UUID)
	}
	if updated.OwnerID != "1" {
		t.Errorf("update returned owner_id %q, want \"1\"", updated.OwnerID)
	}

	// No-op update is a read, not a rewrite.
	noop, err := repo.Update(ctx, applications.UpdateRequest{ProjectID: testProjectID, ApplicationID: app.ID})
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if noop.Name != "after" {
		t.Errorf("no-op update returned %+v", noop)
	}
}

func TestApplicationsRepoPostgres_MissingRowsAreNotFoundNotInternalErrors(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")
	app := createTestApplication(t, repo, "present", 1, &applications.Version{Name: "base"})

	name := "x"
	cases := map[string]error{
		"Get":             func() error { _, err := repo.Get(ctx, testProjectID, "99999"); return err }(),
		"Get non-numeric": func() error { _, err := repo.Get(ctx, testProjectID, "not-an-id"); return err }(),
		"Update": func() error {
			_, err := repo.Update(ctx, applications.UpdateRequest{ProjectID: testProjectID, ApplicationID: "99999", Name: &name})
			return err
		}(),
		"Delete":             repo.Delete(ctx, testProjectID, "99999"),
		"Delete non-numeric": repo.Delete(ctx, testProjectID, "not-an-id"),
		"GetVersion":         func() error { _, err := repo.GetVersion(ctx, testProjectID, app.ID, "99999"); return err }(),
		"DeleteVersion":      repo.DeleteVersion(ctx, testProjectID, app.ID, "99999"),
		"SetDefaultVersion":  repo.SetDefaultVersion(ctx, testProjectID, app.ID, "99999"),
		"BatchReplace":       repo.BatchReplaceVersion(ctx, testProjectID, "99999", "99998", false),
	}
	for label, err := range cases {
		if err == nil {
			t.Errorf("%s: reported success for a row that does not exist", label)
			continue
		}
		if got := apiStatus(err); got != 404 {
			t.Errorf("%s: status = %d, want 404 (err=%v)", label, got, err)
		}
	}
}

func TestApplicationsRepoPostgres_DeleteRemovesTheApplicationAndItsVersions(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "doomed", 1, &applications.Version{Name: "base"})
	versionID := app.Versions[0].ID
	if _, err := pool.Exec(ctx,
		`INSERT INTO p_1.application_variables (application_version_id, name, value) VALUES ($1, 'k', 'v')`,
		versionID); err != nil {
		t.Fatalf("seed variable: %v", err)
	}

	// The previous implementation opened Delete with an unconditional
	// DELETE FROM p_1.application_tools — a table 001_initial.sql never
	// creates — so every delete failed with 42P01.
	if err := repo.Delete(ctx, testProjectID, app.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for label, query := range map[string]string{
		"applications":          `SELECT COUNT(*) FROM p_1.applications WHERE id = $1`,
		"application_versions":  `SELECT COUNT(*) FROM p_1.application_versions WHERE application_id = $1`,
		"application_variables": `SELECT COUNT(*) FROM p_1.application_variables WHERE application_version_id IN (SELECT id FROM p_1.application_versions WHERE application_id = $1)`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, app.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		if count != 0 {
			t.Errorf("%s still has %d rows after delete", label, count)
		}
	}
}

func TestApplicationsRepoPostgres_DeleteClearsApplicationToolsWhenThatTableExists(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	// application_tools exists only in pylon-migrated databases, and has no
	// cascade there. Recreate that shape to prove Delete still clears it.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE p_1.application_tools (
			id SERIAL PRIMARY KEY,
			application_version_id INTEGER NOT NULL REFERENCES p_1.application_versions(id),
			name VARCHAR, type VARCHAR, settings JSONB DEFAULT '{}'::jsonb
		)`); err != nil {
		t.Fatalf("create legacy application_tools: %v", err)
	}

	app := createTestApplication(t, repo, "with-tools", 1, &applications.Version{Name: "base"})
	if _, err := pool.Exec(ctx,
		`INSERT INTO p_1.application_tools (application_version_id, name, type) VALUES ($1, 'n', 't')`,
		app.Versions[0].ID); err != nil {
		t.Fatalf("seed application_tools: %v", err)
	}

	if err := repo.Delete(ctx, testProjectID, app.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.application_tools`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("application_tools still has %d rows after delete", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Versions
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_CreateVersionPersistsEveryColumn(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 3, "three@elitea.ai")
	app := createTestApplication(t, repo, "versioned", 3, nil)

	created, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{
		Name:                 "v2",
		AuthorID:             3,
		AgentType:            "react",
		Instructions:         "Do the thing.",
		WelcomeMessage:       "welcome",
		LLMSettings:          map[string]any{"model_name": "claude", "temperature": 0.5, "max_tokens": float64(1024)},
		ConversationStarters: []any{"start"},
		Meta:                 map[string]any{"step_limit": float64(9)},
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if created.Status != "draft" {
		t.Errorf("status = %q, want draft", created.Status)
	}
	if created.AuthorID != 3 {
		t.Errorf("author_id = %d, want 3", created.AuthorID)
	}

	// Every field must be on the row, not just on the response object.
	var agentType, instructions, welcome, llm, starters, meta string
	var authorID int64
	if err := pool.QueryRow(ctx, `
		SELECT agent_type, instructions, welcome_message,
		       llm_settings::text, conversation_starters::text, meta::text, author_id
		FROM p_1.application_versions WHERE id = $1`, created.ID).Scan(
		&agentType, &instructions, &welcome, &llm, &starters, &meta, &authorID); err != nil {
		t.Fatalf("read back version: %v", err)
	}
	if agentType != "react" || instructions != "Do the thing." || welcome != "welcome" || authorID != 3 {
		t.Errorf("row = agent_type=%q instructions=%q welcome=%q author=%d",
			agentType, instructions, welcome, authorID)
	}
	if !strings.Contains(llm, "claude") {
		t.Errorf("llm_settings = %s", llm)
	}
	if !strings.Contains(starters, "start") {
		t.Errorf("conversation_starters = %s", starters)
	}
	if !strings.Contains(meta, "9") {
		t.Errorf("meta = %s", meta)
	}

	// Defaults when the caller supplies nothing.
	bare, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{AuthorID: 3})
	if err != nil {
		t.Fatalf("create bare version: %v", err)
	}
	if bare.Name != "base" || bare.AgentType != "openai" {
		t.Errorf("bare version = name=%q agent_type=%q, want base/openai", bare.Name, bare.AgentType)
	}
}

func TestApplicationsRepoPostgres_CreateVersionRequiresAnAuthor(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")
	app := createTestApplication(t, repo, "authored", 1, nil)

	_, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{Name: "v"})
	if err == nil {
		t.Fatal("create version without an author succeeded; it must not fall back to user 1")
	}
	if got := apiStatus(err); got != 401 {
		t.Errorf("status = %d, want 401 (err=%v)", got, err)
	}
}

func TestApplicationsRepoPostgres_UpdateVersionWritesOnlySuppliedFields(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "editable", 1, &applications.Version{
		Name:           "base",
		Instructions:   "original",
		WelcomeMessage: "hello",
		AgentType:      "openai",
		LLMSettings:    map[string]any{"model_name": "gpt-4o"},
	})
	versionID := app.Versions[0].ID

	updated, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID,
		applications.Version{Instructions: "revised"})
	if err != nil {
		t.Fatalf("update version: %v", err)
	}
	if updated.Instructions != "revised" {
		t.Errorf("instructions = %q, want revised", updated.Instructions)
	}
	if updated.WelcomeMessage != "hello" || updated.AgentType != "openai" {
		t.Errorf("update clobbered unsupplied fields: %+v", updated)
	}
	if updated.LLMSettings["model_name"] != "gpt-4o" {
		t.Errorf("llm_settings clobbered: %+v", updated.LLMSettings)
	}
	if updated.Name != "base" {
		t.Errorf("name = %q, want base", updated.Name)
	}

	// Explicitly supplied JSON replaces the column — for `llm_settings` and
	// `conversation_starters`. `meta` is the one exception and merges
	// instead; the column is empty here, so both readings agree on this row
	// and the difference is pinned where it can be seen, in
	// TestApplicationsRepoPostgres_UpdateVersionMergesMetaInsteadOfReplacingIt.
	replaced, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		LLMSettings:          map[string]any{"model_name": "claude"},
		ConversationStarters: []any{"a"},
		Meta:                 map[string]any{"step_limit": float64(3)},
	})
	if err != nil {
		t.Fatalf("update version json: %v", err)
	}
	if replaced.LLMSettings["model_name"] != "claude" ||
		!reflect.DeepEqual(replaced.ConversationStarters, []any{"a"}) ||
		replaced.Meta["step_limit"] != float64(3) {
		t.Errorf("json columns did not replace: %+v", replaced)
	}

	// An empty update is a read.
	unchanged, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{})
	if err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if unchanged.Instructions != "revised" {
		t.Errorf("empty update changed the row: %+v", unchanged)
	}
}

// TestApplicationsRepoPostgres_UpdateVersionClearsExplicitEmptyStrings covers
// #824. The test above proves an OMITTED field is left alone; that behaviour
// was implemented as "an empty value is left alone", so a client that cleared
// its welcome message got a 201 and read the old text back. The two cases only
// come apart when both are asserted in one test, which is why they live side
// by side.
func TestApplicationsRepoPostgres_UpdateVersionClearsExplicitEmptyStrings(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "clearable", 1, &applications.Version{
		Name:           "base",
		Instructions:   "x",
		WelcomeMessage: "x",
	})
	versionID := app.Versions[0].ID

	// Present with an empty value CLEARS the column.
	cleared, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		Present: applications.VersionFieldSet{Instructions: true, WelcomeMessage: true},
	})
	if err != nil {
		t.Fatalf("clearing update: %v", err)
	}
	if cleared.Instructions != "" {
		t.Errorf("instructions = %q, want cleared", cleared.Instructions)
	}
	if cleared.WelcomeMessage != "" {
		t.Errorf("welcome_message = %q, want cleared", cleared.WelcomeMessage)
	}

	// Read it back through the repository's own read path, not the UPDATE's
	// RETURNING projection: a SET list that never ran would still echo the
	// values it was handed if the echo came from the request.
	readBack, err := repo.GetVersion(ctx, testProjectID, app.ID, versionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if readBack.Instructions != "" || readBack.WelcomeMessage != "" {
		t.Errorf("stored row still holds the old text: %+v", readBack)
	}

	// Write a value back, then omit the fields: the row must not change.
	if _, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		Instructions:   "y",
		WelcomeMessage: "y",
	}); err != nil {
		t.Fatalf("rewriting update: %v", err)
	}
	untouched, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		Meta: map[string]any{"note": "unrelated"},
	})
	if err != nil {
		t.Fatalf("unrelated update: %v", err)
	}
	if untouched.Instructions != "y" || untouched.WelcomeMessage != "y" {
		t.Errorf("an omitted field was blanked: %+v", untouched)
	}
}

func TestApplicationsRepoPostgres_ListVersionsIsNewestFirstAndScopedToTheApplication(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "many-versions", 1, &applications.Version{Name: "base"})
	second, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{Name: "v2", AuthorID: 1})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	other := createTestApplication(t, repo, "other-app", 1, &applications.Version{Name: "base"})

	versions, err := repo.ListVersions(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	if versions[0].ID != second.ID {
		t.Errorf("versions[0] = %q, want the newest %q", versions[0].ID, second.ID)
	}
	for _, v := range versions {
		if v.ApplicationID != app.ID {
			t.Errorf("version %q belongs to application %q, want %q", v.ID, v.ApplicationID, app.ID)
		}
	}

	otherVersions, err := repo.ListVersions(ctx, testProjectID, other.ID)
	if err != nil {
		t.Fatalf("list other versions: %v", err)
	}
	if len(otherVersions) != 1 {
		t.Errorf("other application has %d versions, want 1", len(otherVersions))
	}

	// A version id belonging to another application is not reachable.
	if _, err := repo.GetVersion(ctx, testProjectID, other.ID, second.ID); err == nil {
		t.Error("GetVersion crossed the application boundary")
	}
	if _, err := repo.UpdateVersion(ctx, testProjectID, other.ID, second.ID,
		applications.Version{Name: "hijacked"}); err == nil {
		t.Error("UpdateVersion crossed the application boundary")
	}
	var nameAfter string
	if err := pool.QueryRow(ctx, `SELECT name FROM p_1.application_versions WHERE id = $1`, second.ID).Scan(&nameAfter); err != nil {
		t.Fatal(err)
	}
	if nameAfter != "v2" {
		t.Errorf("cross-application update renamed the version to %q", nameAfter)
	}
	if err := repo.DeleteVersion(ctx, testProjectID, other.ID, second.ID); err == nil {
		t.Error("DeleteVersion crossed the application boundary")
	}
}

func TestApplicationsRepoPostgres_DeleteVersionRemovesOnlyThatVersion(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "prunable", 1, &applications.Version{Name: "base"})
	extra, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{Name: "v2", AuthorID: 1})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	if err := repo.DeleteVersion(ctx, testProjectID, app.ID, extra.ID); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	remaining, err := repo.ListVersions(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "base" {
		t.Errorf("remaining versions = %+v, want only base", remaining)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SetDefaultVersion
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_SetDefaultVersionRoundTripsThroughApplicationMeta(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "defaulted", 1, &applications.Version{Name: "base"})
	second, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{Name: "v2", AuthorID: 1})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	before, err := repo.ListVersions(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	for _, v := range before {
		if v.IsDefault {
			t.Errorf("version %q is default before any default was set", v.ID)
		}
	}

	if err := repo.SetDefaultVersion(ctx, testProjectID, app.ID, second.ID); err != nil {
		t.Fatalf("set default version: %v", err)
	}

	// The write must be readable, not fire-and-forget: application_versions
	// has no is_default column, so the default lives in
	// applications.meta.default_version_id — the same key the UI's
	// selectDefaultVersion reads.
	var storedDefault string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(meta->>'default_version_id', '') FROM p_1.applications WHERE id = $1`,
		app.ID).Scan(&storedDefault); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if storedDefault != second.ID {
		t.Errorf("meta.default_version_id = %q, want %q", storedDefault, second.ID)
	}

	after, err := repo.ListVersions(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("list versions after: %v", err)
	}
	defaults := []string{}
	for _, v := range after {
		if v.IsDefault {
			defaults = append(defaults, v.ID)
		}
	}
	if !reflect.DeepEqual(defaults, []string{second.ID}) {
		t.Errorf("IsDefault versions = %v, want exactly [%s]", defaults, second.ID)
	}

	single, err := repo.GetVersion(ctx, testProjectID, app.ID, second.ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if !single.IsDefault {
		t.Error("GetVersion does not report the default version as default")
	}
}

func TestApplicationsRepoPostgres_SetDefaultVersionRejectsAForeignVersion(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "app-a", 1, &applications.Version{Name: "base"})
	other := createTestApplication(t, repo, "app-b", 1, &applications.Version{Name: "base"})

	// The previous implementation wrote whatever id it was handed and
	// reported success, leaving a dangling default.
	if err := repo.SetDefaultVersion(ctx, testProjectID, app.ID, other.Versions[0].ID); err == nil {
		t.Fatal("set default accepted a version belonging to another application")
	}
	var meta string
	if err := pool.QueryRow(ctx, `SELECT meta::text FROM p_1.applications WHERE id = $1`, app.ID).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta, "default_version_id") {
		t.Errorf("a rejected set-default still wrote meta = %s", meta)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BatchReplaceVersion
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_BatchReplaceVersionRemapsToolsAndOptionallyDeletes(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "remap", 1, &applications.Version{Name: "base"})
	oldVersion := app.Versions[0]
	newVersion, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{Name: "v2", AuthorID: 1})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	var toolID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, owner_id, author_id) VALUES ('t', 'github', 1, 1)
		RETURNING id`).Scan(&toolID); err != nil {
		t.Fatalf("seed tool: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id)
		VALUES ($1, 'application', $2)`, oldVersion.ID, toolID); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	if err := repo.BatchReplaceVersion(ctx, testProjectID, oldVersion.ID, newVersion.ID, false); err != nil {
		t.Fatalf("batch replace: %v", err)
	}
	var mappedTo string
	if err := pool.QueryRow(ctx,
		`SELECT entity_version_id::text FROM p_1.entity_tool_mapping WHERE tool_id = $1`, toolID).Scan(&mappedTo); err != nil {
		t.Fatalf("read mapping: %v", err)
	}
	if mappedTo != newVersion.ID {
		t.Errorf("mapping points at %q, want the new version %q", mappedTo, newVersion.ID)
	}

	// delete_old=false must leave the old version in place.
	if _, err := repo.GetVersion(ctx, testProjectID, app.ID, oldVersion.ID); err != nil {
		t.Errorf("old version was removed without delete_old: %v", err)
	}

	if err := repo.BatchReplaceVersion(ctx, testProjectID, oldVersion.ID, newVersion.ID, true); err != nil {
		t.Fatalf("batch replace with delete: %v", err)
	}
	if _, err := repo.GetVersion(ctx, testProjectID, app.ID, oldVersion.ID); err == nil {
		t.Error("old version survived delete_old=true")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// VersionConfig — derived projection, never a silent write target
// ─────────────────────────────────────────────────────────────────────────────

func TestApplicationsRepoPostgres_WritingTheDerivedVersionConfigIsRejected(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")
	app := createTestApplication(t, repo, "config-host", 1, &applications.Version{Name: "base"})

	// Each of these has no column of its own; accepting them would silently
	// discard caller data.
	configs := map[string]applications.VersionConfig{
		"model":        {Model: "gpt-4o"},
		"temperature":  {Temperature: 0.5},
		"max_tokens":   {MaxTokens: 128},
		"system_promp": {SystemPrompt: "be brief"},
		"tools":        {Tools: []applications.ToolRef{{ToolkitID: "1", ToolName: "x"}}},
		"skills":       {Skills: []applications.SkillRef{{SkillID: "1"}}},
		"datasources":  {Datasources: []string{"ds"}},
		"guardrails":   {Guardrails: &applications.GuardrailsCfg{}},
	}
	for label, cfg := range configs {
		t.Run(label, func(t *testing.T) {
			_, err := repo.CreateVersion(ctx, testProjectID, app.ID,
				applications.Version{Name: "cfg-" + label, AuthorID: 1, Config: cfg})
			if err == nil {
				t.Fatal("CreateVersion silently accepted a derived config field")
			}
			if got := apiStatus(err); got != 400 {
				t.Errorf("CreateVersion status = %d, want 400 (err=%v)", got, err)
			}

			if _, err := repo.UpdateVersion(ctx, testProjectID, app.ID, app.Versions[0].ID,
				applications.Version{Config: cfg}); err == nil {
				t.Fatal("UpdateVersion silently accepted a derived config field")
			}

			if _, err := repo.Create(ctx, applications.CreateRequest{
				ProjectID: testProjectID, Name: "cfg-app-" + label, AuthorID: 1, Config: &cfg,
			}); err == nil {
				t.Fatal("Create silently accepted a derived config field")
			}
		})
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.application_versions WHERE name LIKE 'cfg-%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d version rows written by rejected config writes, want 0", count)
	}
}
