package conversations

// The two listing filters the DeepWiki wiki chat drawer depends on:
// `?hidden=only` and `?mine=true`.
//
// AGAINST A REAL DATABASE, because both are SQL. The interesting behaviour is
// which rows come back, and a mock repository cannot answer that — List does
// not go through Repository at all, it builds its own statement against the
// pool.
//
// The schema here is a HAND-CUT SUBSET rather than the migration corpus: the
// statement under test touches two tables and two JSON paths, and standing up
// the ledgered tenant history for that would make this test a test of the
// migration runner. The columns are transcribed from
// migrations/tenant/0123_agent_chat_message_tables.sql.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const listFiltersDatabaseURL = "ELITEA_TEST_DATABASE_URL"

func newListFiltersPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(listFiltersDatabaseURL)
	if databaseURL == "" {
		t.Skipf("set %s to run the conversation-listing integration test", listFiltersDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	name := fmt.Sprintf("elitea_list_%d_%d", os.Getpid(), time.Now().UnixNano())
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
CREATE TABLE p_1.chat_conversations (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    name varchar NOT NULL,
    is_private boolean NOT NULL DEFAULT TRUE,
    author_id integer NOT NULL,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    source varchar(64) NOT NULL DEFAULT 'elitea',
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);
CREATE TABLE p_1.chat_message_group (
    id serial PRIMARY KEY,
    conversation_id integer NOT NULL REFERENCES p_1.chat_conversations(id)
);`); err != nil {
		t.Fatalf("create the listing fixture schema: %v", err)
	}
	return pool
}

// seedConversation inserts one row with the meta document the wiki chat store
// writes, so the filters are exercised against the shape they will meet.
func seedConversation(t *testing.T, pool *pgxpool.Pool, name, source string, author int, meta map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO p_1.chat_conversations (name, author_id, meta, source) VALUES ($1, $2, $3::jsonb, $4)`,
		name, author, string(encoded), source); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
}

func wikiMeta(toolkitID int) map[string]any {
	return map[string]any{
		"is_hidden":         true,
		"conversation_type": "deepwiki",
		"single_participant": map[string]any{
			"entity_name": "toolkit",
			"entity_meta": map[string]any{"id": toolkitID},
		},
	}
}

// listNames drives GET / with the given query string and returns the names it
// answered with, in order.
func listNames(t *testing.T, pool *pgxpool.Pool, query string, callerID string) []string {
	t.Helper()
	handler := NewHandler(nil).WithPool(pool)
	router := chi.NewRouter()
	router.Get("/{projectID}", handler.List)

	request := httptest.NewRequest(http.MethodGet, "/1?"+query, nil)
	if callerID != "" {
		request = request.WithContext(
			auth.ContextWithUser(request.Context(), auth.User{UserID: callerID}))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing answered %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Rows []struct {
			Name string `json:"name"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	names := make([]string, 0, len(payload.Rows))
	for _, row := range payload.Rows {
		names = append(names, row.Name)
	}
	return names
}

func seedTheListingCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedConversation(t, pool, "an ordinary chat", "elitea", 7, map[string]any{})
	seedConversation(t, pool, "my wiki chat", "deepwiki", 7, wikiMeta(42))
	seedConversation(t, pool, "my other wiki's chat", "deepwiki", 7, wikiMeta(43))
	seedConversation(t, pool, "somebody else's wiki chat", "deepwiki", 8, wikiMeta(42))
	seedConversation(t, pool, "a support conversation", "support", 7,
		map[string]any{"is_hidden": true, "conversation_type": "support"})
}

// THE RULE THAT KEEPS WIKI CHATS OUT OF THE CHAT LIST. It is the default and
// it is unchanged by this feature — every existing caller sends no `hidden`
// parameter at all.
func TestTheDefaultListingStillExcludesHiddenConversations(t *testing.T) {
	pool := newListFiltersPool(t)
	seedTheListingCorpus(t, pool)

	got := listNames(t, pool, "", "7")
	if fmt.Sprint(got) != fmt.Sprint([]string{"an ordinary chat"}) {
		t.Fatalf("the default listing returned %v, want the ordinary chat alone", got)
	}
}

// And the drawer's own query: this toolkit's hidden deepwiki conversations,
// mine only.
func TestTheDrawersQueryReturnsThisToolkitsOwnConversations(t *testing.T) {
	pool := newListFiltersPool(t)
	seedTheListingCorpus(t, pool)

	got := listNames(t, pool,
		"source=deepwiki&entity_name=toolkit&entity_meta_id=42&hidden=only&mine=true", "7")
	if fmt.Sprint(got) != fmt.Sprint([]string{"my wiki chat"}) {
		t.Fatalf("the drawer's query returned %v, want my wiki chat alone", got)
	}
}

// `mine=true` is what stops one member reading another's questions. This
// listing has never read `is_private`, so without it the drawer would show
// every member's wiki chat in the project.
func TestMineExcludesAnotherMembersConversations(t *testing.T) {
	pool := newListFiltersPool(t)
	seedTheListingCorpus(t, pool)

	withoutMine := listNames(t, pool,
		"source=deepwiki&entity_name=toolkit&entity_meta_id=42&hidden=only", "7")
	if len(withoutMine) != 2 {
		t.Fatalf("without mine=true the listing returned %v, want both members' chats", withoutMine)
	}
	withMine := listNames(t, pool,
		"source=deepwiki&entity_name=toolkit&entity_meta_id=42&hidden=only&mine=true", "7")
	for _, name := range withMine {
		if name == "somebody else's wiki chat" {
			t.Fatalf("mine=true returned another member's conversation: %v", withMine)
		}
	}
}

// An unauthenticated caller cannot narrow to itself. Ignoring the filter would
// hand it EVERY member's conversations, which is how a privacy control becomes
// a no-op.
func TestMineAnswersNothingWithoutACaller(t *testing.T) {
	pool := newListFiltersPool(t)
	seedTheListingCorpus(t, pool)

	if got := listNames(t, pool, "hidden=only&mine=true", ""); len(got) != 0 {
		t.Fatalf("an unauthenticated caller received %v", got)
	}
}

// A value that is not `only` takes the default. Reading "anything truthy" as
// "include hidden" would put every support transcript into the chat list.
func TestAnUnknownHiddenValueTakesTheDefault(t *testing.T) {
	pool := newListFiltersPool(t)
	seedTheListingCorpus(t, pool)

	for _, value := range []string{"true", "1", "yes", "include", ""} {
		got := listNames(t, pool, "hidden="+value, "7")
		if fmt.Sprint(got) != fmt.Sprint([]string{"an ordinary chat"}) {
			t.Fatalf("hidden=%q returned %v, want the ordinary chat alone", value, got)
		}
	}
}
