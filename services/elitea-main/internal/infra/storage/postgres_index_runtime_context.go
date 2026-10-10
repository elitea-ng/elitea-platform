package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AuthorizeRuntimeContext reuses the private content repository's verified
// workload identity and durable claim/session boundary. The resource project
// and actor are selected exclusively by the claimed execution. Agent chat
// execution is interactive in the current contract; scheduled index execution
// retains its existing project-system identity selection.
//
// It admits EVERY dispatchable capability on purpose: the client-token route it
// backs is reached by index ingest and agent execution alike. A route that is
// only meaningful to one of them must not use this method — see
// AuthorizeAgentRuntimeContext below.
func (r *PostgresContentRepository) AuthorizeRuntimeContext(
	ctx context.Context,
	claim ContentClaim,
) (RuntimeContextAuthorization, error) {
	return r.authorizeRuntimeContext(ctx, claim, "")
}

// AuthorizeAgentRuntimeContext is AuthorizeRuntimeContext narrowed to the two
// agent-execution capabilities, for routes that only an agent turn has any
// business reaching.
//
// The narrowing is the authorization, not a tidier error: the project and actor
// come from the claimed execution row, but the nested-version route lets the
// REQUEST name the application and version inside that project. An
// index.ingest.v1 claim carries a real resource_project_id, so without this
// filter an index workload could freeze and read any agent version in the
// project it was admitted for. Initiator cannot stand in for the capability
// either — index_ingest_jobs.initiator is CHECK-constrained to
// ('user', 'llm', 'schedule') (migrations/shared/0036_index_ingest_admission.sql),
// so an index job reports the same 'user' an interactive agent turn does.
func (r *PostgresContentRepository) AuthorizeAgentRuntimeContext(
	ctx context.Context,
	claim ContentClaim,
) (RuntimeContextAuthorization, error) {
	return r.authorizeRuntimeContext(ctx, claim, agentRuntimeContextCapabilityFilter)
}

// agentRuntimeContextCapabilityFilter is appended to the shared authorization
// query's WHERE clause. It carries no bind parameter deliberately: the allowed
// set is a compile-time property of the route, never anything a request or a
// caller can widen.
const agentRuntimeContextCapabilityFilter = `
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )`

// toolkitCallToolInteractiveJob admits a toolkit.call_tool.v1 claim, which
// the query labels initiator 'user'.
//
// A tool run (test_tool, a remote toolkit call, an MCP toolkit tool, a
// provider's source-tool callback) has no per-capability detail row to join:
// it is admitted and dispatched inside one synchronous request
// (internal/db/queries/runtime_toolkit_call_tool.sql says why). Every
// admission goes through toolkitcalltool.Service, whose RunRequest requires a
// positive ActorUserID — the authenticated user who asked — and writes it as
// BOTH actor_id and principal_ref. No schedule or system path creates one.
//
// So the job row itself must carry that interactive shape: a positive
// integer user id as actor_id, repeated as principal_ref. A row in any other
// shape (a system or schedule actor, a principal that is not the actor) is
// not a user's run and is refused rather than labelled 'user'. A new
// non-interactive creator must add a detail row with its own initiator, as
// index_ingest_jobs does, and an arm here that reads it.
const toolkitCallToolInteractiveJob = `
          j.capability_id = 'toolkit.call_tool.v1'
          AND j.actor_id ~ '^[1-9][0-9]{0,18}$'
          AND j.principal_ref = j.actor_id`

func (r *PostgresContentRepository) authorizeRuntimeContext(
	ctx context.Context,
	claim ContentClaim,
	capabilityFilter string,
) (RuntimeContextAuthorization, error) {
	workloadID, err := workloadIdentity(claim.PeerCertificate)
	if err != nil {
		return RuntimeContextAuthorization{}, ErrContentUnauthorized
	}

	var authorization RuntimeContextAuthorization
	err = r.store.QueryRow(ctx, `
SELECT j.resource_project_id,
       j.actor_id,
       CASE
           WHEN j.capability_id = 'index.ingest.v1' THEN i.initiator
           WHEN j.capability_id IN (
               'agent.execute.application.v1',
               'agent.execute.adhoc.v1'
           ) THEN 'user'
           -- A toolkit tool run (test_tool, a remote toolkit call, a provider's
           -- source-tool callback) is created only on a user's request, as
           -- that user: its worker builds the SDK client from this token
           -- before it runs the tool, so without this arm every such run
           -- failed AUTHORIZATION_FAILED ("Execution authorization failed.").
           -- 'user' only for the interactive job shape the WHERE clause
           -- requires (toolkitCallToolInteractiveJob).
           WHEN j.capability_id = 'toolkit.call_tool.v1' THEN 'user'
       END AS initiator,
       -- The claimed AGENT execution's own conversation
       -- (chat_conversations.uuid). COALESCE, not a required column: an index
       -- ingest claim legitimately joins no agent row, and this same query
       -- serves both capabilities. A route that authorizes on the conversation
       -- must therefore require it rather than assume it is populated -- see
       -- RuntimeContextAuthorization.ConversationID.
       COALESCE(a.client_stream_id, '') AS conversation_id
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
LEFT JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
LEFT JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id
 AND a.generation = j.generation
 AND a.capability_id = j.capability_id
JOIN elitea_runtime.workload_sessions AS ws
  ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity
 AND ws.producer_id = c.producer_id
WHERE c.claim_id = $1
  AND c.execution_id = $2
  AND c.generation = $3
  AND c.workload_identity = $4
  AND c.fence_token = $5
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND ws.issued_at <= clock_timestamp()
  AND ws.expires_at > clock_timestamp()
  AND ws.revoked_at IS NULL
  AND j.desired_state = 'RUNNING'
  AND (
      (j.capability_id = 'index.ingest.v1' AND i.execution_id IS NOT NULL)
      OR
      (
          j.capability_id IN (
              'agent.execute.application.v1',
              'agent.execute.adhoc.v1'
          )
          AND a.execution_id IS NOT NULL
      )
      OR
      (`+toolkitCallToolInteractiveJob+`)
  )`+capabilityFilter,
		claim.ClaimID,
		claim.ExecutionID,
		claim.Generation,
		workloadID,
		claim.FenceToken,
	).Scan(
		&authorization.ResourceProjectID,
		&authorization.ActorID,
		&authorization.Initiator,
		&authorization.ConversationID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeContextAuthorization{}, ErrContentUnauthorized
	}
	if err != nil {
		return RuntimeContextAuthorization{}, fmt.Errorf("authorize runtime context: %w", err)
	}
	if authorization.ResourceProjectID <= 0 {
		return RuntimeContextAuthorization{}, errors.New("authorize runtime context: invalid project")
	}
	if authorization.ActorID == "" || authorization.Initiator == "" {
		return RuntimeContextAuthorization{}, errors.New("authorize runtime context: invalid execution identity")
	}
	return authorization, nil
}

var (
	_ RuntimeContextAuthorizer      = (*PostgresContentRepository)(nil)
	_ AgentRuntimeContextAuthorizer = (*PostgresContentRepository)(nil)
)
