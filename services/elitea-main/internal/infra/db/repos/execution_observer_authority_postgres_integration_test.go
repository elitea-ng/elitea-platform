package repos

// The agent execution events stream is bound to the execution (PR #1031 review).
//
// Before, any project member holding models.chat.messages.create could replay
// any agent execution of the project from cursor 0, including another user's
// private chat. The web app now reattaches to the last turn's task_id read from
// the conversation, so the stream must answer only the execution's starter and
// the principals who may read its conversation.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func seedObservedAgentExecution(t *testing.T, pool *pgxpool.Pool, executionID, actorID, conversationUUID string) {
	t.Helper()
	const (
		inputBundleID  = "input-bundle-execution-observer"
		requestEntryID = "request"
	)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES (
    $1, '1', 'application/x-protobuf', 1,
    decode(repeat('ab', 32), 'hex'), 2, '{}'::bytea, 'tests', clock_timestamp()
) ON CONFLICT (input_bundle_id) DO NOTHING`, inputBundleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES (
    $1, $2, '1', 'request', 'application/json',
    decode(repeat('cd', 32), 'hex'), 2, 'inline://request', 'internal',
    'worker', '{}'::bytea
) ON CONFLICT (input_bundle_id, entry_id) DO NOTHING`, inputBundleID, requestEntryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES (
    $1, 1, $2, '1', 1,
    1, $3, 'user:' || $3, $4,
    '1', $5, decode(repeat('ef', 32), 'hex'), 'execution-observer',
    $6, 'RUNNING', 'RUNNING', clock_timestamp()
)`, executionID, "command-"+executionID, actorID, executiondomain.AgentApplicationCapability, inputBundleID, "idempotency-"+executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event
) VALUES (
    $1, 1, $2, $3, $4,
    $5, gen_random_uuid()::text, '1', 'chat_predict'
)`, executionID, executiondomain.AgentApplicationCapability, inputBundleID, requestEntryID, conversationUUID); err != nil {
		t.Fatal(err)
	}
}

func addUserParticipant(t *testing.T, pool *pgxpool.Pool, conversationUUID string, userID int) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', jsonb_build_object('id', $2::int)) RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id)
SELECT c.id, participant.id FROM p_1.chat_conversations c, participant WHERE c.uuid::text = $1`,
		conversationUUID, userID); err != nil {
		t.Fatalf("add participant %d: %v", userID, err)
	}
}

func TestExecutionObserverAuthorityBindsAgentEventsToTheExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	authority, err := NewExecutionObserverAuthority(pool)
	if err != nil {
		t.Fatal(err)
	}
	// A private conversation of user 7. seedConversationWithParticipant makes
	// user 7 its only participant.
	_, conversationUUID, _ := seedConversationWithParticipant(t, repo, "question", "answer")
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.chat_conversations SET is_private = true WHERE uuid::text = $1`, conversationUUID); err != nil {
		t.Fatal(err)
	}
	seedObservedAgentExecution(t, pool, "exec-private-chat", "7", conversationUUID)
	seedObservedAgentExecution(t, pool, "exec-no-conversation", "7", "00000000-0000-4000-8000-000000000000")

	may := func(executionID string, project int64, userID string) bool {
		t.Helper()
		allowed, err := authority.MayObserveAgentExecution(t.Context(), project, executionID, auth.User{ID: userID, UserID: userID})
		if err != nil {
			t.Fatalf("observe %s as %s: %v", executionID, userID, err)
		}
		return allowed
	}

	if !may("exec-private-chat", 1, "7") {
		t.Error("the starter was refused its own execution")
	}
	if may("exec-private-chat", 1, "9") {
		t.Error("member 9, not a participant, may replay user 7's private chat execution")
	}
	if may("exec-no-conversation", 1, "9") {
		t.Error("member 9 may replay an execution whose conversation is gone and that it did not start")
	}
	if !may("exec-no-conversation", 1, "7") {
		t.Error("the starter was refused its own execution whose conversation is gone")
	}
	if may("exec-unknown", 1, "7") || may("exec-private-chat", 2, "7") {
		t.Error("an unknown execution or another project's id was admitted")
	}
	if allowed, err := authority.MayObserveAgentExecution(t.Context(), 1, "exec-private-chat", auth.User{ID: "token-1", TokenID: "token-1"}); err != nil || allowed {
		t.Errorf("a principal with no owning user: allowed=%v err=%v", allowed, err)
	}

	// A participant of the conversation keeps the reattach after a reload.
	addUserParticipant(t, pool, conversationUUID, 8)
	if !may("exec-private-chat", 1, "8") {
		t.Error("participant 8 of the private conversation was refused")
	}

	// A shared (not private) conversation is readable by the project, so its
	// executions are too.
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.chat_conversations SET is_private = false WHERE uuid::text = $1`, conversationUUID); err != nil {
		t.Fatal(err)
	}
	if !may("exec-private-chat", 1, "9") {
		t.Error("member 9 was refused an execution of a shared conversation")
	}
}

func TestExecutionObserverAuthorityRequiresAPool(t *testing.T) {
	if _, err := NewExecutionObserverAuthority(nil); err == nil {
		t.Fatal("an observer authority with no pool was built")
	}
}
