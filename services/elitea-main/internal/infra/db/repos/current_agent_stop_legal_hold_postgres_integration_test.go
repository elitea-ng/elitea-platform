package repos

// #1507: a stop before the first output deletes the empty turn, question
// included. On a conversation under a legal hold that would take the question
// out of the record, so the stop keeps it and settles the answer empty. The
// stop itself is never refused for a hold: it is a safety control.

import (
	"context"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresCurrentAgentStopBeforeOutputKeepsTheQuestionUnderALegalHold(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		questionID   = "20000000-0000-4000-8000-000000001507"
		questionItem = "40000000-0000-4000-8000-000000001507"
		responseID   = "30000000-0000-4000-8000-000000001507"
		executionID  = "execution-stop-under-hold"
	)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	responseMessageID := insertPostgresCurrentApplicationTurn(
		t,
		sqlcgen.New(tx),
		mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		questionID,
		questionItem,
		responseID,
		"keep this question",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "RUNNING", "RUNNING",
	)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := NewRetentionPolicyRepo(pool).CreateHold(t.Context(), retention.Hold{
		Scope: retention.HoldProject, ProjectID: ptrInt64(1), Reason: "matter", CreatedBy: 1,
	}); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID:         1,
		ActorUserID:       11,
		ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
	})
	if err != nil || outcome.Deleted || outcome.Replay {
		t.Fatalf("stop under a hold: outcome=%+v err=%v, want a fresh stop that deletes nothing", outcome, err)
	}

	var questions int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1::uuid`, questionID).Scan(&questions); err != nil {
		t.Fatal(err)
	}
	if questions != 1 {
		t.Fatalf("question rows after the stop = %d, want 1: the hold must keep what the user asked", questions)
	}
	var streaming bool
	var isError *bool
	if err := pool.QueryRow(t.Context(), `
SELECT is_streaming, (meta ->> 'is_error')::boolean
FROM p_1.chat_message_group WHERE uuid = $1::uuid`, responseID).Scan(&streaming, &isError); err != nil {
		t.Fatalf("the empty answer is gone: %v", err)
	}
	if streaming || isError == nil || *isError {
		t.Fatalf("answer is_streaming=%v is_error=%v, want a settled, non-error answer", streaming, isError)
	}
	var desired string
	if err := pool.QueryRow(t.Context(), `
SELECT desired_state FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`, executionID).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if desired != "CANCELLED" {
		t.Fatalf("job desired_state = %q, want CANCELLED: the stop still stops the run", desired)
	}
}
