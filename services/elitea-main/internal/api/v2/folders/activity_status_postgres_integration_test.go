package folders_test

// Client contract 1.12: the folder sidebar's rows carry `activity`,
// `last_activity_at` and the caller's `unread`, and every group counts the
// conversations that need the user (`needs_you`).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type activityRow struct {
	ID       int `json:"id"`
	Activity *struct {
		State string `json:"state"`
		Since string `json:"since"`
	} `json:"activity"`
	LastActivityAt string `json:"last_activity_at"`
	Unread         bool   `json:"unread"`
}

type activityListing struct {
	Pinned struct {
		NeedsYou      int           `json:"needs_you"`
		Conversations []activityRow `json:"conversations"`
	} `json:"pinned"`
	DateGroups []struct {
		Name          string        `json:"name"`
		NeedsYou      int           `json:"needs_you"`
		Conversations []activityRow `json:"conversations"`
	} `json:"date_groups"`
}

func readActivityListing(t *testing.T, pool *pgxpool.Pool, userID string) activityListing {
	t.Helper()
	h := handler.NewHandler(&mockFolderRepo{}).WithPool(pool)
	router := chi.NewRouter()
	router.Route("/api/v2/projects/{projectID}/folders", func(r chi.Router) { r.Mount("/", h.Routes()) })
	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects/1/folders/?grouped=true", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: userID}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("grouped listing answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var listing activityListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	return listing
}

func TestGroupedListingCarriesTheActivityAndCountsNeedsYou(t *testing.T) {
	pool := newPinnedListingPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_participants (id, entity_name, entity_meta)
		VALUES (1, 'user', '{"id": 7}'), (2, 'application', '{"id": 41}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	running := seedPinnedListingConversation(t, pool, "autotest_running", true)
	paused := seedPinnedListingConversation(t, pool, "autotest_paused", true)
	done := seedPinnedListingConversation(t, pool, "autotest_done", true)
	for _, statement := range []struct {
		conversation int
		author       int
		streaming    bool
		meta         string
	}{
		{running, 1, false, `{}`}, {running, 2, true, `{}`},
		{paused, 2, false, `{"authorization_requests": [{}]}`},
		{done, 2, false, `{}`},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_message_group (conversation_id, author_participant_id, is_streaming, meta, created_at)
			VALUES ($1, $2, $3, $4::jsonb, clock_timestamp() AT TIME ZONE 'UTC')`,
			statement.conversation, statement.author, statement.streaming, statement.meta); err != nil {
			t.Fatal(err)
		}
	}
	// User 7 read the finished conversation.
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_conversation_reads (conversation_id, user_id) VALUES ($1, 7)`, done); err != nil {
		t.Fatal(err)
	}

	listing := readActivityListing(t, pool, "7")
	rows := map[int]activityRow{}
	needsYou := 0
	for _, group := range listing.DateGroups {
		needsYou += group.NeedsYou
		for _, row := range group.Conversations {
			rows[row.ID] = row
		}
	}
	if needsYou != 1 {
		t.Fatalf("needs_you over the date groups = %d, want 1", needsYou)
	}
	want := map[int]struct {
		state  string
		unread bool
	}{running: {"running", false}, paused: {"needs_you", true}, done: {"idle", false}}
	for id, expected := range want {
		row, ok := rows[id]
		if !ok || row.Activity == nil || row.Activity.State != expected.state || row.Unread != expected.unread ||
			row.Activity.Since == "" || row.LastActivityAt == "" {
			t.Fatalf("row %d = %+v, want state %s, unread %v", id, row, expected.state, expected.unread)
		}
	}

	// User 8 has no marker: the finished answer is unread for them.
	for _, group := range readActivityListing(t, pool, "8").DateGroups {
		for _, row := range group.Conversations {
			if row.ID == done && !row.Unread {
				t.Fatalf("user 8's row %d = %+v, want unread", done, row)
			}
		}
	}

	// A pinned paused conversation counts in the pinned section.
	if _, err := pool.Exec(ctx, `INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
		VALUES ('conversation', 1, $1, 7, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')`, paused); err != nil {
		t.Fatal(err)
	}
	pinned := readActivityListing(t, pool, "7").Pinned
	if pinned.NeedsYou != 1 || len(pinned.Conversations) != 1 || pinned.Conversations[0].Activity == nil ||
		pinned.Conversations[0].Activity.State != "needs_you" {
		t.Fatalf("pinned = %+v, want the paused conversation with needs_you 1", pinned)
	}
}
