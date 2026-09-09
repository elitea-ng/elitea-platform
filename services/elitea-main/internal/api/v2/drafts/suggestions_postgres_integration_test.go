package drafts_test

// Issue #881's "API-lane" proof: a seeded toolkit named for the draft's
// domain is suggested — AGAINST A REAL DATABASE, because ListToolkits is SQL
// and the interesting behaviour (v2toolkits.NewPostgresRepository actually
// reads what was inserted, drafts.Handler actually scores it, the wire
// response actually carries it) cannot be answered by a mocked repository.
//
// A real HTTP round trip through apps/elitea-web/e2e's own Playwright
// "API-lane" harness (journeys/api/*.spec.ts) was considered and rejected:
// the generate_application_draft route needs a real LLM completion (the
// standalone/e2e-standalone compose stacks configure no LLM_GATEWAY_URL —
// verified against deploy/docker-compose.e2e-standalone.yml, which mounts no
// elitea-llm-gateway service at all), so a Playwright spec against that
// stack would only ever observe a 503 and could never reach the suggestion
// logic. This test is the same claim ("the draft endpoint suggests a seeded
// toolkit"), proven the way the completer's own non-determinism is already
// factored out elsewhere in this package (a stubCompleter) — with a REAL
// Postgres-backed ToolkitsReader in place of the fake one
// suggestions_test.go's TestApplicationDraftSuggestsMatchingProjectResources
// uses, closing the one gap that test cannot: proof that
// v2toolkits.NewPostgresRepository's SQL actually round-trips real rows.
//
// The schema here is a HAND-CUT SUBSET of elitea_tools (internal/infra/db/
// migrations/001_initial.sql:443-455's own DDL, the columns pgRepo.
// ListToolkits selects), not the ledgered tenant migration chain — same
// convention internal/api/v2/conversations/list_filters_postgres_integration_test.go
// already establishes for this exact class of test.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2drafts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	v2toolkits "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

const suggestionsDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

func newSuggestionsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(suggestionsDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the draft-suggestions integration test", suggestionsDatabaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	name := fmt.Sprintf("elitea_draft_suggest_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		adminPool.Close()
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, cancelDrop := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelDrop()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, `
CREATE SCHEMA p_1;
CREATE TABLE p_1.elitea_tools (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    description TEXT,
    owner_id INTEGER NOT NULL,
    author_id INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    uuid UUID UNIQUE DEFAULT gen_random_uuid(),
    meta JSONB DEFAULT '{}'::jsonb,
    settings JSONB DEFAULT '{}'::jsonb,
    env_vars JSONB DEFAULT '{}'::jsonb
);
`); err != nil {
		t.Fatalf("create schema/table: %v", err)
	}

	return pool
}

func TestGenerateApplicationDraftSuggestsASeededToolkit(t *testing.T) {
	pool := newSuggestionsPool(t)
	ctx := context.Background()

	var jiraToolkitID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
VALUES ('Jira', 'jira', 'Track and triage Jira issues', 1, 1)
RETURNING id`).Scan(&jiraToolkitID); err != nil {
		t.Fatalf("seed the Jira toolkit: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
VALUES ('Weather', 'artifact', 'Forecasts upcoming conditions', 1, 1)`); err != nil {
		t.Fatalf("seed the unrelated toolkit: %v", err)
	}

	toolkitsRepo := v2toolkits.NewPostgresRepository(pool)

	// Bypasses the LLM hop entirely (see the file doc comment for why) —
	// stubCompleter (drafts_test.go, same package) answers deterministically,
	// the same v2predict.Completer interface the real gateway client
	// implements.
	completer := &stubCompleter{content: `{
		"name": "Jira Triager",
		"description": "Triages incidents by filing Jira issues",
		"instructions": "Look at severity and file a Jira ticket"
	}`}

	handler := v2drafts.NewHandler(completer, v2drafts.WithToolkitsRepo(toolkitsRepo))
	mux := chi.NewRouter()
	mux.Post("/generate_application_draft/prompt_lib/{projectID}", handler.GenerateApplicationDraft)

	request := httptest.NewRequest(http.MethodPost, "/generate_application_draft/prompt_lib/1",
		strings.NewReader(`{"user_description":"an agent that triages incidents by filing Jira issues"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	var draft v2drafts.ApplicationDraft
	if err := json.NewDecoder(recorder.Body).Decode(&draft); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(draft.SuggestedToolkits) != 1 {
		t.Fatalf("SuggestedToolkits = %+v, want exactly the seeded Jira toolkit", draft.SuggestedToolkits)
	}
	if draft.SuggestedToolkits[0].ID != fmt.Sprint(jiraToolkitID) {
		t.Errorf("SuggestedToolkits[0].ID = %q, want %d (the seeded Jira toolkit, not the Weather one)",
			draft.SuggestedToolkits[0].ID, jiraToolkitID)
	}
	if draft.SuggestedToolkits[0].Name != "Jira" {
		t.Errorf("SuggestedToolkits[0].Name = %q, want Jira", draft.SuggestedToolkits[0].Name)
	}
}
