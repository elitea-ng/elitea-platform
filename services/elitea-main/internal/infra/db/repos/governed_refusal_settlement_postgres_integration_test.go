package repos

import (
	"context"
	"encoding/json"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A refused governed continuation settles the paused turn as an error and
// cancels the job; unlike a stop of a turn with no output it keeps the turn
// (#1485).
func TestPostgresGovernedRefusalSettlesPausedTurnAsError(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		responseID  = "30000000-0000-4000-8000-000000001485"
		questionID  = "20000000-0000-4000-8000-000000001485"
		questionRow = "40000000-0000-4000-8000-000000001485"
		executionID = "execution-governed-refusal-hitl"
	)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	responseMessageID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID, questionRow, responseID,
		"comment on the governed issue", executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "SUCCEEDED", "RUNNING",
	)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-governed-refusal',
        'hitl_interrupt', ($2::jsonb -> 0),
        'hitl_interrupts', $2::jsonb
    )
WHERE uuid = $1`, responseMessageID,
		`[{"interrupt_id":"interrupt-governed-1","available_actions":["approve","reject"]}]`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	const reason = "This agent's access to a toolkit was revoked while it waited for your decision, so the step did not run."
	settled, err := repository.SettleGovernedRefusal(t.Context(), agentexecutionapp.GovernedRefusalSettlement{
		ProjectID: 1, ActorUserID: 11,
		ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
		Code:              "credential_grant_missing", Reason: reason,
	})
	if err != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, err)
	}

	var desiredState string
	if err := pool.QueryRow(t.Context(), `
SELECT desired_state FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`, executionID).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "CANCELLED" {
		t.Fatalf("desired_state=%q", desiredState)
	}
	var streaming bool
	var rawMeta []byte
	if err := pool.QueryRow(t.Context(), `
SELECT is_streaming, meta FROM p_1.chat_message_group WHERE uuid = $1`, responseMessageID).Scan(&streaming, &rawMeta); err != nil {
		t.Fatalf("the refused turn's answer is gone: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests"} {
		if _, ok := meta[key]; ok {
			t.Fatalf("the pause key %q survived: %s", key, rawMeta)
		}
	}
	if streaming || meta["is_error"] != true || meta["error"] != reason ||
		meta["error_code"] != agentexecutionapp.GovernedRefusalErrorCode ||
		meta["governed_refusal_code"] != "credential_grant_missing" {
		t.Fatalf("streaming=%v meta=%s", streaming, rawMeta)
	}
	var questions int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1`, mustCurrentPGUUID(t, questionID)).Scan(&questions); err != nil {
		t.Fatal(err)
	}
	if questions != 1 {
		t.Fatalf("the refused turn's question rows=%d", questions)
	}

	// Another actor finds nothing it may settle.
	settled, err = repository.SettleGovernedRefusal(t.Context(), agentexecutionapp.GovernedRefusalSettlement{
		ProjectID: 1, ActorUserID: 99,
		ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
		Code:              "credential_grant_missing", Reason: reason,
	})
	if err != nil || settled {
		t.Fatalf("another actor: settled=%v err=%v", settled, err)
	}
}
