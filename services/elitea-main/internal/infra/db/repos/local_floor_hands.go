package repos

// Local hand-offs and consults (ADR-0061 decisions 3c and 3e, issues 3063
// and 3065; shared 0282; localturn/handoff.go and consult.go). This file
// owns every statement on elitea_runtime.local_floor_hands and
// local_turn_executions.floor_stopped:
//
//   - HandLocalFloor: a live local turn's hand_floor call, decided under a
//     row lock on the routed message's first local turn;
//   - SkipLocalFloorHands: a person's Stop exchange and Skip;
//   - admitLocalHand / startLocalHand: a handed turn's start;
//   - readLocalHandOf / writeLocalConsults: a handed turn's and a
//     consulting turn's commit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

var _ localturn.HandStore = (*LocalTurnsRepo)(nil)

// newestLocalMessageSQL is true when no newer person's message (the first
// turn of another routed message) started in the conversation of `first`.
const newestLocalMessageSQL = `NOT EXISTS (
    SELECT 1 FROM elitea_runtime.local_turn_executions AS newer
    WHERE newer.project_id = first.project_id
      AND newer.conversation_uuid = first.conversation_uuid
      AND newer.floor_question_id IS NOT NULL
      AND newer.floor_question_id <> first.question_id
      AND newer.floor_position = 0
      AND newer.started_at > first.started_at)`

// localHandingTurn is the calling turn of a hand_floor call.
type localHandingTurn struct {
	participant  int64
	conversation string
	question     string
}

// lockLocalHandingTurn reads the calling turn FOR UPDATE (its commit takes
// the same lock, so no hand-off lands after the commit) and answers it with
// the routed message it belongs to. FOR UPDATE, not FOR SHARE: the caller is
// often the message's first turn, which HandLocalFloor then locks FOR
// UPDATE, and two concurrent calls upgrading a shared lock on one row
// deadlock. The order is always the calling turn, then the first turn;
// admission takes the first turn alone.
func lockLocalHandingTurn(ctx context.Context, tx pgx.Tx, scope localturn.HandScope) (localHandingTurn, error) {
	var (
		turn               localHandingTurn
		executor           string
		floorOf            *string
		committed, expired bool
		credential         localturn.Credential
	)
	err := tx.QueryRow(ctx, `
SELECT target_participant_id, executor, conversation_uuid::text, floor_question_id::text,
       committed_at IS NOT NULL, expires_at <= clock_timestamp(), token_id, native_client_id
FROM elitea_runtime.local_turn_executions
WHERE execution_id = $1 AND project_id = $2 AND actor_id = $3
FOR UPDATE`, scope.ExecutionID, scope.ProjectID, strconv.FormatInt(scope.ActorUserID, 10)).
		Scan(&turn.participant, &executor, &turn.conversation, &floorOf, &committed, &expired,
			&credential.TokenID, &credential.NativeClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return localHandingTurn{}, localturn.ErrNotFound
	}
	if err != nil {
		return localHandingTurn{}, fmt.Errorf("local hand-off: read the turn: %w", err)
	}
	switch {
	case credential != scope.Credential:
		return localHandingTurn{}, localturn.ErrNotFound
	case committed || expired:
		return localHandingTurn{}, localturn.ErrNotLive
	case executor == localturn.ExecutorCloud:
		// A cloud member is a leaf in v1 (ADR-0061 owner decision 3).
		return localHandingTurn{}, localturn.ErrFloorTurn
	}
	if floorOf != nil {
		turn.question = *floorOf
		return turn, nil
	}
	// A handed turn belongs to the message of the hand-off it started.
	err = tx.QueryRow(ctx, `
SELECT question_id::text FROM elitea_runtime.local_floor_hands
WHERE started_execution_id = $1 AND project_id = $2`, scope.ExecutionID, scope.ProjectID).Scan(&turn.question)
	if errors.Is(err, pgx.ErrNoRows) {
		return localHandingTurn{}, localturn.ErrFloorTurn
	}
	if err != nil {
		return localHandingTurn{}, fmt.Errorf("local hand-off: read the handed turn: %w", err)
	}
	return turn, nil
}

// HandLocalFloor implements localturn.HandStore.
func (r *LocalTurnsRepo) HandLocalFloor(
	ctx context.Context, scope localturn.HandScope, decide func(localturn.HandView) localturn.HandWrite,
) ([]string, error) {
	schema, err := tenantSchema(strconv.FormatInt(scope.ProjectID, 10))
	if err != nil {
		return nil, localturn.ErrInvalid
	}
	var ids []string
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		ids = nil
		turn, err := lockLocalHandingTurn(ctx, tx, scope)
		if err != nil {
			return err
		}
		actor := strconv.FormatInt(scope.ActorUserID, 10)
		var (
			targets []int32
			stopped string
			newest  bool
		)
		err = tx.QueryRow(ctx, `
SELECT first.floor_targets, COALESCE(first.floor_stopped, ''), `+newestLocalMessageSQL+`
FROM elitea_runtime.local_turn_executions AS first
WHERE first.project_id = $1 AND first.actor_id = $2 AND first.question_id = $3::uuid
  AND first.conversation_uuid = $4::uuid AND first.floor_question_id = first.question_id
FOR UPDATE OF first`, scope.ProjectID, actor, turn.question, turn.conversation).Scan(&targets, &stopped, &newest)
		if errors.Is(err, pgx.ErrNoRows) {
			return localturn.ErrFloorTurn
		}
		if err != nil {
			return fmt.Errorf("local hand-off: lock the message: %w", err)
		}
		if !newest {
			return localturn.ErrFloorSuperseded
		}
		view, err := readLocalHandView(ctx, tx, schema, scope, turn, targets)
		if err != nil {
			return err
		}
		view.Stopped = stopped
		write := decide(view)
		if write.Stop != "" {
			return stopLocalExchange(ctx, tx, scope.ProjectID, actor, turn, write)
		}
		ids, err = queueLocalHands(ctx, tx, scope, turn, write)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// readLocalHandView builds the guard's input: the routed targets, the
// message's hand-offs not skipped, and its committed consults.
func readLocalHandView(
	ctx context.Context, tx pgx.Tx, schema string, scope localturn.HandScope, turn localHandingTurn, targets []int32,
) (localturn.HandView, error) {
	floor, err := readLocalTurnFloor(ctx, tx, schema, scope.ProjectID, scope.ActorUserID, turn.conversation)
	if err != nil {
		return localturn.HandView{}, err
	}
	view := localturn.HandView{From: turn.participant}
	member := false
	for _, agent := range floor.Agents {
		view.Agents = append(view.Agents, turntaking.Agent{ParticipantID: agent.ParticipantID, Name: agent.Name})
		member = member || agent.ParticipantID == turn.participant
	}
	if !member || len(targets) == 0 {
		return localturn.HandView{}, localturn.ErrFloorTurn
	}
	view.Exchange.First = int64(targets[0])
	for _, to := range targets[1:] {
		view.Exchange.Turns = append(view.Exchange.Turns, turntaking.Turn{To: int64(to)})
	}
	pairs := func(query string, args ...any) ([]turntaking.Turn, error) {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("local hand-off: read the exchange: %w", err)
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (turntaking.Turn, error) {
			var from, to int64
			err := row.Scan(&from, &to)
			return turntaking.Turn{From: from, To: to}, err
		})
	}
	hands, err := pairs(`
SELECT from_participant_id, to_participant_id FROM elitea_runtime.local_floor_hands
WHERE project_id = $1 AND question_id = $2::uuid AND status <> 'SKIPPED'
ORDER BY ord`, scope.ProjectID, turn.question)
	if err != nil {
		return localturn.HandView{}, err
	}
	view.Exchange.Turns = append(view.Exchange.Turns, hands...)
	view.Exchange.Consults, err = pairs(fmt.Sprintf(`
SELECT message.author_participant_id, COALESCE(message.addressed_to[1], 0)
FROM %[1]s.chat_message_group AS message
JOIN %[1]s.chat_conversations AS conversation ON conversation.id = message.conversation_id
WHERE conversation.uuid = $1::uuid AND message.cause = 'delegation' AND message.reply_to_id IS NULL
  AND message.meta -> '%[2]s' ->> 'question_id' = $2
ORDER BY message.id`, schema, localturn.ConsultMetaKey), turn.conversation, turn.question)
	if err != nil {
		return localturn.HandView{}, err
	}
	return view, nil
}

// stopLocalExchange records a guard's stop once: the code on the message's
// first turn, every QUEUED hand-off skipped with it, and the system message.
func stopLocalExchange(
	ctx context.Context, tx pgx.Tx, projectID int64, actor string, turn localHandingTurn, write localturn.HandWrite,
) error {
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.local_turn_executions SET floor_stopped = $4
WHERE project_id = $1 AND actor_id = $2 AND question_id = $3::uuid
  AND floor_question_id = question_id AND floor_stopped IS NULL`, projectID, actor, turn.question, write.Stop)
	if err != nil {
		return fmt.Errorf("local hand-off: stop the exchange: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // stopped already: its message is written
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.local_floor_hands
SET status = 'SKIPPED', skip_code = $3, updated_at = clock_timestamp()
WHERE project_id = $1 AND question_id = $2::uuid AND status = 'QUEUED'`, projectID, turn.question, write.Stop); err != nil {
		return fmt.Errorf("local hand-off: skip the queued hand-offs: %w", err)
	}
	return appendFloorSystemMessage(ctx, tx, projectID, turn.conversation, write.Stop, write.StopText)
}

func queueLocalHands(
	ctx context.Context, tx pgx.Tx, scope localturn.HandScope, turn localHandingTurn, write localturn.HandWrite,
) ([]string, error) {
	if len(write.Queue) == 0 {
		return nil, nil
	}
	ord := 0
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(max(ord), 0) FROM elitea_runtime.local_floor_hands WHERE project_id = $1 AND question_id = $2::uuid`,
		scope.ProjectID, turn.question).Scan(&ord); err != nil {
		return nil, fmt.Errorf("local hand-off: read the last hand-off: %w", err)
	}
	ids := make([]string, 0, len(write.Queue))
	for _, to := range write.Queue {
		ord++
		var id string
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.local_floor_hands (
    project_id, conversation_uuid, question_id, from_execution_id, from_participant_id, to_participant_id, ord, message)
VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)
RETURNING id::text`, scope.ProjectID, turn.conversation, turn.question, scope.ExecutionID, turn.participant,
			to, ord, write.Message).Scan(&id); err != nil {
			return nil, fmt.Errorf("local hand-off: queue an agent: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SkipLocalFloorHands implements localturn.HandStore.
func (r *LocalTurnsRepo) SkipLocalFloorHands(
	ctx context.Context, projectID, actorUserID int64, questionID string, handIDs []string, code string,
) ([]string, error) {
	var owned bool
	if err := r.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.local_turn_executions
               WHERE project_id = $1 AND actor_id = $2 AND question_id = $3::uuid
                 AND floor_question_id = question_id)`,
		projectID, strconv.FormatInt(actorUserID, 10), questionID).Scan(&owned); err != nil {
		return nil, fmt.Errorf("local hand-off: read the message: %w", err)
	}
	if !owned {
		return nil, localturn.ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `
UPDATE elitea_runtime.local_floor_hands
SET status = 'SKIPPED', skip_code = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND question_id = $2::uuid AND id = ANY($3::uuid[]) AND status = 'QUEUED'
RETURNING id::text`, projectID, questionID, handIDs, code)
	if err != nil {
		return nil, fmt.Errorf("local hand-off: skip: %w", err)
	}
	skipped, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("local hand-off: skip: %w", err)
	}
	if skipped == nil {
		skipped = []string{}
	}
	return skipped, nil
}

// admitLocalHand checks a handed turn's start: the hand-off of a message of
// this caller and conversation, QUEUED (or STARTED by this same start: a
// replay), handed to the turn's agent, by a turn that is committed, while
// the message is the conversation's newest. The hand-off row stays locked
// until the start's transaction ends.
func admitLocalHand(
	ctx context.Context, tx pgx.Tx, record localturn.StartRecord, actor string, targetID int64,
) (localTurnFloorFields, error) {
	var (
		hand            localturn.HandOff
		to              int64
		status          string
		startedQuestion string
		committed       bool
		newest          bool
	)
	err := tx.QueryRow(ctx, `
SELECT hand.id::text, hand.from_participant_id, hand.to_participant_id, hand.message, hand.status,
       COALESCE((SELECT started.question_id::text FROM elitea_runtime.local_turn_executions AS started
                 WHERE started.execution_id = hand.started_execution_id), ''),
       handing.committed_at IS NOT NULL, `+newestLocalMessageSQL+`
FROM elitea_runtime.local_floor_hands AS hand
JOIN elitea_runtime.local_turn_executions AS first
  ON first.project_id = hand.project_id AND first.question_id = hand.question_id
 AND first.floor_question_id = first.question_id AND first.actor_id = $3
JOIN elitea_runtime.local_turn_executions AS handing ON handing.execution_id = hand.from_execution_id
WHERE hand.id = $1::uuid AND hand.project_id = $2 AND hand.question_id = $4::uuid
  AND hand.conversation_uuid = $5::uuid
FOR UPDATE OF hand`, record.FloorTurn.HandID, record.ProjectID, actor, record.FloorTurn.QuestionID,
		record.ConversationUUID).Scan(&hand.HandID, &hand.FromParticipantID, &to, &hand.Message, &status,
		&startedQuestion, &committed, &newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return localTurnFloorFields{}, localturn.ErrFloorTurn
	}
	if err != nil {
		return localTurnFloorFields{}, fmt.Errorf("local turn: read the hand-off: %w", err)
	}
	replay := status == "STARTED" && startedQuestion == record.QuestionID
	switch {
	case to != targetID || (status != "QUEUED" && !replay) || !committed:
		return localTurnFloorFields{}, localturn.ErrFloorTurn
	case !newest && !replay:
		return localTurnFloorFields{}, localturn.ErrFloorSuperseded
	}
	hand.FromName, err = readFloorAgentName(ctx, tx, record.ProjectID, record.ConversationUUID, hand.FromParticipantID)
	if err != nil {
		return localTurnFloorFields{}, err
	}
	return localTurnFloorFields{targets: []int32{}, hand: &hand}, nil
}

// startLocalHand moves the admitted hand-off QUEUED -> STARTED for the new
// execution; a replayed start finds it STARTED already.
func startLocalHand(ctx context.Context, tx pgx.Tx, handID, executionID string) error {
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.local_floor_hands
SET status = 'STARTED', started_execution_id = $2, updated_at = clock_timestamp()
WHERE id = $1::uuid AND status = 'QUEUED'`, handID, executionID)
	if err != nil {
		return fmt.Errorf("local turn: start the hand-off: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return localturn.ErrFloorTurn
	}
	return nil
}

// supersedeLocalHands skips the QUEUED hand-offs of the conversation's
// earlier messages when a person's new message starts.
func supersedeLocalHands(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID, questionID string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.local_floor_hands
SET status = 'SKIPPED', skip_code = 'superseded', updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND question_id <> $3::uuid AND status = 'QUEUED'`,
		projectID, conversationUUID, questionID); err != nil {
		return fmt.Errorf("local turn: supersede the earlier hand-offs: %w", err)
	}
	return nil
}

// localHandOf is the hand-off a turn started, read at its commit.
type localHandOf struct {
	from     int64
	message  string
	question string
}

func readLocalHandOf(ctx context.Context, tx pgx.Tx, executionID string) (*localHandOf, error) {
	var hand localHandOf
	err := tx.QueryRow(ctx, `
SELECT from_participant_id, message, question_id::text FROM elitea_runtime.local_floor_hands
WHERE started_execution_id = $1`, executionID).Scan(&hand.from, &hand.message, &hand.question)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("local turn: read the turn's hand-off: %w", err)
	}
	return &hand, nil
}

// insertLocalHandQuestion writes a handed turn's question: the handing
// agent's message (Main's row, never the commit body's text), authored by
// it, addressed to the receiver, as side talk (ADR-0061 decision 3c).
func insertLocalHandQuestion(
	ctx context.Context, tx pgx.Tx, schema string, target localTurnTarget, questionID string,
	meta []byte, hand *localHandOf,
) (int64, time.Time, error) {
	var (
		group int64
		at    time.Time
	)
	err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, created_at,
     addressed_to, visibility, cause)
VALUES ($1::uuid, $2, $3, $4, $5::jsonb, FALSE, GREATEST(
    clock_timestamp() AT TIME ZONE 'UTC',
    (SELECT max(created_at) + interval '1 millisecond'
     FROM %[1]s.chat_message_group WHERE conversation_id = $3)), ARRAY[$4::integer], 'side', 'agent_mention')
RETURNING id, created_at`, schema),
		questionID, hand.from, target.conversationID, target.targetID, string(meta)).Scan(&group, &at)
	if err != nil {
		return 0, time.Time{}, localTurnWriteError("insert the hand-off", err)
	}
	if err := insertLocalTurnText(ctx, tx, schema, group, hand.message); err != nil {
		return 0, time.Time{}, err
	}
	return group, at, nil
}

// writeLocalConsults writes each consult of the turn as two side messages
// (ADR-0061 decision 3e): the question by the turn's agent to the callee,
// and the callee's reply to it, between the turn's question and its answer
// (which is dated a millisecond after the question).
func writeLocalConsults(
	ctx context.Context, tx pgx.Tx, schema string, target localTurnTarget, executor string,
	questionAt time.Time, consults []localturn.Consult, messageQuestion, executionID string,
) error {
	if len(consults) == 0 {
		return nil
	}
	if executor == localturn.ExecutorCloud {
		return localturn.ErrInvalid // a cloud member is a leaf: it consults no one
	}
	meta, err := json.Marshal(map[string]any{
		"executed_by": localturn.ExecutedByDesktop,
		localturn.ConsultMetaKey: map[string]any{
			"question_id": messageQuestion, "execution_id": executionID,
		},
	})
	if err != nil {
		return err
	}
	written := 0
	for _, consult := range consults {
		// Only a local agent of the thread other than the turn's own is a
		// consult. Anything else (an agent removed mid-turn, the agent
		// itself, a cloud member, which never gets the folder) is left out
		// of the thread rather than losing the person's answer: the commit
		// still stores the answer, and the host never ran a cloud member
		// locally (ADR-0061 owner decision 3).
		var member bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1 FROM %[1]s.chat_participant_mapping AS mapping
    JOIN %[1]s.chat_participants AS participant
      ON participant.id = mapping.participant_id AND participant.entity_name = 'application'
    WHERE mapping.conversation_id = $1 AND mapping.participant_id = $2
      AND COALESCE(mapping.entity_settings ->> 'executor', '') <> 'cloud')`, schema),
			target.conversationID, consult.ParticipantID).Scan(&member); err != nil {
			return fmt.Errorf("local turn: read the consulted agent: %w", err)
		}
		if !member || consult.ParticipantID == target.targetID {
			slog.WarnContext(ctx, "local turn: a consult of no local agent of the thread was left out",
				"execution_id", executionID, "participant_id", consult.ParticipantID)
			continue
		}
		step := time.Duration(2*written+1) * 10 * time.Microsecond
		written++
		question, err := insertLocalSideMessage(ctx, tx, schema, target.conversationID, target.targetID,
			consult.ParticipantID, nil, meta, questionAt.Add(step), consult.Question)
		if err != nil {
			return err
		}
		if _, err := insertLocalSideMessage(ctx, tx, schema, target.conversationID, consult.ParticipantID,
			target.targetID, &question, meta, questionAt.Add(step+10*time.Microsecond), consult.Reply); err != nil {
			return err
		}
	}
	return nil
}

// insertLocalSideMessage writes one consult side message and its text.
func insertLocalSideMessage(
	ctx context.Context, tx pgx.Tx, schema string, conversationID, author, to int64, replyTo *int64,
	meta []byte, at time.Time, text string,
) (int64, error) {
	var group int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, reply_to_id, meta, is_streaming, created_at,
     addressed_to, visibility, cause)
VALUES (gen_random_uuid(), $1, $2, $3, $4, $5::jsonb, FALSE, $6::timestamp, ARRAY[$3::integer], 'side', 'delegation')
RETURNING id`, schema), author, conversationID, to, replyTo, string(meta), at).Scan(&group)
	if err != nil {
		return 0, localTurnWriteError("insert a consult", err)
	}
	if text == "" {
		return group, nil
	}
	return group, insertLocalTurnText(ctx, tx, schema, group, text)
}
