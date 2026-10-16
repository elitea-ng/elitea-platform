package toolkits

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

const postgresIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

// p_<id>.elitea_tools.owner_id is INTEGER NOT NULL with no default
// (internal/infra/db/migrations/001_initial.sql). CreateToolkit's INSERT named
// six columns and owner_id was not one of them, so every
// POST /api/v2/elitea_core/tools/prompt_lib/{projectId} died with
//
//	null value in column "owner_id" of relation "elitea_tools"
//	violates not-null constraint (SQLSTATE 23502)
//
// — no toolkit and no MCP could be created through the API at all (#129 D1).
//
// The value is the OWNING PROJECT id, not the creating user; the reasoning and
// its evidence are recorded on createToolkitInsertSQL. This test pins the
// distinction rather than just "not null": the author is deliberately a
// different number from the project, so an INSERT that copies author_id into
// owner_id fails here even though it satisfies the constraint.
func TestCreateToolkitStoresTheProjectAsOwnerAndThePrincipalAsAuthor(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}

	repo := &pgRepo{pool: pool}
	created, err := repo.CreateToolkit(ctx, "1", map[string]any{
		"name":        "owner-id-fixture",
		"type":        "github",
		"description": "created by the D1 regression test",
		"_author_id":  "7",
	})
	if err != nil {
		t.Fatalf("CreateToolkit: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("CreateToolkit returned no id: %#v", created)
	}

	var ownerID, authorID int
	if err := pool.QueryRow(ctx,
		`SELECT owner_id, author_id FROM p_1.elitea_tools WHERE id = $1`, id,
	).Scan(&ownerID, &authorID); err != nil {
		t.Fatalf("read back the created row: %v", err)
	}
	if ownerID != 1 {
		t.Errorf("owner_id = %d, want 1 — the project whose schema the row lives in", ownerID)
	}
	if authorID != 7 {
		t.Errorf("author_id = %d, want 7 — the authenticated principal", authorID)
	}
	if ownerID == authorID {
		t.Errorf("owner_id and author_id are both %d; the fixture chose distinct values precisely so a copy of author_id into owner_id would be visible", ownerID)
	}
}

// A tenant schema name is p_<project id>, so a project id that is not an
// integer cannot address a schema either. Rejecting it here turns what would
// otherwise be a raw PostgreSQL error into a statement about the input.
func TestTenantOwnerIDRejectsNonProjectIdentifiers(t *testing.T) {
	t.Parallel()

	for _, projectID := range []string{"", "prompt_lib", "0", "-3", "1; DROP SCHEMA p_1"} {
		if _, err := tenantOwnerID(projectID); err == nil {
			t.Errorf("tenantOwnerID(%q) succeeded, want an error", projectID)
		}
	}
	ownerID, err := tenantOwnerID("42")
	if err != nil {
		t.Fatalf("tenantOwnerID(\"42\"): %v", err)
	}
	if ownerID != 42 {
		t.Errorf("tenantOwnerID(\"42\") = %d, want 42", ownerID)
	}
}

// The integration test above only runs where a PostgreSQL service is
// provisioned. This one runs everywhere and catches the same regression at the
// statement level: owner_id must be in the column list, and it must be bound to
// a placeholder rather than defaulted or hardcoded.
func TestCreateToolkitInsertSQLBindsOwnerID(t *testing.T) {
	t.Parallel()

	// createToolkitInsertSQL takes an ALREADY-QUOTED identifier, so the test
	// builds it the way the repository does rather than restating the quoting.
	schema, err := tenantschema.Quote("1")
	if err != nil {
		t.Fatalf("Quote(1) failed: %v", err)
	}
	statement := createToolkitInsertSQL(schema, true)

	columns := regexp.MustCompile(`INSERT INTO "p_1"\.elitea_tools \(([^)]*)\)`).FindStringSubmatch(statement)
	if columns == nil {
		t.Fatalf("createToolkitInsertSQL produced an unrecognisable INSERT:\n%s", statement)
	}
	named := strings.Split(strings.ReplaceAll(columns[1], " ", ""), ",")
	if !contains(named, "owner_id") {
		t.Errorf("owner_id is not among the inserted columns %v — every create will fail the NOT NULL constraint", named)
	}
	if !contains(named, "author_id") {
		t.Errorf("author_id is not among the inserted columns %v", named)
	}

	// One placeholder per caller-supplied column, so owner_id is supplied by
	// the caller. created_at is the last column and the server stamps it: the
	// naive column holds the UTC wall clock, which DEFAULT now() would write in
	// the session time zone instead.
	const serverStamp = "now() AT TIME ZONE 'UTC'"
	if named[len(named)-1] != "created_at" {
		t.Fatalf("created_at is not the last inserted column: %v", named)
	}
	placeholders := regexp.MustCompile(`VALUES \(((?:\$\d+, )*\$\d+), ` + regexp.QuoteMeta(serverStamp) + `\)`).FindStringSubmatch(statement)
	if placeholders == nil {
		t.Fatalf("createToolkitInsertSQL has no VALUES list ending in %s:\n%s", serverStamp, statement)
	}
	bound := strings.Split(strings.ReplaceAll(placeholders[1], " ", ""), ",")
	if len(bound) != len(named)-1 {
		t.Errorf("%d caller columns but %d bound values: %v vs %v", len(named)-1, len(bound), named, bound)
	}
	for position := range bound {
		if want := fmt.Sprintf("$%d", position+1); bound[position] != want {
			t.Errorf("value %d is %q, want %q", position+1, bound[position], want)
		}
	}
}

func TestCreateToolkitInsertSQLOmitsOwnerIDForTheCurrentPylonTable(t *testing.T) {
	t.Parallel()

	// The same already-quoted identifier the repository passes; see the sibling
	// case above for why the test builds it rather than restating the quoting.
	schema, err := tenantschema.Quote("1")
	if err != nil {
		t.Fatalf("Quote(1) failed: %v", err)
	}
	statement := createToolkitInsertSQL(schema, false)
	columns := regexp.MustCompile(`INSERT INTO "p_1"\.elitea_tools \(([^)]*)\)`).FindStringSubmatch(statement)
	if columns == nil {
		t.Fatalf("createToolkitInsertSQL produced an unrecognisable INSERT:\n%s", statement)
	}
	named := strings.Split(strings.ReplaceAll(columns[1], " ", ""), ",")
	if contains(named, "owner_id") {
		t.Errorf("owner_id is present in the current Pylon table statement: %v", named)
	}
	if !contains(named, "author_id") {
		t.Errorf("author_id is not among the inserted columns %v", named)
	}
}

func TestCreateToolkitSupportsTheCurrentPylonTableWithoutOwnerID(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE p_1.elitea_tools DROP COLUMN owner_id`); err != nil {
		t.Fatalf("apply the current Pylon table shape: %v", err)
	}

	created, err := (&pgRepo{pool: pool}).CreateToolkit(ctx, "1", map[string]any{
		"name":       "pylon-shape-fixture",
		"type":       "openapi",
		"_author_id": "7",
	})
	if err != nil {
		t.Fatalf("CreateToolkit: %v", err)
	}
	if created["id"] == "" {
		t.Fatalf("CreateToolkit returned no id: %#v", created)
	}

	var authorID int
	if err := pool.QueryRow(ctx, `SELECT author_id FROM p_1.elitea_tools WHERE name = 'pylon-shape-fixture'`).Scan(&authorID); err != nil {
		t.Fatalf("read back the created row: %v", err)
	}
	if authorID != 7 {
		t.Errorf("author_id = %d, want 7", authorID)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// newToolkitsIntegrationPool mirrors the isolated-database helper the repos
// package uses: it creates a throwaway database on the configured server, runs
// the test against it, and drops it afterwards, so a shared PostgreSQL service
// is never mutated.
func newToolkitsIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(postgresIntegrationDatabaseURL)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", postgresIntegrationDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_toolkits_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}
