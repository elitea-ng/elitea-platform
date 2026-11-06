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
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/handoffcontinue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/responsepause"
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

// fenceCloudIdleThread is the Local work fence of a new question, again in
// the admission transaction: the thread's handoff row, under a share lock,
// must be CLOUD_IDLE with no take back pending. Every state change of the
// row takes its update lock, so it waits for this admission to commit, and
// the worker's claim then finds the turn admitted after the last change.
// The refusals are the resolve's: ErrCloudTurnRunning while a cloud turn
// runs, ErrLocalWorkThread for any other state.
func fenceCloudIdleThread(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string) error {
	var (
		state   string
		reclaim bool
	)
	err := tx.QueryRow(ctx, `
SELECT state, reclaim_pending FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND deleted_at IS NULL
FOR SHARE`, projectID, conversationUUID).Scan(&state, &reclaim)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return agentexecutionapp.ErrLocalWorkThread
	case err != nil:
		return fmt.Errorf("workspace handoff: check the thread: %w", err)
	case state == string(workspacestore.StateCloudIdle) && !reclaim:
		return nil
	case state == string(workspacestore.StateCloudRunning) && !reclaim:
		return agentexecutionapp.ErrCloudTurnRunning
	}
	return agentexecutionapp.ErrLocalWorkThread
}

// writeThreadRunPolicy writes the on_approval_required snapshot of a
// schedule's or a trigger's turn on a handed-off thread, over the thread's
// previous one (agentexecution.ThreadRun). The project and the conversation
// are the admission's own. A database without the table (shared 0211) has no
// snapshot, as for every unattended run.
func writeThreadRunPolicy(
	ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string,
	policy *agentexecutionapp.UnattendedRunPolicy,
) error {
	if policy == nil {
		return nil
	}
	var present bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.unattended_run_policies') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("workspace handoff: read the run policy table: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES ($1, $2::uuid, $3, $4, $5, $6)
ON CONFLICT (project_id, conversation_uuid) DO UPDATE SET
    trigger_origin = EXCLUDED.trigger_origin, version_id = EXCLUDED.version_id,
    owner_user_id = EXCLUDED.owner_user_id, on_approval_required = EXCLUDED.on_approval_required`,
		projectID, conversationUUID, policy.TriggerOrigin, policy.VersionID, policy.OwnerUserID,
		policy.OnApprovalRequired); err != nil {
		return fmt.Errorf("workspace handoff: write the run policy: %w", err)
	}
	return nil
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

// endStoppedChain ends the chain of the unattended local turn the desktop
// stopped for a send that does not continue (a complete without
// `continue`), in the complete's transaction: the thread now waits in the
// cloud for the person's next message, so the chain ends `waiting` and its
// origin becomes `cloud` (where it ended; the handoff row is CLOUD_IDLE
// in this transaction, so its run_finished counts as handed off). Only a
// chain of this caller, project and conversation that is still open ends.
// Best-effort: a chain that is not found, not open or not the caller's, and
// a failure, end nothing and never fail the send (the failure rolls back to
// a savepoint; the sweeper still ends an open chain later).
func endStoppedChain(ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread, actorUserID int64, root string) bool {
	if !unattended.ValidExecutionID(root) {
		return false
	}
	var ended bool
	err := pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
		end, err := scanChainEnd(savepoint.QueryRow(ctx, `
UPDATE elitea_runtime.run_chains AS c
SET end_state = $5, end_limit = NULL, origin = 'cloud', ended_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE c.root_execution_id = $1 AND c.owner_user_id = $2 AND c.project_id = $3
  AND c.conversation_uuid = $4::uuid AND c.end_state IS NULL`+chainEndReturning,
			root, actorUserID, thread.ProjectID, thread.ConversationUUID, unattended.EndWaiting))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unattended chain: end the stopped chain: %w", err)
		}
		notifyChainEnd(ctx, savepoint, end)
		ended = true
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "workspace handoff: the send completes without ending the stopped chain",
			"chain_root_execution_id", root, "err", err)
		return false
	}
	return ended
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
	participant, err := threadParticipant(ctx, r.pool, projectID, out.ConversationUUID)
	if err != nil {
		return handoffcontinue.Target{}, err
	}
	return participant.Target, nil
}

// threadParticipant reads the participant the thread's last local turn
// addressed; a thread without one takes its agent participant, else its
// model participant. VersionID is the agent participant's mapped version.
func threadParticipant(
	ctx context.Context, db rowQueryer, projectID int64, conversationUUID string,
) (handoffcontinue.ThreadTarget, error) {
	out := handoffcontinue.ThreadTarget{Target: handoffcontinue.Target{ConversationUUID: conversationUUID}}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrInvalid
	}
	var (
		entity  string
		version *int64
	)
	err = db.QueryRow(ctx, fmt.Sprintf(`
SELECT participant.id, participant.entity_name,
       CASE WHEN mapping.entity_settings ->> 'version_id' ~ '^[0-9]{1,18}$'
            THEN (mapping.entity_settings ->> 'version_id')::bigint END
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
LIMIT 1`, schema), conversationUUID, projectID).Scan(&out.ParticipantID, &entity, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrNotLocalWork
	}
	if err != nil {
		return handoffcontinue.ThreadTarget{}, fmt.Errorf("workspace handoff: read the thread's participant: %w", err)
	}
	out.Agent = entity == "application"
	if out.Agent && version != nil {
		out.VersionID = *version
	}
	return out, nil
}

// ThreadTarget reads a handed-off thread for a schedule or a trigger
// (ADR-0045 decision 9): the participant its next turn addresses, and
// whether a turn may start now. Idle is the Local work fence's open state
// (CLOUD_IDLE, no take back pending) with no answer of the thread
// streaming. A thread with no handoff row, an erased one, or a conversation
// that is gone answers ErrNotFound.
func (r *WorkspaceHandoffsRepository) ThreadTarget(
	ctx context.Context, projectID int64, conversationUUID string,
) (handoffcontinue.ThreadTarget, error) {
	if !workspacestore.ValidUUID(conversationUUID) {
		return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrNotFound
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrInvalid
	}
	var (
		localWork, idle bool
		conversation    *string
	)
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT c.uuid::text, COALESCE(c.source = 'local_work', FALSE),
       h.state = 'CLOUD_IDLE' AND NOT h.reclaim_pending
       AND NOT EXISTS (SELECT 1 FROM %[1]s.chat_message_group AS g
                       WHERE g.conversation_id = c.id AND g.is_streaming)
FROM elitea_runtime.workspace_handoffs AS h
LEFT JOIN %[1]s.chat_conversations AS c ON c.uuid = h.conversation_uuid
WHERE h.project_id = $1 AND h.conversation_uuid = $2::uuid AND h.deleted_at IS NULL`, schema),
		projectID, conversationUUID).Scan(&conversation, &localWork, &idle)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (conversation == nil || !localWork)) {
		return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrNotFound
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return handoffcontinue.ThreadTarget{}, workspacehandoff.ErrNotFound
		}
		return handoffcontinue.ThreadTarget{}, fmt.Errorf("workspace handoff: read the thread: %w", err)
	}
	out, err := threadParticipant(ctx, r.pool, projectID, *conversation)
	if err != nil {
		return handoffcontinue.ThreadTarget{}, err
	}
	out.Idle = idle
	return out, nil
}

// RunActive reports whether the run of an execution of the project still
// goes: its newest generation is not settled, or it is the root of a chain
// that has not ended. A schedule's overlap check reads it for the run its
// last occurrence started (last_execution_id).
func (r *WorkspaceHandoffsRepository) RunActive(ctx context.Context, projectID int64, executionID string) (bool, error) {
	if executionID == "" {
		return false, nil
	}
	var active bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
           SELECT 1 FROM elitea_runtime.run_chains AS c
           WHERE c.root_execution_id = $1 AND c.project_id = $2 AND c.end_state IS NULL)
    OR COALESCE((
           SELECT j.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED')
           FROM elitea_runtime.execution_jobs AS j
           WHERE j.execution_id = $1 AND j.resource_project_id = $2
           ORDER BY j.generation DESC LIMIT 1), FALSE)`,
		executionID, projectID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("workspace handoff: read the previous run: %w", err)
	}
	return active, nil
}

var (
	_ workspacehandoff.CloudRunReader = (*WorkspaceHandoffsRepository)(nil)
	_ handoffcontinue.Targets         = (*WorkspaceHandoffsRepository)(nil)
	_ handoffcontinue.Threads         = (*WorkspaceHandoffsRepository)(nil)
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
               WHEN COALESCE(%[2]s, FALSE) THEN 'waiting'
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
LIMIT 1`, schema, responsepause.SQL(responsepause.Runs)), handoff.ConversationUUID, strconv.FormatInt(handoff.Epoch, 10), handoff.ProjectID).
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
