package repos

// The turn-taking engine's statements (ADR-0050 decisions 7c and 7d, issue
// 1611; shared 0231). This file owns every statement on
// elitea_runtime.agent_floor_exchanges and agent_floor_turns:
//
//   - ConversationFloor: the floor settings and agents of a conversation
//     (agentexecution.FloorStore);
//   - admitFloor: inside the agent admission transaction, a new human
//     message's exchange and queued turns, or the start of a queued turn;
//   - FloorSettle / SkipFloorTurns / PendingFloorTurns: the after-settle
//     hook's store (agentexecution.FloorSettleStore) and the unattended
//     chain hook's gate;
//   - ResolveHandFloorRun / HandFloorInExchange: the hand_floor tool's sink
//     (storage.HandFloorSink).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// AgentFloorRepository is the PostgreSQL store of the turn-taking engine.
type AgentFloorRepository struct {
	pool *pgxpool.Pool
}

// NewAgentFloorRepository builds the repository.
func NewAgentFloorRepository(pool *pgxpool.Pool) (*AgentFloorRepository, error) {
	if pool == nil {
		return nil, errors.New("agent floor repository requires a pool")
	}
	return &AgentFloorRepository{pool: pool}, nil
}

var (
	_ agentexecutionapp.FloorStore       = (*AgentFloorRepository)(nil)
	_ agentexecutionapp.FloorSettleStore = (*AgentFloorRepository)(nil)
	_ storage.HandFloorSink              = (*AgentFloorRepository)(nil)
)

// floorTurnSystemKind marks the tenant system participant that authors the
// loop guard's message.
const floorTurnSystemKind = "turn_taking"

/* ── the conversation's floor ───────────────────────────────────────────── */

// ConversationFloor reads the conversation's floor settings and its agent
// participants, in participant order. A floor block that does not parse is
// the default floor: a malformed setting never stops a conversation.
func (r *AgentFloorRepository) ConversationFloor(
	ctx context.Context, projectID int64, conversationUUID string,
) (agentexecutionapp.ConversationFloor, error) {
	return readConversationFloor(ctx, r.pool, projectID, conversationUUID)
}

type floorQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readConversationFloor(
	ctx context.Context, queryer floorQueryer, projectID int64, conversationUUID string,
) (agentexecutionapp.ConversationFloor, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return agentexecutionapp.ConversationFloor{}, err
	}
	var raw []byte
	err = queryer.QueryRow(ctx, fmt.Sprintf(`SELECT meta -> 'floor' FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema),
		conversationUUID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ConversationFloor{Settings: turntaking.Default}, nil
	}
	if err != nil {
		return agentexecutionapp.ConversationFloor{}, fmt.Errorf("agent floor: read the conversation: %w", err)
	}
	floor := agentexecutionapp.ConversationFloor{Settings: turntaking.Default}
	if len(raw) > 0 {
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			if settings, parseErr := turntaking.Parse(decoded); parseErr == nil {
				floor.Settings = settings
			}
		}
	}
	rows, err := queryer.Query(ctx, fmt.Sprintf(`
SELECT participant.id,
       COALESCE(NULLIF(btrim(mapping.persona ->> 'display_name'), ''),
                NULLIF(btrim(participant.meta ->> 'name'), ''),
                NULLIF(btrim(participant.entity_meta ->> 'name'), ''), ''),
       COALESCE(NULLIF(btrim(mapping.persona ->> 'role'), ''), '')
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'application'
WHERE conversation.uuid = $1::uuid
ORDER BY participant.id`, schema), conversationUUID)
	if err != nil {
		return agentexecutionapp.ConversationFloor{}, fmt.Errorf("agent floor: read the agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agent agentexecutionapp.FloorAgent
		if err := rows.Scan(&agent.ParticipantID, &agent.Name, &agent.Role); err != nil {
			return agentexecutionapp.ConversationFloor{}, fmt.Errorf("agent floor: scan an agent: %w", err)
		}
		floor.Agents = append(floor.Agents, agent)
	}
	return floor, rows.Err()
}

/* ── admission ──────────────────────────────────────────────────────────── */

// admitFloor writes the turn's turn-taking rows in the admission
// transaction, after the turn: a new human message's exchange (which
// supersedes the QUEUED turns of the conversation's earlier exchanges) and
// its queued addressed agents, or the QUEUED -> STARTED move of the floor
// turn this admission starts.
func admitFloor(
	ctx context.Context, tx pgx.Tx, turn agentexecutionapp.CurrentApplicationTurn,
	floor *agentexecutionapp.FloorAdmission, executionID string,
) error {
	if floor == nil {
		return nil
	}
	if floor.StartTurnQuestionID != "" {
		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_turns
SET state = 'STARTED', execution_id = $4, updated_at = clock_timestamp()
WHERE turn_question_id = $1::uuid AND project_id = $2 AND conversation_uuid = $3::uuid
  AND to_participant_id = $5 AND state = 'QUEUED'`,
			floor.StartTurnQuestionID, turn.ProjectID, turn.ConversationUUID, executionID, turn.TargetParticipantID)
		if err != nil {
			return fmt.Errorf("agent floor: start the queued turn: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return agentexecutionapp.ErrFloorTurnNotQueued
		}
		return nil
	}
	exchange := floor.Exchange
	if exchange == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_turns
SET state = 'SKIPPED', skip_code = 'superseded', updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id <> $3::uuid AND state = 'QUEUED'`,
		turn.ProjectID, turn.ConversationUUID, exchange.QuestionID); err != nil {
		return fmt.Errorf("agent floor: supersede the earlier turns: %w", err)
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_floor_exchanges (
    project_id, conversation_uuid, question_id, owner_user_id, trigger_origin, first_participant_id)
VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6)
ON CONFLICT DO NOTHING`, turn.ProjectID, turn.ConversationUUID, exchange.QuestionID, exchange.OwnerUserID,
		floorTriggerOrigin(exchange.TriggerOrigin), exchange.FirstParticipantID)
	if err != nil {
		return fmt.Errorf("agent floor: write the exchange: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	for index, to := range exchange.Queued {
		position := index + 1
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_floor_turns (
    project_id, conversation_uuid, question_id, position, turn_question_id, to_participant_id, cause, state)
VALUES ($1, $2::uuid, $3::uuid, $4, $5::uuid, $6, 'human_message', 'QUEUED')`,
			turn.ProjectID, turn.ConversationUUID, exchange.QuestionID, position,
			agentexecutionapp.FloorTurnQuestionID(exchange.QuestionID, position), to); err != nil {
			return fmt.Errorf("agent floor: queue an addressed agent: %w", err)
		}
	}
	return nil
}

// floorTriggerOrigin is the exchange's stored origin: manual or api.
func floorTriggerOrigin(origin string) string {
	if origin == "api" {
		return origin
	}
	return "manual"
}

/* ── the exchange of a run ──────────────────────────────────────────────── */

// floorRun is what the floor reads of an agent execution.
type floorRun struct {
	projectID        int64
	participantID    int64
	conversationUUID string
	generation       string
	responseID       string
}

func readFloorRun(ctx context.Context, queryer rowQueryer, executionID string) (floorRun, bool, error) {
	var run floorRun
	var participant *int32
	var project int32
	err := queryer.QueryRow(ctx, `
SELECT job.projection_project_id, binding.participant_id, binding.client_stream_id,
       binding.client_execution_generation, binding.client_message_id
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
WHERE job.execution_id = $1 AND job.capability_id = 'agent.execute.application.v1'
ORDER BY job.generation DESC LIMIT 1`, executionID).Scan(
		&project, &participant, &run.conversationUUID, &run.generation, &run.responseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return floorRun{}, false, nil
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return floorRun{}, false, nil
		}
		return floorRun{}, false, fmt.Errorf("agent floor: read the run: %w", err)
	}
	if participant == nil || *participant <= 0 || project <= 0 {
		return floorRun{}, false, nil
	}
	run.projectID, run.participantID = int64(project), int64(*participant)
	return run, true, nil
}

// floorExchangeRow is one exchange, locked.
type floorExchangeRow struct {
	questionID    string
	ownerUserID   int64
	triggerOrigin string
	first         int64
	stopped       string
	newest        bool
}

// lockRunExchange locks the exchange the run is a turn of: the exchange of
// its question (its first turn), or the one with a turn of its execution.
// A run of an unattended chain is a turn of the exchange of any run of its
// chain: a step-limit or output-limit continuation of a turn continues that
// turn.
func lockRunExchange(ctx context.Context, tx pgx.Tx, run floorRun, executionID string) (floorExchangeRow, bool, error) {
	var row floorExchangeRow
	var stopped *string
	err := tx.QueryRow(ctx, `
WITH runs AS (
    SELECT $4::text AS execution_id
    UNION
    SELECT sibling.execution_id
    FROM elitea_runtime.run_chain_links AS self
    JOIN elitea_runtime.run_chain_links AS sibling ON sibling.root_execution_id = self.root_execution_id
    WHERE self.execution_id = $4
), facts AS (
    SELECT binding.execution_id, binding.client_execution_generation AS question, binding.participant_id
    FROM runs
    JOIN elitea_runtime.agent_execution_jobs AS binding ON binding.execution_id = runs.execution_id
    WHERE binding.participant_id = $3
)
SELECT e.question_id::text, e.owner_user_id, e.trigger_origin, e.first_participant_id, e.stopped,
       NOT EXISTS (SELECT 1 FROM elitea_runtime.agent_floor_exchanges AS newer
                   WHERE newer.project_id = e.project_id AND newer.conversation_uuid = e.conversation_uuid
                     AND newer.created_at > e.created_at)
FROM elitea_runtime.agent_floor_exchanges AS e
WHERE e.project_id = $1 AND e.conversation_uuid = $2::uuid
  AND (EXISTS (SELECT 1 FROM facts
               WHERE facts.question = e.question_id::text AND facts.participant_id = e.first_participant_id)
       OR EXISTS (SELECT 1 FROM elitea_runtime.agent_floor_turns AS t
                  JOIN facts ON facts.execution_id = t.execution_id
                  WHERE t.project_id = e.project_id AND t.conversation_uuid = e.conversation_uuid
                    AND t.question_id = e.question_id))
ORDER BY e.created_at DESC
LIMIT 1
FOR UPDATE OF e`, run.projectID, run.conversationUUID, run.participantID, executionID).Scan(
		&row.questionID, &row.ownerUserID, &row.triggerOrigin, &row.first, &stopped, &row.newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return floorExchangeRow{}, false, nil
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return floorExchangeRow{}, false, nil
		}
		return floorExchangeRow{}, false, fmt.Errorf("agent floor: lock the exchange: %w", err)
	}
	if stopped != nil {
		row.stopped = *stopped
	}
	return row, true, nil
}

/* ── after settle ───────────────────────────────────────────────────────── */

// FloorSettle marks a settled run's floor turn DONE and answers what the
// floor hook needs: the exchange, the run's outcome facts, whether another
// turn of the exchange runs, and its next QUEUED turn. It answers false for
// a run that is no turn of an exchange.
func (r *AgentFloorRepository) FloorSettle(ctx context.Context, executionID string) (agentexecutionapp.FloorSettleView, bool, error) {
	var view agentexecutionapp.FloorSettleView
	found := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		view, found = agentexecutionapp.FloorSettleView{}, false
		run, ok, err := readFloorRun(ctx, tx, executionID)
		if err != nil || !ok {
			return err
		}
		exchange, ok, err := lockRunExchange(ctx, tx, run, executionID)
		if err != nil || !ok {
			return err
		}
		found = true
		view.Exchange = agentexecutionapp.FloorExchangeKey{
			ProjectID: run.projectID, ConversationUUID: run.conversationUUID, QuestionID: exchange.questionID,
		}
		view.OwnerUserID, view.TriggerOrigin = exchange.ownerUserID, exchange.triggerOrigin
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_turns SET state = 'DONE', updated_at = clock_timestamp()
WHERE execution_id = $1 AND state = 'STARTED'`, executionID); err != nil {
			return fmt.Errorf("agent floor: finish the turn: %w", err)
		}
		facts, err := readResponseFacts(ctx, tx, run.projectID, run.responseID, executionID)
		if err != nil {
			return err
		}
		view.Paused, view.IsError = facts.Paused, facts.IsError
		view.StepLimitReached = facts.StepLimitReached
		view.OutputLimitReached = facts.OutputLimitReached && facts.OutputLimitSequence > 0
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.agent_floor_turns
               WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid
                 AND state = 'STARTED')`, run.projectID, run.conversationUUID, exchange.questionID).Scan(&view.Busy); err != nil {
			return fmt.Errorf("agent floor: read the running turns: %w", err)
		}
		if chain, linked, err := readRunChainLink(ctx, tx, executionID); err != nil {
			return err
		} else if linked {
			view.Chain = &chain
		}
		if !exchange.newest || exchange.stopped != "" {
			return nil
		}
		next, ok, err := readNextFloorTurn(ctx, tx, view.Exchange)
		if err != nil || !ok {
			return err
		}
		view.Next = &next
		return nil
	})
	if err != nil {
		return agentexecutionapp.FloorSettleView{}, false, err
	}
	return view, found, nil
}

func readNextFloorTurn(
	ctx context.Context, tx pgx.Tx, key agentexecutionapp.FloorExchangeKey,
) (agentexecutionapp.QueuedFloorTurn, bool, error) {
	var next agentexecutionapp.QueuedFloorTurn
	var from *int32
	var message *string
	var to int32
	err := tx.QueryRow(ctx, `
SELECT position, turn_question_id::text, to_participant_id, from_participant_id, message, cause
FROM elitea_runtime.agent_floor_turns
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid AND state = 'QUEUED'
ORDER BY position LIMIT 1`, key.ProjectID, key.ConversationUUID, key.QuestionID).Scan(
		&next.Position, &next.TurnQuestionID, &to, &from, &message, &next.Cause)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.QueuedFloorTurn{}, false, nil
	}
	if err != nil {
		return agentexecutionapp.QueuedFloorTurn{}, false, fmt.Errorf("agent floor: read the next turn: %w", err)
	}
	next.ToParticipantID = int64(to)
	if from != nil {
		next.FromParticipantID = int64(*from)
	}
	if message != nil {
		next.Message = *message
	}
	if next.FromParticipantID > 0 {
		floor, err := readConversationFloor(ctx, tx, key.ProjectID, key.ConversationUUID)
		if err != nil {
			return agentexecutionapp.QueuedFloorTurn{}, false, err
		}
		for _, agent := range floor.Agents {
			if agent.ParticipantID == next.FromParticipantID {
				next.FromName = agent.Name
			}
		}
	}
	return next, true, nil
}

// SkipFloorTurns skips the exchange's QUEUED turns with code.
func (r *AgentFloorRepository) SkipFloorTurns(ctx context.Context, key agentexecutionapp.FloorExchangeKey, code string) error {
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_turns
SET state = 'SKIPPED', skip_code = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid AND state = 'QUEUED'`,
		key.ProjectID, key.ConversationUUID, key.QuestionID, code); err != nil {
		return fmt.Errorf("agent floor: skip the queued turns: %w", err)
	}
	return nil
}

// PendingFloorTurns reports whether the conversation still owes a floor
// turn other than the settling run's own: a QUEUED turn, or a STARTED turn
// of another execution. The unattended chain hook does not end a chain as
// completed while it does. The after-settle hooks run concurrently, so the
// floor hook may already have moved the next turn QUEUED -> STARTED when
// this runs; counting STARTED turns makes the answer the same either way.
func (r *AgentFloorRepository) PendingFloorTurns(
	ctx context.Context, projectID int64, conversationUUID, settlingExecutionID string,
) (bool, error) {
	var pending bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.agent_floor_turns
               WHERE project_id = $1 AND conversation_uuid = $2::uuid
                 AND (state = 'QUEUED' OR (state = 'STARTED' AND execution_id <> $3)))`,
		projectID, conversationUUID, settlingExecutionID).Scan(&pending)
	if err != nil {
		if undefinedColumnOrTable(err) {
			return false, nil
		}
		return false, fmt.Errorf("agent floor: read the pending turns: %w", err)
	}
	return pending, nil
}

/* ── hand_floor ─────────────────────────────────────────────────────────── */

// ResolveHandFloorRun reads the claimed run of a hand_floor call. The
// caller authorized the claim; projectID is the project it resolved.
func (r *AgentFloorRepository) ResolveHandFloorRun(
	ctx context.Context, projectID int64, executionID string, generation uint64,
) (storage.HandFloorRun, error) {
	if generation == 0 || generation > 1<<63-1 {
		return storage.HandFloorRun{}, storage.ErrContentUnauthorized
	}
	var participant *int32
	var stream string
	err := r.pool.QueryRow(ctx, `
SELECT binding.participant_id, binding.client_stream_id
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
WHERE job.execution_id = $1 AND job.generation = $2 AND job.resource_project_id = $3`,
		executionID, int64(generation), projectID).Scan(&participant, &stream)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.HandFloorRun{}, storage.ErrContentUnauthorized
	}
	if err != nil {
		return storage.HandFloorRun{}, fmt.Errorf("agent floor: read the claimed run: %w", err)
	}
	if participant == nil || *participant <= 0 {
		// A model chat has no floor.
		return storage.HandFloorRun{}, storage.ErrContentRejected
	}
	return storage.HandFloorRun{
		ProjectID: projectID, ExecutionID: executionID, ConversationUUID: stream,
		ParticipantID: int64(*participant),
	}, nil
}

// HandFloorInExchange runs decide on the run's exchange under its row lock
// and applies the write: queue the handed agents, or stop the exchange (the
// stop code on the exchange, the QUEUED turns skipped, the loop guard's
// system message, and the message's unattended chain ended limit_reached).
// A run that is no turn of the conversation's newest exchange is
// ErrContentRejected.
func (r *AgentFloorRepository) HandFloorInExchange(
	ctx context.Context, run storage.HandFloorRun,
	decide func(storage.FloorExchangeView) (storage.FloorHandWrite, error),
) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		source, ok, err := readFloorRun(ctx, tx, run.ExecutionID)
		if err != nil {
			return err
		}
		if !ok || source.projectID != run.ProjectID || source.participantID != run.ParticipantID {
			return storage.ErrContentRejected
		}
		exchange, ok, err := lockRunExchange(ctx, tx, source, run.ExecutionID)
		if err != nil {
			return err
		}
		if !ok || !exchange.newest {
			return storage.ErrContentRejected
		}
		floor, err := readConversationFloor(ctx, tx, run.ProjectID, run.ConversationUUID)
		if err != nil {
			return err
		}
		view := storage.FloorExchangeView{
			Exchange: turntaking.Exchange{First: exchange.first}, Agents: floor.Agents, Stopped: exchange.stopped,
		}
		rows, err := tx.Query(ctx, `
SELECT COALESCE(from_participant_id, 0), to_participant_id, position
FROM elitea_runtime.agent_floor_turns
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid AND state <> 'SKIPPED'
ORDER BY position`, run.ProjectID, run.ConversationUUID, exchange.questionID)
		if err != nil {
			return fmt.Errorf("agent floor: read the exchange's turns: %w", err)
		}
		for rows.Next() {
			var from, to int64
			var at int
			if err := rows.Scan(&from, &to, &at); err != nil {
				rows.Close()
				return fmt.Errorf("agent floor: scan a turn: %w", err)
			}
			view.Exchange.Turns = append(view.Exchange.Turns, turntaking.Turn{From: from, To: to})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("agent floor: read the exchange's turns: %w", err)
		}
		// The next position follows every turn, a skipped one included.
		position := 0
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(max(position), 0) FROM elitea_runtime.agent_floor_turns
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid`,
			run.ProjectID, run.ConversationUUID, exchange.questionID).Scan(&position); err != nil {
			return fmt.Errorf("agent floor: read the last position: %w", err)
		}
		write, err := decide(view)
		if err != nil {
			return err
		}
		if write.Stop != "" {
			return stopFloorExchange(ctx, tx, run, exchange.questionID, write)
		}
		for _, to := range write.Queue {
			position++
			if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_floor_turns (
    project_id, conversation_uuid, question_id, position, turn_question_id,
    from_participant_id, to_participant_id, cause, message, state)
VALUES ($1, $2::uuid, $3::uuid, $4, $5::uuid, $6, $7, 'agent_mention', $8, 'QUEUED')`,
				run.ProjectID, run.ConversationUUID, exchange.questionID, position,
				agentexecutionapp.FloorTurnQuestionID(exchange.questionID, position),
				run.ParticipantID, to, write.Message); err != nil {
				return fmt.Errorf("agent floor: queue a handed agent: %w", err)
			}
		}
		return nil
	})
}

func stopFloorExchange(ctx context.Context, tx pgx.Tx, run storage.HandFloorRun, questionID string, write storage.FloorHandWrite) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_exchanges SET stopped = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid AND stopped IS NULL`,
		run.ProjectID, run.ConversationUUID, questionID, write.Stop); err != nil {
		return fmt.Errorf("agent floor: stop the exchange: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_floor_turns
SET state = 'SKIPPED', skip_code = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id = $3::uuid AND state = 'QUEUED'`,
		run.ProjectID, run.ConversationUUID, questionID, write.Stop); err != nil {
		return fmt.Errorf("agent floor: skip the queued turns: %w", err)
	}
	if err := appendFloorSystemMessage(ctx, tx, run.ProjectID, run.ConversationUUID, write.Stop, write.StopText); err != nil {
		return err
	}
	chain, linked, err := readRunChainLink(ctx, tx, run.ExecutionID)
	if err != nil {
		return err
	}
	if linked && chain.EndState == "" {
		if _, err := endRunChain(ctx, tx, chain.RootExecutionID, unattended.EndLimitReached, write.Stop); err != nil {
			return err
		}
	}
	return nil
}

// appendFloorSystemMessage writes the loop guard's message into the
// conversation, authored by the tenant's turn-taking system participant
// (as appendHandoffEventAt writes a handoff's).
func appendFloorSystemMessage(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID, stopped, text string) error {
	if text == "" {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return err
	}
	var conversationID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema),
		conversationUUID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent floor: system message conversation: %w", err)
	}
	var participantID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = 'system' AND entity_meta ->> 'kind' = $1
ORDER BY id LIMIT 1`, schema), floorTurnSystemKind).Scan(&participantID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'system', jsonb_build_object('kind', $1::text), '{}'::json)
RETURNING id`, schema), floorTurnSystemKind).Scan(&participantID)
	}
	if err != nil {
		return fmt.Errorf("agent floor: system participant: %w", err)
	}
	meta, _ := json.Marshal(map[string]any{"turn_taking": "stopped", "stopped": stopped})
	var groupID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
VALUES (gen_random_uuid(), $1, $2, $3::jsonb, FALSE, GREATEST(
    clock_timestamp() AT TIME ZONE 'UTC',
    (SELECT max(created_at) + interval '1 millisecond'
     FROM %[1]s.chat_message_group WHERE conversation_id = $2)))
RETURNING id`, schema), participantID, conversationID, string(meta)).Scan(&groupID); err != nil {
		return fmt.Errorf("agent floor: system message: %w", err)
	}
	return insertLocalTurnText(ctx, tx, schema, groupID, text)
}
