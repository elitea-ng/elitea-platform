package repos

// Send a running task to the cloud and continue; take it back when done
// (ADR-0045 decision 8, #1439; shared 0224).
//
// A complete with `continue` is the admission of the cloud turn
// (agentexecution/handoff_continue.go). The admission transaction, in this
// order:
//
//  1. completes the send (completeHandoffTx): the row lock, the owner, the
//     state, the epoch and the snapshot refs, HANDING_OFF → CLOUD_IDLE and
//     the event message (stamped before the turn's messages);
//  2. writes the person's message, the answer, the execution and its
//     command, and the chain's root or its `handoff` link, as every start
//     does;
//  3. starts the cloud turn (startCloudTurnTx): CLOUD_IDLE → CLOUD_RUNNING
//     bound to the new execution, with the answer's cloud markers. The
//     worker's claim then redeems a running turn (a repeated redemption of
//     the same execution).
//
// The execution's admitted_at is read after step 1, so the claim's
// "admitted before the last state change" check holds. Any refusal rolls
// the whole transaction back: the send stays HANDING_OFF.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/handoffcontinue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// completeHandoffInAdmission completes the admission's send. The send must
// be of the turn's own conversation, and the actor must own it.
func completeHandoffInAdmission(
	ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID, actorID string,
	completion *agentexecutionapp.HandoffCompletion,
) (workspacehandoff.Handoff, error) {
	actor, err := strconv.ParseInt(actorID, 10, 64)
	if err != nil || actor <= 0 || !completion.Valid() {
		return workspacehandoff.Handoff{}, workspacehandoff.ErrInvalid
	}
	var conversation string
	err = tx.QueryRow(ctx, `
SELECT conversation_uuid::text FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND handoff_id = $2::uuid`,
		projectID, completion.HandoffID).Scan(&conversation)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && conversation != conversationUUID) {
		return workspacehandoff.Handoff{}, workspacehandoff.ErrNotFound
	}
	if err != nil {
		return workspacehandoff.Handoff{}, fmt.Errorf("workspace handoff: resolve handoff: %w", err)
	}
	row, err := completeHandoffTx(ctx, tx, workspacehandoff.Thread{ProjectID: projectID, ConversationUUID: conversation},
		workspacehandoff.CompleteRecord{
			ProjectID: projectID, HandoffID: completion.HandoffID, ActorUserID: actor, Epoch: completion.Epoch,
			HandoffRef: completion.HandoffRef, SHA: completion.SHA, TakeBackWhenDone: completion.TakeBackWhenDone,
			Message: workspacehandoff.EventMessage{Event: "handoff.complete"},
		}, true)
	if err != nil {
		return workspacehandoff.Handoff{}, err
	}
	return row.Handoff, nil
}

// startHandoffCloudTurn moves the completed send to CLOUD_RUNNING for the
// admitted execution.
func startHandoffCloudTurn(
	ctx context.Context, tx pgx.Tx, completed workspacehandoff.Handoff, executionID, responseMessageID string,
) error {
	thread := workspacehandoff.Thread{ProjectID: completed.ProjectID, ConversationUUID: completed.ConversationUUID}
	started, err := startCloudTurnTx(ctx, tx, thread, completed.Epoch, executionID, responseMessageID)
	if err != nil {
		return err
	}
	if started.Handoff.State != workspacestore.StateCloudRunning {
		return workspacehandoff.ErrStaleEpoch
	}
	return nil
}

// moveChainToCloud is the second half of a `handoff` link: the chain now
// ends in the cloud (its origin), and its later cloud continuations start as
// this turn did (grant_json.start).
func moveChainToCloud(ctx context.Context, tx pgx.Tx, root string, start *unattended.CloudStart) error {
	if start == nil {
		return errors.New("unattended chain: a handoff link needs the cloud start")
	}
	startJSON, err := json.Marshal(start)
	if err != nil {
		return fmt.Errorf("unattended chain: encode the cloud start: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.run_chains
SET origin = 'cloud', grant_json = grant_json || jsonb_build_object('start', $2::jsonb), updated_at = clock_timestamp()
WHERE root_execution_id = $1`, root, string(startJSON)); err != nil {
		return fmt.Errorf("unattended chain: move the chain to the cloud: %w", err)
	}
	return nil
}

// LatestChainLink answers the chain's newest link (its last_execution_id):
// the run a handoff continues.
func (r *RunChainsRepository) LatestChainLink(ctx context.Context, root string) (unattended.Chain, bool, error) {
	if !unattended.ValidExecutionID(root) {
		return unattended.Chain{}, false, nil
	}
	var last string
	err := r.pool.QueryRow(ctx, `SELECT last_execution_id FROM elitea_runtime.run_chains WHERE root_execution_id = $1`,
		root).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return unattended.Chain{}, false, nil
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return unattended.Chain{}, false, nil
		}
		return unattended.Chain{}, false, fmt.Errorf("unattended chain: read the latest link: %w", err)
	}
	return readRunChainLink(ctx, r.pool, last)
}

// HandoffContinueTarget reads the send's thread (the caller's own send) and
// the participant its last local turn addressed; a thread without one takes
// its agent participant, else its model participant.
func (r *WorkspaceHandoffsRepository) HandoffContinueTarget(
	ctx context.Context, projectID int64, handoffID string, actorUserID int64,
) (handoffcontinue.Target, error) {
	var (
		out   handoffcontinue.Target
		owner int64
	)
	err := r.pool.QueryRow(ctx, `
SELECT conversation_uuid::text, owner_user_id FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND handoff_id = $2::uuid`, projectID, handoffID).Scan(&out.ConversationUUID, &owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != actorUserID) {
		return handoffcontinue.Target{}, workspacehandoff.ErrNotFound
	}
	if err != nil {
		return handoffcontinue.Target{}, fmt.Errorf("workspace handoff: read the send: %w", err)
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return handoffcontinue.Target{}, workspacehandoff.ErrInvalid
	}
	var entity string
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT participant.id, participant.entity_name
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name IN ('application', 'dummy')
LEFT JOIN LATERAL (
    SELECT turn.target_participant_id FROM elitea_runtime.local_turn_executions AS turn
    WHERE turn.project_id = $2 AND turn.conversation_uuid = conversation.uuid
    ORDER BY turn.started_at DESC LIMIT 1
) AS last_turn ON TRUE
WHERE conversation.uuid = $1::uuid
ORDER BY participant.id = last_turn.target_participant_id DESC NULLS LAST,
         participant.entity_name = 'application' DESC, participant.id
LIMIT 1`, schema), out.ConversationUUID, projectID).Scan(&out.ParticipantID, &entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return handoffcontinue.Target{}, workspacehandoff.ErrNotLocalWork
	}
	if err != nil {
		return handoffcontinue.Target{}, fmt.Errorf("workspace handoff: read the thread's participant: %w", err)
	}
	out.Agent = entity == "application"
	return out, nil
}

var (
	_ workspacehandoff.CloudRunReader = (*WorkspaceHandoffsRepository)(nil)
	_ handoffcontinue.Targets         = (*WorkspaceHandoffsRepository)(nil)
	_ handoffcontinue.Chains          = (*RunChainsRepository)(nil)
)

// LastCloudRun answers the last finished cloud run of the thread's current
// cloud epoch: the cloud turns are the answers stamped executed_by
// cloud_executor and workspace_epoch = the epoch (StampCloudTurnAnswer).
// A chain with such a link answers its end state, last run and end time
// once it ended; a cloud turn that is no chain link answers from its
// execution (cancelled, failed) and its answer (failed, waiting for input,
// completed), as its run_finished does.
func (r *WorkspaceHandoffsRepository) LastCloudRun(
	ctx context.Context, handoff workspacehandoff.Handoff,
) (*workspacehandoff.CloudRun, error) {
	schema, err := tenantSchema(strconv.FormatInt(handoff.ProjectID, 10))
	if err != nil || !workspacestore.ValidUUID(handoff.ConversationUUID) || handoff.Epoch <= 0 {
		return nil, nil
	}
	var (
		run      workspacehandoff.CloudRun
		finished time.Time
	)
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
WITH answers AS MATERIALIZED (
    SELECT g.uuid::text AS message_id, g.meta
    FROM %[1]s.chat_message_group AS g
    JOIN %[1]s.chat_conversations AS c ON c.id = g.conversation_id
    WHERE c.uuid = $1::uuid
      AND g.meta ->> 'executed_by' = 'cloud_executor'
      AND g.meta ->> 'workspace_epoch' = $2::text
), runs AS MATERIALIZED (
    SELECT DISTINCT a.execution_id, answers.meta
    FROM answers
    JOIN elitea_runtime.agent_execution_jobs AS a
      ON a.client_message_id = answers.message_id
), chained AS (
    SELECT c.end_state AS outcome, c.last_execution_id AS execution_id, c.ended_at AS finished_at
    FROM elitea_runtime.run_chains AS c
    WHERE c.project_id = $3 AND c.conversation_uuid = $1::uuid AND c.end_state IS NOT NULL
      AND EXISTS (SELECT 1 FROM elitea_runtime.run_chain_links AS k
                  JOIN runs ON runs.execution_id = k.execution_id
                  WHERE k.root_execution_id = c.root_execution_id)
), standalone AS (
    SELECT CASE
               WHEN bool_or(j.state = 'CANCELLED') THEN 'cancelled'
               WHEN bool_or(j.state IN ('FAILED', 'QUARANTINED')) THEN 'failed'
               WHEN COALESCE(runs.meta -> 'is_error' = 'true'::jsonb, FALSE) THEN 'failed'
               WHEN COALESCE(runs.meta ? 'hitl_interrupt' OR runs.meta ? 'hitl_interrupts'
                             OR runs.meta ? 'authorization_requests', FALSE) THEN 'waiting'
               ELSE 'completed'
           END AS outcome,
           runs.execution_id, max(j.settled_at) AS finished_at
    FROM runs
    JOIN elitea_runtime.execution_jobs AS j ON j.execution_id = runs.execution_id
    WHERE NOT EXISTS (SELECT 1 FROM elitea_runtime.run_chain_links AS k WHERE k.execution_id = runs.execution_id)
    GROUP BY runs.execution_id, runs.meta
    HAVING bool_and(j.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'))
       AND max(j.settled_at) IS NOT NULL
)
SELECT outcome, execution_id, finished_at
FROM (SELECT * FROM chained UNION ALL SELECT * FROM standalone) AS finished
ORDER BY finished_at DESC, execution_id DESC
LIMIT 1`, schema), handoff.ConversationUUID, handoff.Epoch, handoff.ProjectID).
		Scan(&run.Outcome, &run.ExecutionID, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace handoff: read the last cloud run: %w", err)
	}
	run.FinishedAt = finished.UTC()
	return &run, nil
}
