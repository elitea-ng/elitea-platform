package conversations

// The conversation-list delta (`changes_since`, ADR-0025 WP6) through the real
// List handler, against chat tables built by the tenant migrations themselves
// (0123, which declares them, and 0144, which installs the sync triggers) in a
// private database. What is under test is per-caller: which rows and which
// tombstones a given user is told about.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func newChangesPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newListFiltersPool(t) // private database with a hand-cut p_1
	ctx := context.Background()
	// Replace the hand-cut tables with the corpus's own declarations.
	if _, err := pool.Exec(ctx, `DROP SCHEMA p_1 CASCADE; CREATE SCHEMA p_1`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"tenant/0123_agent_chat_message_tables.sql", "tenant/0144_chat_sync.sql"} {
		sql, err := platformmigrations.Files.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('search_path', 'p_1', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", path, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// seedChat creates a conversation authored by `author` with each of `users` as
// a user participant, and returns its id.
func seedChat(t *testing.T, pool *pgxpool.Pool, name string, private bool, author int, users ...int) int {
	t.Helper()
	ctx := context.Background()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_conversations (name, is_private, author_id)
		VALUES ($1, $2, $3) RETURNING id`, name, private, author).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if _, err := pool.Exec(ctx, `WITH participant AS (
			INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
			VALUES (gen_random_uuid(), 'user', jsonb_build_object('id', $1::integer)) RETURNING id)
			INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id)
			SELECT $2, id FROM participant`, user, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// deleteChat deletes a conversation the way ConversationsRepo.Delete does:
// mappings first, under the sync cascade, in one transaction.
func deleteChat(t *testing.T, pool *pgxpool.Pool, id int) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`SELECT set_config('elitea.sync_cascade', 'conversation', true)`,
		`DELETE FROM p_1.chat_participant_mapping WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_message_group WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_conversations WHERE id = $1`,
	} {
		args := []any{id}
		if statement[0] == 'S' {
			args = nil
		}
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type changesPage struct {
	Total      int                    `json:"total"`
	Rows       []map[string]any       `json:"rows"`
	Tombstones []changesync.Tombstone `json:"tombstones"`
	NextCursor string                 `json:"next_cursor"`
	HasMore    bool                   `json:"has_more"`
}

func listRaw(t *testing.T, pool *pgxpool.Pool, query url.Values, caller string) (int, []byte) {
	t.Helper()
	handler := NewHandler(nil).WithPool(pool)
	router := chi.NewRouter()
	router.Get("/{projectID}", handler.List)
	request := httptest.NewRequest(http.MethodGet, "/1?"+query.Encode(), nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func listChanges(t *testing.T, pool *pgxpool.Pool, cursor string, caller string, extra url.Values) changesPage {
	t.Helper()
	query := url.Values{"changes_since": {cursor}}
	for key, values := range extra {
		query[key] = values
	}
	code, body := listRaw(t, pool, query, caller)
	if code != http.StatusOK {
		t.Fatalf("delta answered %d: %s", code, body)
	}
	var page changesPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func rowIDs(page changesPage) []int {
	ids := []int{}
	for _, row := range page.Rows {
		ids = append(ids, int(row["id"].(float64)))
	}
	sort.Ints(ids)
	return ids
}

func tombstonesByID(page changesPage) map[int64]string {
	reasons := map[int64]string{}
	for _, tomb := range page.Tombstones {
		reasons[tomb.ID] = tomb.Reason
	}
	return reasons
}

func TestConversationDeltaLegacyResponseIsUnchanged(t *testing.T) {
	pool := newChangesPool(t)
	seedChat(t, pool, "mine", true, 7, 7)
	code, body := listRaw(t, pool, url.Values{}, "7")
	if code != http.StatusOK {
		t.Fatalf("legacy list answered %d", code)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["total"] == nil || keys["rows"] == nil {
		t.Fatalf("legacy envelope keys changed: %s", body)
	}
	var rows []map[string]json.RawMessage
	_ = json.Unmarshal(keys["rows"], &rows)
	// Eight keys: the seven of the legacy row and `is_pinned` (client
	// contract 1.4), which is additive.
	if len(rows) != 1 || len(rows[0]) != 8 || rows[0]["sync_at"] != nil || string(rows[0]["is_pinned"]) != "false" {
		t.Fatalf("legacy row shape changed: %s", body)
	}
}

// TestConversationListCarriesTheProjectPin pins client contract 1.4's
// `is_pinned` on the list row, in the legacy page and the delta alike: true
// exactly when the project holds a conversation pin for the row, never for a
// pin of another entity type or another project with the same id, and never
// with the pinner's identity.
func TestConversationListCarriesTheProjectPin(t *testing.T) {
	pool := newChangesPool(t)
	pinned := seedChat(t, pool, "pinned", true, 7, 7)
	samePinOtherType := seedChat(t, pool, "a prompt pin with my id", true, 7, 7)
	samePinOtherProject := seedChat(t, pool, "pinned in project 2", true, 7, 7)
	plain := seedChat(t, pool, "plain", true, 7, 7)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.social_pins (entity, user_id, project_id, entity_id) VALUES
		('conversation', 8, 1, $1), ('prompt', 7, 1, $2), ('conversation', 7, 2, $3)`,
		pinned, samePinOtherType, samePinOtherProject); err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{pinned: true, samePinOtherType: false, samePinOtherProject: false, plain: false}
	check := func(label string, rows []map[string]any) {
		t.Helper()
		if len(rows) != len(want) {
			t.Fatalf("%s: %d rows, want %d", label, len(rows), len(want))
		}
		for _, row := range rows {
			id := int(row["id"].(float64))
			flag, ok := row["is_pinned"].(bool)
			if !ok || flag != want[id] {
				t.Errorf("%s: conversation %d is_pinned = %#v, want %v", label, id, row["is_pinned"], want[id])
			}
			for key := range row {
				if key == "pinned_by" || key == "pin_user_id" || key == "user_id" {
					t.Errorf("%s: row carries the pinner (%s)", label, key)
				}
			}
		}
	}
	code, body := listRaw(t, pool, url.Values{"limit": {"100"}}, "7")
	if code != http.StatusOK {
		t.Fatalf("legacy list answered %d: %s", code, body)
	}
	var legacy changesPage
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	check("legacy", legacy.Rows)
	check("delta", listChanges(t, pool, "", "7", nil).Rows)
}

func TestConversationDeltaVisibilityParityAndPaging(t *testing.T) {
	pool := newChangesPool(t)
	ids := []int{
		seedChat(t, pool, "public", false, 8, 8),
		seedChat(t, pool, "mine", true, 7, 7),
		seedChat(t, pool, "theirs private", true, 8, 8),
		seedChat(t, pool, "shared with me", true, 8, 8, 7),
	}
	// Settle every row so a one-row page can walk them.
	if _, err := pool.Exec(context.Background(), `SET session_replication_role = replica;
		UPDATE p_1.chat_conversations SET sync_at = clock_timestamp() - interval '1 hour' * (10 - id);
		RESET session_replication_role`); err != nil {
		t.Fatal(err)
	}

	var all []int
	cursor := ""
	for i := 0; i < 10; i++ {
		page := listChanges(t, pool, cursor, "7", url.Values{"limit": {"1"}})
		all = append(all, rowIDs(page)...)
		cursor = page.NextCursor
		if page.Total != 3 {
			t.Fatalf("total = %d, want the 3 conversations user 7 can list", page.Total)
		}
		if !page.HasMore {
			break
		}
	}
	sort.Ints(all)
	want := []int{ids[0], ids[1], ids[3]}
	if fmt.Sprint(all) != fmt.Sprint(want) {
		t.Fatalf("paged delta = %v, want %v (never the private conversation user 7 is not in)", all, want)
	}

	// Every delta row is also in the legacy list for the same caller.
	code, body := listRaw(t, pool, url.Values{"limit": {"100"}}, "7")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	var legacy changesPage
	_ = json.Unmarshal(body, &legacy)
	if fmt.Sprint(rowIDs(legacy)) != fmt.Sprint(want) {
		t.Fatalf("legacy list %v and delta %v disagree", rowIDs(legacy), want)
	}
}

func TestConversationDeltaTombstonesArePerCaller(t *testing.T) {
	pool := newChangesPool(t)
	public := seedChat(t, pool, "public", false, 8, 8)
	private := seedChat(t, pool, "private", true, 8, 8, 9)
	turnedPrivate := seedChat(t, pool, "turned private", false, 8, 8)
	hidden := seedChat(t, pool, "to hide", true, 7, 7)
	leftBy9 := seedChat(t, pool, "nine leaves", true, 8, 8, 9)

	start7 := listChanges(t, pool, "", "7", nil).NextCursor
	start9 := listChanges(t, pool, "", "9", nil).NextCursor
	startHiddenOnly := listChanges(t, pool, "", "7", url.Values{"hidden": {"only"}}).NextCursor

	deleteChat(t, pool, public)
	deleteChat(t, pool, private)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET is_private = true WHERE id = $1`, turnedPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET meta = '{"is_hidden": true}' WHERE id = $1`, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_participant_mapping m USING p_1.chat_participants p
		WHERE m.participant_id = p.id AND m.conversation_id = $1 AND p.entity_meta->>'id' = '9'`, leftBy9); err != nil {
		t.Fatal(err)
	}

	got7 := tombstonesByID(listChanges(t, pool, start7, "7", nil))
	want7 := map[int64]string{
		int64(public):        changesync.ReasonDeleted,
		int64(turnedPrivate): changesync.ReasonAccessLost,
		int64(hidden):        changesync.ReasonAccessLost,
	}
	if fmt.Sprint(got7) != fmt.Sprint(want7) {
		t.Fatalf("user 7 tombstones = %v, want %v (no word of the private conversation it was never in)", got7, want7)
	}

	page9 := listChanges(t, pool, start9, "9", nil)
	got9 := tombstonesByID(page9)
	want9 := map[int64]string{
		int64(public):        changesync.ReasonDeleted,
		int64(private):       changesync.ReasonDeleted,
		int64(turnedPrivate): changesync.ReasonAccessLost,
		int64(leftBy9):       changesync.ReasonAccessLost,
	}
	if fmt.Sprint(got9) != fmt.Sprint(want9) {
		t.Fatalf("user 9 tombstones = %v, want %v", got9, want9)
	}

	// The author of the conversation turned private still sees it as a row,
	// and a conversation that left the default list entered `hidden=only`.
	onlyHidden := listChanges(t, pool, startHiddenOnly, "7", url.Values{"hidden": {"only"}})
	if ids := rowIDs(onlyHidden); fmt.Sprint(ids) != fmt.Sprint([]int{hidden}) {
		t.Fatalf("hidden=only delta rows = %v, want the newly hidden conversation", ids)
	}
	author := listChanges(t, pool, "", "8", nil)
	found := false
	for _, id := range rowIDs(author) {
		found = found || id == turnedPrivate
	}
	if !found || tombstonesByID(author)[int64(turnedPrivate)] != "" {
		t.Fatalf("the author of the private conversation must keep it as a row: %+v", author)
	}
}

func TestConversationDeltaRefusesForeignAndExpiredCursors(t *testing.T) {
	pool := newChangesPool(t)
	seedChat(t, pool, "mine", true, 7, 7)
	page := listChanges(t, pool, "", "7", nil)

	code, body := listRaw(t, pool, url.Values{"changes_since": {page.NextCursor}, "hidden": {"only"}}, "7")
	if code != http.StatusBadRequest || !jsonHasError(body, changesync.CodeInvalidCursor) {
		t.Fatalf("a cursor under another filter answered %d %s, want 400 invalid_sync_cursor", code, body)
	}

	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	expired := changesync.Encode(changesync.Cursor{
		Stream: changesync.StreamConversations,
		Scope:  conversationScope("1/"+changesync.FilterDigest(url.Values{}, conversationFilterParams...), false),
		Tombs:  changesync.Position{At: now.Add(-changesync.TombstoneRetention - time.Minute)},
	})
	code, body = listRaw(t, pool, url.Values{"changes_since": {expired}}, "7")
	if code != http.StatusGone || !jsonHasError(body, changesync.CodeCursorExpired) {
		t.Fatalf("a back-dated cursor answered %d %s, want 410 sync_cursor_expired", code, body)
	}
}

func jsonHasError(body []byte, code string) bool {
	var payload changesync.ErrorBody
	return json.Unmarshal(body, &payload) == nil && payload.Error == code
}

// TestConversationDeltaPrivateThenDeletedWhileOffline: a member who synced a
// public conversation, then went offline while it was made private and
// deleted, must still be told to drop it. Its access_private marker can no
// longer join a live conversation, and its private deletion tombstone names
// neither the member as author nor as a former participant.
func TestConversationDeltaPrivateThenDeletedWhileOffline(t *testing.T) {
	pool := newChangesPool(t)
	withdrawn := seedChat(t, pool, "public, then withdrawn", false, 8, 8)
	neverPublic := seedChat(t, pool, "always private", true, 8, 8)

	first := listChanges(t, pool, "", "7", nil)
	if fmt.Sprint(rowIDs(first)) != fmt.Sprint([]int{withdrawn}) {
		t.Fatalf("member 7 first sync rows = %v, want the public conversation", rowIDs(first))
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE p_1.chat_conversations SET is_private = true WHERE id = $1`, withdrawn); err != nil {
		t.Fatal(err)
	}
	deleteChat(t, pool, withdrawn)
	deleteChat(t, pool, neverPublic)

	got := tombstonesByID(listChanges(t, pool, first.NextCursor, "7", nil))
	want := map[int64]string{int64(withdrawn): changesync.ReasonDeleted}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("member 7 tombstones = %v, want %v (and no word of the never-public conversation)", got, want)
	}
}

// grantProjectAdmin installs the minimal role projection chatauthority reads
// and makes user an admin of project 1 (or withdraws it).
func setProjectAdmin(t *testing.T, pool *pgxpool.Pool, user int, admin bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public.auth_core__project_role (id serial PRIMARY KEY, project_id integer, name text);
		CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (project_id integer, user_id integer, role_id integer);
		INSERT INTO public.auth_core__project_role (id, project_id, name)
		VALUES (1, 1, 'admin') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__project_user_role WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if admin {
		if _, err := pool.Exec(ctx, `INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			VALUES (1, $1, 1)`, user); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConversationDeltaRoleChangeForcesAResync: an admin lists every private
// conversation that has a user participant. Losing (or gaining) admin writes
// no tombstone — nothing in the chat tables changed — so a cursor issued
// under the other role must not keep advancing: it answers 410 and the client
// resyncs, dropping the private conversations it can no longer see.
func TestConversationDeltaRoleChangeForcesAResync(t *testing.T) {
	pool := newChangesPool(t)
	setProjectAdmin(t, pool, 5, true)
	private := seedChat(t, pool, "someone else's private", true, 8, 8)

	asAdmin := listChanges(t, pool, "", "5", nil)
	if fmt.Sprint(rowIDs(asAdmin)) != fmt.Sprint([]int{private}) {
		t.Fatalf("admin full sync rows = %v, want the private conversation", rowIDs(asAdmin))
	}

	setProjectAdmin(t, pool, 5, false)
	code, body := listRaw(t, pool, url.Values{"changes_since": {asAdmin.NextCursor}}, "5")
	if code != http.StatusGone || !jsonHasError(body, changesync.CodeCursorExpired) {
		t.Fatalf("a cursor issued while admin answered %d %s after demotion, want 410 sync_cursor_expired", code, body)
	}
	if again := listChanges(t, pool, "", "5", nil); len(again.Rows) != 0 {
		t.Fatalf("the resync after demotion still lists %v", rowIDs(again))
	}

	// And the reverse: a cursor from before a promotion cannot hide what the
	// new admin can now see.
	asMember := listChanges(t, pool, "", "5", nil).NextCursor
	setProjectAdmin(t, pool, 5, true)
	code, body = listRaw(t, pool, url.Values{"changes_since": {asMember}}, "5")
	if code != http.StatusGone || !jsonHasError(body, changesync.CodeCursorExpired) {
		t.Fatalf("a cursor issued before promotion answered %d %s, want 410", code, body)
	}
}

// TestConversationDeltaLastParticipantLeavingTellsAdmins: an admin sees a
// private conversation only while it has a user participant. When the last
// one is removed, the admins who cached it are told it is gone; a member who
// never saw it is told nothing.
func TestConversationDeltaLastParticipantLeavingTellsAdmins(t *testing.T) {
	pool := newChangesPool(t)
	setProjectAdmin(t, pool, 5, true)
	orphaned := seedChat(t, pool, "nine's private", true, 8, 9)

	admin := listChanges(t, pool, "", "5", nil)
	if fmt.Sprint(rowIDs(admin)) != fmt.Sprint([]int{orphaned}) {
		t.Fatalf("admin full sync rows = %v, want the private conversation", rowIDs(admin))
	}
	member := listChanges(t, pool, "", "7", nil).NextCursor

	if _, err := pool.Exec(context.Background(), `DELETE FROM p_1.chat_participant_mapping WHERE conversation_id = $1`, orphaned); err != nil {
		t.Fatal(err)
	}

	got := tombstonesByID(listChanges(t, pool, admin.NextCursor, "5", nil))
	if want := map[int64]string{int64(orphaned): changesync.ReasonAccessLost}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admin tombstones = %v, want %v", got, want)
	}
	if got := tombstonesByID(listChanges(t, pool, member, "7", nil)); len(got) != 0 {
		t.Fatalf("a member who never saw it is told %v", got)
	}
}
