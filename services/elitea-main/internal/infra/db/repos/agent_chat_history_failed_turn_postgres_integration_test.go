package repos

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPostgresApplicationChatHistoryOmitsFailedTurnAsPair(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")

	failed := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID,
		"20000000-0000-4000-8000-000000000051",
		"30000000-0000-4000-8000-000000000051",
		"40000000-0000-4000-8000-000000000051",
		"repeat this failed instruction", "execution-history-failed-application",
	)
	failPostgresCurrentAgentTurn(t, tx, failed, "partial failed output")

	succeeded := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID,
		"50000000-0000-4000-8000-000000000051",
		"60000000-0000-4000-8000-000000000051",
		"70000000-0000-4000-8000-000000000051",
		"keep this successful instruction", "execution-history-success-application",
	)
	completePostgresCurrentApplicationTurn(t, tx, succeeded, "successful answer")

	history := resolvePostgresApplicationChatHistory(
		t, queries, conversationID, "80000000-0000-4000-8000-000000000051",
	)
	assertOnlySuccessfulHistoryPair(t, history)
}

func TestPostgresAdhocChatHistoryOmitsFailedTurnAsPair(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")

	failed := insertPostgresCurrentAdhocTurn(
		t, queries, conversationID,
		"20000000-0000-4000-8000-000000000052",
		"30000000-0000-4000-8000-000000000052",
		"40000000-0000-4000-8000-000000000052",
		"repeat this failed instruction", "execution-history-failed-adhoc",
	)
	failPostgresCurrentAgentTurn(t, tx, failed, "partial failed output")

	succeeded := insertPostgresCurrentAdhocTurn(
		t, queries, conversationID,
		"50000000-0000-4000-8000-000000000052",
		"60000000-0000-4000-8000-000000000052",
		"70000000-0000-4000-8000-000000000052",
		"keep this successful instruction", "execution-history-success-adhoc",
	)
	completePostgresCurrentApplicationTurn(t, tx, succeeded, "successful answer")

	target, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, "80000000-0000-4000-8000-000000000052"),
			ConversationUuid: conversationID,
		},
	)
	if err != nil {
		t.Fatalf("resolve ad-hoc history: %v", err)
	}
	var history []postgresHistoryGroup
	if err := json.Unmarshal([]byte(target.ChatHistoryJson), &history); err != nil {
		t.Fatalf("decode ad-hoc history: %v", err)
	}
	assertOnlySuccessfulHistoryPair(t, history)
}

func failPostgresCurrentAgentTurn(
	t *testing.T,
	tx pgx.Tx,
	responseID pgtype.UUID,
	partialContent string,
) {
	t.Helper()
	completePostgresCurrentApplicationTurn(t, tx, responseID, partialContent)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET meta = meta || jsonb_build_object(
        'is_error', true,
        'error', 'The runtime operation failed.'
    )
WHERE uuid = $1`, responseID); err != nil {
		t.Fatal(err)
	}
}

func assertOnlySuccessfulHistoryPair(t *testing.T, history []postgresHistoryGroup) {
	t.Helper()
	if len(history) != 2 ||
		history[0].Role != "user" || len(history[0].Content) != 1 ||
		history[0].Content[0]["text"] != "keep this successful instruction" ||
		history[1].Role != "assistant" || len(history[1].Content) != 1 ||
		history[1].Content[0]["text"] != "successful answer" {
		t.Fatalf("history=%+v", history)
	}
}
