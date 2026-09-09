package eliteacore_test

// Real-PostgreSQL coverage for the catalogue twin (catalog_mirror.go).
//
// The defect these tests pin: Publish cloned the version inside the AUTHOR's
// own schema, and PublicApplications reads the PUBLIC project's schema only.
// Publishing from any project other than the public one therefore produced a
// `status = 'published'` row the catalogue could not see. The Published tab
// listed the agent, ELITEA Catalog stayed empty, and neither side reported a
// problem.
//
// So a 200 from Publish proves nothing here. Every assertion below reads the
// catalogue back through the route the UI calls — GET
// /elitea_core/public_applications/prompt_lib — or reads the public schema
// directly. The publish route answering 200 is exactly what the defect did.
//
// The schema is the production migration chain (db.RunMigrations then
// migrate.Runner.ApplyShared/ApplyTenant), applied to TWO tenants: p_1, which
// is the public project by default (internal/publicproject.Default), and p_2,
// which stands for an ordinary user project.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// catalogMirrorToken mints the approval token that skips the inline validation
// gate, as the publish wizard does after its separate validate call.
//
// It is MINTED rather than written down. The token is a signed grant bound to
// the version it was issued for (publish_validation_token.go, issue 855), so a
// hard-coded hexadecimal string no longer stands in for a check that passed —
// and a test that kept one would be asserting the defect that let any string
// through.
func catalogMirrorToken(t *testing.T, pool *pgxpool.Pool, fixture catalogMirrorFixture) string {
	t.Helper()
	return eliteacore.NewHandler(pool).PublishValidationTokenFor(
		context.Background(), fmt.Sprintf("p_%d", fixture.projectID), strconv.Itoa(fixture.versionID))
}

// catalogMirrorFixture is one agent with one draft version, in the project
// named by projectID.
type catalogMirrorFixture struct {
	projectID int
	appID     int
	versionID int
}

// TestPublishFromAPrivateProjectReachesTheCatalog is the acceptance test.
// Publish an agent that lives in p_2 and read ELITEA Catalog back.
func TestPublishFromAPrivateProjectReachesTheCatalog(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := catalogMirrorRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "private project agent")

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
		"category":         "Development",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		CatalogAgentID   string `json:"catalog_agent_id"`
		CatalogVersionID string `json:"catalog_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response %q: %v", recorder.Body.String(), err)
	}
	if response.CatalogAgentID == "" || response.CatalogVersionID == "" {
		t.Fatalf("publish from a private project named no catalogue rows: %s", recorder.Body.String())
	}

	rows := catalogMirrorPublicApplications(t, router)
	if len(rows) != 1 {
		t.Fatalf("catalog holds %d rows, want 1: %v", len(rows), rows)
	}
	if rows[0]["name"] != "private project agent" {
		t.Errorf("catalog row name = %v, want %q", rows[0]["name"], "private project agent")
	}
	if rows[0]["version_name"] != "v-one" {
		t.Errorf("catalog row version_name = %v, want %q", rows[0]["version_name"], "v-one")
	}
	meta, _ := rows[0]["meta"].(map[string]any)
	if meta["category"] != "Development" {
		t.Errorf("catalog row category = %v, want %q", meta["category"], "Development")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The origin key is what unpublish and a re-publish both find the twin by.
	var sharedOwnerID, sharedID int
	if err := pool.QueryRow(ctx, `
SELECT shared_owner_id, shared_id FROM p_1.applications WHERE id = $1`, response.CatalogAgentID).
		Scan(&sharedOwnerID, &sharedID); err != nil {
		t.Fatalf("catalogue application twin carries no origin: %v", err)
	}
	if sharedOwnerID != fixture.projectID || sharedID != fixture.appID {
		t.Errorf("twin origin = (%d, %d), want the source (%d, %d)",
			sharedOwnerID, sharedID, fixture.projectID, fixture.appID)
	}

	// The author's own project keeps its published clone — that row is what
	// the Published tab lists, and it must not have moved.
	var ownProjectPublished int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_2.application_versions WHERE application_id = $1 AND status = 'published'`,
		fixture.appID).Scan(&ownProjectPublished); err != nil {
		t.Fatal(err)
	}
	if ownProjectPublished != 1 {
		t.Errorf("source project holds %d published versions, want 1", ownProjectPublished)
	}
}

// TestUnpublishRemovesTheCatalogTwin closes the other direction. An agent that
// stayed in the catalogue after its author unpublished it is the failure the
// publish dialog's own terms promise cannot happen.
func TestUnpublishRemovesTheCatalogTwin(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := catalogMirrorRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "retractable agent")

	publishRecorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if publishRecorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", publishRecorder.Code, publishRecorder.Body.String())
	}
	if len(catalogMirrorPublicApplications(t, router)) != 1 {
		t.Fatalf("catalog did not receive the agent, so the unpublish assertion would pass vacuously")
	}

	var published struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(publishRecorder.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}

	target := fmt.Sprintf("/elitea_core/unpublish/prompt_lib/2/%s", published.PublicVersionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	if rows := catalogMirrorPublicApplications(t, router); len(rows) != 0 {
		t.Fatalf("catalog still holds %d rows after unpublish: %v", len(rows), rows)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The emptied twin application goes too, so a re-publish reuses the origin
	// key rather than leaving a nameless catalogue entry behind.
	var twinApplications int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.applications WHERE shared_owner_id = 2`).Scan(&twinApplications); err != nil {
		t.Fatal(err)
	}
	if twinApplications != 0 {
		t.Errorf("unpublish left %d empty catalogue application twins", twinApplications)
	}

	// The source version is back to draft, which is what the Published tab
	// reads to stop listing it.
	var status string
	if err := pool.QueryRow(ctx, `
SELECT status FROM p_2.application_versions WHERE id = $1`, published.PublicVersionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Errorf("source version status = %q, want %q", status, "draft")
	}
}

// TestRepublishReusesTheSameCatalogTwin proves the origin key is doing its
// work: a second publish of the same agent must add a version to the existing
// catalogue entry, not a second entry for the same agent.
func TestRepublishReusesTheSameCatalogTwin(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := catalogMirrorRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "twice published agent")

	for _, versionName := range []string{"v-one", "v-two"} {
		recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
			"version_name":     versionName,
			"validation_token": catalogMirrorToken(t, pool, fixture),
		})
		if recorder.Code != http.StatusOK {
			t.Fatalf("publish %q status = %d, body = %s", versionName, recorder.Code, recorder.Body.String())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var twinApplications, twinVersions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.applications WHERE shared_owner_id = 2 AND shared_id = $1`, fixture.appID).
		Scan(&twinApplications); err != nil {
		t.Fatal(err)
	}
	if twinApplications != 1 {
		t.Errorf("catalogue holds %d application twins for one agent, want 1", twinApplications)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.application_versions WHERE shared_owner_id = 2`).Scan(&twinVersions); err != nil {
		t.Fatal(err)
	}
	if twinVersions != 2 {
		t.Errorf("catalogue holds %d version twins, want 2", twinVersions)
	}
}

// TestPublishFromThePublicProjectWritesNoTwin keeps the mirror from doubling
// the catalogue for an author who already stands inside the public project —
// their clone IS the catalogue row.
func TestPublishFromThePublicProjectWritesNoTwin(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := catalogMirrorRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 1, "public project agent")

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	// The key is OMITTED rather than zero: a zero would read as "the catalogue
	// holds row 0".
	if _, present := response["catalog_version_id"]; present {
		t.Errorf("publish from the public project named a catalogue twin: %v", response["catalog_version_id"])
	}

	if rows := catalogMirrorPublicApplications(t, router); len(rows) != 1 {
		t.Fatalf("catalog holds %d rows, want exactly 1: %v", len(rows), rows)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func catalogMirrorRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", handler.Unpublish)
	router.Get("/elitea_core/public_applications/prompt_lib", handler.PublicApplications)
	return router
}

func catalogMirrorPublish(t *testing.T, router chi.Router, fixture catalogMirrorFixture, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/elitea_core/publish/prompt_lib/%d/%d", fixture.projectID, fixture.versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// catalogMirrorPublicApplications reads the catalogue through the same route
// the web app's ELITEA Catalog calls.
func catalogMirrorPublicApplications(t *testing.T, router chi.Router) []map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/elitea_core/public_applications/prompt_lib", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("public_applications status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode public_applications %q: %v", recorder.Body.String(), err)
	}
	return body.Rows
}

func seedCatalogMirrorFixture(t *testing.T, pool *pgxpool.Pool, projectID int, name string) catalogMirrorFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	schema := fmt.Sprintf("p_%d", projectID)
	fixture := catalogMirrorFixture{projectID: projectID}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, owner_id) VALUES ($1, 'seeded for the catalogue mirror', $2) RETURNING id`, schema),
		name, projectID).Scan(&fixture.appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the thing', 'agent') RETURNING id`, schema), fixture.appID).
		Scan(&fixture.versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	return fixture
}

// newCatalogMirrorPool opens an isolated database on the server named by
// ELITEA_TEST_DATABASE_URL and applies the production migration chain to two
// tenants: 1 (the public project) and 2 (an ordinary user project).
func newCatalogMirrorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL catalogue-mirror integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	databaseName := fmt.Sprintf("elitea_catmirror_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	// ApplyTenant refuses a project that `centry.project` does not list, so the
	// second tenant needs its row before its schema. Project 1 arrives with the
	// bootstrap chain; project 2 is this fixture's ordinary user project.
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, name, owner_id, create_success, suspended)
VALUES (2, 'catalog mirror fixture', 1, true, false)
ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed the second project: %v", err)
	}
	// 001_initial.sql builds p_1 with its own `SELECT create_tenant_schema('p_1')`
	// and stops there. p_2 is built by the same function, so the fixture's two
	// tenants have identical shape and neither is a hand-written subset.
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema('p_2')`); err != nil {
		t.Fatalf("build the second tenant schema: %v", err)
	}
	for _, tenant := range []int64{1, 2} {
		if err := runner.ApplyTenant(ctx, tenant); err != nil {
			t.Fatalf("apply embedded tenant migrations for project %d: %v", tenant, err)
		}
	}

	// Guard the premise: p_1 must be the public project for these tests to
	// mean anything, and p_2 must exist and be a different schema.
	for _, schema := range []string{"p_1", "p_2"} {
		var present bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, schema+".application_versions").Scan(&present); err != nil {
			t.Fatalf("probe %s: %v", schema, err)
		}
		if !present {
			t.Fatalf("%s.application_versions is missing; the fixture cannot exercise the mirror", schema)
		}
	}
	if id := os.Getenv("ELITEA_AI_PROJECT_ID"); id != "" && id != "1" {
		t.Skipf("ELITEA_AI_PROJECT_ID=%s: these tests assume the default public project 1", id)
	}
	return pool
}
