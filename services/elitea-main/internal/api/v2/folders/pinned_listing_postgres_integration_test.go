package folders_test

// Where the sidebar's PINNED group comes from.
//
// "Pin on top" writes `social_pins` — that is what both pin routes do
// (`POST`/`DELETE /social/pin/prompt_lib/{p}/conversation/{id}` and the
// `elitea_core/pin` pair beside them), and it is the table legacy reads for
// the same group (elitea_core/api/v2/folder.py queries the social Pin model
// for `entity == 'conversation'`). This listing read `meta->>'is_pinned'`
// instead — a key nothing in this service or its client ever writes for a
// conversation. So a pin answered `{"ok": true}`, the sidebar moved the row
// optimistically, and the next listing put it straight back: a user could pin
// a conversation as many times as they liked and it never stayed.
//
// The second fact this file pins is `is_private` on the same rows. The row
// menu offers "Make public" only while a conversation IS private, and this
// listing is where the sidebar learns that; the client normaliser already
// reads the key and defaults an absent one to private, so a published
// conversation went on being offered the control that publishes it.
//
// AGAINST A REAL DATABASE, because both facts are one SQL statement. The
// schema is a hand-cut subset for the reason the conversations package's own
// listing test gives: the statement touches three tables, and standing up the
// migration corpus for it would make this a test of the migration runner.
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

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const pinnedListingDatabaseURL = "ELITEA_TEST_DATABASE_URL"

func newPinnedListingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(pinnedListingDatabaseURL)
	if databaseURL == "" {
		t.Skipf("set %s to run the pinned-listing integration test", pinnedListingDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	name := fmt.Sprintf("elitea_pins_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	// Columns transcribed from migrations/tenant/001_initial.sql's
	// `create_tenant_schema` (chat_conversations, social_pins).
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA p_1;
CREATE TABLE p_1.chat_conversations (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    name varchar NOT NULL,
    is_private boolean NOT NULL DEFAULT TRUE,
    author_id integer NOT NULL,
    folder_id integer,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);
CREATE TABLE p_1.social_pins (
    id serial PRIMARY KEY,
    entity_name varchar NOT NULL,
    entity_id integer NOT NULL,
    user_id integer NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    CONSTRAINT _pin_unique UNIQUE (entity_name, entity_id, user_id)
);`); err != nil {
		t.Fatalf("create the pinned-listing fixture schema: %v", err)
	}
	return pool
}

func seedPinnedListingConversation(t *testing.T, pool *pgxpool.Pool, name string, isPrivate bool) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO p_1.chat_conversations (name, author_id, is_private) VALUES ($1, 1, $2) RETURNING id`,
		name, isPrivate).Scan(&id); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
	return id
}

type groupedListing struct {
	Pinned struct {
		Conversations []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			IsPrivate bool   `json:"is_private"`
		} `json:"conversations"`
	} `json:"pinned"`
	DateGroups []struct {
		Name          string `json:"name"`
		Conversations []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			IsPrivate bool   `json:"is_private"`
		} `json:"conversations"`
	} `json:"date_groups"`
}

// readGroupedListing calls the sidebar's own read as `userID` sees it.
func readGroupedListing(t *testing.T, pool *pgxpool.Pool, userID string) groupedListing {
	t.Helper()
	h := handler.NewHandler(&mockFolderRepo{}).WithPool(pool)
	router := chi.NewRouter()
	router.Route("/api/v2/projects/{projectID}/folders", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects/1/folders/?grouped=true", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: userID}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("grouped listing answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var listing groupedListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode the grouped listing: %v", err)
	}
	return listing
}

func TestGroupedListingReadsThePinnedSetFromSocialPins(t *testing.T) {
	pool := newPinnedListingPool(t)
	pinned := seedPinnedListingConversation(t, pool, "autotest_pinned", true)
	seedPinnedListingConversation(t, pool, "autotest_plain", true)

	// Before the pin: the row is in a date group, and nothing is pinned.
	if got := readGroupedListing(t, pool, "7").Pinned.Conversations; len(got) != 0 {
		t.Fatalf("pinned=%d before any pin, want 0", len(got))
	}

	// The pin, written the way the pin routes write it.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO p_1.social_pins (entity_name, entity_id, user_id) VALUES ('conversation', $1, 7)`,
		pinned); err != nil {
		t.Fatalf("pin the conversation: %v", err)
	}

	listing := readGroupedListing(t, pool, "7")
	if len(listing.Pinned.Conversations) != 1 || listing.Pinned.Conversations[0].ID != pinned {
		t.Fatalf("pinned=%v, want exactly the pinned conversation %d", listing.Pinned.Conversations, pinned)
	}
	// …and it left the date groups, which is what makes the sidebar show it
	// once rather than twice.
	for _, group := range listing.DateGroups {
		for _, conversation := range group.Conversations {
			if conversation.ID == pinned {
				t.Errorf("the pinned conversation is still in date group %q", group.Name)
			}
		}
	}
}

// The pin is PER READER. `social_pins` is keyed by (entity_name, entity_id,
// user_id), so a listing that ignored the caller would show one member the
// rows another member pinned.
func TestGroupedListingPinsAreScopedToTheReader(t *testing.T) {
	pool := newPinnedListingPool(t)
	pinned := seedPinnedListingConversation(t, pool, "autotest_pinned_by_7", true)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO p_1.social_pins (entity_name, entity_id, user_id) VALUES ('conversation', $1, 7)`,
		pinned); err != nil {
		t.Fatalf("pin the conversation: %v", err)
	}

	if got := readGroupedListing(t, pool, "8").Pinned.Conversations; len(got) != 0 {
		t.Errorf("reader 8 sees %d pinned rows, want 0 — the pin belongs to reader 7", len(got))
	}
}

// A pin on a DIFFERENT entity type is not a conversation pin. The table is
// shared with configurations, agents and toolkits, so the discriminator is
// load-bearing.
func TestGroupedListingIgnoresPinsOnOtherEntities(t *testing.T) {
	pool := newPinnedListingPool(t)
	conversation := seedPinnedListingConversation(t, pool, "autotest_not_pinned", true)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO p_1.social_pins (entity_name, entity_id, user_id) VALUES ('configuration', $1, 7)`,
		conversation); err != nil {
		t.Fatalf("pin a configuration: %v", err)
	}

	if got := readGroupedListing(t, pool, "7").Pinned.Conversations; len(got) != 0 {
		t.Errorf("pinned=%d, want 0 — the pin names a configuration, not this conversation", len(got))
	}
}

// The sidebar must be able to tell a published conversation from a private
// one, or "Make public" is offered forever.
func TestGroupedListingCarriesEachConversationsVisibility(t *testing.T) {
	pool := newPinnedListingPool(t)
	seedPinnedListingConversation(t, pool, "autotest_private", true)
	seedPinnedListingConversation(t, pool, "autotest_public", false)

	byName := map[string]bool{}
	for _, group := range readGroupedListing(t, pool, "7").DateGroups {
		for _, conversation := range group.Conversations {
			byName[conversation.Name] = conversation.IsPrivate
		}
	}
	if len(byName) != 2 {
		t.Fatalf("date groups carried %d conversations, want 2", len(byName))
	}
	if !byName["autotest_private"] {
		t.Error("the private conversation came back as public")
	}
	if byName["autotest_public"] {
		t.Error("the published conversation came back as private")
	}
}
