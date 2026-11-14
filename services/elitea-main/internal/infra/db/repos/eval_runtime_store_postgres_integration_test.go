package repos

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The chat-side SQL of a runtime_plane evaluation case (tenant schema p_1)
// against a REAL migrated database: the five rows a turn needs, the answer read
// back the way the transcript reads it, and the step count the step limit uses.

func newCaseStore(t *testing.T) (*EvalRuntimeStore, func(string, ...any)) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	store, err := NewEvalRuntimeStore(pool, nil)
	require.NoError(t, err)
	exec := func(statement string, args ...any) {
		t.Helper()
		_, err := pool.Exec(context.Background(), statement, args...)
		require.NoError(t, err, statement)
	}
	return store, exec
}

func TestEvalRuntimeStorePreparesAConversationTheTurnResolverCanUse(t *testing.T) {
	store, _ := newCaseStore(t)
	ctx := context.Background()

	conversation, participant, err := store.PrepareConversation(ctx, 1, 7, 4001, 4002, "Evaluation run 3, case 21")
	require.NoError(t, err)
	require.NotEmpty(t, conversation)
	require.NotZero(t, participant)

	// The same joins ResolveCurrentApplicationTurn performs: the agent
	// participant is mapped into THIS conversation with the version pinned on
	// the mapping, and the author has a user participant.
	var source, entity, versionID string
	require.NoError(t, store.pool.QueryRow(ctx, `
SELECT c.source, p.entity_name, m.entity_settings ->> 'version_id'
FROM p_1.chat_conversations c
JOIN p_1.chat_participant_mapping m ON m.conversation_id = c.id
JOIN p_1.chat_participants p ON p.id = m.participant_id
WHERE c.uuid = $1::uuid AND m.participant_id = $2`, conversation, participant).Scan(&source, &entity, &versionID))
	require.Equal(t, EvalConversationSource, source)
	require.Equal(t, "application", entity)
	require.Equal(t, "4002", versionID)

	var users int
	require.NoError(t, store.pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.chat_participant_mapping m
JOIN p_1.chat_participants p ON p.id = m.participant_id
JOIN p_1.chat_conversations c ON c.id = m.conversation_id
WHERE c.uuid = $1::uuid AND p.entity_name = 'user' AND p.entity_meta ->> 'id' = '7'`, conversation).Scan(&users))
	require.Equal(t, 1, users)

	// A second case of the same agent reuses the participant.
	_, again, err := store.PrepareConversation(ctx, 1, 7, 4001, 4002, "Evaluation run 3, case 22")
	require.NoError(t, err)
	require.Equal(t, participant, again)

	// A refused start leaves nothing behind.
	store.DiscardConversation(ctx, 1, conversation)
	var left int
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.chat_conversations WHERE uuid = $1::uuid`, conversation).Scan(&left))
	require.Zero(t, left)
}

func TestEvalRuntimeStoreReadsTheProjectedTurn(t *testing.T) {
	store, exec := newCaseStore(t)
	ctx := context.Background()
	conversation, participant, err := store.PrepareConversation(ctx, 1, 7, 4001, 4002, "case")
	require.NoError(t, err)

	group := func(uuid, meta string, streaming bool, chunks ...string) {
		t.Helper()
		var groupID int64
		require.NoError(t, store.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group (uuid, conversation_id, author_participant_id, meta, is_streaming, created_at)
VALUES ($1::uuid, (SELECT id FROM p_1.chat_conversations WHERE uuid = $2::uuid), $5, $3::jsonb, $4, now() AT TIME ZONE 'UTC')
RETURNING id`, uuid, conversation, meta, streaming, participant).Scan(&groupID))
		for index, chunk := range chunks {
			var itemID int64
			require.NoError(t, store.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
VALUES (gen_random_uuid(), 'text_message', $1, '{}'::jsonb, $2, now() AT TIME ZONE 'UTC') RETURNING id`,
				index, groupID).Scan(&itemID))
			exec(`INSERT INTO p_1.chat_messages_text (id, content) VALUES ($1, $2)`, itemID, chunk)
		}
	}

	const (
		streaming = "00000000-0000-4000-8000-000000000001"
		done      = "00000000-0000-4000-8000-000000000002"
		failed    = "00000000-0000-4000-8000-000000000003"
		hitl      = "00000000-0000-4000-8000-000000000004"
		gone      = "00000000-0000-4000-8000-000000000005"
	)
	group(streaming, `{}`, true)
	group(done, `{"is_error": false}`, false, "Go is ", "a language")
	group(failed, `{"is_error": true, "error": "model refused"}`, false)
	group(hitl, `{"hitl_interrupt": {"id": "x"}}`, false)

	turn, err := store.ReadTurn(ctx, 1, streaming)
	require.NoError(t, err)
	require.False(t, turn.Settled, "a streaming group is not settled")

	turn, err = store.ReadTurn(ctx, 1, done)
	require.NoError(t, err)
	require.True(t, turn.Settled)
	require.Equal(t, "Go is a language", turn.Text, "the text items, in order")
	require.False(t, turn.Failed)
	require.Empty(t, turn.Paused)

	turn, err = store.ReadTurn(ctx, 1, failed)
	require.NoError(t, err)
	require.True(t, turn.Failed)
	require.Equal(t, "model refused", turn.Failure)

	turn, err = store.ReadTurn(ctx, 1, hitl)
	require.NoError(t, err)
	require.Equal(t, "human approval", turn.Paused)

	// A removed turn (a cancel deletes an empty response group) is settled with
	// a reason, not a row to poll for ever.
	turn, err = store.ReadTurn(ctx, 1, gone)
	require.NoError(t, err)
	require.True(t, turn.Settled)
	require.True(t, turn.Failed)
}

func TestEvalRuntimeStoreCountsTheStepsTheLimitCounts(t *testing.T) {
	store, exec := newCaseStore(t)
	ctx := context.Background()

	exec(`INSERT INTO elitea_runtime.run_step_heads (run_id, project_id) VALUES ('exec-steps', 1), ('exec-other', 1)`)
	hash := fmt.Sprintf(`decode(repeat('00', 32), 'hex')`)
	for index, kind := range []string{"model_call", "tool_call", "retrieval", "delegation", "approval", "output", "error", "cap"} {
		exec(fmt.Sprintf(`
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, kind, actor, started_at, ended_at, prev_hash, hash)
VALUES ('exec-steps', %d, gen_random_uuid(), 1, 1, '%s', 'user:7', now(), now(), %s, %s)`, index+1, kind, hash, hash))
	}
	exec(fmt.Sprintf(`
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, kind, actor, started_at, ended_at, prev_hash, hash)
VALUES ('exec-other', 1, gen_random_uuid(), 1, 1, 'model_call', 'user:7', now(), now(), %s, %s)`, hash, hash))

	count, err := store.StepCount(ctx, 1, "exec-steps")
	require.NoError(t, err)
	require.Equal(t, 4, count, "model, tool, retrieval and delegation steps; not approval, output, error or cap")

	none, err := store.StepCount(ctx, 1, "exec-missing")
	require.NoError(t, err)
	require.Zero(t, none)
}
