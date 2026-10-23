package repos

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

// A FAST FOLLOW-UP TURN (#1082): user, user, assistant, assistant.
//
// Admission used to stamp the response group `clock_timestamp() + 1 second`
// (InsertCurrentAdhocTurn / InsertCurrentApplicationTurn). A turn answered in
// under a second — every mock-model turn in the chat journeys — lets the next
// question be admitted BEFORE its predecessor's response timestamp, so:
//
//   - the transcript, ordered by created_at, read Q1, Q2, A1, A2
//     (chat.multiturn.spec.ts:91, chat.history-and-delegated-input.spec.ts:121);
//   - the next turn's history window, `created_at < statement_timestamp()`,
//     left out the previous answer, so the model never saw it.
//
// The existing turn fixtures hid it: completePostgresCurrentApplicationTurn
// rewrites `created_at` when it settles a response. This test settles the
// response the way the terminal projection does — `is_streaming` only — and
// admits the second turn straight away, as the composer allows.
func TestPostgresCurrentFastFollowUpTurnKeepsQuestionAnswerOrder(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	// Conversation 2 of the seed is the ad-hoc (`dummy` participant) one.
	const conversationRaw = "10000000-0000-4000-8000-000000000032"
	conversation := mustCurrentPGUUID(t, conversationRaw)
	first := commitPostgresCurrentStreamingAdhocTurn(
		t, pool, conversation,
		"21000000-0000-4000-8000-000000000033",
		"31000000-0000-4000-8000-000000000033",
		"41000000-0000-4000-8000-000000000033",
		"first question", "execution-fast-turn-1",
	)
	if _, err := pool.Exec(t.Context(), `
WITH response_group AS (
    UPDATE p_1.chat_message_group
    SET is_streaming = FALSE,
        meta = meta || jsonb_build_object('is_error', FALSE, 'error', '')
    WHERE uuid = $1
    RETURNING id
), response_item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, response_group.id, now() AT TIME ZONE 'UTC'
    FROM response_group
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT response_item.id, 'first answer'
FROM response_item`, first); err != nil {
		t.Fatal(err)
	}

	// The next turn's history, resolved the instant the first one settled.
	var history []map[string]any
	if err := withPostgresCurrentProjectTx(t, pool, func(tx pgx.Tx) error {
		resolved, err := sqlcgen.New(tx).ResolveCurrentAdhocTurn(t.Context(), sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			QuestionID:       mustCurrentPGUUID(t, "51000000-0000-4000-8000-000000000033"),
			ConversationUuid: conversation, ProjectID: 1,
		})
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(resolved.ChatHistoryJson), &history)
	}); err != nil {
		t.Fatalf("resolve the follow-up turn: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("the follow-up turn's history must carry the first question AND its answer, got %d entries: %v",
			len(history), history)
	}

	commitPostgresCurrentStreamingAdhocTurn(
		t, pool, conversation,
		"51000000-0000-4000-8000-000000000033",
		"61000000-0000-4000-8000-000000000033",
		"71000000-0000-4000-8000-000000000033",
		"second question", "execution-fast-turn-2",
	)

	listed, err := NewConversationsRepo(pool).ListMessages(t.Context(), "1", conversationRaw,
		conversations.MessagesQuery{Limit: 50, SortBy: "created_at", SortOrder: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	roles := make([]string, 0, len(listed.Items))
	for _, item := range listed.Items {
		roles = append(roles, item.Role)
	}
	want := []string{"user", "assistant", "user", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("transcript roles = %v, want %v", roles, want)
	}
	for index := range want {
		if roles[index] != want[index] {
			t.Fatalf("transcript roles = %v, want %v: the second question sorts before the first answer", roles, want)
		}
	}
}
