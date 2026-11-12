package repos

// Issue 1892: a HITL-resumed turn re-records its answer's view in the
// transaction that resumes it, so the inspector shows what the continued
// run saw, not the start's view.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

func TestPostgresContinuationReRecordsTheAnswersView(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	conversationRaw := "10000000-0000-4000-8000-000000000031"
	questionID := "20000000-0000-4000-8000-000000000892"
	pausedExecution := "execution-view-continue-paused"
	startView := agentexecutionapp.ContextViewRecord{
		ReaderParticipantID: 21, Settings: contextview.Default, WindowTokens: 8192,
		Digest:  strings.Repeat("a", 64),
		Entries: []agentexecutionapp.ContextViewEntry{{MessageID: "00000000-0000-4000-8000-000000000101"}},
	}
	continuedView := agentexecutionapp.ContextViewRecord{
		ReaderParticipantID: 21, Settings: contextview.Default, WindowTokens: 8192,
		Digest: strings.Repeat("b", 64),
		Entries: []agentexecutionapp.ContextViewEntry{
			{MessageID: "00000000-0000-4000-8000-000000000101"},
			{MessageID: "00000000-0000-4000-8000-000000000102"},
		},
	}

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	responsePG := insertPostgresCurrentApplicationTurn(t, sqlcgen.New(tx), mustCurrentPGUUID(t, conversationRaw), questionID,
		"30000000-0000-4000-8000-000000000892", "40000000-0000-4000-8000-000000000892",
		"file two issues", pausedExecution)
	responseID := responsePG.String()
	if err := recordContextView(t.Context(), tx, responseID, &startView); err != nil {
		t.Fatal(err)
	}
	if _, err := projectRootApprovalPause(t.Context(), pgxExecutor{queryer: tx}, outputapp.ExpectedAgentExecution{
		ProjectionProjectID: "1", ExecutionID: pausedExecution, ClientMessageID: responseID,
		ClientStreamID: conversationRaw,
	}, json.RawMessage(rootLedgerPauses), false); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group SET is_streaming = FALSE,
    meta = meta || jsonb_build_object('thread_id', 'thread-view-1', 'execution_generation', $2::text,
        'hitl_interrupt', ($3::jsonb -> 0), 'hitl_interrupts', $3::jsonb)
WHERE uuid = $1`, responseID, questionID, rootLedgerPauses); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	turn := agentexecutionapp.CurrentContinueTurn{
		ProjectID: 1, ActorUserID: 11, ConversationUUID: conversationRaw, TargetParticipantID: 21,
		Kind: agentexecutionapp.CurrentRegenerationApplication, ApplicationID: 31, ApplicationVersionID: 41,
		QuestionID: questionID, ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-view-1",
		HITLDecisions: json.RawMessage(`[{"interrupt_id":"interrupt-root-1","action":"approve"},
			{"interrupt_id":"interrupt-root-2","action":"reject"}]`),
		ContextView: &continuedView,
	}
	tx, err = pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := resumeCurrentContinueTurn(t.Context(), tx, sqlcgen.New(tx),
		"execution-view-continue-resumed", turn, "manual"); err != nil {
		t.Fatal(err)
	}
	var digest string
	var entries int
	if err := tx.QueryRow(t.Context(), `
SELECT view.digest, COALESCE(frame.entry_count, jsonb_array_length(view.entries))
FROM chat_message_context_views view
LEFT JOIN chat_message_context_view_frames frame ON frame.id = view.frame_id
WHERE view.message_group_uuid = $1::uuid`, responseID).Scan(&digest, &entries); err != nil {
		t.Fatal(err)
	}
	if digest != continuedView.Digest || entries != 2 {
		t.Fatalf("the continued run's view was not recorded: digest=%s entries=%d", digest, entries)
	}
}
