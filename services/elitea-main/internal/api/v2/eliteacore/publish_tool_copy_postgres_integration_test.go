package eliteacore_test

// Real-PostgreSQL coverage for the tool-mapping copy in the PUBLISH path
// (handler.go, Handler.Publish) — issue #331.
//
// #331 names two copy statements in internal/api/oapiserver. This is the third
// copy, and the only one production serves. It named its columns correctly, so
// it wrote rows, but it ran as `_, _ = h.pool.Exec(...)` on the pool. A failed
// copy therefore published a version and answered 200, and the user got a
// published agent that had lost every toolkit.
//
// So these tests never assert on the status code alone. A 200 from Publish is
// exactly what the defect produced. The assertions read the mapping rows back
// out of the database.
//
// The schema comes from the REAL migration corpus — db.RunMigrations followed by
// migrate.Runner.ApplyShared/ApplyTenant. That is the production chain, and the
// only way migration 0125's `entity_id NOT NULL` is in scope. A hand-built
// fixture that declares the table with no NOT NULL is how this class of defect
// survives.

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

// publishCopyToken mints the approval token that skips the inline validation
// gate, as the UI does after a separate validate call.
//
// It is MINTED rather than written down, for the reason catalogMirrorToken
// gives: the token is a signed grant bound to the version, so a literal string
// is no longer a token at all (issue 855).
func publishCopyToken(t *testing.T, pool *pgxpool.Pool, versionID int) string {
	t.Helper()
	return eliteacore.NewHandler(pool).PublishValidationTokenFor(
		context.Background(), "p_1", strconv.Itoa(versionID))
}

// publishCopyFixture is one agent in p_1 with one toolkit attached to its
// single draft version.
type publishCopyFixture struct {
	appID         int
	versionID     int
	toolID        int
	selectedTools string
}

// TestPublishCarriesToolAttachments is the acceptance test #331 asks for on the
// publish half: publish an agent that HAS a tool attachment, then prove the
// copied row survives with the correct entity_type.
func TestPublishCarriesToolAttachments(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedPublishCopyFixture(t, pool)
	router := publishCopyRouter(eliteacore.NewHandler(pool))

	recorder := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response %q: %v", recorder.Body.String(), err)
	}
	var publishedVersionID int
	if _, err := fmt.Sscanf(response.PublicVersionID, "%d", &publishedVersionID); err != nil {
		t.Fatalf("publish returned public_version_id %q: %v", response.PublicVersionID, err)
	}
	if publishedVersionID == fixture.versionID {
		t.Fatalf("publish returned the source version %d", publishedVersionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var status string
	if err := pool.QueryRow(ctx, `
SELECT status FROM p_1.application_versions WHERE id = $1`, publishedVersionID).Scan(&status); err != nil {
		t.Fatalf("published version %d does not exist: %v", publishedVersionID, err)
	}
	if status != "published" {
		t.Errorf("published version status = %q, want %q", status, "published")
	}

	var toolID, entityID int
	var entityType, selectedTools string
	if err := pool.QueryRow(ctx, `
SELECT tool_id, entity_id, entity_type, COALESCE(selected_tools::text, '')
FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`, publishedVersionID).
		Scan(&toolID, &entityID, &entityType, &selectedTools); err != nil {
		t.Fatalf("published version carries no tool attachment: %v", err)
	}
	if toolID != fixture.toolID {
		t.Errorf("copied tool_id = %d, want %d", toolID, fixture.toolID)
	}
	// The publish clone keeps the source application_id, so the published
	// version belongs to the same agent and entity_id must still name it. The
	// chat read joins on it (internal/db/queries/agent_chat.sql:107).
	if entityID != fixture.appID {
		t.Errorf("copied entity_id = %d, want the owning application %d", entityID, fixture.appID)
	}
	if entityType != "agent" {
		t.Errorf("copied entity_type = %q, want %q", entityType, "agent")
	}
	// Dropping selected_tools silently re-enables every tool in the toolkit.
	if selectedTools != fixture.selectedTools {
		t.Errorf("copied selected_tools = %q, want %q", selectedTools, fixture.selectedTools)
	}
}

// TestPublishReportsAFailedToolCopy proves the error is no longer discarded.
// The copy ran as `_, _ = h.pool.Exec(...)`, so a permanently failing statement
// was indistinguishable from a working one: the caller got 200 and a published
// agent with no toolkits. The caller must now see the failure, and the publish
// must leave no published version behind to retry around.
func TestPublishReportsAFailedToolCopy(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedPublishCopyFixture(t, pool)
	router := publishCopyRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Make the tool-mapping copy the statement that fails, and leave the version
	// clone before it untouched. The seeded row satisfies this constraint; every
	// copied row carries a higher entity_version_id and cannot.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
ALTER TABLE p_1.entity_tool_mapping ADD CONSTRAINT copy_must_fail CHECK (entity_version_id < %d)`,
		fixture.versionID+1)); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	recorder := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if recorder.Code == http.StatusOK {
		t.Fatalf("publish reported success while the tool copy failed: %s", recorder.Body.String())
	}

	var publishedVersions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND status = 'published'`,
		fixture.appID).Scan(&publishedVersions); err != nil {
		t.Fatal(err)
	}
	if publishedVersions != 0 {
		t.Fatalf("failed publish left %d published versions behind", publishedVersions)
	}

	// The source version must still hold its own attachment.
	var toolID int
	if err := pool.QueryRow(ctx, `
SELECT tool_id FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`, fixture.versionID).Scan(&toolID); err != nil {
		t.Fatalf("failed publish detached the source toolkit: %v", err)
	}
	if toolID != fixture.toolID {
		t.Fatalf("source tool_id = %d, want the untouched %d", toolID, fixture.toolID)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func publishCopyRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	return router
}

func publishCopyDo(t *testing.T, router chi.Router, versionID int, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/elitea_core/publish/prompt_lib/1/%d", versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// seedPublishCopyFixture creates the agent, its draft version and one toolkit,
// and attaches the toolkit through the five-column shape the production writers
// use (internal/api/v2/toolkits/handler.go:818).
func seedPublishCopyFixture(t *testing.T, pool *pgxpool.Pool) publishCopyFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fixture := publishCopyFixture{selectedTools: `["get_issue", "create_issue"]`}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('fixture agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the thing', 'agent') RETURNING id`, fixture.appID).Scan(&fixture.versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
VALUES ('fixture toolkit', 'github', '', 1, 1) RETURNING id`).Scan(&fixture.toolID); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
VALUES ($1, $2, 'agent', $3, $4::jsonb)`, fixture.versionID, fixture.appID, fixture.toolID, fixture.selectedTools); err != nil {
		t.Fatalf("seed tool attachment: %v", err)
	}
	// Read selected_tools back in the database's own normalization so the
	// assertion after the publish compares like with like.
	if err := pool.QueryRow(ctx, `
SELECT selected_tools::text FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`, fixture.versionID).
		Scan(&fixture.selectedTools); err != nil {
		t.Fatalf("read back seeded selected_tools: %v", err)
	}
	return fixture
}

// newPublishCopyPool opens an isolated database on the server named by
// ELITEA_TEST_DATABASE_URL and applies the production migration chain to it.
func newPublishCopyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL publish tool-copy integration test", environment)
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

	databaseName := fmt.Sprintf("elitea_pubcopy_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
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
	// Guard the point of the exercise: if the corpus stopped delivering the NOT
	// NULL columns, these tests would pass against a schema that cannot
	// reproduce the defect.
	assertPublishCopyNotNullColumns(t, pool)
	return pool
}

func assertPublishCopyNotNullColumns(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, column := range []string{"entity_id", "entity_type"} {
		var notNull bool
		if err := pool.QueryRow(ctx, `
SELECT attnotnull FROM pg_attribute
WHERE attrelid = 'p_1.entity_tool_mapping'::regclass AND attname = $1 AND NOT attisdropped`,
			column).Scan(&notNull); err != nil {
			t.Fatalf("read p_1.entity_tool_mapping.%s: %v", column, err)
		}
		if !notNull {
			t.Fatalf("p_1.entity_tool_mapping.%s is not NOT NULL — the migration corpus no longer reproduces the defect", column)
		}
	}
}
