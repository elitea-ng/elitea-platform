package repos

// Client contract 1.10: the conversation archive.
//
// The archive is the PROJECT's, as the conversation pin is (tenant
// chat_conversation_archives, one row per archived conversation). It takes
// the conversation out of every member's default list, and every row names
// who archived it and when. Any member who can see the conversation may
// unarchive it. Archive and unarchive re-deliver the row in the
// `changes_since` delta of every member who can see it, never as a
// tombstone.
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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// archiveList is one GET of project 1's conversation list as caller.
func archiveList(t *testing.T, pool *pgxpool.Pool, caller string, query url.Values) (int, pinSyncPage, string) {
	t.Helper()
	if query.Get("limit") == "" {
		query.Set("limit", "100")
	}
	handler := conversations.NewHandler(NewConversationsRepo(pool)).WithPool(pool)
	router := chi.NewRouter()
	router.Get("/{projectID}", handler.List)
	request := httptest.NewRequest(http.MethodGet, "/1?"+query.Encode(), nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var page pinSyncPage
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, page, recorder.Body.String()
}

func mustArchiveList(t *testing.T, pool *pgxpool.Pool, caller string, query url.Values) pinSyncPage {
	t.Helper()
	code, page, body := archiveList(t, pool, caller, query)
	if code != http.StatusOK {
		t.Fatalf("list as %s %v answered %d: %s", caller, query, code, body)
	}
	return page
}

// archiveRoute is one PUT (archive) or DELETE (unarchive) through the real
// handler and repository.
func archiveRoute(t *testing.T, pool *pgxpool.Pool, method, caller, conversationID string) (int, string) {
	t.Helper()
	handler := conversations.NewHandler(NewConversationsRepo(pool)).WithPool(pool)
	router := chi.NewRouter()
	router.Put("/{projectID}/{conversationID}", handler.Archive)
	router.Delete("/{projectID}/{conversationID}", handler.Unarchive)
	request := httptest.NewRequest(method, "/1/"+conversationID, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

type archiveRow struct {
	by int64
	at time.Time
}

func readArchive(t *testing.T, pool *pgxpool.Pool, id int64) []archiveRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT archived_by, archived_at FROM p_1.chat_conversation_archives WHERE conversation_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := []archiveRow{}
	for rows.Next() {
		var row archiveRow
		if err := rows.Scan(&row.by, &row.at); err != nil {
			t.Fatal(err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// archivedBy reads a list row's `archived_by` as (id, name).
func archivedBy(t *testing.T, row map[string]any) (int64, string) {
	t.Helper()
	by, ok := row["archived_by"].(map[string]any)
	if !ok {
		t.Fatalf("row %v has no archived_by object", row)
	}
	id, _ := by["id"].(float64)
	name, _ := by["name"].(string)
	return int64(id), name
}

// seedArchiveChats makes user 7's private conversation and a public one
// user 8 authored, both settled a minute ago. Users 7, 8 and 9 are members.
func seedArchiveChats(t *testing.T, pool *pgxpool.Pool) (repo *ConversationsRepo, private, public int64, privateUUID string) {
	t.Helper()
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo = NewConversationsRepo(pool)
	numericID, conversationUUID, _ := seedConversationWithParticipant(t, repo, "hello")
	private, _ = strconv.ParseInt(numericID, 10, 64)
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.chat_conversations (uuid, name, author_id, is_private, source, created_at)
		VALUES (gen_random_uuid(), 'team notes', 8, false, 'elitea', now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&public); err != nil {
		t.Fatal(err)
	}
	backdate(t, pool, "p_1.chat_conversations", private, time.Minute)
	backdate(t, pool, "p_1.chat_conversations", public, time.Minute)
	return repo, private, public, conversationUUID
}

func TestConversationArchiveIsSharedByTheProjectAndNamesTheArchiver(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, private, public, _ := seedArchiveChats(t, pool)
	pins := NewCurrentSocialPinsRepository(pool)
	publicID := strconv.FormatInt(public, 10)

	if err := pins.Pin(asUser("8"), "1", "conversation", publicID); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	// User 7 archives user 8's public conversation.
	if code, body := archiveRoute(t, pool, http.MethodPut, "7", publicID); code != http.StatusOK || body != "{\"ok\":true}\n" {
		t.Fatalf("archive answered %d %q, want 200 {\"ok\":true}", code, body)
	}

	// No user table carries names yet: the row still names the archiver,
	// with an empty name.
	only := mustArchiveList(t, pool, "8", url.Values{"archived": {"only"}})
	if row := pinSyncRow(only, public); row == nil {
		t.Fatalf("user 8's archived list = %+v, want the public row", only.Rows)
	} else if id, name := archivedBy(t, row); id != 7 || name != "" {
		t.Fatalf("archived_by = (%d, %q), want (7, \"\") without a name source", id, name)
	}
	if _, err := pool.Exec(context.Background(), `ALTER TABLE public.auth_core__user ADD COLUMN IF NOT EXISTS name text;
		INSERT INTO public.auth_core__user (id, name) VALUES (7, 'Alice'), (8, 'Bob'), (9, 'Carol')
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`); err != nil {
		t.Fatal(err)
	}

	// Every member sees the same archive: the default list leaves it out, the
	// counts agree, and `only` lists it with who, when and its pin.
	for _, member := range []string{"7", "8", "9"} {
		page := mustArchiveList(t, pool, member, url.Values{})
		if pinSyncRow(page, public) != nil {
			t.Fatalf("user %s's default list still has the archived row: %+v", member, page.Rows)
		}
		only := mustArchiveList(t, pool, member, url.Values{"archived": {"only"}})
		row := pinSyncRow(only, public)
		if only.Total != 1 || len(only.Rows) != 1 || row == nil || row["is_archived"] != true || row["is_pinned"] != true {
			t.Fatalf("user %s's archived=only = %+v, want the public row archived and still pinned", member, only)
		}
		if id, name := archivedBy(t, row); id != 7 || name != "Alice" {
			t.Fatalf("user %s reads archived_by (%d, %q), want (7, Alice)", member, id, name)
		}
		if at, err := time.Parse(time.RFC3339Nano, row["archived_at"].(string)); err != nil || time.Since(at) > time.Minute {
			t.Fatalf("archived_at = %v (%v), want a recent RFC 3339 time", row["archived_at"], err)
		}
	}
	// A row that is not archived carries no archiver.
	both := mustArchiveList(t, pool, "7", url.Values{"archived": {"include"}})
	if r := pinSyncRow(both, private); both.Total != 2 || r == nil || r["is_archived"] != false || r["archived_by"] != nil || r["archived_at"] != nil {
		t.Fatalf("archived=include = %+v, want both rows and no archiver on the private one", both)
	}
	// `exclude` is the default spelled out; an unknown value is the default.
	for _, value := range []string{"exclude", "bogus"} {
		if p := mustArchiveList(t, pool, "7", url.Values{"archived": {value}}); pinSyncRow(p, public) != nil || p.Total != 1 {
			t.Fatalf("archived=%s = %+v, want the default list", value, p)
		}
	}

	// The pin is untouched by the archive.
	var pinned int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM centry.social_pins
		WHERE entity = 'conversation' AND project_id = 1 AND entity_id = $1`, public).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != 1 {
		t.Fatalf("%d pin rows after the archive, want 1", pinned)
	}

	// Another member (user 9, not the archiver) unarchives it for everyone.
	if code, body := archiveRoute(t, pool, http.MethodDelete, "9", publicID); code != http.StatusOK {
		t.Fatalf("unarchive by user 9 answered %d: %s", code, body)
	}
	for _, member := range []string{"7", "8"} {
		if r := pinSyncRow(mustArchiveList(t, pool, member, url.Values{}), public); r == nil || r["is_archived"] != false {
			t.Fatalf("after user 9's unarchive user %s's row = %v", member, r)
		}
	}
}

func TestConversationArchiveIsIdempotentAndStampsOnlyAChange(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, _, public, _ := seedArchiveChats(t, pool)
	publicID := strconv.FormatInt(public, 10)
	before := syncAt(t, pool, "p_1.chat_conversations", public)

	if err := repo.ArchiveConversation(asUser("7"), "1", publicID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	first := readArchive(t, pool, public)
	stamped := syncAt(t, pool, "p_1.chat_conversations", public)
	if len(first) != 1 || first[0].by != 7 || !stamped.After(before) {
		t.Fatalf("archive rows %v, sync_at %v -> %v: want one row by 7 and a new stamp", first, before, stamped)
	}
	backdate(t, pool, "p_1.chat_conversations", public, 30*time.Second)
	settled := syncAt(t, pool, "p_1.chat_conversations", public)

	// A repeated archive, by the same member or another, keeps the first
	// archive and stamps nothing.
	for _, user := range []string{"7", "8"} {
		if code, body := archiveRoute(t, pool, http.MethodPut, user, publicID); code != http.StatusOK {
			t.Fatalf("repeated archive by %s answered %d: %s", user, code, body)
		}
	}
	if again := readArchive(t, pool, public); len(again) != 1 || again[0] != first[0] {
		t.Fatalf("repeated archive rows = %v, want %v", again, first)
	}
	if after := syncAt(t, pool, "p_1.chat_conversations", public); !after.Equal(settled) {
		t.Fatalf("a repeated archive stamped the row: %v -> %v", settled, after)
	}

	// Unarchive removes and stamps; a repeated unarchive does neither.
	if err := repo.UnarchiveConversation(asUser("8"), "1", publicID); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if rows := readArchive(t, pool, public); len(rows) != 0 {
		t.Fatalf("archive rows after the unarchive = %v", rows)
	}
	if after := syncAt(t, pool, "p_1.chat_conversations", public); !after.After(settled) {
		t.Fatalf("the unarchive did not stamp the row")
	}
	backdate(t, pool, "p_1.chat_conversations", public, 30*time.Second)
	settled = syncAt(t, pool, "p_1.chat_conversations", public)
	if code, body := archiveRoute(t, pool, http.MethodDelete, "7", publicID); code != http.StatusOK {
		t.Fatalf("repeated unarchive answered %d: %s", code, body)
	}
	if after := syncAt(t, pool, "p_1.chat_conversations", public); !after.Equal(settled) {
		t.Fatalf("a repeated unarchive stamped the row: %v -> %v", settled, after)
	}
}

func TestConversationArchiveOfAnInvisibleConversationIs404(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, private, _, privateUUID := seedArchiveChats(t, pool)
	privateID := strconv.FormatInt(private, 10)

	// The UUID form resolves for a caller who can see it.
	if err := repo.ArchiveConversation(asUser("7"), "1", privateUUID); err != nil {
		t.Fatalf("Archive by uuid: %v", err)
	}
	backdate(t, pool, "p_1.chat_conversations", private, 30*time.Second)
	before := syncAt(t, pool, "p_1.chat_conversations", private)

	// User 9 is a member but no participant of user 7's private conversation:
	// it can neither archive nor unarchive it.
	for _, call := range []func() error{
		func() error { return repo.ArchiveConversation(asUser("9"), "1", privateID) },
		func() error { return repo.UnarchiveConversation(asUser("9"), "1", privateID) },
		func() error { return repo.ArchiveConversation(asUser("7"), "1", "2147483647") },
		func() error { return repo.ArchiveConversation(asUser("7"), "1", "not-an-id") },
	} {
		err := call()
		var refusal *apierr.APIError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
			t.Fatalf("archive refusal = %v, want the 404 access refusal", err)
		}
	}
	if code, _ := archiveRoute(t, pool, http.MethodDelete, "9", privateID); code != http.StatusNotFound {
		t.Fatalf("DELETE as user 9 answered %d, want 404", code)
	}
	if rows := readArchive(t, pool, private); len(rows) != 1 || rows[0].by != 7 {
		t.Fatalf("a refused unarchive changed the archive: %v", rows)
	}
	if after := syncAt(t, pool, "p_1.chat_conversations", private); !after.Equal(before) {
		t.Fatalf("a refused call stamped the row: %v -> %v", before, after)
	}
	// User 9 is told nothing about the private archive either.
	if p := mustArchiveList(t, pool, "9", url.Values{"archived": {"only"}}); len(p.Rows) != 0 {
		t.Fatalf("user 9's archived list = %+v, want empty", p.Rows)
	}
}

func TestConversationArchiveReachesEveryMemberThroughTheDelta(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, private, public, _ := seedArchiveChats(t, pool)
	publicID := strconv.FormatInt(public, 10)

	// User 7's device and user 9 each finish a full sync of the default list.
	full := mustArchiveList(t, pool, "7", url.Values{"changes_since": {""}})
	if pinSyncRow(full, public) == nil || pinSyncRow(full, private) == nil {
		t.Fatalf("full sync = %+v, want both rows", full.Rows)
	}
	deviceB := full.NextCursor
	other := mustArchiveList(t, pool, "9", url.Values{"changes_since": {""}}).NextCursor
	private9 := mustArchiveList(t, pool, "9", url.Values{"changes_since": {""}})
	if pinSyncRow(private9, private) != nil {
		t.Fatalf("user 9 lists user 7's private conversation: %+v", private9.Rows)
	}

	// User 7 archives the public conversation and the private one.
	if err := repo.ArchiveConversation(asUser("7"), "1", publicID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := repo.ArchiveConversation(asUser("7"), "1", strconv.FormatInt(private, 10)); err != nil {
		t.Fatalf("Archive private: %v", err)
	}
	for caller, cursor := range map[string]string{"7": deviceB, "9": other} {
		page := mustArchiveList(t, pool, caller, url.Values{"changes_since": {cursor}})
		r := pinSyncRow(page, public)
		if r == nil || r["is_archived"] != true {
			t.Fatalf("user %s's delta = %+v, want the public row with is_archived true", caller, page.Rows)
		}
		if id, _ := archivedBy(t, r); id != 7 {
			t.Fatalf("user %s's delta names archiver %d, want 7", caller, id)
		}
		if len(page.Tombstones) != 0 {
			t.Fatalf("an archive wrote tombstones for user %s: %+v", caller, page.Tombstones)
		}
		if caller == "9" && pinSyncRow(page, private) != nil {
			t.Fatalf("user 9's delta mentions the private conversation: %+v", page.Rows)
		}
	}
	if total := mustArchiveList(t, pool, "7", url.Values{"changes_since": {deviceB}}).Total; total != 0 {
		t.Fatalf("delta total = %d, want 0 (both rows are out of the default list)", total)
	}
	// `archived` absent and `exclude` are the same list: the cursor is valid.
	if code, _, body := archiveList(t, pool, "7", url.Values{"changes_since": {deviceB}, "archived": {"exclude"}}); code != http.StatusOK {
		t.Fatalf("the default cursor with archived=exclude answered %d: %s", code, body)
	}

	// A new device's full sync leaves the archived rows out of the default
	// list, and an `only` full sync has just them.
	if fresh := mustArchiveList(t, pool, "7", url.Values{"changes_since": {"0"}}); len(fresh.Rows) != 0 {
		t.Fatalf("a new full sync = %+v, want no rows", fresh.Rows)
	}
	archivedSync := mustArchiveList(t, pool, "7", url.Values{"changes_since": {"0"}, "archived": {"only"}})
	if pinSyncRow(archivedSync, public) == nil || pinSyncRow(archivedSync, private) == nil || archivedSync.Total != 2 {
		t.Fatalf("the archived full sync = %+v, want both rows", archivedSync)
	}

	// A cursor of another `archived` list is a 400.
	for _, value := range []string{"only", "include"} {
		code, _, body := archiveList(t, pool, "7", url.Values{"changes_since": {deviceB}, "archived": {value}})
		if code != http.StatusBadRequest {
			t.Fatalf("the default cursor with archived=%s answered %d: %s, want 400", value, code, body)
		}
	}
	if code, _, _ := archiveList(t, pool, "7", url.Values{"changes_since": {archivedSync.NextCursor}}); code != http.StatusBadRequest {
		t.Fatalf("the archived cursor on the default list answered %d, want 400", code)
	}

	// User 9 unarchives the public conversation: user 7's default list gets
	// the row back, and its archived list sees the row leave as a row
	// (is_archived false), not a tombstone.
	if err := repo.UnarchiveConversation(asUser("9"), "1", publicID); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if r := pinSyncRow(mustArchiveList(t, pool, "7", url.Values{"changes_since": {deviceB}}), public); r == nil || r["is_archived"] != false || r["archived_by"] != nil {
		t.Fatalf("user 7's delta after the unarchive = %v", r)
	}
	gone := mustArchiveList(t, pool, "7", url.Values{"changes_since": {archivedSync.NextCursor}, "archived": {"only"}})
	if r := pinSyncRow(gone, public); r == nil || r["is_archived"] != false || len(gone.Tombstones) != 0 || gone.Total != 1 {
		t.Fatalf("the archived list's delta after the unarchive = %+v", gone)
	}
}

func TestDeletingAnArchivedConversationRemovesItsArchiveRow(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, private, public, privateUUID := seedArchiveChats(t, pool)
	if err := repo.ArchiveConversation(asUser("7"), "1", privateUUID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ArchiveConversation(asUser("8"), "1", strconv.FormatInt(public, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Delete(asUser("7"), "1", privateUUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rows := readArchive(t, pool, private); len(rows) != 0 {
		t.Fatalf("%d archive rows outlived the conversation", len(rows))
	}
	if rows := readArchive(t, pool, public); len(rows) != 1 {
		t.Fatalf("the other conversation's archive rows = %v, want one", rows)
	}
}
