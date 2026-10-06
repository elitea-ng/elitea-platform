package repos

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5"
)

func (r *ConversationsRepo) rejectEditorTestParticipants(ctx context.Context, projectID, conversationID string) error {
	current, err := r.Get(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	if current.Source == conversations.EditorTestSource {
		return apierr.BadRequest("editor Test participants are immutable")
	}
	return nil
}

// EditorTestRuns reads existing receipts and frozen input identities in one read-only snapshot.
func (r *ConversationsRepo) EditorTestRuns(ctx context.Context, projectID, conversationID string, limit, offset int) (conversations.EditorTestRunsPage, error) {
	page := conversations.EditorTestRunsPage{Rows: []conversations.EditorTestRun{}, Limit: limit, Offset: offset}
	if limit < 1 || limit > 50 || offset < 0 || offset > 10000 {
		return page, apierr.BadRequest("invalid editor Test run page")
	}
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return page, err
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return page, err
	}
	predicate, ok := idPredicate(conversationID)
	if !ok {
		return page, apierr.NotFound("conversation not found")
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var isTest bool
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(source='editor_test' AND meta->'editor_test'->>'project_id'=$2,FALSE) FROM %s.chat_conversations c WHERE %s`, s, predicate), conversationID, projectID).Scan(&isTest)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !isTest) {
		return page, apierr.NotFound("editor Test conversation not found")
	}
	if err != nil {
		return page, err
	}
	query := fmt.Sprintf(`SELECT response.id,
 COALESCE(binding.execution_id=response.task_id AND binding.client_execution_generation=response.meta->>'execution_generation'
  AND EXISTS (SELECT 1 FROM %[1]s.chat_message_trace_step trace WHERE trace.message_group_id=response.id
   AND (trace.kind<>'thinking_step' OR trace.has_visible_content)),FALSE),response.uuid::text,question.uuid::text,job.execution_id,binding.client_execution_generation,
 CASE WHEN binding.execution_id IS DISTINCT FROM response.task_id OR binding.client_execution_generation IS DISTINCT FROM response.meta->>'execution_generation' THEN 'TERMINAL'
 WHEN NOT response.is_streaming AND (response.meta ? 'hitl_interrupt' OR response.meta ? 'hitl_interrupts' OR response.meta ? 'authorization_requests' OR response.meta->'output_limit_reached'='true'::jsonb OR response.meta ? 'pipeline_static_v1' OR response.meta ? 'node_recovery_required_v1') THEN 'PAUSED'
 WHEN job.settled_at IS NOT NULL THEN 'TERMINAL' ELSE 'RUNNING' END,
 job.state,job.desired_state,job.admitted_at,job.settled_at,
 job.input_bundle_id,binding.request_entry_id,entry.entry_version,encode(entry.content_digest,'hex'),
 COALESCE((job.actor_id=$3 AND c.meta->'editor_test'->>'actor_id'=$3 AND binding.execution_id=response.task_id AND binding.client_execution_generation=response.meta->>'execution_generation'),FALSE)
 FROM %[1]s.chat_conversations c
 JOIN %[1]s.chat_message_group response ON response.conversation_id=c.id
 JOIN %[1]s.chat_message_group question ON question.id=response.reply_to_id AND question.conversation_id=c.id
 JOIN elitea_runtime.agent_execution_jobs binding ON binding.client_message_id=response.uuid::text
  AND binding.client_stream_id=c.uuid::text
 JOIN elitea_runtime.execution_jobs job ON job.execution_id=binding.execution_id AND job.generation=binding.generation
  AND job.capability_id=binding.capability_id AND job.input_bundle_id=binding.input_bundle_id
 JOIN elitea_runtime.input_bundle_entries entry ON entry.input_bundle_id=job.input_bundle_id AND entry.entry_id=binding.request_entry_id
 WHERE %[2]s AND c.source='editor_test' AND job.tenant_id=$2
  AND job.resource_project_id=$2::integer AND job.projection_project_id=$2::integer
  AND job.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
 ORDER BY job.admitted_at DESC,job.execution_id DESC,job.generation DESC,response.id DESC LIMIT $4 OFFSET $5`, s, predicate)
	rows, err := tx.Query(ctx, query, conversationID, projectID, strconv.FormatInt(access.ActorID, 10), limit+1, offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var run conversations.EditorTestRun
		if err := rows.Scan(&run.ResponseMessageGroupID, &run.TraceAvailable, &run.ResponseMessageID, &run.QuestionID, &run.ExecutionID, &run.ExecutionGeneration, &run.Phase, &run.State, &run.DesiredState, &run.AdmittedAt, &run.SettledAt, &run.InputReference.BundleID, &run.InputReference.EntryID, &run.InputReference.ImmutableVersion, &run.InputReference.ContentDigest, &run.CanControl); err != nil {
			return page, err
		}
		if run.CanControl && run.Phase == "RUNNING" {
			run.EventsURL = "/api/v2/executions/" + url.PathEscape(projectID) + "/" + url.PathEscape(run.ExecutionID) + "/events"
		}
		page.Rows = append(page.Rows, run)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	rows.Close()
	if len(page.Rows) > limit {
		page.HasMore = true
		page.Rows = page.Rows[:limit]
	}
	if err := tx.Commit(ctx); err != nil {
		return page, err
	}
	return page, nil
}
