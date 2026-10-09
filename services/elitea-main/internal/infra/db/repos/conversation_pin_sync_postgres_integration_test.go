package repos

// Client contract 1.4: a conversation pin reaches the caller's other devices.
//
// The pin is the project's shared pin (centry.social_pins, the one the web
// rail sets through pinEntity), and the list row carries it as `is_pinned`.
// What is under test is the sync half: a pin or unpin made on one device (or
// in the web app) must come back to another device's `changes_since` delta as
// the row with the new flag, in cursor order, exactly once, and only to a
// caller who can see the conversation at all.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type pinSyncPage struct {
	Total      int              `json:"total"`
	Rows       []map[string]any `json:"rows"`
	Tombstones []struct {
		ID     int64  `json:"id"`
		Reason string `json:"reason"`
	} `json:"tombstones"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// pinSyncList is one device's GET of the conversation list as `caller`.
func pinSyncList(t *testing.T, pool *pgxpool.Pool, caller, cursor string, delta bool) pinSyncPage {
	t.Helper()
	query := url.Values{"limit": {"100"}}
	if delta {
		query.Set("changes_since", cursor)
	}
	handler := conversations.NewHandler(NewConversationsRepo(pool)).WithPool(pool)
	router := chi.NewRouter()
	router.Get("/{projectID}", handler.List)
	request := httptest.NewRequest(http.MethodGet, "/1?"+query.Encode(), nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list as %s answered %d: %s", caller, recorder.Code, recorder.Body.String())
	}
	var page pinSyncPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func pinSyncRow(page pinSyncPage, id int64) map[string]any {
	for _, row := range page.Rows {
		if n, _ := row["id"].(float64); int64(n) == id {
			return row
		}
	}
	return nil
}

func asUser(id string) context.Context {
	return auth.ContextWithUser(context.Background(), auth.User{UserID: id})
}

func TestConversationPinReachesTheOtherDeviceThroughTheDelta(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo := NewConversationsRepo(pool)
	pins := NewCurrentSocialPinsRepository(pool)
	ctx := context.Background()

	// User 7's private conversation, and a public one user 8 authored.
	numericID, _, _ := seedConversationWithParticipant(t, repo, "hello")
	private, _ := strconv.ParseInt(numericID, 10, 64)
	var public int64
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_conversations (uuid, name, author_id, is_private, source)
		VALUES (gen_random_uuid(), 'team notes', 8, false, 'elitea') RETURNING id`).Scan(&public); err != nil {
		t.Fatal(err)
	}
	// Both rows settled well before the devices sync, so a full sync passes
	// them and its cursor stands after them.
	backdate(t, pool, "p_1.chat_conversations", private, time.Minute)
	backdate(t, pool, "p_1.chat_conversations", public, time.Minute)

	// Device B (user 7) and user 9 (a member who is no participant of the
	// private conversation) each finish a full sync.
	full := pinSyncList(t, pool, "7", "", true)
	if row := pinSyncRow(full, private); row == nil || row["is_pinned"] != false {
		t.Fatalf("full sync row = %v, want the private conversation unpinned", row)
	}
	deviceB := full.NextCursor
	other := pinSyncList(t, pool, "9", "", true).NextCursor
	if quiet := pinSyncList(t, pool, "7", deviceB, true); pinSyncRow(quiet, private) != nil {
		t.Fatalf("nothing changed, but the delta re-delivered the row: %+v", quiet.Rows)
	}
	var updatedBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT COALESCE(updated_at, created_at) FROM p_1.chat_conversations WHERE id = $1`, private).Scan(&updatedBefore); err != nil {
		t.Fatal(err)
	}

	// Device A (or the web rail) pins it.
	if err := pins.Pin(asUser("7"), "1", "conversation", numericID); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	page := pinSyncList(t, pool, "7", deviceB, true)
	row := pinSyncRow(page, private)
	if row == nil || row["is_pinned"] != true {
		t.Fatalf("device B delta after the pin = %+v, want the row with is_pinned true", page.Rows)
	}
	if len(page.Tombstones) != 0 {
		t.Fatalf("a pin wrote tombstones: %+v", page.Tombstones)
	}
	// A pin is not an edit: the row's "last modified" is unchanged.
	if row["updated_at"] != updatedBefore.Format("2006-01-02T15:04:05.000000") {
		t.Fatalf("the pin moved updated_at to %v (was %v)", row["updated_at"], updatedBefore)
	}
	// The legacy page agrees with the delta.
	if legacy := pinSyncRow(pinSyncList(t, pool, "7", "", false), private); legacy == nil || legacy["is_pinned"] != true {
		t.Fatalf("legacy row after the pin = %v", legacy)
	}
	// Per-user privacy: a member who cannot see the private conversation is
	// told nothing about its pin, neither a row nor a tombstone.
	if seen := pinSyncList(t, pool, "9", other, true); pinSyncRow(seen, private) != nil || len(seen.Tombstones) != 0 {
		t.Fatalf("user 9's delta mentions user 7's private conversation: %+v", seen)
	}

	// Sync ordering: a change inside the settle window is re-delivered until
	// it settles (a cursor never passes now - SettleWindow), and once it has
	// settled the cursor that read it does not deliver it again.
	time.Sleep(changesync.SettleWindow + time.Second)
	settled := pinSyncList(t, pool, "7", deviceB, true)
	if r := pinSyncRow(settled, private); r == nil || r["is_pinned"] != true {
		t.Fatalf("the settled pin is missing from the delta: %+v", settled.Rows)
	}
	if again := pinSyncList(t, pool, "7", settled.NextCursor, true); pinSyncRow(again, private) != nil {
		t.Fatalf("the pin was delivered twice: %+v", again.Rows)
	}
	deviceB = settled.NextCursor

	// Unpin on device A: device B sees the row again, unpinned.
	if err := pins.Unpin(asUser("7"), "1", "conversation", numericID); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if r := pinSyncRow(pinSyncList(t, pool, "7", deviceB, true), private); r == nil || r["is_pinned"] != false {
		t.Fatalf("device B delta after the unpin = %v, want the row with is_pinned false", r)
	}

	// A repeated unpin of a conversation that is not pinned changes nothing,
	// so it re-delivers nothing.
	time.Sleep(changesync.SettleWindow + time.Second)
	afterUnpin := pinSyncList(t, pool, "7", deviceB, true).NextCursor
	if err := pins.Unpin(asUser("7"), "1", "conversation", numericID); err != nil {
		t.Fatalf("repeated Unpin: %v", err)
	}
	if r := pinSyncRow(pinSyncList(t, pool, "7", afterUnpin, true), private); r != nil {
		t.Fatalf("a no-op unpin re-delivered the row: %v", r)
	}

	// The pin is the project's shared pin (the web rail's semantics): a pin
	// user 8 sets on the public conversation reaches user 9's delta too,
	// without saying who pinned it.
	if err := pins.Pin(asUser("8"), "1", "conversation", strconv.FormatInt(public, 10)); err != nil {
		t.Fatalf("Pin public: %v", err)
	}
	shared := pinSyncRow(pinSyncList(t, pool, "9", other, true), public)
	if shared == nil || shared["is_pinned"] != true {
		t.Fatalf("user 9's delta after user 8 pinned the public conversation = %v", shared)
	}
	for key := range shared {
		if key == "pinned_by" || key == "user_id" || key == "pin_user_id" {
			t.Fatalf("the row names its pinner (%s): %v", key, shared)
		}
	}

	// A caller who cannot see a conversation cannot pin it either (404, and
	// nothing is stamped).
	backdate(t, pool, "p_1.chat_conversations", private, 30*time.Second)
	before := syncAt(t, pool, "p_1.chat_conversations", private)
	// The refusal must be the access rule's 404, not any error: a 500 from
	// the upsert, a 403 before the predicate runs, or a failing authority
	// load would all leave the old `err == nil` check green.
	refused := pins.Pin(asUser("9"), "1", "conversation", numericID)
	var refusal *apierr.APIError
	if !errors.As(refused, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("user 9 pinning a private conversation it cannot see = %v, want the 404 access refusal", refused)
	}
	if after := syncAt(t, pool, "p_1.chat_conversations", private); !after.Equal(before) {
		t.Fatalf("a refused pin stamped the conversation: %v -> %v", before, after)
	}
}

// TestDeletingAPinnedConversationIsOneDeletedTombstone confirms the delete
// half of contract 1.4 against the real repository: a pinned conversation
// deleted on one device reaches the other as one `deleted` tombstone and no
// row, and its pin goes with it.
func TestDeletingAPinnedConversationIsOneDeletedTombstone(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo := NewConversationsRepo(pool)
	pins := NewCurrentSocialPinsRepository(pool)
	numericID, conversationUUID, _ := seedConversationWithParticipant(t, repo, "bye")
	id, _ := strconv.ParseInt(numericID, 10, 64)
	if err := pins.Pin(asUser("7"), "1", "conversation", numericID); err != nil {
		t.Fatal(err)
	}
	backdate(t, pool, "p_1.chat_conversations", id, time.Minute)
	cursor := pinSyncList(t, pool, "7", "", true).NextCursor

	if _, err := repo.Delete(asUser("7"), "1", conversationUUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	page := pinSyncList(t, pool, "7", cursor, true)
	if pinSyncRow(page, id) != nil {
		t.Fatalf("the deleted conversation came back as a row: %+v", page.Rows)
	}
	if len(page.Tombstones) != 1 || page.Tombstones[0].ID != id || page.Tombstones[0].Reason != "deleted" {
		t.Fatalf("tombstones = %+v, want one `deleted` for %d", page.Tombstones, id)
	}
	var left int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM centry.social_pins
		WHERE entity = 'conversation' AND project_id = 1 AND entity_id = $1`, id).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d pin rows outlived the conversation", left)
	}
}

// TestAPinRacingTheConversationDeleteLeavesNoOrphanPin is the regression for
// review F3 on PR #1139. Delete removes the conversation and then its pin in
// one transaction; a Pin that read the conversation before that commit used
// to insert its pin unseen by Delete's pin DELETE, find the row gone at the
// sync_at stamp, ignore that, and commit a centry.social_pins row for a
// conversation that no longer exists. The interleaving is forced here: the
// test's transaction deletes the conversation (as Delete does) and holds it
// uncommitted until Pin is blocked on the row, then removes the pin and
// commits.
func TestAPinRacingTheConversationDeleteLeavesNoOrphanPin(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo := NewConversationsRepo(pool)
	pins := NewCurrentSocialPinsRepository(pool)
	numericID, _, _ := seedConversationWithParticipant(t, repo, "racing")
	id, _ := strconv.ParseInt(numericID, 10, 64)
	ctx := context.Background()

	deleting, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleting.Rollback(ctx) }()
	var deleterPID int
	if err := deleting.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&deleterPID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`SELECT set_config('elitea.sync_cascade', 'conversation', true)`,
		`DELETE FROM p_1.chat_participant_mapping WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_message_items WHERE message_group_id IN (SELECT id FROM p_1.chat_message_group WHERE conversation_id = $1)`,
		`DELETE FROM p_1.chat_message_group WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_selected_conversations WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_conversations WHERE id = $1`,
	} {
		args := []any{id}
		if !strings.Contains(statement, "$1") {
			args = nil
		}
		if _, err := deleting.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	pinned := make(chan error, 1)
	go func() { pinned <- pins.Pin(asUser("7"), "1", "conversation", numericID) }()

	// Wait until Pin is blocked on a lock the deleting transaction holds.
	deadline := time.Now().Add(4 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> $1`, deleterPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-pinned:
			t.Fatalf("Pin finished (%v) before the delete committed; the race was not forced", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Pin never blocked on the deleting transaction")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := deleteConversationPin(ctx, deleting, "1", id); err != nil {
		t.Fatal(err)
	}
	if err := deleting.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	pinErr := <-pinned

	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.social_pins
		WHERE entity = 'conversation' AND project_id = 1 AND entity_id = $1`, id).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d pin rows outlived the deleted conversation (Pin answered %v)", left, pinErr)
	}
	var refusal *apierr.APIError
	if !errors.As(pinErr, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("Pin on a conversation deleted under it = %v, want 404", pinErr)
	}
}
