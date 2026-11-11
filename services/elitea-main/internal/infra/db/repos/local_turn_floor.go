package repos

// The floor of a desktop thread with several agents (ADR-0050 decision 7f,
// issue 1806; localturn/floor.go): the conversation's agents and where each
// runs, and the start-time checks of a later turn of a routed message.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// readLocalTurnFloor reads the conversation's floor and its agent
// participants in participant order, for a caller that is a user
// participant of it, inside the start's transaction (localturn.StartRecord.Admit).
// The agents are the `application` participants the cloud floor counts
// (readConversationFloor), with the executor of each mapping. A floor block
// that does not parse is the default floor.
func readLocalTurnFloor(
	ctx context.Context, queryer floorQueryer, schema string, actorUserID int64, conversationUUID string,
) (localturn.ConversationFloor, error) {
	var raw []byte
	err := queryer.QueryRow(ctx, fmt.Sprintf(`
SELECT conversation.meta -> 'floor'
FROM %[1]s.chat_conversations AS conversation
WHERE conversation.uuid = $1::uuid
  AND EXISTS (
      SELECT 1 FROM %[1]s.chat_participant_mapping AS mapping
      JOIN %[1]s.chat_participants AS author ON author.id = mapping.participant_id
      WHERE mapping.conversation_id = conversation.id
        AND author.entity_name = 'user'
        AND author.entity_meta ->> 'id' = ($2::bigint)::text)`, schema), conversationUUID, actorUserID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.ConversationFloor{}, localturn.ErrNotFound
	}
	if err != nil {
		return localturn.ConversationFloor{}, fmt.Errorf("local turn floor: read the conversation: %w", err)
	}
	floor := localturn.ConversationFloor{Settings: turntaking.Default}
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
       CASE WHEN mapping.entity_settings ->> 'executor' = 'cloud' THEN 'cloud' ELSE 'local' END
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'application'
WHERE conversation.uuid = $1::uuid
ORDER BY participant.id`, schema), conversationUUID)
	if err != nil {
		return localturn.ConversationFloor{}, fmt.Errorf("local turn floor: read the agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agent localturn.FloorAgent
		if err := rows.Scan(&agent.ParticipantID, &agent.Name, &agent.Executor); err != nil {
			return localturn.ConversationFloor{}, fmt.Errorf("local turn floor: scan an agent: %w", err)
		}
		floor.Agents = append(floor.Agents, agent)
	}
	return floor, rows.Err()
}

// localTurnExecutor is the executor of the answering participant's mapping:
// cloud only for an `application` participant whose entity_settings say so.
func localTurnExecutor(ctx context.Context, tx pgx.Tx, schema string, conversationID, participantID int64) (string, error) {
	var executor string
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT CASE WHEN participant.entity_name = 'application'
             AND mapping.entity_settings ->> 'executor' = 'cloud' THEN 'cloud' ELSE 'local' END
FROM %[1]s.chat_participant_mapping AS mapping
JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
WHERE mapping.conversation_id = $1 AND mapping.participant_id = $2
LIMIT 1`, schema), conversationID, participantID).Scan(&executor)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.ExecutorLocal, nil
	}
	if err != nil {
		return "", fmt.Errorf("local turn: read the executor: %w", err)
	}
	return executor, nil
}

// localTurnFloorFields are the floor columns a start writes.
type localTurnFloorFields struct {
	questionID *string
	position   int
	targets    []int32
}

// admitLocalTurnFloor answers the floor columns of a start: the routed
// order of a person's message (its own question), or the checked place of a
// later turn. A later turn must continue a routed message of the same
// caller and conversation whose first turn is committed, name the agent at
// its position, and the message must still be the conversation's newest
// routed message (ErrFloorSuperseded otherwise).
func admitLocalTurnFloor(
	ctx context.Context, tx pgx.Tx, record localturn.StartRecord, actor string, targetID int64,
) (localTurnFloorFields, error) {
	if record.FloorTurn == nil {
		if len(record.FloorTargets) == 0 {
			return localTurnFloorFields{targets: []int32{}}, nil
		}
		targets, ok := narrowIDs(record.FloorTargets)
		if !ok || record.FloorTargets[0] != targetID {
			return localTurnFloorFields{}, localturn.ErrInvalid
		}
		question := record.QuestionID
		return localTurnFloorFields{questionID: &question, targets: targets}, nil
	}
	var (
		targets   []int32
		committed bool
		newest    bool
	)
	err := tx.QueryRow(ctx, `
SELECT first.floor_targets, first.committed_at IS NOT NULL,
       NOT EXISTS (
           SELECT 1 FROM elitea_runtime.local_turn_executions AS newer
           WHERE newer.project_id = first.project_id
             AND newer.conversation_uuid = first.conversation_uuid
             AND newer.floor_question_id IS NOT NULL
             AND newer.floor_question_id <> first.question_id
             AND newer.floor_position = 0
             AND newer.started_at > first.started_at)
FROM elitea_runtime.local_turn_executions AS first
WHERE first.project_id = $1 AND first.actor_id = $2 AND first.question_id = $3::uuid
  AND first.conversation_uuid = $4::uuid AND first.floor_question_id = first.question_id
FOR SHARE`, record.ProjectID, actor, record.FloorTurn.QuestionID, record.ConversationUUID).
		Scan(&targets, &committed, &newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return localTurnFloorFields{}, localturn.ErrFloorTurn
	}
	if err != nil {
		return localTurnFloorFields{}, fmt.Errorf("local turn: read the routed message: %w", err)
	}
	position := record.FloorTurn.Position
	switch {
	case position < 1 || position >= len(targets) || int64(targets[position]) != targetID || !committed:
		return localTurnFloorFields{}, localturn.ErrFloorTurn
	case !newest:
		return localTurnFloorFields{}, localturn.ErrFloorSuperseded
	}
	question := record.FloorTurn.QuestionID
	return localTurnFloorFields{questionID: &question, position: position, targets: targets}, nil
}

func narrowIDs(ids []int64) ([]int32, bool) {
	narrowed := make([]int32, 0, len(ids))
	for _, id := range ids {
		value, ok := narrowRowID(id)
		if !ok {
			return nil, false
		}
		narrowed = append(narrowed, value)
	}
	return narrowed, true
}

func widenIDs(ids []int32) []int64 {
	if len(ids) == 0 {
		return nil
	}
	wide := make([]int64, 0, len(ids))
	for _, id := range ids {
		wide = append(wide, int64(id))
	}
	return wide
}
