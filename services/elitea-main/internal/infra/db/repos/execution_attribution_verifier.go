package repos

// The /llm edge's check on an inbound X-Elitea-Execution-Id
// (internal/llmproxy ExecutionVerifier).
//
// The id is signed into the gateway request log, and the analytics reads
// decide two things from it: a call whose id names a schedule, webhook or
// index execution leaves every active-user figure, and a call is added to the
// spend of the execution it names. The runtime worker calls /llm with the
// execution actor's own access token, so the edge cannot tell the worker from
// that person's SDK client by the credential. It checks the execution instead.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// attributionSlackSQL is how far outside an execution's lifetime a call may
// fall and still be attributed to it. It covers clock skew between the
// gateway and the database, and a worker's last call racing settlement. The
// edge check and the analytics reads use the same value, so a call the edge
// accepted is a call the reads count.
const attributionSlackSQL = `interval '5 minutes'`

// ExecutionAttributionVerifier implements llmproxy.ExecutionVerifier over
// elitea_runtime.execution_jobs.
type ExecutionAttributionVerifier struct {
	pool *pgxpool.Pool
}

func NewExecutionAttributionVerifier(pool *pgxpool.Pool) *ExecutionAttributionVerifier {
	return &ExecutionAttributionVerifier{pool: pool}
}

// VerifyExecution reports whether executionID names an execution that
//
//   - belongs to projectID (either of the two project columns, the guard the
//     analytics reads apply),
//   - runs as userID (actor_id is the user id the worker's token resolves to),
//   - is live: not settled, or settled less than attributionSlackSQL ago.
//
// Any generation qualifies: a retry runs under the same id and actor.
func (v *ExecutionAttributionVerifier) VerifyExecution(ctx context.Context, projectID, userID, executionID string) (bool, error) {
	if v == nil || v.pool == nil {
		return false, nil
	}
	project, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || project < 1 || userID == "" || executionID == "" {
		return false, nil
	}
	var ok bool
	if err := v.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.execution_jobs AS j
    WHERE j.execution_id = $1
      AND (j.resource_project_id = $2 OR j.projection_project_id = $2)
      AND j.actor_id = $3
      AND (j.settled_at IS NULL OR j.settled_at > now() - `+attributionSlackSQL+`)
)`, executionID, project, userID).Scan(&ok); err != nil {
		return false, fmt.Errorf("verify execution attribution: %w", err)
	}
	return ok, nil
}

// VerifyCallbackExecution implements llmproxy.CallbackExecutionVerifier: a
// provider invocation's execution id `callback-<uuid>` is kept only when
// tokenUUID names the token that authenticated the request (tokenID), owned
// by userID, bound to projectID (every callback token is bound), and not
// expired more than attributionSlackSQL ago. A callback token is minted per
// invocation and revoked or expired after it, so liveness is the token's.
func (v *ExecutionAttributionVerifier) VerifyCallbackExecution(ctx context.Context, projectID, userID, tokenID, tokenUUID string) (bool, error) {
	if v == nil || v.pool == nil {
		return false, nil
	}
	project, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || project < 1 {
		return false, nil
	}
	user, err := strconv.ParseInt(userID, 10, 32)
	if err != nil || user < 1 {
		return false, nil
	}
	token, err := strconv.ParseInt(tokenID, 10, 32)
	if err != nil || token < 1 || tokenUUID == "" || len(tokenUUID) > 36 {
		return false, nil
	}
	var ok bool
	if err := v.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__token AS t
    JOIN elitea_identity.token_project_binding AS b ON b.token_id = t.id
    WHERE t.uuid = $1
      AND t.id = $2
      AND t.user_id = $3
      AND b.project_id = $4
      AND (t.expires IS NULL OR t.expires > (clock_timestamp() AT TIME ZONE 'UTC') - `+attributionSlackSQL+`)
)`, tokenUUID, token, user, project).Scan(&ok); err != nil {
		return false, fmt.Errorf("verify callback execution attribution: %w", err)
	}
	return ok, nil
}
