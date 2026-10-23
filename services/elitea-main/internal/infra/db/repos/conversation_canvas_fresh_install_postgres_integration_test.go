package repos

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// The canvas half of the same fresh-install question the sibling file asks of
// the transcript tables, and it is asked here rather than there because the two
// paths had DIFFERENT owners and only one of them was ever reachable.
//
// `chat_messages_canvas` and `chat_canvas_versions` were referenced by
// ConversationsRepo — CreateCanvas INSERTs into both, GetMessageByUUID LEFT
// JOINs the first — and created by NOTHING in this repository. They existed
// only in the pg_catalog dump of the live legacy database, which is to say they
// existed only where the legacy runtime had made them.
//
// WHAT THAT ACTUALLY BROKE, stated precisely because the two call sites are not
// equally live:
//
//   - POST /api/v2/elitea_core/canvases/prompt_lib/{projectID} is a REGISTERED
//     production route (router.go, behind `models.chat.canvas.create`, which
//     shared/0068 seeds), pinned by production_router_test.go and exercised by
//     router_elitea_core_project_scope_test.go. On any deployment the legacy
//     runtime never touched it answered 42P01 the moment it got past the
//     permission gate. That is the live defect.
//
//   - GetMessageByUUID's LEFT JOIN was LATENT when this test was written:
//     its only caller, `(*conversations.Handler).GetMessage`, was registered
//     by no router, so it 500'd nowhere and would have 500'd everywhere the
//     day someone added the missing route line. That day came in the same
//     session: the GET is now bound beside its DELETE sibling under
//     `models.chat.messages.details`, so the read is LIVE and this test is
//     what keeps the 42P01 from coming back with it.
func seedFreshInstallTextItem(t *testing.T, pool *pgxpool.Pool, content string) (groupID, itemID int, groupUUID string) {
	t.Helper()
	seedFreshInstallTranscript(t, pool, content)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
SELECT mg.id, mi.id, mg.uuid::text
FROM p_1.chat_message_group mg
JOIN p_1.chat_message_items mi ON mi.message_group_id = mg.id
ORDER BY mi.id
LIMIT 1`).Scan(&groupID, &itemID, &groupUUID); err != nil {
		t.Fatalf("read back the seeded group and item: %v", err)
	}
	return groupID, itemID, groupUUID
}

// The read. GetMessageByUUID LEFT JOINs chat_messages_canvas for EVERY item of
// EVERY message, canvas or not, so a tenant without that table cannot answer
// this call at all — the failure is not scoped to messages that carry a canvas.
// The seeded transcript deliberately has no canvas item for exactly that
// reason: it proves the join itself is what fails, not the canvas payload.
func TestGetMessageByUUIDAnswersOnAFreshInstallTenant(t *testing.T) {
	pool := newFreshInstallPool(t)
	_, _, groupUUID := seedFreshInstallTextItem(t, pool, "does a plain message read back?")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	message, err := NewConversationsRepo(pool).GetMessageByUUID(ctx, "1", groupUUID)
	if err != nil {
		t.Fatalf("GetMessageByUUID on a fresh-install tenant: %v", err)
	}
	items, _ := message["message_items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("message_items=%d, want the one text item that was seeded", len(items))
	}
	if got := items[0]["item_type"]; got != "text_message" {
		t.Errorf("item_type=%v, want text_message", got)
	}
	details, _ := items[0]["item_details"].(map[string]any)
	if got := details["content"]; got != "does a plain message read back?" {
		t.Errorf("content=%v, want the seeded question", got)
	}
}

// The write, end to end: the route's own path. CreateCanvas splits a text item
// in two around the selected range, writes the chat_messages_canvas row keyed
// on the new item's id and the first chat_canvas_versions row under it, and
// returns the detail the client renders. Round-tripping it — asserting the rows
// are actually THERE afterwards, not just that no error came back — is what
// separates "the tables exist" from "the tables hold what this code writes".
func TestCreateCanvasRoundTripsOnAFreshInstallTenant(t *testing.T) {
	pool := newFreshInstallPool(t)
	const seeded = "before CANVAS BODY after"
	groupID, itemID, _ := seedFreshInstallTextItem(t, pool, seeded)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The offsets of "CANVAS BODY" inside the seeded content: the client sends
	// the selection, not the text.
	const startsAt, endsAt = 7, 18
	canvas, err := NewConversationsRepo(pool).CreateCanvas(ctx, "1", map[string]any{
		"message_group_id":         groupID,
		"message_item_id":          itemID,
		"name":                     "extracted snippet",
		"canvas_type":              "code",
		"code_language":            "python",
		"canvas_content_starts_at": startsAt,
		"canvas_content_ends_at":   endsAt,
	})
	if err != nil {
		t.Fatalf("CreateCanvas on a fresh-install tenant: %v", err)
	}
	if got := canvas["name"]; got != "extracted snippet" {
		t.Errorf("name=%v, want the requested name", got)
	}
	latest, _ := canvas["latest_version"].(map[string]any)
	if got := latest["canvas_content"]; got != seeded[startsAt:endsAt] {
		t.Errorf("canvas_content=%v, want %q", got, seeded[startsAt:endsAt])
	}

	canvasItemID, _ := canvas["id"].(int)
	var storedName, storedType, storedContent string
	var storedLanguage *string
	if err := pool.QueryRow(ctx, `
SELECT mc.name, mc.canvas_type, v.canvas_content, v.code_language
FROM p_1.chat_messages_canvas mc
JOIN p_1.chat_canvas_versions v ON v.canvas_item_id = mc.id
WHERE mc.id = $1`, canvasItemID).Scan(&storedName, &storedType, &storedContent, &storedLanguage); err != nil {
		t.Fatalf("read the canvas back out of the tenant: %v", err)
	}
	if storedName != "extracted snippet" || storedType != "code" {
		t.Errorf("stored canvas is (%q, %q), want (extracted snippet, code)", storedName, storedType)
	}
	if storedContent != seeded[startsAt:endsAt] {
		t.Errorf("stored content=%q, want %q", storedContent, seeded[startsAt:endsAt])
	}
	if storedLanguage == nil || *storedLanguage != "python" {
		t.Errorf("stored code_language=%v, want python", storedLanguage)
	}

	// The split the same call performs. A canvas is carved OUT of a text item,
	// so the group must end up holding the text before it, the canvas, and the
	// text after it — three items where one was seeded. Asserting only the
	// canvas row would pass on a version that lost the surrounding prose.
	var items int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.chat_message_items WHERE message_group_id = $1`, groupID).Scan(&items); err != nil {
		t.Fatalf("count the group's items: %v", err)
	}
	if items != 3 {
		t.Errorf("message items=%d, want 3 (pre-text, canvas, post-text)", items)
	}

	// And the read now sees it, which is the pair of call sites this migration
	// exists for meeting in one transcript.
	message, err := NewConversationsRepo(pool).GetMessageByUUID(ctx, "1", canvasGroupUUID(t, pool, groupID))
	if err != nil {
		t.Fatalf("GetMessageByUUID after CreateCanvas: %v", err)
	}
	read, _ := message["message_items"].([]map[string]any)
	if len(read) != 3 {
		t.Fatalf("read back %d items, want 3", len(read))
	}
	var sawCanvas bool
	for _, item := range read {
		if item["item_type"] != "canvas_message" {
			continue
		}
		sawCanvas = true
		details, _ := item["item_details"].(map[string]any)
		if details["name"] != "extracted snippet" || details["canvas_type"] != "code" {
			t.Errorf("canvas item details=%v, want the created canvas's name and type", details)
		}
	}
	if !sawCanvas {
		t.Error("no canvas_message item came back from GetMessageByUUID")
	}
}

func canvasGroupUUID(t *testing.T, pool *pgxpool.Pool, groupID int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var groupUUID string
	if err := pool.QueryRow(ctx,
		`SELECT uuid::text FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&groupUUID); err != nil {
		t.Fatalf("read the group uuid: %v", err)
	}
	return groupUUID
}

// THE SHAPE, read from the corpus rather than restated here — the same
// discrimination the sibling file's third test performs, for the same reason.
//
// The canvas tables are now declared TWICE: by `create_tenant_schema` in
// 001_initial.sql, which is the only definition a fresh install sees, and by
// migrations/tenant/0129, which is the only definition a database the baseline
// never reached sees. Both are CREATE TABLE IF NOT EXISTS, so whichever runs
// second changes nothing — which means a disagreement between them is not an
// error anywhere. It is two deployments quietly carrying different columns for
// the same table, and only a comparison catches it.
func TestFreshInstallCanvasTablesMatchTheTenantCorpusShape(t *testing.T) {
	pool := newFreshInstallPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const corpusSchema = "canvas_corpus_shape"
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+corpusSchema); err != nil {
		t.Fatalf("create the comparison schema: %v", err)
	}
	// 0123 builds the chat graph the canvas tables hang off — chat_message_items
	// for the payload row, chat_participants for the version authors — and 0129
	// is guarded on both, so it would silently no-op without 0123 ahead of it.
	// Both files are written unqualified; a transaction-local search_path is
	// what puts them in the comparison schema, the same mechanism
	// migrate.Runner uses for a tenant.
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(ctx,
		"SELECT set_config('search_path', $1, true)", corpusSchema); err != nil {
		t.Fatalf("set the comparison search path: %v", err)
	}
	for _, path := range []string{
		"tenant/0123_agent_chat_message_tables.sql",
		"tenant/0129_chat_canvas_tables.sql",
	} {
		body, err := platformmigrations.Files.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := transaction.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s into the comparison schema: %v", path, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("commit the comparison schema: %v", err)
	}

	for _, table := range []string{
		"chat_messages_canvas",
		"chat_canvas_versions",
		"chat_canvas_version_authors",
	} {
		baseline := readColumns(ctx, t, pool, "p_1", table)
		corpus := readColumns(ctx, t, pool, corpusSchema, table)
		if len(baseline) == 0 {
			t.Errorf("p_1.%s does not exist; create_tenant_schema does not build it", table)
			continue
		}
		if len(corpus) == 0 {
			t.Errorf("%s.%s does not exist; the corpus file no longer creates it", corpusSchema, table)
			continue
		}
		if len(baseline) != len(corpus) {
			t.Errorf("%s: baseline has %d columns, the corpus has %d\n baseline: %v\n corpus:   %v",
				table, len(baseline), len(corpus), baseline, corpus)
			continue
		}
		for i := range baseline {
			if baseline[i] != corpus[i] {
				t.Errorf("%s column %d: baseline %v, corpus %v", table, i+1, baseline[i], corpus[i])
			}
		}
	}
}
