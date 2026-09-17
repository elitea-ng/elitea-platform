package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// CurrentAgentContextPolicyRepository uses existing product and input storage.
// The caller resolves and authorizes the conversation before invoking this source.
type CurrentAgentContextPolicyRepository struct {
	pool          *pgxpool.Pool
	conversations *ConversationsRepo
	defaults      *UserContextDefaultsRepo
}

func NewCurrentAgentContextPolicyRepository(pool *pgxpool.Pool) *CurrentAgentContextPolicyRepository {
	return &CurrentAgentContextPolicyRepository{pool: pool, conversations: NewConversationsRepo(pool), defaults: NewUserContextDefaultsRepo(pool)}
}

func (r *CurrentAgentContextPolicyRepository) ContextStrategy(ctx context.Context, projectID, actorID int64, conversationID string) (contextsettings.Strategy, error) {
	if !validContextPolicyIdentity(projectID, actorID, conversationID) {
		return contextsettings.Strategy{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	state, err := r.conversations.GetContextState(ctx, strconv.FormatInt(projectID, 10), conversationID)
	if err != nil {
		return contextsettings.Strategy{}, err
	}
	defaults, err := r.defaults.ContextDefaults(ctx, actorID)
	if err != nil {
		return contextsettings.Strategy{}, err
	}
	return contextsettings.Resolve(state.Strategy, defaults), nil
}

const continuationContextPolicySQL = `
SELECT e.content_bytes, e.content_digest
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.agent_execution_jobs a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
 AND a.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.input_bundle_entries e
  ON e.input_bundle_id = j.input_bundle_id AND e.entry_id = a.request_entry_id
WHERE j.tenant_id = $1::text AND j.resource_project_id = $1::integer
  AND j.projection_project_id = $1::integer AND j.actor_id = $2::text
  AND a.client_stream_id = $3::text AND a.client_message_id = $4::text
  AND a.client_execution_generation = $5::text
  AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  AND e.semantic_role = '` + executiondomain.AgentExecutionRequestRole + `'
ORDER BY j.admitted_at DESC, j.generation DESC
LIMIT 1`

func (r *CurrentAgentContextPolicyRepository) ContinuationContextPolicy(
	ctx context.Context, projectID, actorID int64, conversationID, responseID, generation string,
) (agentexecutionapp.FrozenContextPolicy, error) {
	if !validContextPolicyIdentity(projectID, actorID, conversationID) || uuid.Validate(responseID) != nil || uuid.Validate(generation) != nil {
		return agentexecutionapp.FrozenContextPolicy{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	var encoded, storedDigest []byte
	err := r.pool.QueryRow(ctx, continuationContextPolicySQL, strconv.FormatInt(projectID, 10), strconv.FormatInt(actorID, 10), conversationID, responseID, generation).Scan(&encoded, &storedDigest)
	if err != nil {
		return agentexecutionapp.FrozenContextPolicy{}, fmt.Errorf("read admitted context policy: %w", err)
	}
	return decodeFrozenContextPolicy(encoded, storedDigest, conversationID, generation)
}

func decodeFrozenContextPolicy(encoded, storedDigest []byte, conversationID, generation string) (agentexecutionapp.FrozenContextPolicy, error) {
	invalid := errors.New("stored execution context policy is invalid")
	if len(encoded) == 0 || len(encoded) > executiondomain.MaxAgentExecutionInputBytes {
		return agentexecutionapp.FrozenContextPolicy{}, invalid
	}
	digest := sha256.Sum256(encoded)
	if !bytes.Equal(storedDigest, digest[:]) {
		return agentexecutionapp.FrozenContextPolicy{}, invalid
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(encoded, &input) != nil || input.GetSchemaRevision() != "elitea.runtime.agent-execution-input.v1" ||
		input.GetConversationId() != conversationID || input.GetExecutionGeneration() != generation {
		return agentexecutionapp.FrozenContextPolicy{}, invalid
	}
	return agentexecutionapp.FrozenContextPolicy{Settings: input.ContextSettings, SummaryModel: input.SummaryModel}, nil
}

func validContextPolicyIdentity(projectID, actorID int64, conversationID string) bool {
	const maxRowID = 1<<31 - 1
	return projectID > 0 && projectID <= maxRowID && actorID > 0 && actorID <= maxRowID && uuid.Validate(conversationID) == nil
}
