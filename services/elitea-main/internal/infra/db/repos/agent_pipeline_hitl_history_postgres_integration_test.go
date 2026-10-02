package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresDirectPipelineHITLHistoryAtomicSegments(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	for _, action := range []string{"approve", "reject", "edit"} {
		t.Run(action, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
				t.Fatal(err)
			}
			q := sqlcgen.New(tx)
			conversation := "10000000-0000-4000-8000-000000000031"
			question := "20000000-0000-4000-8000-000000000061"
			response := "40000000-0000-4000-8000-000000000061"
			responseID := insertPostgresCurrentApplicationTurn(t, q, mustCurrentPGUUID(t, conversation), question,
				"30000000-0000-4000-8000-000000000061", response, "Create a joke.", "execution-paused")
			review := "Here is the joke:\n\n  Exact static review.  "
			metadata, _ := json.Marshal(map[string]any{"thread_id": "pipeline-thread", "execution_generation": question,
				"hitl_interrupt": map[string]any{"interaction_type": "pipeline_hitl_node", "history_contract_version": 1, "interrupt_id": "review-1", "node_name": "review", "message": review, "available_actions": []string{"approve", "reject", "edit"}}})
			if _, err := tx.Exec(t.Context(), `UPDATE application_versions SET agent_type='pipeline' WHERE id=41`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET is_streaming=false,meta=$2::jsonb WHERE uuid=$1`, responseID, metadata); err != nil {
				t.Fatal(err)
			}
			var responseGroup int64
			if err := tx.QueryRow(t.Context(), `SELECT id FROM chat_message_group WHERE uuid=$1`, responseID).Scan(&responseGroup); err != nil {
				t.Fatal(err)
			}
			item, err := q.InsertCurrentAgentTextItem(t.Context(), responseGroup)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.InsertCurrentAgentTextContent(t.Context(), sqlcgen.InsertCurrentAgentTextContentParams{ItemID: int64(item), Content: "provisional generation"}); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(t.Context(), `UPDATE chat_message_items SET meta='{"runtime_stream_provisional":true}' WHERE id=$1`, item); err != nil {
				t.Fatal(err)
			}
			value := ""
			want := "Approved"
			if action == "reject" {
				want = "Rejected"
			}
			if action == "edit" {
				value = "  Make it about bears.\nKeep it short.  "
				want = value
			}
			decisions, _ := json.Marshal([]agentexecutionapp.CurrentHITLDecision{{InterruptID: "review-1", Action: action, Value: value}})
			turn := agentexecutionapp.CurrentContinueTurn{ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation, TargetParticipantID: 21,
				Kind: agentexecutionapp.CurrentRegenerationApplication, ApplicationID: 31, ApplicationVersionID: 41,
				QuestionID: question, ResponseMessageID: response, ExecutionGeneration: question, ThreadID: "pipeline-thread", InterruptID: "review-1", Action: action, HITLDecisions: decisions,
				PipelineHITLReview: &agentexecutionapp.CurrentPipelineHITLReview{InterruptID: "review-1", NodeName: "review", Message: review}}
			// Reject a changed review under the transaction lock and roll back consumption.
			savepoint, err := tx.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			altered := turn.Clone()
			altered.PipelineHITLReview.Message = "different review"
			if err := resumeCurrentAgentHITL(t.Context(), sqlcgen.New(savepoint), "execution-resumed", *altered); !errors.Is(err, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved) {
				t.Fatalf("changed review error=%v", err)
			}
			if err := savepoint.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			// A successful segmentation also rolls back as one unit.
			savepoint, err = tx.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := resumeCurrentAgentHITL(t.Context(), sqlcgen.New(savepoint), "execution-resumed", turn); err != nil {
				t.Fatal(err)
			}
			if err := savepoint.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			var groups int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM chat_message_group WHERE conversation_id=1`).Scan(&groups); err != nil || groups != 2 {
				t.Fatalf("rollback groups=%d error=%v", groups, err)
			}
			if err := resumeCurrentAgentHITL(t.Context(), q, "execution-resumed", turn); err != nil {
				t.Fatal(err)
			}
			if err := resumeCurrentAgentHITL(t.Context(), q, "execution-duplicate", turn); !errors.Is(err, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved) {
				t.Fatalf("duplicate error=%v", err)
			}
			var static, decision, thread, task, oldTask string
			var streaming, oldStreaming bool
			var count int
			err = tx.QueryRow(t.Context(), `SELECT rt.content,dt.content,c.meta->>'thread_id',c.task_id,c.is_streaming,r.task_id,r.is_streaming,
   (SELECT count(*) FROM chat_message_group WHERE conversation_id=1)
   FROM chat_message_group r JOIN chat_message_items ri ON ri.message_group_id=r.id JOIN chat_messages_text rt ON rt.id=ri.id
   JOIN chat_message_group d ON d.uuid=$2 JOIN chat_message_items di ON di.message_group_id=d.id JOIN chat_messages_text dt ON dt.id=di.id
   JOIN chat_message_group c ON c.uuid=$3 AND c.reply_to_id=d.id
   WHERE r.uuid=$1 AND d.author_participant_id=20 AND c.author_participant_id=21`, responseID, uuid.MustParse(turn.PipelineDecisionID()), uuid.MustParse(turn.ProjectionResponseID())).Scan(&static, &decision, &thread, &task, &streaming, &oldTask, &oldStreaming, &count)
			if err != nil {
				t.Fatal(err)
			}
			if static != review || decision != want || thread != "pipeline-thread" || task != "execution-resumed" || !streaming || oldTask != "execution-paused" || oldStreaming || count != 4 {
				t.Fatalf("incorrect segmentation static=%q decision=%q thread=%q task=%q streaming=%v old=%q/%v groups=%d", static, decision, thread, task, streaming, oldTask, oldStreaming, count)
			}
		})
	}
}

// Competing decisions must serialize on the persisted pause. Observe the real
// PostgreSQL lock before releasing the first transaction, rather than assuming
// goroutine scheduling produced an overlap.
func TestPostgresDirectPipelineHITLHistoryCompetingDecisions(t *testing.T) {
	for _, commitFirst := range []bool{true, false} {
		name := "first_commits"
		if !commitFirst {
			name = "first_rolls_back"
		}
		t.Run(name, func(t *testing.T) {
			pool := newMigratedPostgresIntegrationPool(t)
			seedCurrentAgentContinuationSchema(t, pool)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			begin := func() pgx.Tx {
				tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
				if err := tenant.BindProject(ctx, tx, tenant.Project{ID: 1}); err != nil {
					t.Fatal(err)
				}
				return tx
			}
			setup := begin()
			conversation := "10000000-0000-4000-8000-000000000031"
			question := "20000000-0000-4000-8000-000000000061"
			response := "40000000-0000-4000-8000-000000000061"
			responseID := insertPostgresCurrentApplicationTurn(t, sqlcgen.New(setup), mustCurrentPGUUID(t, conversation), question,
				"30000000-0000-4000-8000-000000000061", response, "Create a joke.", "execution-paused")
			review := "Static review for competing decisions."
			metadata, err := json.Marshal(map[string]any{"thread_id": "pipeline-thread", "execution_generation": question,
				"hitl_interrupt": map[string]any{"interaction_type": "pipeline_hitl_node", "history_contract_version": 1,
					"interrupt_id": "review-1", "node_name": "review", "message": review, "available_actions": []string{"approve", "reject", "edit"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := setup.Exec(ctx, `UPDATE application_versions SET agent_type='pipeline' WHERE id=41`); err != nil {
				t.Fatal(err)
			}
			if _, err := setup.Exec(ctx, `UPDATE chat_message_group SET is_streaming=false,meta=$2::jsonb WHERE uuid=$1`, responseID, metadata); err != nil {
				t.Fatal(err)
			}
			if err := setup.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			turn := agentexecutionapp.CurrentContinueTurn{ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation, TargetParticipantID: 21,
				Kind: agentexecutionapp.CurrentRegenerationApplication, ApplicationID: 31, ApplicationVersionID: 41,
				QuestionID: question, ResponseMessageID: response, ExecutionGeneration: question, ThreadID: "pipeline-thread", InterruptID: "review-1",
				Action: "approve", HITLDecisions: json.RawMessage(`[{"interrupt_id":"review-1","action":"approve","value":""}]`),
				PipelineHITLReview: &agentexecutionapp.CurrentPipelineHITLReview{InterruptID: "review-1", NodeName: "review", Message: review}}
			first, second := begin(), begin()
			firstPID, secondPID := first.Conn().PgConn().PID(), second.Conn().PgConn().PID()
			if err := resumeCurrentAgentHITL(ctx, sqlcgen.New(first), "execution-first", turn); err != nil {
				t.Fatal(err)
			}
			contender := turn.Clone()
			contender.Action = "reject"
			contender.HITLDecisions = json.RawMessage(`[{"interrupt_id":"review-1","action":"reject","value":""}]`)
			result := make(chan error, 1)
			go func() {
				err := resumeCurrentAgentHITL(ctx, sqlcgen.New(second), "execution-second", *contender)
				if err == nil {
					err = second.Commit(ctx)
				} else {
					_ = second.Rollback(context.Background())
				}
				result <- err
			}()
			// On any assertion failure, release the first lock and join the contender.
			joined := false
			defer func() {
				cancel()
				_ = first.Rollback(context.Background())
				if !joined {
					<-result
				}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := pool.QueryRow(ctx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, firstPID, secondPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-result:
					joined = true
					t.Fatalf("contender completed before observing the first transaction lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			var releaseErr error
			if commitFirst {
				releaseErr = first.Commit(ctx)
			} else {
				releaseErr = first.Rollback(ctx)
			}
			if releaseErr != nil {
				t.Fatal(releaseErr)
			}
			contenderErr := <-result
			joined = true
			if commitFirst && !errors.Is(contenderErr, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved) {
				t.Fatalf("losing decision error=%v", contenderErr)
			}
			if !commitFirst && contenderErr != nil {
				t.Fatalf("decision after rollback error=%v", contenderErr)
			}
			var groups int
			var decision, task string
			err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM p_1.chat_message_group WHERE conversation_id=1),dt.content,c.task_id
    FROM p_1.chat_message_group d JOIN p_1.chat_message_items di ON di.message_group_id=d.id
    JOIN p_1.chat_messages_text dt ON dt.id=di.id
    JOIN p_1.chat_message_group c ON c.reply_to_id=d.id
    WHERE d.uuid=$1 AND c.uuid=$2`, uuid.MustParse(turn.PipelineDecisionID()), uuid.MustParse(turn.ProjectionResponseID())).Scan(&groups, &decision, &task)
			if err != nil {
				t.Fatal(err)
			}
			wantDecision, wantTask := "Approved", "execution-first"
			if !commitFirst {
				wantDecision, wantTask = "Rejected", "execution-second"
			}
			if groups != 4 || decision != wantDecision || task != wantTask {
				t.Fatalf("groups=%d decision=%q task=%q; want 4, %q, %q", groups, decision, task, wantDecision, wantTask)
			}
		})
	}
}
