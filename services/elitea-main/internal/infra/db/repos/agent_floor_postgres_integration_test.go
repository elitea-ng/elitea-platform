package repos

// The turn-taking engine's SQL against a real PostgreSQL (ADR-0050
// decisions 7c and 7d, issue 1611): an exchange and its queued addressed
// agents, a newer message superseding them, hand_floor queueing and the
// loop guard's stop (system message, skipped turns), and the settle view.
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type floorHarness struct {
	*interruptHarness
	floors       *AgentFloorRepository
	conversation string
	agents       [3]int64
}

func newFloorHarness(t *testing.T) *floorHarness {
	t.Helper()
	h := newInterruptHarness(t)
	floors, err := NewAgentFloorRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	f := &floorHarness{interruptHarness: h, floors: floors}
	if err := h.pool.QueryRow(t.Context(), `SELECT uuid::text FROM p_1.chat_conversations WHERE id = $1`,
		h.conversationID).Scan(&f.conversation); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(t.Context(), `
SELECT participant.id FROM p_1.chat_participants participant
JOIN p_1.chat_participant_mapping mapping ON mapping.participant_id = participant.id AND mapping.conversation_id = $1
WHERE participant.entity_name = 'application'`, h.conversationID).Scan(&f.agents[0]); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"Bo", "Cy"} {
		if err := h.pool.QueryRow(t.Context(), `
WITH participant AS (
  INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
  VALUES (gen_random_uuid(), 'application', '{"id": 2}'::jsonb, json_build_object('name', $2::text)) RETURNING id),
mapped AS (
  INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
  SELECT $1, id, now() AT TIME ZONE 'UTC' FROM participant)
SELECT id FROM participant`, h.conversationID, name).Scan(&f.agents[index+1]); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *floorHarness) admitExchange(questionID string, first int64, queued ...int64) {
	f.t.Helper()
	err := pgx.BeginFunc(f.t.Context(), f.pool, func(tx pgx.Tx) error {
		return admitFloor(f.t.Context(), tx, agentexecutionapp.CurrentApplicationTurn{
			ProjectID: 1, ConversationUUID: f.conversation, TargetParticipantID: first,
		}, &agentexecutionapp.FloorAdmission{Exchange: &agentexecutionapp.FloorExchange{
			QuestionID: questionID, OwnerUserID: 8, TriggerOrigin: "manual", FirstParticipantID: first, Queued: queued,
		}}, "")
	})
	if err != nil {
		f.t.Fatalf("admit exchange: %v", err)
	}
}

// runOf binds a harness response's execution to the conversation as the
// participant's turn of questionID.
func (f *floorHarness) runOf(participant int64, questionID string) string {
	f.t.Helper()
	responseID := f.newResponse()
	executionID := f.executions[responseID]
	f.exec(`UPDATE elitea_runtime.agent_execution_jobs
SET client_stream_id = $2, client_execution_generation = $3, participant_id = $4
WHERE execution_id = $1`, executionID, f.conversation, questionID, participant)
	return executionID
}

func (f *floorHarness) turnStates(questionID string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.t.Context(), `
SELECT state || COALESCE(':' || skip_code, '') FROM elitea_runtime.agent_floor_turns
WHERE question_id = $1::uuid ORDER BY position`, questionID)
	if err != nil {
		f.t.Fatal(err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return states
}

const (
	floorQuestion1 = "11111111-1111-4111-8111-111111111111"
	floorQuestion2 = "22222222-2222-4222-8222-222222222222"
)

func TestAgentFloorExchangeQueuesAndSupersedes(t *testing.T) {
	f := newFloorHarness(t)
	floor, err := f.floors.ConversationFloor(t.Context(), 1, f.conversation)
	if err != nil || len(floor.Agents) != 3 || floor.Agents[1].Name != "Bo" || floor.Settings != turntaking.Default {
		t.Fatalf("floor=%+v err=%v", floor, err)
	}
	f.admitExchange(floorQuestion1, f.agents[0], f.agents[1], f.agents[2])
	f.admitExchange(floorQuestion1, f.agents[0], f.agents[1], f.agents[2]) // a replay writes nothing more
	if got := f.turnStates(floorQuestion1); len(got) != 2 || got[0] != "QUEUED" || got[1] != "QUEUED" {
		t.Fatalf("turns = %v", got)
	}
	pending, err := f.floors.PendingFloorTurns(t.Context(), 1, f.conversation)
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	// Starting a queued turn moves it to STARTED once.
	start := func() error {
		return pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
			return admitFloor(t.Context(), tx, agentexecutionapp.CurrentApplicationTurn{
				ProjectID: 1, ConversationUUID: f.conversation, TargetParticipantID: f.agents[1],
			}, &agentexecutionapp.FloorAdmission{
				StartTurnQuestionID: agentexecutionapp.FloorTurnQuestionID(floorQuestion1, 1),
			}, "0123456789abcdef0123456789abcdef")
		})
	}
	if err := start(); err != nil {
		t.Fatal(err)
	}
	if err := start(); !errors.Is(err, agentexecutionapp.ErrFloorTurnNotQueued) {
		t.Fatalf("second start err = %v", err)
	}
	// A newer human message supersedes the QUEUED turn of the earlier one.
	f.admitExchange(floorQuestion2, f.agents[2])
	if got := f.turnStates(floorQuestion1); got[0] != "STARTED" || got[1] != "SKIPPED:superseded" {
		t.Fatalf("turns = %v", got)
	}
}

func TestAgentFloorHandQueuesThenTheGuardStops(t *testing.T) {
	f := newFloorHarness(t)
	f.admitExchange(floorQuestion1, f.agents[0])
	run := storage.HandFloorRun{ProjectID: 1, ConversationUUID: f.conversation, ParticipantID: f.agents[0],
		ExecutionID: f.runOf(f.agents[0], floorQuestion1)}
	hand := func(run storage.HandFloorRun, to int64) storage.FloorExchangeView {
		var seen storage.FloorExchangeView
		if err := f.floors.HandFloorInExchange(t.Context(), run, func(view storage.FloorExchangeView) (storage.FloorHandWrite, error) {
			seen = view
			decision, err := turntaking.Hand(view.Exchange, run.ParticipantID, []int64{to},
				[]int64{f.agents[0], f.agents[1], f.agents[2]})
			if err != nil {
				return storage.FloorHandWrite{}, err
			}
			if decision.Stopped != "" {
				return storage.FloorHandWrite{Stop: decision.Stopped, StopText: "Loop guard stopped the exchange."}, nil
			}
			return storage.FloorHandWrite{Queue: decision.Handed, Message: "over to you"}, nil
		}); err != nil {
			t.Fatalf("hand: %v", err)
		}
		return seen
	}
	hand(run, f.agents[1])
	if got := f.turnStates(floorQuestion1); len(got) != 1 || got[0] != "QUEUED" {
		t.Fatalf("turns = %v", got)
	}
	// The settle of the first run answers Bo's queued turn as next.
	view, found, err := f.floors.FloorSettle(t.Context(), run.ExecutionID)
	if err != nil || !found || view.Next == nil || view.Next.ToParticipantID != f.agents[1] ||
		view.Next.FromParticipantID != f.agents[0] || view.Next.Message != "over to you" ||
		view.Next.Cause != turntaking.CauseAgentMention || view.OwnerUserID != 8 {
		t.Fatalf("view=%+v next=%+v found=%v err=%v", view, view.Next, found, err)
	}
	// Bo's turn runs and hands back; Ada hands to Bo again: Bo's third turn
	// passes the two-turns guard and stops the exchange.
	bo := storage.HandFloorRun{ProjectID: 1, ConversationUUID: f.conversation, ParticipantID: f.agents[1],
		ExecutionID: f.runOf(f.agents[1], agentexecutionapp.FloorTurnQuestionID(floorQuestion1, 1))}
	f.exec(`UPDATE elitea_runtime.agent_floor_turns SET state = 'STARTED', execution_id = $1 WHERE position = 1`, bo.ExecutionID)
	hand(bo, f.agents[2])
	hand(run, f.agents[1])
	seen := hand(run, f.agents[1])
	if seen.Stopped != "" || len(seen.Exchange.Turns) != 3 {
		t.Fatalf("view before the stop = %+v", seen)
	}
	var stopped *string
	if err := f.pool.QueryRow(t.Context(), `SELECT stopped FROM elitea_runtime.agent_floor_exchanges WHERE question_id = $1::uuid`,
		floorQuestion1).Scan(&stopped); err != nil || stopped == nil || *stopped != turntaking.StopAgentTurns {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
	if got := f.turnStates(floorQuestion1); len(got) != 3 || got[1] != "SKIPPED:consecutive_turns" || got[2] != "SKIPPED:consecutive_turns" {
		t.Fatalf("turns = %v", got)
	}
	if messages := f.count(`
SELECT count(*) FROM p_1.chat_message_group message
JOIN p_1.chat_participants author ON author.id = message.author_participant_id
WHERE message.conversation_id = $1 AND author.entity_name = 'system' AND author.entity_meta ->> 'kind' = 'turn_taking'`,
		f.conversationID); messages != 1 {
		t.Fatalf("system messages = %d", messages)
	}
	// A run of an older exchange cannot hand.
	f.admitExchange(floorQuestion2, f.agents[2])
	err = f.floors.HandFloorInExchange(context.Background(), run, func(storage.FloorExchangeView) (storage.FloorHandWrite, error) {
		return storage.FloorHandWrite{}, nil
	})
	if !errors.Is(err, storage.ErrContentRejected) {
		t.Fatalf("an older exchange's run handed: %v", err)
	}
}
