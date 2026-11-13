package repos

// Views stored as frame chains (issue 1893, tenant/0169): every answer of a
// long conversation, written through the admission's write path, reads back
// exactly the entries it recorded — across keyframes, a regeneration in the
// middle of a chain, a deleted message, a deleted base answer and a row
// written before the migration.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
)

func TestContextViewFramesReadBackEveryAnswer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	store := NewContextViews(pool)
	ctx := context.Background()
	conversationID := seedConversation(t, repo)
	for _, body := range []map[string]any{
		{"entity_name": "user", "entity_meta": map[string]any{"id": 7, "user_name": "Ann"}},
		{"entity_name": "application", "entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Reader"}},
		{"entity_name": "application", "entity_meta": map[string]any{"id": 43, "project_id": 1, "name": "Second"}},
	} {
		if err := repo.AddParticipant(ctx, "1", conversationID, body); err != nil {
			t.Fatalf("add participant: %v", err)
		}
	}
	person := participantIDOf(t, repo, "user", 7)
	reader := participantIDOf(t, repo, "application", 42)
	second := participantIDOf(t, repo, "application", 43)
	digest := contextview.Digest(contextview.HistoryFull, 8192, []byte(`[]`), nil)

	var transcript []string // live messages, in order
	deleted := map[string]bool{}
	recorded := map[string][]agentexecutionapp.ContextViewEntry{}
	readerOf := map[string]int{}
	// view is what the resolver would project for an answer now: every live
	// message, the first two dropped as before the join, the second
	// agent's answers dropped as side talk for the reader.
	authorOf := map[string]int{}
	view := func(who int) []agentexecutionapp.ContextViewEntry {
		out := []agentexecutionapp.ContextViewEntry{}
		for i, id := range transcript {
			entry := agentexecutionapp.ContextViewEntry{MessageID: id}
			switch {
			case i < 2:
				entry.Reason = contextview.ReasonBeforeJoined
			case who == reader && authorOf[id] == second:
				entry.Reason = contextview.ReasonSideTalkOfOthers
			}
			out = append(out, entry)
		}
		return out
	}
	record := func(answer string, by int, entries []agentexecutionapp.ContextViewEntry) {
		recordView(t, repo, answer, agentexecutionapp.ContextViewRecord{
			ReaderParticipantID: int64(by), Settings: contextview.Default, WindowTokens: 8192,
			Digest: digest, Entries: entries,
		})
		recorded[answer], readerOf[answer] = entries, by
	}
	say := func(by int) string {
		id := seedContextViewMessage(t, repo, conversationID, by, "message")
		authorOf[id] = by
		return id
	}
	answer := func(by int) string {
		entries := view(by)
		id := say(by)
		record(id, by, entries)
		transcript = append(transcript, id)
		return id
	}
	checkAll := func(stage string) {
		t.Helper()
		for id, want := range recorded {
			if deleted[id] {
				continue
			}
			got, err := store.MessageContextView(ctx, "1", id)
			if err != nil {
				t.Fatalf("%s: read %s: %v", stage, id, err)
			}
			if int(got.ReaderParticipantID) != readerOf[id] || len(got.Entries) != len(want) {
				t.Fatalf("%s: %s has %d entries (reader %d), want %d (reader %d)",
					stage, id, len(got.Entries), got.ReaderParticipantID, len(want), readerOf[id])
			}
			for i, entry := range got.Entries {
				reason := ""
				if entry.Reason != nil {
					reason = *entry.Reason
				}
				if entry.MessageID != want[i].MessageID || reason != string(want[i].Reason) {
					t.Fatalf("%s: %s entry %d = %s/%q, want %s/%q",
						stage, id, i, entry.MessageID, reason, want[i].MessageID, want[i].Reason)
				}
				// A message deleted since keeps its place with no author.
				if gone := deleted[entry.MessageID]; gone != (entry.AuthorName == nil) || gone != (entry.CreatedAt == nil) {
					t.Fatalf("%s: %s entry %d (deleted %v) author = %v", stage, id, i, gone, entry.AuthorName)
				}
			}
		}
	}

	transcript = append(transcript, say(person), say(person))

	// A row written before tenant/0169: the full list, no frame.
	legacy := say(reader)
	legacyEntries := view(reader)
	encoded, _ := json.Marshal(legacyEntries)
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_context_views
    (message_group_uuid, reader_participant_id, history, include_side_talk, window_tokens, digest, entries)
VALUES ($1::uuid, $2, 'full', 'party', 8192, $3, $4::jsonb)`, legacy, reader, digest, encoded); err != nil {
		t.Fatal(err)
	}
	recorded[legacy], readerOf[legacy] = legacyEntries, reader
	transcript = append(transcript, legacy)
	checkAll("legacy row")

	// Enough answers to cross a keyframe, two readers interleaved.
	var answers []string
	for turn := 0; turn < contextViewKeyframeInterval+20; turn++ {
		transcript = append(transcript, say(person))
		answers = append(answers, answer(reader))
		if turn%5 == 0 {
			answer(second)
		}
	}
	checkAll("after the answers")

	var legacyFrame *int64
	var legacyStored string
	if err := pool.QueryRow(ctx, `SELECT frame_id, entries::text FROM p_1.chat_message_context_views WHERE message_group_uuid = $1::uuid`,
		legacy).Scan(&legacyFrame, &legacyStored); err != nil || legacyFrame == nil || legacyStored != "[]" {
		t.Fatalf("the legacy row is the chain base: frame %v entries %s err %v", legacyFrame, legacyStored, err)
	}
	var keyframes, maxDepth, frames int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE base_frame_id IS NULL), max(depth), count(*)
FROM p_1.chat_message_context_view_frames WHERE reader_participant_id = $1`, reader).Scan(&keyframes, &maxDepth, &frames); err != nil {
		t.Fatal(err)
	}
	if keyframes != 2 || maxDepth != contextViewKeyframeInterval-1 {
		t.Fatalf("reader chain: %d keyframes, max depth %d over %d frames", keyframes, maxDepth, frames)
	}

	// A regeneration in the middle of a chain: the answer gets a new view
	// (a narrower one); the answers after it, built on its old frame, still
	// read what they recorded.
	middle := answers[10]
	narrower := append([]agentexecutionapp.ContextViewEntry(nil), recorded[middle][:5]...)
	narrower[3].Reason = contextview.ReasonSideTalkOfOthers
	record(middle, reader, narrower)
	checkAll("after a regeneration in the middle")

	// A message deleted since: the views that held it keep it, authorless.
	gone := transcript[4]
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_message_group WHERE uuid = $1::uuid`, gone); err != nil {
		t.Fatal(err)
	}
	deleted[gone] = true
	without := transcript[:0:0]
	for _, id := range transcript {
		if id != gone {
			without = append(without, id)
		}
	}
	transcript = without
	checkAll("after a message was deleted")

	// A deleted base answer: its view row goes with it, its frame stays as
	// the base of the frames built on it.
	base := answers[len(answers)-3]
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_message_group WHERE uuid = $1::uuid`, base); err != nil {
		t.Fatal(err)
	}
	deleted[base] = true
	without = transcript[:0:0]
	for _, id := range transcript {
		if id != base {
			without = append(without, id)
		}
	}
	transcript = without
	transcript = append(transcript, say(person))
	answers = append(answers, answer(reader))
	checkAll("after a base answer was deleted")

	// Regenerating the newest answer again and again does not pile up
	// frames: the frame each run replaces is collected.
	last := answers[len(answers)-1]
	before := countContextViewFrames(t, pool, reader)
	for range 5 {
		record(last, reader, view(reader)[:len(recorded[last])])
	}
	if after := countContextViewFrames(t, pool, reader); after != before {
		t.Fatalf("regenerations left frames: %d before, %d after", before, after)
	}
	checkAll("after regenerating the newest answer")

	// Deleting the conversation takes every frame with it (its messages
	// first: this schema's message key does not cascade).
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_message_group WHERE conversation_id = $1::int`, conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_conversations WHERE id = $1::int`, conversationID); err != nil {
		t.Fatal(err)
	}
	if left := countContextViewFrames(t, pool, reader) + countContextViewFrames(t, pool, second); left != 0 {
		t.Fatalf("%d frames outlived their conversation", left)
	}
}

func countContextViewFrames(t *testing.T, pool *pgxpool.Pool, reader int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.chat_message_context_view_frames WHERE reader_participant_id = $1`, reader).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
