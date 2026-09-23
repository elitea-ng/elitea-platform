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
CREATE SCHEMA centry;
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
CREATE TABLE p_1.chat_participants(id integer PRIMARY KEY,entity_name text,entity_meta jsonb);
INSERT INTO p_1.chat_participants VALUES (7,'user','{"id":7}'),(8,'user','{"id":8}');
CREATE TABLE p_1.chat_participant_mapping(conversation_id integer,participant_id integer);
CREATE TABLE centry.social_pins (
    id serial PRIMARY KEY,
    entity varchar NOT NULL,
    project_id integer,
    entity_id integer NOT NULL,
    user_id integer NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    CONSTRAINT _pin_unique UNIQUE (entity, project_id, entity_id)
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
	// Both readers participate; these tests isolate pin and ordering behavior.
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.chat_participant_mapping VALUES ($1,7),($1,8)`, id); err != nil {
		t.Fatal(err)
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
		`INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id) VALUES ('conversation', 1, $1, 7)`,
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
		`INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id) VALUES ('conversation', 1, $1, 7)`,
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
		`INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id) VALUES ('configuration', 1, $1, 7)`,
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

// seedPinnedListingConversationAt seeds one conversation with the two
// timestamps the listing sorts and groups on. `updatedAt` is nil for a
// conversation nobody has revised — which is the state EVERY freshly created
// conversation is in, because the column is nullable and carries no default.
func seedPinnedListingConversationAt(t *testing.T, pool *pgxpool.Pool, name string, createdAt time.Time, updatedAt *time.Time) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO p_1.chat_conversations (name, author_id, is_private, created_at, updated_at)
		 VALUES ($1, 1, TRUE, $2, $3) RETURNING id`,
		name, createdAt, updatedAt).Scan(&id); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
	// Both readers participate; these tests isolate pin and ordering behavior.
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.chat_participant_mapping VALUES ($1,7),($1,8)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// namesInDateGroups flattens the listing the sidebar renders: buckets in the
// order the answer lists them, rows in the order inside each bucket.
func namesInDateGroups(listing groupedListing) []string {
	names := make([]string, 0)
	for _, group := range listing.DateGroups {
		for _, conversation := range group.Conversations {
			names = append(names, conversation.Name)
		}
	}
	return names
}

// A conversation nobody has revised must still sort by WHEN IT WAS MADE.
//
// `updated_at` is nullable with no default, so every conversation starts life
// with NULL there. `ORDER BY c.updated_at DESC` made all of them compare equal
// AND sort above every revised row (PostgreSQL orders NULLs first descending),
// so the rail's order came back different on every request for the same data.
// The client re-sorts by `updated_at ?? created_at`, so the answer and the
// screen disagreed at random.
func TestGroupedListingOrdersNeverRevisedConversationsByCreationTime(t *testing.T) {
	pool := newPinnedListingPool(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	// Seeded oldest-first, so an answer that merely echoed insertion order
	// would fail: the listing must return them newest-first.
	seedPinnedListingConversationAt(t, pool, "autotest_order_oldest", base, nil)
	seedPinnedListingConversationAt(t, pool, "autotest_order_middle", base.Add(time.Minute), nil)
	seedPinnedListingConversationAt(t, pool, "autotest_order_newest", base.Add(2*time.Minute), nil)

	want := []string{"autotest_order_newest", "autotest_order_middle", "autotest_order_oldest"}
	// Read twice: the defect this covers was an UNSTABLE order, which one
	// reading can pass by luck.
	for attempt := 1; attempt <= 3; attempt++ {
		got := namesInDateGroups(readGroupedListing(t, pool, "7"))
		if len(got) != len(want) {
			t.Fatalf("reading %d carried %d conversations, want %d", attempt, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("reading %d ordered the rail %v, want %v", attempt, got, want)
			}
		}
	}
}

// A revised conversation is newer than one created after it but never touched.
//
// The grouping already reads `created_at` when `updated_at` is absent
// (`groupByDate`); the sort must read the same value, or the two halves of one
// answer describe two different orders.
func TestGroupedListingSortsARevisedConversationAboveANeverRevisedOne(t *testing.T) {
	pool := newPinnedListingPool(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	revisedAt := base.Add(30 * time.Minute)
	seedPinnedListingConversationAt(t, pool, "autotest_order_revised", base, &revisedAt)
	seedPinnedListingConversationAt(t, pool, "autotest_order_untouched", base.Add(10*time.Minute), nil)

	got := namesInDateGroups(readGroupedListing(t, pool, "7"))
	want := []string{"autotest_order_revised", "autotest_order_untouched"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the rail order is %v, want %v — the revised conversation is the more recent one", got, want)
	}
}
