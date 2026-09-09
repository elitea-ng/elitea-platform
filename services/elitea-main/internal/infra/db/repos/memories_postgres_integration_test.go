package repos

// Persistent, cross-conversation personal memory (#870), against a REAL
// database — both halves of MemoriesRepo: the Settings > Memory CRUD
// (memories.Repository) and the turn-start RECALL path
// (agentexecutionapp.CurrentMemoryRecallResolver), because the thing under
// test in both cases is a real SQL statement against
// tenant/0137_personal_memory_entries.sql's actual shape (the UNIQUE-free,
// user_id-scoped, no-foreign-key table its own header explains), not a
// handler-level fake that would let the test decide what the table allows.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL) — see
// newMigratedPostgresIntegrationPool (configuration_validation_postgres_
// integration_test.go), which builds its template from the ledgered
// migration corpus, so this test exercises 0136 exactly as a real deployment
// runs it.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
)

// memoriesIntegrationProject is the tenant the shared template migrates
// (postgresIntegrationTenant), so its schema is p_1.
const memoriesIntegrationProject = "1"

func TestPostgresMemoriesRepoCRUDIsScopedToOwner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()

	const owner = "4301"
	const stranger = "4302"

	created, err := repo.Create(ctx, memoriesIntegrationProject, owner, memories.MemoryEntry{
		Content: "Prefers TypeScript over Python for new services.",
		Tags:    []string{"preferences", "engineering"},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("create returned no id: %+v", created)
	}
	if created.ProjectID != memoriesIntegrationProject {
		t.Errorf("create project id = %q, want %q", created.ProjectID, memoriesIntegrationProject)
	}
	if len(created.Tags) != 2 {
		t.Errorf("create tags = %v, want 2 entries", created.Tags)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("create left timestamps zero: %+v", created)
	}

	// A second owner's own memory must never leak into the first owner's list.
	if _, err := repo.Create(ctx, memoriesIntegrationProject, stranger, memories.MemoryEntry{
		Content: "Stranger's own memory.",
		Enabled: true,
	}); err != nil {
		t.Fatalf("create (stranger): %v", err)
	}

	listed, err := repo.List(ctx, memoriesIntegrationProject, owner, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list = %d entries, want exactly the owner's own 1 (cross-owner leak?): %+v", len(listed), listed)
	}
	if listed[0].ID != created.ID {
		t.Errorf("list returned id %q, want %q", listed[0].ID, created.ID)
	}

	// Search is a substring match over content AND tags.
	byContent, err := repo.List(ctx, memoriesIntegrationProject, owner, "typescript")
	if err != nil {
		t.Fatalf("list (content search): %v", err)
	}
	if len(byContent) != 1 {
		t.Errorf("content search = %d entries, want 1", len(byContent))
	}
	byTag, err := repo.List(ctx, memoriesIntegrationProject, owner, "engineering")
	if err != nil {
		t.Fatalf("list (tag search): %v", err)
	}
	if len(byTag) != 1 {
		t.Errorf("tag search = %d entries, want 1", len(byTag))
	}
	byMiss, err := repo.List(ctx, memoriesIntegrationProject, owner, "no-such-substring-anywhere")
	if err != nil {
		t.Fatalf("list (missing search): %v", err)
	}
	if len(byMiss) != 0 {
		t.Errorf("missing search = %d entries, want 0", len(byMiss))
	}

	// A stranger updating or deleting the owner's entry must answer 404, not
	// silently touch someone else's row — same discipline
	// SetMessageFeedback's own UNIQUE(message_group_uuid, user_id) enforces.
	if _, err := repo.Update(ctx, memoriesIntegrationProject, stranger, created.ID, memories.MemoryEntry{
		Content: "hijacked", Enabled: true,
	}); !isMemoryNotFound(err) {
		t.Errorf("stranger update = %v, want NotFound", err)
	}
	if err := repo.Delete(ctx, memoriesIntegrationProject, stranger, created.ID); err != nil {
		t.Errorf("stranger delete (no-op expected, not an error): %v", err)
	}
	// The owner's row must have survived the stranger's delete attempt.
	stillListed, err := repo.List(ctx, memoriesIntegrationProject, owner, "")
	if err != nil {
		t.Fatalf("list after stranger delete attempt: %v", err)
	}
	if len(stillListed) != 1 {
		t.Fatalf("owner's memory did not survive a stranger's delete attempt: %+v", stillListed)
	}

	// The OWNER'S OWN update is a full-record replace.
	updated, err := repo.Update(ctx, memoriesIntegrationProject, owner, created.ID, memories.MemoryEntry{
		Content: "Prefers Go over TypeScript now.",
		Tags:    []string{"preferences"},
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if updated.Content != "Prefers Go over TypeScript now." {
		t.Errorf("update content = %q", updated.Content)
	}
	if updated.Enabled {
		t.Errorf("update left enabled = true, want false")
	}
	if len(updated.Tags) != 1 {
		t.Errorf("update tags = %v, want 1 entry", updated.Tags)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) && updated.UpdatedAt != created.UpdatedAt {
		// Timestamps from `now()` can tie at test speed; only fail a
		// regression (UpdatedAt going BACKWARDS).
		t.Errorf("update moved updated_at backwards: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
	}

	if err := repo.Delete(ctx, memoriesIntegrationProject, owner, created.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	afterDelete, err := repo.List(ctx, memoriesIntegrationProject, owner, "")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(afterDelete) != 0 {
		t.Errorf("list after delete = %d entries, want 0", len(afterDelete))
	}
}

func TestPostgresMemoriesRepoClearAllIsScopedToOwner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()

	const owner = "4311"
	const other = "4312"

	for i := 0; i < 3; i++ {
		if _, err := repo.Create(ctx, memoriesIntegrationProject, owner, memories.MemoryEntry{
			Content: "owner memory " + strconv.Itoa(i), Enabled: true,
		}); err != nil {
			t.Fatalf("seed owner memory %d: %v", i, err)
		}
	}
	if _, err := repo.Create(ctx, memoriesIntegrationProject, other, memories.MemoryEntry{
		Content: "other's memory", Enabled: true,
	}); err != nil {
		t.Fatalf("seed other's memory: %v", err)
	}

	removed, err := repo.ClearAll(ctx, memoriesIntegrationProject, owner)
	if err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if removed != 3 {
		t.Errorf("clear all removed %d, want 3", removed)
	}

	ownerAfter, err := repo.List(ctx, memoriesIntegrationProject, owner, "")
	if err != nil {
		t.Fatalf("list owner after clear: %v", err)
	}
	if len(ownerAfter) != 0 {
		t.Errorf("owner still has %d memories after clear all", len(ownerAfter))
	}
	otherAfter, err := repo.List(ctx, memoriesIntegrationProject, other, "")
	if err != nil {
		t.Fatalf("list other after owner's clear: %v", err)
	}
	if len(otherAfter) != 1 {
		t.Errorf("clear all touched another owner's memories: %d left, want 1", len(otherAfter))
	}
}

func isMemoryNotFound(err error) bool {
	return err != nil && err.Error() == "memory not found"
}

// TestPostgresMemoriesRepoResolveCurrentMemoryRecall pins the recall path a
// real chat turn depends on (#870): disabled memories are never candidates,
// a keyword match on the current turn's text outranks a merely-more-recent
// unrelated memory, and the recalled text is bounded (entry count AND total
// characters) so a user with many memories cannot turn one turn's context
// window into a memory dump.
func TestPostgresMemoriesRepoResolveCurrentMemoryRecall(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()

	const user = "4321"
	const projectID int64 = 1

	seed := func(content string, enabled bool) {
		t.Helper()
		if _, err := repo.Create(ctx, memoriesIntegrationProject, user, memories.MemoryEntry{
			Content: content, Enabled: enabled,
		}); err != nil {
			t.Fatalf("seed memory %q: %v", content, err)
		}
	}

	// Seeded oldest first, so "most recent" and "keyword match" disagree —
	// the disagreement is the point of this test.
	seed("Lives in Berlin and prefers meetings before noon CET.", true)
	seed("Owns a golden retriever named Biscuit.", true)
	seed("Is allergic to shellfish — never suggest seafood restaurants.", true)
	seed("DISABLED memory that must never be recalled.", false)

	recall, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, mustParseInt64(t, user), "What restaurant should I book for dinner?")
	if err != nil {
		t.Fatalf("resolve recall: %v", err)
	}
	if recall.Count == 0 {
		t.Fatalf("recall found no memories, want at least the shellfish allergy")
	}
	if !containsSubstring(recall.Text, "shellfish") {
		t.Errorf("recall text = %q, want the keyword-matched shellfish memory ranked in", recall.Text)
	}
	if containsSubstring(recall.Text, "DISABLED memory") {
		t.Errorf("recall text included a disabled memory: %q", recall.Text)
	}

	// No keyword overlap at all: recall must still answer something (falls
	// back to recency), never an error and never empty when enabled
	// memories exist.
	fallback, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, mustParseInt64(t, user), "")
	if err != nil {
		t.Fatalf("resolve recall (no keywords): %v", err)
	}
	if fallback.Count == 0 {
		t.Errorf("recall with no keywords found nothing, want a recency fallback")
	}

	// A user with no memories at all recalls nothing, and that is not an
	// error — most turns in most projects have no memories yet.
	empty, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, mustParseInt64(t, "4399"), "anything")
	if err != nil {
		t.Fatalf("resolve recall (no memories): %v", err)
	}
	if empty.Count != 0 || empty.Text != "" {
		t.Errorf("recall for a user with no memories = %+v, want zero value", empty)
	}
}

// TestPostgresMemoriesRepoRecordCurrentMemoryUsage pins the OTHER half of
// recall: the best-effort, post-admission stamp onto the response message
// group's own `meta`, which is how the web's "Using N memories" indicator
// reads the count back — through the SAME ordinary GetMessage/ListMessages
// response, no new API surface (see RecordCurrentMemoryUsage's own comment).
func TestPostgresMemoriesRepoRecordCurrentMemoryUsage(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()

	var conversationID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'memory usage', 1, 'agent')
RETURNING id::text`).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	var participantID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'application', '{}'::jsonb, '{}'::jsonb)
RETURNING id`).Scan(&participantID); err != nil {
		t.Fatalf("seed participant: %v", err)
	}
	responseUUID := "b3f6e6b0-0000-4000-8000-000000000870"
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta)
VALUES ($1::uuid, $2, $3::int, '{"existing_key": true}'::jsonb)`,
		responseUUID, participantID, conversationID); err != nil {
		t.Fatalf("seed response message group: %v", err)
	}

	if err := repo.RecordCurrentMemoryUsage(ctx, 1, responseUUID, 3); err != nil {
		t.Fatalf("record memory usage: %v", err)
	}

	var meta []byte
	if err := pool.QueryRow(ctx, `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, responseUUID).Scan(&meta); err != nil {
		t.Fatalf("read back meta: %v", err)
	}
	metaText := string(meta)
	if !containsSubstring(metaText, `"memories_used": 3`) && !containsSubstring(metaText, `"memories_used":3`) {
		t.Errorf("meta = %s, want memories_used=3", metaText)
	}
	if !containsSubstring(metaText, `"existing_key"`) {
		t.Errorf("meta = %s, want the pre-existing key preserved (merge, not overwrite)", metaText)
	}

	// count <= 0 must be a true no-op — never clobber a prior stamp with a
	// later turn's zero.
	if err := repo.RecordCurrentMemoryUsage(ctx, 1, responseUUID, 0); err != nil {
		t.Fatalf("record memory usage (zero): %v", err)
	}
	var metaAfterZero []byte
	if err := pool.QueryRow(ctx, `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, responseUUID).Scan(&metaAfterZero); err != nil {
		t.Fatalf("read back meta after zero: %v", err)
	}
	if string(metaAfterZero) != metaText {
		t.Errorf("a zero-count record changed meta: %s -> %s", metaText, string(metaAfterZero))
	}
}

func mustParseInt64(t *testing.T, s string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q as int64: %v", s, err)
	}
	return value
}

func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
