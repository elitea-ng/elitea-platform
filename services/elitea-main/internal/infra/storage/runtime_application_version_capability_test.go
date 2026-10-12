package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// agentRuntimeContextAuthorizerFunc is the nested route's authorizer stub. It
// deliberately does NOT also satisfy RuntimeContextAuthorizer: the whole point
// of the narrow interface is that a route which needs an agent claim cannot be
// wired to the authorizer that admits every live claim.
type agentRuntimeContextAuthorizerFunc func(
	context.Context,
	ContentClaim,
) (RuntimeContextAuthorization, error)

func (f agentRuntimeContextAuthorizerFunc) AuthorizeAgentRuntimeContext(
	ctx context.Context,
	claim ContentClaim,
) (RuntimeContextAuthorization, error) {
	return f(ctx, claim)
}

// TestPostgresAgentRuntimeContextNarrowsCapabilityWithoutWideningInputs pins
// where the nested route's extra restriction lives and what it costs.
//
// The filter is appended to the SAME query with the SAME five bind parameters,
// so nothing about workload identity, session, fence, generation or desired
// state is relaxed to gain it — and the broad method keeps serving the
// client-token route both capabilities legitimately reach.
func TestPostgresAgentRuntimeContextNarrowsCapabilityWithoutWideningInputs(t *testing.T) {
	t.Parallel()

	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-nested")
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{5}, sha256.Size)

	var broadQuery, agentQuery string
	var broadArgs, agentArgs []any
	capture := func(target *string, captured *[]any) contentQueryerFunc {
		return func(_ context.Context, query string, args ...any) pgx.Row {
			*target = query
			*captured = args
			return contentRowFunc(func(dest ...any) error {
				require.Len(t, dest, 4)
				*dest[0].(*int64) = 42
				*dest[1].(*string) = "17"
				*dest[2].(*string) = runtimeContextInitiatorUser
				*dest[3].(*string) = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
				return nil
			})
		}
	}
	claim := ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-nested",
		Generation:      9,
		ClaimID:         "claim-nested",
		FenceToken:      fence,
	}

	broadRepository, err := newPostgresContentRepository(capture(&broadQuery, &broadArgs))
	require.NoError(t, err)
	_, err = broadRepository.AuthorizeRuntimeContext(context.Background(), claim)
	require.NoError(t, err)

	agentRepository, err := newPostgresContentRepository(capture(&agentQuery, &agentArgs))
	require.NoError(t, err)
	_, err = agentRepository.AuthorizeAgentRuntimeContext(context.Background(), claim)
	require.NoError(t, err)

	// The narrowing, stated as SQL rather than as a comment.
	require.Contains(t, agentQuery, agentRuntimeContextCapabilityFilter)
	require.NotContains(t, broadQuery, agentRuntimeContextCapabilityFilter)
	normalizedAgentQuery := strings.Join(strings.Fields(agentQuery), " ")
	require.Contains(
		t,
		normalizedAgentQuery,
		"AND j.capability_id IN ( 'agent.execute.application.v1', 'agent.execute.adhoc.v1' )",
	)
	// No bind parameter carries the capability set: a caller cannot widen it.
	require.NotContains(t, agentRuntimeContextCapabilityFilter, "$")

	// Everything else the broad query checks is still checked, on the same
	// inputs. A narrowing that quietly dropped the session or fence predicate
	// would be a worse bug than the one it fixes.
	require.Equal(t, broadQuery+agentRuntimeContextCapabilityFilter, agentQuery)
	require.Equal(t, broadArgs, agentArgs)
	require.Equal(
		t,
		[]any{"claim-nested", "execution-nested", uint64(9), identity.String(), fence},
		agentArgs,
	)
}

// TestNestedApplicationVersionRouteRefusesRejectedCapabilityBeforeReadingAnything
// proves the refusal reaches the wire as a 403 and, just as importantly, that
// nothing was read or frozen on the way there. An index workload that is told
// "no" only after the version has been read out of the project's tenant schema
// would still have had the definition materialized on its behalf.
func TestNestedApplicationVersionRouteRefusesRejectedCapabilityBeforeReadingAnything(t *testing.T) {
	t.Parallel()

	server := newNestedApplicationVersionTestServerWithAuthorizer(
		t,
		agentRuntimeContextAuthorizerFunc(func(
			_ context.Context, claim ContentClaim,
		) (RuntimeContextAuthorization, error) {
			require.Equal(t, "execution-1", claim.ExecutionID)
			// What AuthorizeAgentRuntimeContext returns for a live
			// index.ingest.v1 claim: the row does not match, and the caller
			// cannot tell a refused capability from a stale claim.
			return RuntimeContextAuthorization{}, ErrContentUnauthorized
		}),
		currentApplicationVersionSourceFunc(func(
			context.Context, int64, int64, int64,
		) (CurrentApplicationVersionRecord, error) {
			t.Fatal("a refused claim must not read an application version")
			return CurrentApplicationVersionRecord{}, nil
		}),
		currentApplicationVersionFreezerFunc(func(
			context.Context, agentexecutionapp.CurrentApplicationVersionFreezeRequest,
		) (json.RawMessage, error) {
			t.Fatal("a refused claim must not freeze an application version")
			return nil, nil
		}),
	)

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nestedApplicationVersionRequest(
		t, certificateWithURI(mustNestedCapabilityURI(t)), bytes.Repeat([]byte{4}, sha256.Size), "7", "41",
	))
	require.Equal(t, http.StatusForbidden, response.Code)
	// The refusal must not leak which agent the claim tried to name.
	require.NotContains(t, response.Body.String(), "41")
}

func mustNestedCapabilityURI(t *testing.T) *url.URL {
	t.Helper()
	identity, err := url.Parse("spiffe://elitea.internal/runtime/worker-nested")
	require.NoError(t, err)
	return identity
}

// TestPostgresAgentRuntimeContextRefusesIndexClaimIntegration is the real
// proof, against a real PostgreSQL: one project, one workload session, two live
// claims — an index.ingest.v1 one and an agent.execute.application.v1 one, both
// admitted for project 42 by the broad authorizer that backs the client-token
// route. Only the agent claim may authorize the nested-version route.
//
// Before the capability filter, the index claim resolved here exactly like the
// agent claim did, which is what let an index workload freeze and read any
// agent version in the project it was admitted for.
func TestPostgresAgentRuntimeContextRefusesIndexClaimIntegration(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL agent runtime-context capability test")
	}
	pool := isolatedStoragePostgresPool(t, databaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		`CREATE SCHEMA elitea_runtime`,
		`CREATE TABLE elitea_runtime.execution_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    resource_project_id INTEGER NOT NULL,
    actor_id TEXT NOT NULL,
    -- Read by the toolkit.call_tool.v1 arm (toolkitCallToolInteractiveJob).
    principal_ref TEXT NOT NULL DEFAULT '',
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
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	identity, err := url.Parse("spiffe://elitea.internal/runtime/index-worker-capability")
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{3}, sha256.Size)
	// initiator 'user' on purpose: index ingest can be user-initiated
	// (0036_index_ingest_admission.sql CHECKs 'user'/'llm'/'schedule'), so the
	// initiator column this authorizer already returns cannot tell the two
	// claims apart. Only the capability can.
	for _, statement := range []string{
		`INSERT INTO elitea_runtime.execution_jobs
    (execution_id, generation, resource_project_id, actor_id, desired_state, capability_id)
VALUES ('execution-index', 1, 42, '17', 'RUNNING', 'index.ingest.v1')`,
		`INSERT INTO elitea_runtime.index_ingest_jobs
    (execution_id, generation, capability_id, initiator)
VALUES ('execution-index', 1, 'index.ingest.v1', 'user')`,
		`INSERT INTO elitea_runtime.execution_jobs
    (execution_id, generation, resource_project_id, actor_id, desired_state, capability_id)
VALUES ('execution-agent', 1, 42, '17', 'RUNNING', 'agent.execute.application.v1')`,
		`INSERT INTO elitea_runtime.agent_execution_jobs
    (execution_id, generation, capability_id, client_stream_id)
VALUES ('execution-agent', 1, 'agent.execute.application.v1',
        '5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.workload_sessions
    (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ('session-capability', $1, 'producer-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '5 minutes')`,
		identity.String()); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		claimID     string
		executionID string
	}{
		{claimID: "claim-index", executionID: "execution-index"},
		{claimID: "claim-agent", executionID: "execution-agent"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_claims
    (claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id, fence_token, lease_expires_at)
VALUES ($1, $2, 1, 'session-capability', $3, 'producer-1', $4, clock_timestamp() + interval '1 minute')`,
			seed.claimID, seed.executionID, identity.String(), fence); err != nil {
			t.Fatal(err)
		}
	}

	repository, err := NewPostgresContentRepository(pool)
	require.NoError(t, err)
	indexClaim := ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-index",
		Generation:      1,
		ClaimID:         "claim-index",
		FenceToken:      fence,
	}
	agentClaim := ContentClaim{
		PeerCertificate: certificateWithURI(identity),
		ExecutionID:     "execution-agent",
		Generation:      1,
		ClaimID:         "claim-agent",
		FenceToken:      fence,
	}

	// Unchanged: the client-token route still serves the index claim.
	indexAuthorization, err := repository.AuthorizeRuntimeContext(ctx, indexClaim)
	require.NoError(t, err)
	require.EqualValues(t, 42, indexAuthorization.ResourceProjectID)
	require.Equal(t, runtimeContextInitiatorUser, indexAuthorization.Initiator)

	// The fix: that same live claim cannot authorize the nested-version route.
	_, err = repository.AuthorizeAgentRuntimeContext(ctx, indexClaim)
	require.ErrorIs(t, err, ErrContentUnauthorized)

	// And the agent claim still can, with the same project and actor.
	agentAuthorization, err := repository.AuthorizeAgentRuntimeContext(ctx, agentClaim)
	require.NoError(t, err)
	require.EqualValues(t, 42, agentAuthorization.ResourceProjectID)
	require.Equal(t, "17", agentAuthorization.ActorID)
	require.Equal(t, runtimeContextInitiatorUser, agentAuthorization.Initiator)

	// The narrow method is narrower only in capability: every other predicate
	// still applies to an agent claim.
	revoked := agentClaim
	revoked.FenceToken = bytes.Repeat([]byte{9}, sha256.Size)
	_, err = repository.AuthorizeAgentRuntimeContext(ctx, revoked)
	require.ErrorIs(t, err, ErrContentUnauthorized)
}
