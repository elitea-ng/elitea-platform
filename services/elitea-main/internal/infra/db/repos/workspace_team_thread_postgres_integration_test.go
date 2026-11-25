package repos

// A team work thread's cloud turn follows the floor (ADR-0050 decision 7d,
// issue 3053) against a REAL database (ELITEA_TEST_DATABASE_URL): the send's
// continue and a schedule's turn on a handed-off thread address the lead of
// a `lead` thread, not the participant the last local turn addressed; an
// `addressed` thread and a thread of one agent keep that participant.

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// addThreadAgent adds an application participant mapped at version to the
// fixture's conversation and answers its participant id.
func addThreadAgent(t *testing.T, pool *pgxpool.Pool, conversationID, application, version int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'application', jsonb_build_object('id', $1::bigint, 'name', $2::text, 'project_id', 1), '{}'::json)
RETURNING id`, application, name).Scan(&id); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, jsonb_build_object('version_id', $3::bigint), now() AT TIME ZONE 'UTC')`, conversationID, id, version); err != nil {
		t.Fatalf("map agent: %v", err)
	}
	return id
}

// commitLocalTurn records a committed local turn of the thread on target.
func commitLocalTurn(t *testing.T, pool *pgxpool.Pool, owner int64, conversationUUID string, target int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO elitea_runtime.local_turn_executions (execution_id, project_id, actor_id, token_id, native_client_id,
    conversation_uuid, question_id, response_message_id, target_participant_id, memories_used, expires_at,
    committed_at, commit_digest)
VALUES (replace(gen_random_uuid()::text, '-', ''), 1, $1::text, '91', 'ai.elitea.desktop', $2::uuid, gen_random_uuid(),
        gen_random_uuid(), $3, 0, clock_timestamp() + interval '1 hour', clock_timestamp(), sha256('x'::bytea))`,
		strconv.FormatInt(owner, 10), conversationUUID, target); err != nil {
		t.Fatalf("seed local turn: %v", err)
	}
}

func setThreadFloor(t *testing.T, pool *pgxpool.Pool, conversationID int64, floor string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE p_1.chat_conversations SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('floor', $2::jsonb) WHERE id = $1`,
		conversationID, floor); err != nil {
		t.Fatalf("set floor: %v", err)
	}
}

func TestPostgresTeamWorkThreadContinuesOnTheFloorsTarget(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a lead thread continues on the lead", func(t *testing.T) {
		const owner int64 = 5601
		f := newHandoffContinueFixture(t, pool, owner)
		conversation := f.thread.thread.ConversationUUID
		coder := addThreadAgent(t, pool, f.thread.conversationID, 5, 31, "Coder")
		reviewer := addThreadAgent(t, pool, f.thread.conversationID, 6, 41, "Reviewer")
		// The last local turn was Coder's; the team's lead is Reviewer.
		commitLocalTurn(t, pool, owner, conversation, coder)
		setThreadFloor(t, pool, f.thread.conversationID, `{"policy": "lead", "lead_participant_id": `+strconv.FormatInt(reviewer, 10)+`}`)

		target, err := repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner)
		if err != nil || target.ParticipantID != reviewer || !target.Agent || target.ConversationUUID != conversation {
			t.Fatalf("continue target = %+v, %v; want the lead %d", target, err, reviewer)
		}
		thread, err := repo.ThreadTarget(ctx, 1, conversation)
		if err != nil || thread.ParticipantID != reviewer || !thread.Agent || thread.VersionID != 41 {
			t.Fatalf("thread target = %+v, %v; want the lead %d at version 41", thread, err, reviewer)
		}
	})

	t.Run("an addressed thread continues on the last local turn's agent", func(t *testing.T) {
		const owner int64 = 5602
		f := newHandoffContinueFixture(t, pool, owner)
		conversation := f.thread.thread.ConversationUUID
		addThreadAgent(t, pool, f.thread.conversationID, 5, 31, "Coder")
		reviewer := addThreadAgent(t, pool, f.thread.conversationID, 6, 41, "Reviewer")
		commitLocalTurn(t, pool, owner, conversation, reviewer)
		setThreadFloor(t, pool, f.thread.conversationID, `{"policy": "addressed"}`)

		target, err := repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner)
		if err != nil || target.ParticipantID != reviewer || !target.Agent {
			t.Fatalf("continue target = %+v, %v; want %d", target, err, reviewer)
		}
	})

	t.Run("a lead that left the thread does not strand it", func(t *testing.T) {
		const owner int64 = 5603
		f := newHandoffContinueFixture(t, pool, owner)
		conversation := f.thread.thread.ConversationUUID
		coder := addThreadAgent(t, pool, f.thread.conversationID, 5, 31, "Coder")
		addThreadAgent(t, pool, f.thread.conversationID, 6, 41, "Reviewer")
		commitLocalTurn(t, pool, owner, conversation, coder)
		setThreadFloor(t, pool, f.thread.conversationID, `{"policy": "lead", "lead_participant_id": 999999}`)

		target, err := repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner)
		if err != nil || target.ParticipantID != coder {
			t.Fatalf("continue target = %+v, %v; want %d", target, err, coder)
		}
	})

	t.Run("a malformed floor reads as the default floor", func(t *testing.T) {
		const owner int64 = 5604
		f := newHandoffContinueFixture(t, pool, owner)
		conversation := f.thread.thread.ConversationUUID
		addThreadAgent(t, pool, f.thread.conversationID, 5, 31, "Coder")
		reviewer := addThreadAgent(t, pool, f.thread.conversationID, 6, 41, "Reviewer")
		commitLocalTurn(t, pool, owner, conversation, reviewer)
		setThreadFloor(t, pool, f.thread.conversationID, `{"policy": "lead"}`)

		target, err := repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner)
		if err != nil || target.ParticipantID != reviewer {
			t.Fatalf("continue target = %+v, %v; want %d", target, err, reviewer)
		}
	})

	t.Run("a thread of one agent and the model is unchanged", func(t *testing.T) {
		const owner int64 = 5605
		f := newHandoffContinueFixture(t, pool, owner)
		// No agent: the model participant, as before.
		target, err := repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner)
		if err != nil || target.ParticipantID != f.model || target.Agent {
			t.Fatalf("model thread = %+v, %v; want the model %d", target, err, f.model)
		}
		coder := addThreadAgent(t, pool, f.thread.conversationID, 5, 31, "Coder")
		if target, err = repo.HandoffContinueTarget(ctx, 1, f.handoffID, owner); err != nil || target.ParticipantID != coder || !target.Agent {
			t.Fatalf("one agent = %+v, %v; want %d", target, err, coder)
		}
	})
}
