package eliteacore_test

// #255 acceptance for `GET /elitea_core/admin_published_agents/administration`.
//
// The listing's contract is a filter, and a filter is exactly what a status
// code cannot check: a handler that ignored `status = 'published'` would answer
// 200 with a longer list, and one that read the wrong schema would answer 200
// with an empty one. Both are asserted against directly here, on a fixture that
// contains a draft-only agent, a published agent in ANOTHER project's schema,
// and an agent with two published versions and one draft.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
)

type publishedAgentVersionBody struct {
	VersionID   int     `json:"version_id"`
	VersionName string  `json:"version_name"`
	PublishedAt *string `json:"published_at"`
	PublishedBy any     `json:"published_by"`
}

type publishedAgentBody struct {
	PublicAgentID          int                         `json:"public_agent_id"`
	Name                   string                      `json:"name"`
	Description            string                      `json:"description"`
	AuthorProjectID        *int                        `json:"author_project_id"`
	PublishedVersions      []publishedAgentVersionBody `json:"published_versions"`
	TotalPublishedVersions int                         `json:"total_published_versions"`
	Adoption               struct {
		ConversationCount *int `json:"conversation_count"`
		ProjectCount      *int `json:"project_count"`
	} `json:"adoption"`
	CreatedAt *string `json:"created_at"`
}

type publishedAgentsBody struct {
	Items    []publishedAgentBody `json:"items"`
	Total    int                  `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
}

func (b publishedAgentsBody) byName(name string) (publishedAgentBody, bool) {
	for _, item := range b.Items {
		if item.Name == name {
			return item, true
		}
	}
	return publishedAgentBody{}, false
}

func readPublishedAgents(t *testing.T, router chi.Router, query string) publishedAgentsBody {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet,
		"/elitea_core/admin_published_agents/administration"+query, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET%s status = %d, want 200 (body %s)", query, recorder.Code, recorder.Body.String())
	}
	var body publishedAgentsBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

func newPublishedAgentsEnvironment(t *testing.T) chi.Router {
	t.Helper()
	pool := newPublishedAgentsPool(t)
	ctx := context.Background()

	for _, statement := range []string{
		// The public project's catalogue.
		// The naive created_at/updated_at columns are named with the UTC wall
		// clock: their DEFAULT now() is the session clock, which the naive-clock
		// probe refuses.
		`INSERT INTO p_1.applications (id, name, description, owner_id, shared_owner_id, meta, created_at, updated_at)
		SELECT id, name, description, owner_id, shared_owner_id, meta::jsonb,
		       now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC'
		FROM (VALUES
			(1, 'Adopted Agent', 'two published versions', 1, 7, '{"adoption":{"conversation_count":12,"project_count":3}}'),
			(2, 'Quiet Agent',   'published, unmeasured', 1, 8, '{}'),
			(3, 'Draft Agent',   'never published',       1, 9, '{}')
		) AS seed (id, name, description, owner_id, shared_owner_id, meta)`,
		`INSERT INTO p_1.application_versions (application_id, name, status, author_id, meta, created_at)
		SELECT application_id, name, status, author_id, meta::jsonb, now() AT TIME ZONE 'UTC'
		FROM (VALUES
			(1, 'v1', 'published', 1, '{"published_by": 42}'),
			(1, 'v2', 'published', 1, '{}'),
			(1, 'wip', 'draft',    1, '{}'),
			(2, 'v1', 'published', 1, '{}'),
			(3, 'wip', 'draft',    1, '{}')
		) AS seed (application_id, name, status, author_id, meta)`,
		// Another project's schema, holding a published agent of its own. The
		// listing reads the PUBLIC project; a handler that walked every `p_%`
		// schema — which the dead code this endpoint replaces did — would
		// include it.
		`SELECT create_tenant_schema('p_2')`,
		`INSERT INTO p_2.applications (id, name, description, owner_id, created_at, updated_at) VALUES
			(1, 'Tenant Agent', 'not in the public catalogue', 1,
			 now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')`,
		`INSERT INTO p_2.application_versions (application_id, name, status, author_id, created_at) VALUES
			(1, 'v1', 'published', 1, now() AT TIME ZONE 'UTC')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	router := chi.NewRouter()
	router.Get("/elitea_core/admin_published_agents/administration",
		eliteacore.NewHandler(pool).AdminPublishedAgents)
	return router
}

func TestPublishedAgentsListsOnlyPublishedAgentsInThePublicProject(t *testing.T) {
	router := newPublishedAgentsEnvironment(t)

	body := readPublishedAgents(t, router, "")

	if body.Total != 2 {
		t.Fatalf("total = %d, want 2 (the draft-only agent and the other tenant's are excluded)", body.Total)
	}
	if _, found := body.byName("Draft Agent"); found {
		t.Fatal("an agent with no published version is in the listing")
	}
	if _, found := body.byName("Tenant Agent"); found {
		t.Fatal("another project's published agent is in the PUBLIC catalogue listing")
	}

	adopted, found := body.byName("Adopted Agent")
	if !found {
		t.Fatalf("the published agent is missing; items = %+v", body.Items)
	}
	// Two published versions, and the draft is NOT one of them.
	if adopted.TotalPublishedVersions != 2 || len(adopted.PublishedVersions) != 2 {
		t.Fatalf("published versions = %+v, want exactly v1 and v2", adopted.PublishedVersions)
	}
	for _, version := range adopted.PublishedVersions {
		if version.VersionName == "wip" {
			t.Fatal("a draft version is reported as published")
		}
	}
	if adopted.AuthorProjectID == nil || *adopted.AuthorProjectID != 7 {
		t.Fatalf("author_project_id = %v, want 7", adopted.AuthorProjectID)
	}
}

// TestPublishedAgentsDistinguishesUnmeasuredAdoptionFromZero is the divergence
// this port exists for: the reference defaults both counters to 0, so an agent
// nobody measured is indistinguishable from one nobody used.
func TestPublishedAgentsDistinguishesUnmeasuredAdoptionFromZero(t *testing.T) {
	router := newPublishedAgentsEnvironment(t)
	body := readPublishedAgents(t, router, "")

	adopted, _ := body.byName("Adopted Agent")
	if adopted.Adoption.ConversationCount == nil || *adopted.Adoption.ConversationCount != 12 {
		t.Fatalf("stored conversation_count = %v, want 12", adopted.Adoption.ConversationCount)
	}
	if adopted.Adoption.ProjectCount == nil || *adopted.Adoption.ProjectCount != 3 {
		t.Fatalf("stored project_count = %v, want 3", adopted.Adoption.ProjectCount)
	}

	quiet, found := body.byName("Quiet Agent")
	if !found {
		t.Fatalf("the second published agent is missing; items = %+v", body.Items)
	}
	if quiet.Adoption.ConversationCount != nil || quiet.Adoption.ProjectCount != nil {
		t.Fatalf("unmeasured adoption = %+v, want nulls rather than zeroes", quiet.Adoption)
	}
}

func TestPublishedAgentsPagesAndSorts(t *testing.T) {
	router := newPublishedAgentsEnvironment(t)

	first := readPublishedAgents(t, router, "?page=1&page_size=1&sort=name")
	if len(first.Items) != 1 || first.Total != 2 {
		t.Fatalf("page 1 = %d items of %d total, want 1 of 2", len(first.Items), first.Total)
	}
	second := readPublishedAgents(t, router, "?page=2&page_size=1&sort=name")
	if len(second.Items) != 1 {
		t.Fatalf("page 2 = %d items, want 1", len(second.Items))
	}
	if first.Items[0].Name == second.Items[0].Name {
		t.Fatalf("both pages returned %q — the offset is not applied", first.Items[0].Name)
	}
	// `sort=name` is DESC, so page 1 is the later name.
	if first.Items[0].Name != "Quiet Agent" {
		t.Fatalf("first page by name = %q, want %q", first.Items[0].Name, "Quiet Agent")
	}

	// A page past the end is an empty page, not an error and not page 1 again.
	beyond := readPublishedAgents(t, router, "?page=9&page_size=1")
	if len(beyond.Items) != 0 || beyond.Total != 2 {
		t.Fatalf("page 9 = %d items of %d total, want 0 of 2", len(beyond.Items), beyond.Total)
	}
}

/* ── database ──────────────────────────────────────────────────────────── */

func newPublishedAgentsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	t.Setenv("PUBLIC_PROJECT_ID", "1")

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_published_agents_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
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

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
