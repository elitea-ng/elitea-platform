package repos

// The approval ledger for static pipeline pauses (interrupt_before /
// interrupt_after) against a real PostgreSQL (ADR-0041 decision 5, #1491).
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

var staticLedgerPauseID = "pipeline-static:sha256:" + strings.Repeat("a", 64)

// pauseStaticResponse projects a static full message as the terminal
// projection does: the ledger rows and the response meta in one
// transaction. It returns the projection outcome.
func (h *interruptHarness) pauseStaticResponse(responseID string) rootPauseOutcome {
	h.t.Helper()
	executionID := h.executions[responseID]
	message, err := decodeCurrentAgentFullMessage([]byte(`"paused before tick"`), []byte(`[]`),
		[]byte(staticMessageMetadataFixture()))
	if err != nil {
		h.t.Fatal(err)
	}
	tx, err := h.pool.Begin(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(h.t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		h.t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	expected := outputapp.ExpectedAgentExecution{
		ProjectionProjectID: "1", ExecutionID: executionID, ClientMessageID: responseID,
		ClientStreamID: h.conversationUUID(),
	}
	outcome, err := projectStaticApprovalPause(h.t.Context(), executor, expected, &message)
	if err != nil {
		h.t.Fatalf("project static pause: %v", err)
	}
	var groupID int64
	if err := tx.QueryRow(h.t.Context(), `SELECT id FROM chat_message_group WHERE uuid = $1::uuid`, responseID).
		Scan(&groupID); err != nil {
		h.t.Fatal(err)
	}
	proof := string(message.PipelineStaticProof)
	if outcome == rootPauseFailed {
		proof = "null"
	}
	if _, err := tx.Exec(h.t.Context(), `
UPDATE chat_message_group SET is_streaming = FALSE, task_id = $2,
    meta = meta || jsonb_build_object('thread_id', 'thread-current-1')
        || CASE WHEN $3::jsonb = 'null'::jsonb THEN '{}'::jsonb
                ELSE jsonb_build_object('pipeline_static_v1', $3::jsonb) END
WHERE id = $1`, groupID, executionID, proof); err != nil {
		h.t.Fatal(err)
	}
	if outcome == rootPauseFailed {
		if err := finalizeStaticApprovalFailure(h.t.Context(), executor, groupID); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := tx.Commit(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
	return outcome
}

func TestApprovalLedgerStaticPauseRecordsItsPauseRow(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	if outcome := h.pauseStaticResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused"}) {
		t.Fatalf("ledger = %v", got)
	}
	if rows[0].InterruptID != staticLedgerPauseID || rows[0].Path != "root" || rows[0].Digest != "" ||
		rows[0].ToolName != "" || rows[0].Origin != "manual" {
		t.Fatalf("static pause row = %+v", rows[0])
	}
	if meta := h.responseMeta(response); meta["pipeline_static_v1"] == nil {
		t.Fatalf("an attended static pause stays continuable: %v", meta)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

// A scheduled pipeline with interrupt_before used to wait forever: now it
// takes its unattended action. skip has no refusal to answer a static wait
// with, so it fails as fail does.
func TestApprovalLedgerStaticPauseUnattendedFailsForFailAndSkip(t *testing.T) {
	for _, policy := range []string{"", "skip"} {
		name := policy
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			h := newInterruptHarness(t)
			response := h.newResponse()
			h.makeUnattended(response, "schedule")
			if policy != "" {
				h.setRunPolicy(policy)
			}
			if outcome := h.pauseStaticResponse(response); outcome != rootPauseFailed {
				t.Fatalf("outcome = %v", outcome)
			}
			rows := readApprovalLedger(t, h.pool, h.executions[response])
			if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:unattended_failed"}) {
				t.Fatalf("ledger = %v", got)
			}
			if rows[1].ActorKind != "system" || rows[1].Reason != ApprovalRequiredUnattendedCode ||
				rows[1].Origin != "schedule" {
				t.Fatalf("decision row = %+v", rows[1])
			}
			meta := h.responseMeta(response)
			if meta["error_code"] != ApprovalRequiredUnattendedCode || meta["is_error"] != true ||
				meta["pipeline_static_v1"] != nil {
				t.Fatalf("meta = %v", meta)
			}
			var queued int
			if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.unattended_approval_actions
WHERE execution_id = $1`, h.executions[response]).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("a failed static pause queues no intent: %d %v", queued, err)
			}
			assertApprovalAudit(t, h.pool, 1, rows)
		})
	}
}

func TestApprovalLedgerStaticPauseUnattendedNotifyAndWaitThenContinue(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.setRunPolicy("notify_and_wait")
	if outcome := h.pauseStaticResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "notice:unattended_waiting"}) {
		t.Fatalf("ledger = %v", got)
	}
	// The owner continues the pause: the continuation writes the decision
	// row and ends the wait intent.
	turn := agentexecutionapp.CurrentContinueTurn{
		ProjectID: 1, ActorUserID: 8, ConversationUUID: h.conversationUUID(), ResponseMessageID: response,
	}
	tx, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	paused, err := lockStaticPausedExecution(t.Context(), executor, response)
	if err != nil || paused != h.executions[response] {
		t.Fatalf("paused execution = %q, err %v", paused, err)
	}
	if err := recordStaticApprovalDecisions(t.Context(), executor, turn, paused, []string{staticLedgerPauseID}); err != nil {
		t.Fatal(err)
	}
	// A second continue of the same wait is the racing approver that lost.
	if err := recordStaticApprovalDecisions(t.Context(), executor, turn, paused,
		[]string{staticLedgerPauseID}); !errors.Is(err, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved) {
		t.Fatalf("second decision err = %v", err)
	}
	// The failed statement aborted the transaction: replay the winner alone.
	_ = tx.Rollback(context.Background())
	tx, err = h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	executor = pgxExecutor{queryer: tx}
	if err := recordStaticApprovalDecisions(t.Context(), executor, turn, paused, []string{staticLedgerPauseID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows = readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got,
		[]string{"pause:paused", "notice:unattended_waiting", "decision:approved"}) {
		t.Fatalf("ledger = %v", got)
	}
	decision := rows[2]
	if decision.Action != "continue" || decision.ActorUserID == nil || *decision.ActorUserID != 8 ||
		decision.InterruptID != staticLedgerPauseID || decision.Origin != "schedule" {
		t.Fatalf("decision row = %+v", decision)
	}
	var state string
	if err := h.pool.QueryRow(t.Context(), `SELECT state FROM elitea_runtime.unattended_approval_actions
WHERE execution_id = $1 AND interrupt_id = $2`, h.executions[response], staticLedgerPauseID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "DONE" {
		t.Fatalf("wait intent state = %s", state)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}
