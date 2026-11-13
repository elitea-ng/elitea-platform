package repos

// A people-only post (PostHumanMessage) never gets a reply, so the history's
// "completed pair" rule would drop it from every later turn. It is a visible
// message of a person (ADR-0050 decision 9a): the next agent turn sees it,
// named and marked as not addressed to the reader. The marker is explicit, so
// the unanswered question of a failed turn stays out.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestPostgresChatHistoryCarriesAPeopleOnlyPostAndNotAFailedQuestion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	if _, err := tx.Exec(t.Context(), `
INSERT INTO chat_participants (id, uuid, entity_name, entity_meta, meta) VALUES
    (27, '90000000-0000-4000-8000-000000001970', 'user', '{"id":12}'::jsonb, '{"user_name":"Sam"}'::json);
INSERT INTO chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES
    (1, 27, '{}'::jsonb, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}
	// Turn one, a failed question of the asker (no completed reply), a
	// people-only post by Sam, and the asker's people-only post.
	first := insertPostgresHistoryGroup(t, tx, 1970001, 20, 0, "why is CI red?", 9)
	insertPostgresHistoryGroup(t, tx, 1970002, 21, first, "Step 3 failed.", 8)
	insertPostgresHistoryGroup(t, tx, 1970003, 20, 0, "an unanswered question of a failed turn", 7)
	insertPostgresHistoryGroup(t, tx, 1970004, 27, 0, "@Aksana can you see this?", 6)
	insertPostgresHistoryGroup(t, tx, 1970005, 20, 0, "@Sam yes, looking", 5)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group SET meta = '{"people_only":true,"mentioned_user_ids":[7]}'::jsonb
WHERE id IN (1970004, 1970005)`); err != nil {
		t.Fatal(err)
	}

	history := resolvePostgresApplicationChatHistory(
		t, queries, mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		"50000000-0000-4000-8000-000000001970",
	)
	if len(history) != 4 {
		raw, _ := json.Marshal(history)
		t.Fatalf("history=%s", raw)
	}
	if history[0].Content[0]["text"] != "why is CI red?" || history[1].Role != "assistant" {
		t.Fatalf("the answered pair changed: %+v", history[:2])
	}
	for index, want := range []struct{ name, text string }{{"Sam", "@Aksana can you see this?"}, {"Sam", "@Sam yes, looking"}} {
		group := history[2+index]
		if group.Role != "user" || len(group.Content) != 2 {
			t.Fatalf("people-only entry %d = %+v", index, group)
		}
		note, _ := group.Content[0]["text"].(string)
		if note == "" || group.Content[0]["text"] == want.text || group.Content[1]["text"] != want.text {
			t.Fatalf("people-only entry %d content = %+v", index, group.Content)
		}
		author, _ := group.AdditionalKwargs["elitea_author"].(map[string]any)
		if author["kind"] != "person" || group.AdditionalKwargs["elitea_addressed"] != "people" {
			t.Fatalf("people-only entry %d kwargs = %+v", index, group.AdditionalKwargs)
		}
	}
	// The asker's own post is named too (it is not the reader's question).
	if author, _ := history[3].AdditionalKwargs["elitea_author"].(map[string]any); author["name"] != "" && author["name"] == nil {
		t.Fatalf("asker kwargs = %+v", history[3].AdditionalKwargs)
	}
	if author, _ := history[2].AdditionalKwargs["elitea_author"].(map[string]any); author["name"] != "Sam" {
		t.Fatalf("Sam's post is named: %+v", history[2].AdditionalKwargs)
	}
}

// The ad-hoc projection is a separate copy of the same SQL.
func TestPostgresAdhocChatHistoryCarriesAPeopleOnlyPost(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := insertPostgresHistoryGroupIn(t, tx, 2, 1970011, 20, 0, "check the build", 4)
	insertPostgresHistoryGroupIn(t, tx, 2, 1970012, 23, first, "It is green.", 3)
	insertPostgresHistoryGroupIn(t, tx, 2, 1970013, 20, 0, "unanswered", 2)
	insertPostgresHistoryGroupIn(t, tx, 2, 1970014, 20, 0, "@Sam lunch?", 1)
	if _, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET meta = '{"people_only":true}'::jsonb WHERE id = 1970014`); err != nil {
		t.Fatal(err)
	}
	target, err := queries.ResolveCurrentAdhocTurn(t.Context(), sqlcgen.ResolveCurrentAdhocTurnParams{
		ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
		QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000001971"),
		ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032"),
	})
	if err != nil {
		t.Fatalf("resolve ad-hoc turn: %v", err)
	}
	var history []postgresHistoryGroup
	if err := json.Unmarshal([]byte(target.ChatHistoryJson), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Content[len(history[2].Content)-1]["text"] != "@Sam lunch?" ||
		history[2].AdditionalKwargs["elitea_addressed"] != "people" {
		t.Fatalf("history=%s", target.ChatHistoryJson)
	}
}
