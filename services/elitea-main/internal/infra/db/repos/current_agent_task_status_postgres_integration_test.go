package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The status read joins five tables across two schemas. A stub cannot tell a
// correct join from one that silently matches the wrong generation, so the
// whole point of this file is that the SQL runs against the real migrated
// schema.
func TestPostgresCurrentAgentTaskStatusReportsEveryLifecyclePhase(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	for _, test := range []struct {
		name          string
		responseID    string
		questionID    string
		questionItem  string
		executionID   string
		state         string
		desiredState  string
		wantState     string
		wantDesired   string
		wantSettledAt bool
	}{
		{
			name:         "admitted but not dispatched",
			responseID:   "30000000-0000-4000-8000-000000000151",
			questionID:   "20000000-0000-4000-8000-000000000151",
			questionItem: "40000000-0000-4000-8000-000000000151",
			executionID:  "execution-status-pending",
			state:        "PENDING",
			desiredState: "RUNNING",
			wantState:    "PENDING",
			wantDesired:  "RUNNING",
		},
		{
			name:         "running",
			responseID:   "30000000-0000-4000-8000-000000000152",
			questionID:   "20000000-0000-4000-8000-000000000152",
			questionItem: "40000000-0000-4000-8000-000000000152",
			executionID:  "execution-status-running",
			state:        "RUNNING",
			desiredState: "RUNNING",
			wantState:    "RUNNING",
			wantDesired:  "RUNNING",
		},
		{
			name:         "stop requested while still running",
			responseID:   "30000000-0000-4000-8000-000000000153",
			questionID:   "20000000-0000-4000-8000-000000000153",
			questionItem: "40000000-0000-4000-8000-000000000153",
			executionID:  "execution-status-stopping",
			state:        "RUNNING",
			desiredState: "CANCELLED",
			wantState:    "RUNNING",
			wantDesired:  "CANCELLED",
		},
		{
			name:          "settled",
			responseID:    "30000000-0000-4000-8000-000000000154",
			questionID:    "20000000-0000-4000-8000-000000000154",
			questionItem:  "40000000-0000-4000-8000-000000000154",
			executionID:   "execution-status-settled",
			state:         "SUCCEEDED",
			desiredState:  "RUNNING",
			wantState:     "SUCCEEDED",
			wantDesired:   "RUNNING",
			wantSettledAt: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			responseMessageID := insertPostgresCurrentAgentTaskStatusTurn(
				t, pool, test.questionID, test.questionItem, test.responseID,
				test.executionID, test.state, test.desiredState)

			repository, err := NewCurrentAgentTaskStatusRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			state, err := repository.ReadCurrentAgentTaskState(
				t.Context(),
				agentexecutionapp.CurrentAgentTaskStatusRequest{
					ProjectID: 1, ActorUserID: 11, ResponseMessageID: responseMessageID,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if state.State != test.wantState || state.DesiredState != test.wantDesired {
				t.Fatalf("state=%+v want state=%q desired=%q",
					state, test.wantState, test.wantDesired)
			}
			if (state.SettledAt != nil) != test.wantSettledAt {
				t.Fatalf("settled_at=%v want present=%v", state.SettledAt, test.wantSettledAt)
			}
		})
	}
}

// TestPostgresCurrentAgentTaskStatusRefusesAForeignActorAndAnUnknownID pins the
// ownership predicate. Both cases have to be the SAME error, so an HTTP caller
// cannot tell "this response exists but is not yours" from "no such response".
func TestPostgresCurrentAgentTaskStatusRefusesAForeignActorAndAnUnknownID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	responseMessageID := insertPostgresCurrentAgentTaskStatusTurn(
		t, pool,
		"20000000-0000-4000-8000-000000000161",
		"40000000-0000-4000-8000-000000000161",
		"30000000-0000-4000-8000-000000000161",
		"execution-status-foreign", "RUNNING", "RUNNING")

	repository, err := NewCurrentAgentTaskStatusRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]agentexecutionapp.CurrentAgentTaskStatusRequest{
		"another user": {
			ProjectID: 1, ActorUserID: 12, ResponseMessageID: responseMessageID,
		},
		"unknown response": {
			ProjectID: 1, ActorUserID: 11,
			ResponseMessageID: "30000000-0000-4000-8000-000000000999",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := repository.ReadCurrentAgentTaskState(t.Context(), request); !errors.Is(
				err, agentexecutionapp.ErrCurrentAgentTaskStatusNotFound) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

// TestPostgresCurrentAgentTaskStatusFollowsAStartedRunThroughCancellation is the
// chain the restored surface exists to serve: a started turn produces a job
// row, the poll reports it running, the cancel moves the job, and the poll
// stops answering for a projection the cancel removed.
func TestPostgresCurrentAgentTaskStatusFollowsAStartedRunThroughCancellation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const executionID = "execution-status-cancel-chain"
	responseMessageID := insertPostgresCurrentAgentTaskStatusTurn(
		t, pool,
		"20000000-0000-4000-8000-000000000171",
		"40000000-0000-4000-8000-000000000171",
		"30000000-0000-4000-8000-000000000171",
		executionID, "RUNNING", "RUNNING")

	var jobs int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id
 AND binding.generation = job.generation
WHERE binding.client_message_id = $1`, responseMessageID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("agent execution job rows=%d", jobs)
	}

	statusRepository, err := NewCurrentAgentTaskStatusRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	request := agentexecutionapp.CurrentAgentTaskStatusRequest{
		ProjectID: 1, ActorUserID: 11, ResponseMessageID: responseMessageID,
	}
	state, err := statusRepository.ReadCurrentAgentTaskState(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "RUNNING" || state.DesiredState != "RUNNING" {
		t.Fatalf("state before the stop=%+v", state)
	}

	cancelRepository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := cancelRepository.CancelCurrentAgent(
		t.Context(),
		agentexecutionapp.CurrentAgentCancelRequest{
			ProjectID: 1, ActorUserID: 11, ResponseMessageID: responseMessageID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Deleted {
		t.Fatalf("outcome=%+v", outcome)
	}

	var desiredState string
	if err := pool.QueryRow(t.Context(), `
SELECT desired_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`, executionID).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "CANCELLED" {
		t.Fatalf("desired_state=%q", desiredState)
	}

	// The stop deleted an answerless response pair, so the identifier the poll
	// takes no longer names anything. The read must say so rather than report a
	// state for a projection that is gone.
	if _, err := statusRepository.ReadCurrentAgentTaskState(t.Context(), request); !errors.Is(
		err, agentexecutionapp.ErrCurrentAgentTaskStatusNotFound) {
		t.Fatalf("error after the stop=%v", err)
	}
}

func insertPostgresCurrentAgentTaskStatusTurn(
	t *testing.T,
	pool interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	},
	questionID, questionItemID, responseID, executionID, state, desiredState string,
) string {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	// Retire the previous fixture turn. InsertCurrentApplicationTurn carries a
	// claim fence: it returns no row while the conversation still holds a
	// streaming response, so a second turn in the same conversation cannot be
	// seeded until the first one stops streaming. The production path settles
	// the flag when the run ends; the fixture does it here.
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group AS message
SET is_streaming = FALSE
FROM chat_conversations AS conversation
WHERE conversation.id = message.conversation_id
  AND conversation.uuid = $1::uuid
  AND message.is_streaming`, "10000000-0000-4000-8000-000000000031"); err != nil {
		t.Fatal(err)
	}
	responseMessageID := insertPostgresCurrentApplicationTurn(
		t,
		sqlcgen.New(tx),
		mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		questionID,
		questionItemID,
		responseID,
		"poll this run",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", state, desiredState,
	)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(responseMessageID.Bytes).String()
}
