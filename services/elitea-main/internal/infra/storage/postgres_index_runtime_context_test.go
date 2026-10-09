package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestPostgresRuntimeContextRequiresExactActiveClaimSessionAndFence(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{7}, sha256.Size)
	store := contentQueryerFunc(func(_ context.Context, query string, args ...any) pgx.Row {
		for _, predicate := range []string{
			"j.actor_id",
			"WHEN j.capability_id = 'index.ingest.v1' THEN i.initiator",
			"i.capability_id = j.capability_id",
			"a.capability_id = j.capability_id",
			"ws.workload_session_id = c.workload_session_id",
			"ws.workload_identity = c.workload_identity",
			"ws.producer_id = c.producer_id",
			"c.released_at IS NULL",
			"c.lease_expires_at > clock_timestamp()",
			"ws.expires_at > clock_timestamp()",
			"ws.revoked_at IS NULL",
			"j.desired_state = 'RUNNING'",
			"j.capability_id = 'index.ingest.v1' AND i.execution_id IS NOT NULL",
			"'agent.execute.application.v1'",
			"'agent.execute.adhoc.v1'",
			"a.execution_id IS NOT NULL",
			// A toolkit tool run redeems its user's client token too (test_tool).
			"WHEN j.capability_id = 'toolkit.call_tool.v1' THEN 'user'",
		} {
			require.Contains(t, query, predicate)
		}
		require.Equal(t, []any{"claim-1", "execution-1", uint64(3), identity.String(), fence}, args)
		return contentRowFunc(func(dest ...any) error {
			require.Len(t, dest, 4)
			*dest[0].(*int64) = 42
			*dest[1].(*string) = "17"
			*dest[2].(*string) = runtimeContextInitiatorUser
			*dest[3].(*string) = ""
			return nil
		})
	})
	repository, err := newPostgresContentRepository(store)
	require.NoError(t, err)

	authorization, err := repository.AuthorizeRuntimeContext(context.Background(), ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-1",
		Generation:      3,
		ClaimID:         "claim-1",
		FenceToken:      fence,
	})
	require.NoError(t, err)
	require.EqualValues(t, 42, authorization.ResourceProjectID)
	require.Equal(t, "17", authorization.ActorID)
	require.Equal(t, runtimeContextInitiatorUser, authorization.Initiator)
}

func TestPostgresRuntimeContextAuthorizesAgentClaimAsInteractiveActor(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-2")
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{6}, sha256.Size)
	store := contentQueryerFunc(func(_ context.Context, query string, args ...any) pgx.Row {
		for _, predicate := range []string{
			"LEFT JOIN elitea_runtime.agent_execution_jobs AS a",
			"'agent.execute.application.v1'",
			"'agent.execute.adhoc.v1'",
			"THEN 'user'",
			"a.execution_id IS NOT NULL",
			"c.released_at IS NULL",
			"j.desired_state = 'RUNNING'",
			// The conversation the attachment object route authorizes on. It
			// is projected from the agent row, never from the request — see
			// RuntimeContextAuthorization.ConversationID.
			"COALESCE(a.client_stream_id, '') AS conversation_id",
		} {
			require.Contains(t, query, predicate)
		}
		require.Equal(t, []any{"claim-agent", "execution-agent", uint64(5), identity.String(), fence}, args)
		return contentRowFunc(func(dest ...any) error {
			require.Len(t, dest, 4)
			*dest[0].(*int64) = 84
			*dest[1].(*string) = "19"
			*dest[2].(*string) = runtimeContextInitiatorUser
			*dest[3].(*string) = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
			return nil
		})
	})
	repository, err := newPostgresContentRepository(store)
	require.NoError(t, err)

	authorization, err := repository.AuthorizeRuntimeContext(context.Background(), ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-agent",
		Generation:      5,
		ClaimID:         "claim-agent",
		FenceToken:      fence,
	})
	require.NoError(t, err)
	require.EqualValues(t, 84, authorization.ResourceProjectID)
	require.Equal(t, "19", authorization.ActorID)
	require.Equal(t, runtimeContextInitiatorUser, authorization.Initiator)
	require.Equal(t, "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1", authorization.ConversationID)
}

func TestPostgresRuntimeContextHidesInactiveOrMismatchedClaims(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	repository, err := newPostgresContentRepository(contentQueryerFunc(
		func(_ context.Context, query string, _ ...any) pgx.Row {
			require.True(t, strings.Contains(query, "j.capability_id = 'index.ingest.v1'"))
			require.True(t, strings.Contains(query, "a.execution_id IS NOT NULL"))
			return contentRowFunc(func(...any) error { return pgx.ErrNoRows })
		},
	))
	require.NoError(t, err)

	_, err = repository.AuthorizeRuntimeContext(context.Background(), ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-1",
		Generation:      1,
		ClaimID:         "claim-1",
		FenceToken:      make([]byte, sha256.Size),
	})
	require.ErrorIs(t, err, ErrContentUnauthorized)
}

// TestPostgresRuntimeContextAuthorizationIntegration crosses a real
// PostgreSQL service and proves the active claim/session/fence predicates. It
// intentionally leaves mTLS termination and PAT validation to their focused
// component tests.
func TestPostgresRuntimeContextAuthorizationIntegration(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL runtime-context authorization test")
	}
	pool := isolatedStoragePostgresPool(t, databaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ddl := []string{
		`CREATE SCHEMA elitea_runtime`,
		`CREATE TABLE elitea_runtime.execution_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    resource_project_id INTEGER NOT NULL,
    actor_id TEXT NOT NULL,
    desired_state TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    PRIMARY KEY (execution_id, generation)
)`,
		`CREATE TABLE elitea_runtime.index_ingest_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    capability_id TEXT NOT NULL,
    initiator TEXT NOT NULL,
    PRIMARY KEY (execution_id, generation)
)`,
		// client_stream_id is chat_conversations.uuid, and it is NOT NULL in
		// the real table (0055_agent_execution_admission.sql). The attachment
		// object route authorizes on it, so a fixture without it would let a
		// query that stopped projecting it still pass here.
		`CREATE TABLE elitea_runtime.agent_execution_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    capability_id TEXT NOT NULL,
    client_stream_id TEXT NOT NULL,
    PRIMARY KEY (execution_id, generation)
)`,
		`CREATE TABLE elitea_runtime.workload_sessions (
    workload_session_id TEXT PRIMARY KEY,
    workload_identity TEXT NOT NULL,
    producer_id TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
)`,
		`CREATE TABLE elitea_runtime.execution_claims (
    claim_id TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    workload_session_id TEXT NOT NULL,
    workload_identity TEXT NOT NULL,
    producer_id TEXT NOT NULL,
    fence_token BYTEA NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ
)`,
	}
	for _, statement := range ddl {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := url.Parse("spiffe://elitea.internal/runtime/index-worker-1")
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{8}, sha256.Size)
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_jobs
    (execution_id, generation, resource_project_id, actor_id, desired_state, capability_id)
VALUES ('execution-1', 1, 42, '17', 'RUNNING', 'index.ingest.v1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.index_ingest_jobs
    (execution_id, generation, capability_id, initiator)
VALUES ('execution-1', 1, 'index.ingest.v1', 'user')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_jobs
    (execution_id, generation, resource_project_id, actor_id, desired_state, capability_id)
VALUES ('execution-agent', 1, 42, '17', 'RUNNING', 'agent.execute.application.v1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.agent_execution_jobs
    (execution_id, generation, capability_id, client_stream_id)
VALUES ('execution-agent', 1, 'agent.execute.application.v1',
        '5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.workload_sessions
    (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ('session-1', $1, 'producer-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '5 minutes')`, identity.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_claims
    (claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id, fence_token, lease_expires_at)
VALUES ('claim-1', 'execution-1', 1, 'session-1', $1, 'producer-1', $2, clock_timestamp() + interval '1 minute')`, identity.String(), fence); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_claims
    (claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id, fence_token, lease_expires_at)
VALUES ('claim-agent', 'execution-agent', 1, 'session-1', $1, 'producer-1', $2, clock_timestamp() + interval '1 minute')`, identity.String(), fence); err != nil {
		t.Fatal(err)
	}
	repository, err := NewPostgresContentRepository(pool)
	require.NoError(t, err)
	claim := ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-1",
		Generation:      1,
		ClaimID:         "claim-1",
		FenceToken:      fence,
	}
	authorization, err := repository.AuthorizeRuntimeContext(ctx, claim)
	require.NoError(t, err)
	require.EqualValues(t, 42, authorization.ResourceProjectID)
	require.Equal(t, "17", authorization.ActorID)
	require.Equal(t, runtimeContextInitiatorUser, authorization.Initiator)
	// An index claim joins no agent row: the conversation is empty, which is
	// exactly why the attachment route requires it rather than assuming it.
	require.Empty(t, authorization.ConversationID)

	agentAuthorization, err := repository.AuthorizeRuntimeContext(ctx, ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-agent",
		Generation:      1,
		ClaimID:         "claim-agent",
		FenceToken:      fence,
	})
	require.NoError(t, err)
	require.EqualValues(t, 42, agentAuthorization.ResourceProjectID)
	require.Equal(t, "17", agentAuthorization.ActorID)
	require.Equal(t, runtimeContextInitiatorUser, agentAuthorization.Initiator)
	require.Equal(t,
		"5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1",
		agentAuthorization.ConversationID,
		"the agent claim must carry its own conversation out of Postgres",
	)

	claim.FenceToken = bytes.Repeat([]byte{9}, sha256.Size)
	_, err = repository.AuthorizeRuntimeContext(ctx, claim)
	require.ErrorIs(t, err, ErrContentUnauthorized)
	claim.FenceToken = fence
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workload_sessions SET revoked_at = clock_timestamp() WHERE workload_session_id = 'session-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = repository.AuthorizeRuntimeContext(ctx, claim)
	require.ErrorIs(t, err, ErrContentUnauthorized)
}
