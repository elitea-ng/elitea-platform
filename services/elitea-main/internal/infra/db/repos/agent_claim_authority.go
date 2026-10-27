package repos

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// agentClaimMode selects which live agent claim a private Worker route
// accepts. It is the only difference between the node recovery control
// authority and the per-interrupt decision ledger authority (FU-063): both
// lock the same rows with the same lease, session, deadline, manifest and
// terminal predicates.
type agentClaimMode struct {
	recoveryModes []string
	desiredStates []string
}

var (
	// nodeRecoveryClaimMode: a NODE_RECOVERY claim of a job suspended for
	// recovery (or resumed, while it reconciles).
	nodeRecoveryClaimMode = agentClaimMode{recoveryModes: []string{"NODE_RECOVERY"}, desiredStates: []string{"SUSPENDED", "RUNNING"}}
	// interruptClaimMode: an ordinary RUNNING claim, first or reclaimed with
	// model-checkpoint recovery (the production reclaim label of the same
	// ordinary claim); a NODE_RECOVERY claim is refused
	// (fanout-interrupt-decisions-v1.md section 7).
	interruptClaimMode = agentClaimMode{recoveryModes: []string{"NONE", "AGENT_MODEL_CHECKPOINT"}, desiredStates: []string{"RUNNING"}}
)

// agentClaimScope is what the authority returns: the execution's resource
// project, its original actor, the bound root response, the desired state
// and the immutable input manifest.
type agentClaimScope struct {
	projectID      int64
	actorID        int64
	responseID     string
	desired        string
	bundleID       string
	manifestDigest []byte
}

// agentClaimAuthoritySQL is the one live-claim authority of the private agent
// routes. $6 is the accepted claim recovery modes and $7 the accepted desired states.
const agentClaimAuthoritySQL = `
SELECT j.resource_project_id, j.actor_id::bigint, binding.client_message_id, j.desired_state, j.input_bundle_id, bundle.manifest_digest
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.execution_claims c USING (execution_id, generation)
JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity AND ws.producer_id = c.producer_id
JOIN elitea_runtime.command_outbox command USING (execution_id, generation)
JOIN elitea_runtime.agent_execution_jobs binding USING (execution_id, generation)
JOIN elitea_runtime.input_bundles bundle ON bundle.input_bundle_id = j.input_bundle_id
WHERE c.claim_id = $1 AND c.execution_id = $2 AND c.generation = $3 AND c.workload_identity = $4 AND c.fence_token = $5
 AND c.released_at IS NULL AND c.lease_expires_at > clock_timestamp() AND c.recovery_mode = ANY($6::text[])
 AND ws.issued_at <= clock_timestamp() AND ws.expires_at > clock_timestamp() AND ws.revoked_at IS NULL
 AND j.state = 'RUNNING' AND j.desired_state = ANY($7::text[])
 AND j.tenant_id = j.resource_project_id::text AND j.projection_project_id = j.resource_project_id
 AND j.actor_id ~ '^[1-9][0-9]{0,17}$'
 AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1') AND binding.capability_id = j.capability_id
 AND command.retired_at IS NULL AND command.authority_granted_at IS NOT NULL AND command.deadline > clock_timestamp()
 AND NOT EXISTS (SELECT 1 FROM elitea_runtime.output_inbox terminal
                 WHERE terminal.execution_id = j.execution_id AND terminal.generation = j.generation)
FOR UPDATE OF j, c, ws, command`

// errAgentClaimRefused is returned when no live claim matches; callers map
// it to their own typed refusal.
var errAgentClaimRefused = errors.New("agent claim is not the live claim")

// lockAgentClaim takes the job, claim, session and command locks of the live
// claim. The first lookup takes the locks; a READ COMMITTED statement that
// waited for them does not re-read its joined rows, so the second lookup
// re-evaluates the lease, session, deadline and terminal predicates with the
// database clock while the locks are held.
func lockAgentClaim(ctx context.Context, tx sqlExecutor, mode agentClaimMode, claimID, executionID string, generation int64, workloadIdentity string, fence []byte) (agentClaimScope, error) {
	var scope agentClaimScope
	for range 2 {
		err := tx.QueryRow(ctx, agentClaimAuthoritySQL, claimID, executionID, generation, workloadIdentity, fence, mode.recoveryModes, mode.desiredStates).
			Scan(&scope.projectID, &scope.actorID, &scope.responseID, &scope.desired, &scope.bundleID, &scope.manifestDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return agentClaimScope{}, errAgentClaimRefused
		}
		if err != nil {
			return agentClaimScope{}, err
		}
	}
	if len(scope.manifestDigest) != 32 || scope.bundleID == "" {
		return agentClaimScope{}, errAgentClaimRefused
	}
	return scope, nil
}
