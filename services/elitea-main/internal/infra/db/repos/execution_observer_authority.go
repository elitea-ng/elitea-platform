package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
)

// ExecutionObserverAuthority decides who may replay an AGENT execution's
// durable event log.
//
// The events route used to admit any project member who held
// models.chat.messages.create. The log carries the answer, the tool inputs and
// outputs and attachment-derived text, so a member who learned another user's
// execution id could replay that user's private chat from cursor 0. The web app
// now subscribes to the last turn's task_id read from conversation history, so
// the route is a cross-user read path and needs a binding to the execution.
//
// A principal may observe the execution when it STARTED it (job.actor_id), or
// when it may read the conversation the execution answers (the agent
// admission's client_stream_id), under the same visibility rule the chat
// detail reads apply. A participant of a shared conversation therefore keeps
// the reattach after a reload; a member who is not a participant of a private
// conversation does not.
type ExecutionObserverAuthority struct {
	pool *pgxpool.Pool
}

// NewExecutionObserverAuthority returns an authority on pool. It refuses a nil
// pool, so a composition root cannot build an authority that admits nobody by
// accident or everybody by omission.
func NewExecutionObserverAuthority(pool *pgxpool.Pool) (*ExecutionObserverAuthority, error) {
	if pool == nil {
		return nil, errors.New("execution observer authority requires a database pool")
	}
	return &ExecutionObserverAuthority{pool: pool}, nil
}

// MayObserveAgentExecution reports whether principal may replay the events of
// executionID in project. An unknown execution, a principal with no owning
// user, and an execution with no conversation that principal did not start all
// answer false.
func (a *ExecutionObserverAuthority) MayObserveAgentExecution(
	ctx context.Context,
	project int64,
	executionID string,
	principal auth.User,
) (bool, error) {
	actor, ok := principal.OwningUserID()
	if !ok || project <= 0 || executionID == "" {
		return false, nil
	}
	var actorID, conversationUUID string
	err := a.pool.QueryRow(ctx, `
		SELECT job.actor_id, COALESCE(agent.client_stream_id, '')
		FROM elitea_runtime.execution_jobs AS job
		LEFT JOIN elitea_runtime.agent_execution_jobs AS agent
		  ON agent.execution_id = job.execution_id AND agent.generation = job.generation
		WHERE job.execution_id = $1
		  AND job.resource_project_id = $2
		ORDER BY job.generation DESC
		LIMIT 1`, executionID, project).Scan(&actorID, &conversationUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve agent execution observer: %w", err)
	}
	if actorID == strconv.FormatInt(actor, 10) {
		return true, nil
	}
	if conversationUUID == "" {
		return false, nil
	}
	return a.mayReadConversation(ctx, strconv.FormatInt(project, 10), conversationUUID, principal)
}

func (a *ExecutionObserverAuthority) mayReadConversation(
	ctx context.Context,
	projectID string,
	conversationUUID string,
	principal auth.User,
) (bool, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return false, nil
	}
	var present bool
	if err := a.pool.QueryRow(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, schema+".chat_conversations",
	).Scan(&present); err != nil {
		return false, fmt.Errorf("resolve agent execution conversation schema: %w", err)
	}
	if !present {
		return false, nil
	}
	access, err := chatauthority.Load(auth.ContextWithUser(ctx, principal), a.pool, projectID)
	if err != nil {
		return false, fmt.Errorf("load agent execution conversation access: %w", err)
	}
	visible, args := access.Predicate(schema, "c", 2, chatauthority.Detail)
	var allowed bool
	err = a.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS (SELECT 1 FROM %s.chat_conversations c WHERE c.uuid::text = $1 AND %s)`,
		schema, visible,
	), append([]any{conversationUUID}, args...)...).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("authorize agent execution conversation: %w", err)
	}
	return allowed, nil
}
