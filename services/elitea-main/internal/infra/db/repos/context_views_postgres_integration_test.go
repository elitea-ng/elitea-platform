package repos

// "What the agent saw" (ADR-0050 decision 9, issue 1669): the view recorded
// with an answer reads back in order with its authors, only for the answer's
// own reader, and a re-add with "share history from: now" moves the agent's
// join time instead of being dropped.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func seedContextViewMessage(t *testing.T, repo *ConversationsRepo, conversationID string, authorID int, text string) string {
	t.Helper()
	var id string
	if err := repo.pool.QueryRow(context.Background(), `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, created_at)
VALUES (gen_random_uuid(), $1, $2::int, jsonb_build_object('note', $3::text), now() AT TIME ZONE 'UTC')
RETURNING uuid::text`, authorID, conversationID, text).Scan(&id); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return id
}

func participantIDOf(t *testing.T, repo *ConversationsRepo, entityName string, id int) int {
	t.Helper()
	var participantID int
	if err := repo.pool.QueryRow(context.Background(), `
SELECT id FROM p_1.chat_participants WHERE entity_name = $1 AND entity_meta->>'id' = $2::int::text`,
		entityName, id).Scan(&participantID); err != nil {
		t.Fatalf("participant %s %d: %v", entityName, id, err)
	}
	return participantID
}

func recordView(t *testing.T, repo *ConversationsRepo, answer string, view agentexecutionapp.ContextViewRecord) {
	t.Helper()
	ctx := context.Background()
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO p_1, public`); err != nil {
		t.Fatal(err)
	}
	if err := recordContextView(ctx, tx, answer, &view); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestContextViewReadsBackTheAnswersOwnView(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	conversationID := seedConversation(t, repo)
	for _, body := range []map[string]any{
		{"entity_name": "user", "entity_meta": map[string]any{"id": 7, "user_name": "Ann <b>"}},
		{"entity_name": "application", "entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Reader"}},
		{"entity_name": "application", "entity_meta": map[string]any{"id": 43, "project_id": 1, "name": "Other"}},
	} {
		if err := repo.AddParticipant(ctx, "1", conversationID, body); err != nil {
			t.Fatalf("add participant: %v", err)
		}
	}
	person := participantIDOf(t, repo, "user", 7)
	reader := participantIDOf(t, repo, "application", 42)
	other := participantIDOf(t, repo, "application", 43)
	before := seedContextViewMessage(t, repo, conversationID, person, "before")
	after := seedContextViewMessage(t, repo, conversationID, person, "after")
	answer := seedContextViewMessage(t, repo, conversationID, reader, "answer")
	otherAnswer := seedContextViewMessage(t, repo, conversationID, other, "other answer")

	digest := contextview.Digest(contextview.HistorySinceJoined, 8192, []byte(`[]`), nil)
	recordView(t, repo, answer, agentexecutionapp.ContextViewRecord{
		ReaderParticipantID: int64(reader), ParticipantHistory: "since_joined",
		Settings:     contextview.Settings{History: contextview.HistorySinceJoined, SideTalk: contextview.SideTalkParty},
		WindowTokens: 8192, Digest: digest,
		Entries: []agentexecutionapp.ContextViewEntry{
			{MessageID: before, Reason: contextview.ReasonBeforeJoined},
			{MessageID: after},
			// An id of no message in this conversation labels nothing.
			{MessageID: "00000000-0000-4000-8000-0000000000ff"},
		},
	})
	// The other agent's answer carries a record whose reader is not its
	// author: it must never be served as the other agent's view.
	recordView(t, repo, otherAnswer, agentexecutionapp.ContextViewRecord{
		ReaderParticipantID: int64(reader), Settings: contextview.Default, Digest: digest,
	})

	store := NewContextViews(pool)
	view, err := store.MessageContextView(ctx, "1", answer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if view.History != "since_joined" || view.WindowTokens != 8192 || view.Digest != digest ||
		view.ParticipantHistory == nil || *view.ParticipantHistory != "since_joined" ||
		view.ReaderName == nil || *view.ReaderName != "Reader" {
		t.Fatalf("view = %+v", view)
	}
	if len(view.Entries) != 3 || view.Entries[0].MessageID != before || view.Entries[0].Reason == nil ||
		*view.Entries[0].Reason != "before_joined" || view.Entries[1].Reason != nil ||
		view.Entries[0].AuthorName == nil || *view.Entries[0].AuthorName != "Ann <b>" ||
		view.Entries[2].AuthorName != nil || view.Entries[2].CreatedAt != nil {
		t.Fatalf("entries = %+v", view.Entries)
	}

	var notFound *apierr.APIError
	if _, err := store.MessageContextView(ctx, "1", otherAnswer); !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("another reader's record: err = %v, want 404", err)
	}
	if _, err := store.MessageContextView(ctx, "1", after); !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("no record: err = %v, want 404", err)
	}

	// The compaction measurement is this run's only: one stamped with the
	// current task and generation shows, one left from an earlier run does not.
	measurement := `{"phase":"compacted","estimated_input_tokens":90000,"usable_input_tokens":100000,"compaction_trigger_tokens":80000}`
	if _, err := pool.Exec(ctx, `
UPDATE p_1.chat_message_group
SET task_id = 'run-2',
    meta = meta || jsonb_build_object('execution_generation', '2',
        'runtime_context', jsonb_build_object('execution_id', $2::text, 'execution_generation', '2', 'measurement', $3::jsonb))
WHERE uuid = $1::uuid`, answer, "run-2", measurement); err != nil {
		t.Fatal(err)
	}
	if view, err = store.MessageContextView(ctx, "1", answer); err != nil || view.Compaction == nil ||
		view.Compaction.Phase != "compacted" || view.Compaction.EstimatedInputTokens != 90000 {
		t.Fatalf("current run's measurement: %+v %v", view.Compaction, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_message_group SET task_id = 'run-3' WHERE uuid = $1::uuid`, answer); err != nil {
		t.Fatal(err)
	}
	if view, err = store.MessageContextView(ctx, "1", answer); err != nil || view.Compaction != nil {
		t.Fatalf("a previous run's measurement must not show: %+v %v", view.Compaction, err)
	}

	// A regeneration replaces the record.
	recordView(t, repo, answer, agentexecutionapp.ContextViewRecord{
		ReaderParticipantID: int64(reader), Settings: contextview.Default, WindowTokens: 4096, Digest: digest,
	})
	if view, err = store.MessageContextView(ctx, "1", answer); err != nil || view.History != "full" || len(view.Entries) != 0 {
		t.Fatalf("after regeneration: %+v %v", view, err)
	}
}

func TestReAddWithShareFromNowMovesTheJoinTime(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	conversationID := seedConversation(t, repo)
	agent := func(settings map[string]any) map[string]any {
		return map[string]any{
			"entity_name":     "application",
			"entity_meta":     map[string]any{"id": 42, "project_id": 1, "name": "Foo"},
			"entity_settings": settings,
		}
	}
	read := func() (string, time.Time, string) {
		var history *string
		var joined time.Time
		var version *string
		if err := pool.QueryRow(ctx, `
SELECT m.entity_settings->>'context_history', m.created_at, m.entity_settings->>'pinned'
FROM p_1.chat_participant_mapping m WHERE m.conversation_id = $1::int`, conversationID).Scan(&history, &joined, &version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("no mapping")
			}
			t.Fatal(err)
		}
		value := ""
		if history != nil {
			value = *history
		}
		versionID := ""
		if version != nil {
			versionID = *version
		}
		return value, joined, versionID
	}
	if err := repo.AddParticipant(ctx, "1", conversationID, agent(map[string]any{"pinned": "a"})); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_participant_mapping SET created_at = created_at - interval '1 hour'
WHERE conversation_id = $1::int`, conversationID); err != nil {
		t.Fatal(err)
	}
	_, firstJoined, _ := read()

	// A plain re-add changes nothing.
	if err := repo.AddParticipant(ctx, "1", conversationID, agent(map[string]any{"pinned": "b"})); err != nil {
		t.Fatal(err)
	}
	if history, joined, version := read(); history != "" || !joined.Equal(firstJoined) || version != "a" {
		t.Fatalf("plain re-add: history=%q joined=%v version=%q", history, joined, version)
	}

	// "Now" narrows and restarts the join time; other settings are kept.
	if err := repo.AddParticipant(ctx, "1", conversationID, agent(map[string]any{"context_history": "since_joined"})); err != nil {
		t.Fatal(err)
	}
	history, nowJoined, version := read()
	if history != "since_joined" || !nowJoined.After(firstJoined.Add(30*time.Minute)) || version != "a" {
		t.Fatalf("re-add with now: history=%q joined=%v (first %v) version=%q", history, nowJoined, firstJoined, version)
	}

	// "The start" widens again and keeps the join time.
	if err := repo.AddParticipant(ctx, "1", conversationID, agent(map[string]any{"context_history": "full"})); err != nil {
		t.Fatal(err)
	}
	if history, joined, _ := read(); history != "full" || !joined.Equal(nowJoined) {
		t.Fatalf("re-add with start: history=%q joined=%v", history, joined)
	}
	if participants, mappings := countParticipantRows(t, repo, conversationID); participants != 1 || mappings != 1 {
		t.Fatalf("rows: %d %d", participants, mappings)
	}
}
