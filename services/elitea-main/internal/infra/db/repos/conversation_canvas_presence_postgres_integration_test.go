package repos

// #622 — the cross-project negative, asked of PostgreSQL rather than of a
// double.
//
// The whole reason the presence route resolves a canvas instead of trusting the
// path segment is that canvas ids are per-tenant-schema SERIAL integers. Two
// tenants built by the same create_tenant_schema get independent sequences, so
// the FIRST canvas in every project is id 1. A double cannot demonstrate that —
// it is a property of the schema layout — so this test builds two real tenants
// and seeds one canvas in each.
//
// RED WITHOUT THE FIX: interpolating a fixed schema, or dropping the
// tenantSchema call and querying the search path, makes
// TestResolveCanvasIsScopedToTheProjectSchema return project 1's uuid for
// project 2's id.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// seedCanvasItem writes one canvas message item into the named tenant schema
// and returns its integer id and its uuid. Raw SQL rather than CreateCanvas: the
// point here is the SHAPE of the id, and CreateCanvas would drag its text-split
// behaviour into a test about scoping.
func seedCanvasItem(t *testing.T, pool *pgxpool.Pool, schemaName, canvasName string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var conversationID, participantID, groupID, itemID int
	var itemUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_conversations (uuid, name, is_private, author_id)
VALUES (gen_random_uuid(), $1, TRUE, 1) RETURNING id`, canvasName).Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation into %s: %v", schemaName, err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_participants (uuid, entity_name, entity_meta)
VALUES (gen_random_uuid(), 'user', '{"id": 1}'::jsonb) RETURNING id`).Scan(&participantID); err != nil {
		t.Fatalf("insert participant into %s: %v", schemaName, err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_message_group
    (uuid, author_participant_id, conversation_id, meta, is_streaming)
VALUES (gen_random_uuid(), $1, $2, '{}'::jsonb, false) RETURNING id`,
		participantID, conversationID).Scan(&groupID); err != nil {
		t.Fatalf("insert message group into %s: %v", schemaName, err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
VALUES (gen_random_uuid(), 'canvas_message', 0, '{}'::jsonb, $1)
RETURNING id, uuid::text`, groupID).Scan(&itemID, &itemUUID); err != nil {
		t.Fatalf("insert canvas item into %s: %v", schemaName, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO `+schemaName+`.chat_messages_canvas (id, name, canvas_type)
VALUES ($1, $2, 'code')`, itemID, canvasName); err != nil {
		t.Fatalf("insert canvas payload into %s: %v", schemaName, err)
	}
	return itemID, itemUUID
}

// seedTextItem writes a NON-canvas item, so the item_type filter has something
// to refuse.
func seedTextItem(t *testing.T, pool *pgxpool.Pool, schemaName string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var conversationID, participantID, groupID, itemID int
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_conversations (uuid, name, is_private, author_id)
VALUES (gen_random_uuid(), 'text only', TRUE, 1) RETURNING id`).Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_participants (uuid, entity_name, entity_meta)
VALUES (gen_random_uuid(), 'user', '{"id": 1}'::jsonb) RETURNING id`).Scan(&participantID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_message_group
    (uuid, author_participant_id, conversation_id, meta, is_streaming)
VALUES (gen_random_uuid(), $1, $2, '{}'::jsonb, false) RETURNING id`,
		participantID, conversationID).Scan(&groupID); err != nil {
		t.Fatalf("insert message group: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO `+schemaName+`.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
VALUES (gen_random_uuid(), 'text_message', 0, '{}'::jsonb, $1) RETURNING id`, groupID).Scan(&itemID); err != nil {
		t.Fatalf("insert text item: %v", err)
	}
	return itemID
}

func newTwoTenantPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newFreshInstallPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema('p_2')`); err != nil {
		t.Fatalf("create the second tenant schema: %v", err)
	}
	return pool
}

func TestResolveCanvasIsScopedToTheProjectSchema(t *testing.T) {
	pool := newTwoTenantPool(t)
	repo := NewConversationsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstID, firstUUID := seedCanvasItem(t, pool, "p_1", "project one canvas")
	secondID, secondUUID := seedCanvasItem(t, pool, "p_2", "project two canvas")

	// The premise. If the sequences ever stopped being per-schema this test
	// would silently stop discriminating, so it is asserted rather than assumed.
	if firstID != secondID {
		t.Fatalf("canvas ids are %d and %d; this test needs the SAME id in both tenants to mean anything", firstID, secondID)
	}
	if firstUUID == secondUUID {
		t.Fatal("the two seeded canvases share a uuid; the fixture is wrong")
	}

	gotFirst, groupFirst, err := repo.ResolveCanvas(ctx, "1", strconv.Itoa(firstID))
	if err != nil {
		t.Fatalf("resolve project 1's canvas: %v", err)
	}
	if gotFirst != firstUUID {
		t.Fatalf("project 1 resolved %q, want %q", gotFirst, firstUUID)
	}
	if groupFirst == "" {
		t.Fatal("no message group uuid came back; the reference event carries one and the SPA matches on it")
	}

	gotSecond, _, err := repo.ResolveCanvas(ctx, "2", strconv.Itoa(secondID))
	if err != nil {
		t.Fatalf("resolve project 2's canvas: %v", err)
	}
	if gotSecond != secondUUID {
		t.Fatalf("project 2 resolved %q, want %q", gotSecond, secondUUID)
	}

	// THE ONE THAT MATTERS. The same id, resolved in two projects, must be two
	// different canvases — never one shared room.
	if gotFirst == gotSecond {
		t.Fatalf("canvas id %d resolved to the same uuid in both projects (%q); the id was not scoped to the schema", firstID, gotFirst)
	}

	// And a uuid belonging to the other tenant does not resolve here at all.
	if _, _, err := repo.ResolveCanvas(ctx, "1", secondUUID); !isNotFound(err) {
		t.Fatalf("resolving project 2's canvas uuid inside project 1 returned %v, want 404", err)
	}
}

func TestResolveCanvasRefusesNonCanvasAndUnknownIDs(t *testing.T) {
	pool := newFreshInstallPool(t)
	repo := NewConversationsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	textID := seedTextItem(t, pool, "p_1")

	// chat_message_items holds text, tool-call and canvas items on one id
	// sequence, so without the item_type filter a text item's id would resolve
	// and presence would be announced for something that is not a canvas.
	if _, _, err := repo.ResolveCanvas(ctx, "1", strconv.Itoa(textID)); !isNotFound(err) {
		t.Fatalf("resolving a TEXT item as a canvas returned %v, want 404", err)
	}
	if _, _, err := repo.ResolveCanvas(ctx, "1", "999999"); !isNotFound(err) {
		t.Fatalf("resolving an unknown id returned %v, want 404", err)
	}
	// Neither an integer nor a uuid. This must be a 404, not the 500 that
	// PostgreSQL's "invalid input syntax for type uuid" would produce.
	if _, _, err := repo.ResolveCanvas(ctx, "1", "not-an-id"); !isNotFound(err) {
		t.Fatalf("resolving a malformed id returned %v, want 404", err)
	}
	// A project id that is not a plain decimal never reaches SQL at all.
	if _, _, err := repo.ResolveCanvas(ctx, "p_1; DROP SCHEMA public", "1"); err == nil {
		t.Fatal("a non-decimal project id resolved; tenantSchema must refuse it")
	}
}

func isNotFound(err error) bool {
	var apiError *apierr.APIError
	return errors.As(err, &apiError) && apiError.Status == 404
}
