package repos

// ADR-0029 decision 8: the next-turn guarantee of personal memory recall,
// against a REAL database (ELITEA_TEST_DATABASE_URL), because the rule reads
// the user's previous turn out of chat_message_group and compares two
// timestamp types in SQL.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
)

// TestPostgresMemoriesRepoRecallReservesNewestMemoryAfterPreviousTurn: a
// memory saved after the user's previous turn in the project is recalled in
// the next turn even when it shares no word with the input and eight older
// memories outrank it on overlap, and it keeps its full text while the others
// give up budget. A memory saved BEFORE the previous turn gets no
// reservation, and another user's message is never this user's previous turn.
func TestPostgresMemoriesRepoRecallReservesNewestMemoryAfterPreviousTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()

	const user = "4331"
	const otherUser = "4332"
	const projectID int64 = 1
	userID := mustParseInt64(t, user)

	seedAt := func(owner, content string, minutesAgo int) string {
		t.Helper()
		created, err := repo.Create(ctx, memoriesIntegrationProject, owner, memories.MemoryEntry{
			Content: content, Enabled: true,
		})
		if err != nil {
			t.Fatalf("seed memory %q: %v", content, err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE p_1.personal_memory_entries
SET created_at = now() - make_interval(mins => $2)
WHERE id = $1::bigint`, created.ID, minutesAgo); err != nil {
			t.Fatalf("date memory %q: %v", content, err)
		}
		return created.ID
	}
	var conversationID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'recall reservation', $1::int, 'elitea')
RETURNING id`, userID).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	userMessageAt := func(owner string, minutesAgo int) {
		t.Helper()
		var participantID int
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'user', jsonb_build_object('id', $1::int), '{}'::json)
RETURNING id`, mustParseInt64(t, owner)).Scan(&participantID); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
VALUES (gen_random_uuid(), $1, $2, (now() - make_interval(mins => $3))::timestamp)`,
			participantID, conversationID, minutesAgo); err != nil {
			t.Fatalf("seed user message: %v", err)
		}
	}

	// Eight older memories, each long and each overlapping the input, so the
	// overlap ranking alone fills every slot and the whole budget.
	long := strings.Repeat("restaurant dinner booking preference detail ", 7)
	for i := 0; i < 8; i++ {
		seedAt(user, strconv.Itoa(i)+" "+long, 120-i)
	}
	// The user's previous turn was an hour ago.
	userMessageAt(user, 60)
	// Another user spoke a minute ago: that is not THIS user's previous turn.
	userMessageAt(otherUser, 1)
	// Saved from the web after the previous turn; no word in common.
	freshID := seedAt(user, "Prefers dark mode in every editor.", 5)

	const input = "Book a restaurant for dinner, same preference as before."
	recall, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, userID, input)
	if err != nil {
		t.Fatalf("resolve recall: %v", err)
	}
	if !strings.Contains(recall.Text, "- Prefers dark mode in every editor.") {
		t.Fatalf("recall text omits the memory saved after the previous turn: %q", recall.Text)
	}
	if !containsRecallID(recall.IDs, freshID) {
		t.Errorf("recall ids %v omit the reserved memory %s", recall.IDs, freshID)
	}
	if recall.Count > currentMemoryRecallMaxEntries || recall.Count < 2 {
		t.Errorf("recall count = %d, want the reserved entry plus overlap-ranked entries, at most %d",
			recall.Count, currentMemoryRecallMaxEntries)
	}
	if got := recalledMemoryBytes(recall.Text); got > currentMemoryRecallMaxChars {
		t.Errorf("recalled memory text = %d bytes, want at most %d", got, currentMemoryRecallMaxChars)
	}

	// A newer user turn now exists: the same memory is no longer "after the
	// previous turn", so it has no reservation and overlap ranks it out.
	userMessageAt(user, 2)
	later, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, userID, input)
	if err != nil {
		t.Fatalf("resolve recall after a newer turn: %v", err)
	}
	if strings.Contains(later.Text, "dark mode") {
		t.Errorf("a memory older than the previous turn kept a reserved slot: %q", later.Text)
	}

	// A disabled newest memory is never reserved.
	disabledID := seedAt(user, "Disabled newest memory.", 0)
	if _, err := pool.Exec(ctx, `UPDATE p_1.personal_memory_entries SET enabled = FALSE WHERE id = $1::bigint`, disabledID); err != nil {
		t.Fatalf("disable memory: %v", err)
	}
	afterDisable, err := repo.ResolveCurrentMemoryRecall(ctx, projectID, userID, input)
	if err != nil {
		t.Fatalf("resolve recall after disable: %v", err)
	}
	if strings.Contains(afterDisable.Text, "Disabled newest") {
		t.Errorf("a disabled memory was recalled: %q", afterDisable.Text)
	}
}

// TestPostgresMemoriesRepoRecallReservesForUserWithNoTurns: a user who has
// never sent a message in the project has no previous turn, so the newest
// memory is always reserved, and the reserved entry is never truncated while
// the others still fit the budget.
func TestPostgresMemoriesRepoRecallReservesForUserWithNoTurns(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewMemoriesRepo(pool)
	ctx := context.Background()
	const user = "4341"
	long := strings.Repeat("garden watering schedule plants ", 9)
	for i := 0; i < 9; i++ {
		if _, err := repo.Create(ctx, memoriesIntegrationProject, user, memories.MemoryEntry{
			Content: strconv.Itoa(i) + " " + long, Enabled: true,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	fresh := "Speaks Polish at home. " + strings.Repeat("x", 500)
	if _, err := repo.Create(ctx, memoriesIntegrationProject, user, memories.MemoryEntry{
		Content: fresh, Enabled: true,
	}); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	// Make it unambiguously the most recent, whatever the clock resolution.
	if _, err := pool.Exec(ctx, `
UPDATE p_1.personal_memory_entries SET created_at = now() + interval '1 second'
WHERE user_id = $1 AND content = $2`, mustParseInt64(t, user), fresh); err != nil {
		t.Fatalf("date fresh: %v", err)
	}
	recall, err := repo.ResolveCurrentMemoryRecall(ctx, 1, mustParseInt64(t, user), "When should I water the garden plants?")
	if err != nil {
		t.Fatalf("resolve recall: %v", err)
	}
	if !strings.Contains(recall.Text, "- "+fresh) {
		t.Errorf("recall text omits the full newest memory for a user with no turns: %q", recall.Text)
	}
	if got := recalledMemoryBytes(recall.Text); got > currentMemoryRecallMaxChars {
		t.Errorf("recalled memory text = %d bytes, want at most %d", got, currentMemoryRecallMaxChars)
	}
}

func TestTruncateRecallTextKeepsUTF8Valid(t *testing.T) {
	text := "abécd" // é is two bytes at offsets 2..3
	if got := truncateRecallText(text, 3); got != "ab" {
		t.Errorf("truncate(3) = %q, want %q", got, "ab")
	}
	if got := truncateRecallText(text, 4); got != "abé" {
		t.Errorf("truncate(4) = %q", got)
	}
	if got := truncateRecallText(text, 0); got != "" {
		t.Errorf("truncate(0) = %q, want empty", got)
	}
	if got := truncateRecallText(text, 100); got != text {
		t.Errorf("truncate(100) = %q, want unchanged", got)
	}
}

// recalledMemoryBytes counts the memory text in a recall block, without the
// header line and the "- " bullet markers the budget does not cover.
func recalledMemoryBytes(text string) int {
	lines := strings.Split(text, "\n")
	total := 0
	for _, line := range lines[1:] {
		total += len(strings.TrimPrefix(line, "- "))
	}
	return total
}

func containsRecallID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
