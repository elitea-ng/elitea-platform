package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	agentapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The dedicated editor fixture owns all rows. Admission, Stop, retirement and
// fresh History reads use production paths; no result or trace is invented.
func TestEditorEmptyStopPostgres(t *testing.T) {
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "99")
	}
	pool := editorLifecyclePostgresPool(t)

	t.Run("empty_cancelled_receipt_survives_fresh_history_and_replay", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		service, jobs := editorEmptyStopAdmissions(t, pool)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil || !outcome.Created {
			t.Fatalf("admission: %+v %v", outcome, err)
		}
		before, err := NewConversationsRepo(pool).EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, 0)
		if err != nil || len(before.Rows) != 1 {
			t.Fatalf("original receipt: %+v %v", before, err)
		}
		responseGroup, questionGroup := editorEmptyStopPair(t, pool, request, outcome, true)
		cancelRepo := editorEmptyStopCancelRepo(t, pool)
		stopped, err := cancelRepo.CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request))
		if err != nil || stopped.Deleted || stopped.Salvaged || stopped.Replay {
			t.Fatalf("empty EditorTest Stop: %+v %v", stopped, err)
		}
		gotResponse, gotQuestion := editorEmptyStopPair(t, pool, request, outcome, false)
		if gotResponse != responseGroup || gotQuestion != questionGroup {
			t.Fatal("Stop replaced the original message groups")
		}
		editorEmptyStopRetire(t, jobs)
		durable := editorEmptyStopRuntime(t, pool, outcome.ExecutionID, request.IdempotencyKey)
		readSnapshot := editorLifecycleSnapshot(t, pool)
		for _, id := range []string{c.ID, c.UUID} {
			page, err := NewConversationsRepo(pool).EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", id, 50, 0)
			if err != nil || len(page.Rows) != 1 || page.HasMore {
				t.Fatalf("fresh History %s: %+v %v", id, page, err)
			}
			run := page.Rows[0]
			if run.ExecutionID != outcome.ExecutionID || run.ExecutionGeneration != request.CurrentTurn.QuestionID ||
				run.QuestionID != request.CurrentTurn.QuestionID || run.ResponseMessageID != request.ClientMessageID ||
				run.ResponseMessageGroupID != int64(responseGroup) || run.State != "CANCELLED" || run.DesiredState != "CANCELLED" ||
				run.Phase != "TERMINAL" || run.SettledAt == nil || run.TraceAvailable || run.EventsURL != "" ||
				!run.AdmittedAt.Equal(before.Rows[0].AdmittedAt) || run.InputReference != before.Rows[0].InputReference {
				t.Fatalf("fresh History changed the original cancelled receipt: %+v", run)
			}
		}
		response := editorLifecycleHTTP(t, f, "1", "?editor_test_runs=true&runs_limit=50&runs_offset=0&messages_limit=0", c.UUID, true)
		var payload struct {
			Runs conversations.EditorTestRunsPage `json:"editor_test_runs"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil || len(payload.Runs.Rows) != 1 ||
			payload.Runs.Rows[0].ExecutionID != outcome.ExecutionID || payload.Runs.Rows[0].Phase != "TERMINAL" || payload.Runs.Rows[0].TraceAvailable {
			t.Fatalf("fresh HTTP History: %d %s", response.Code, response.Body.String())
		}
		if after := editorLifecycleSnapshot(t, pool); after != readSnapshot {
			t.Fatal("fresh History created durable work")
		}
		replay, err := service.Submit(t.Context(), request)
		if err != nil || replay.Created || replay.ExecutionID != outcome.ExecutionID || replay.CommandID != outcome.CommandID {
			t.Fatalf("admission replay changed cancelled identity: %+v %v", replay, err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != readSnapshot {
			t.Fatal("admission replay created messages or execution work")
		}
		if _, err := cancelRepo.CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request)); err != nil {
			t.Fatalf("terminal cancellation replay: %v", err)
		}
		if after := editorEmptyStopRuntime(t, pool, outcome.ExecutionID, request.IdempotencyKey); after != durable {
			t.Fatal("Stop replay changed terminal execution, inputs, outbox, claim or replay events")
		}
		editorEmptyStopPair(t, pool, request, outcome, false)
	})

	t.Run("actor_project_and_generation_fences_remain", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		service, jobs := editorEmptyStopAdmissions(t, pool)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		responseGroup, questionGroup := editorEmptyStopPair(t, pool, request, outcome, true)
		before := editorLifecycleSnapshot(t, pool)
		wrongActor := editorEmptyStopRequest(request)
		wrongActor.ActorUserID = 2
		cancelRepo := editorEmptyStopCancelRepo(t, pool)
		if _, err := cancelRepo.CancelCurrentAgent(t.Context(), wrongActor); !errors.Is(err, agentapp.ErrCurrentAgentCancelNotAllowed) {
			t.Fatalf("foreign actor Stop: %v", err)
		}
		tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
			t.Fatal(err)
		}
		queries := sqlcgen.New(tx)
		if _, err := queries.CancelCurrentAgentExecution(t.Context(), sqlcgen.CancelCurrentAgentExecutionParams{
			ResponseMessageID: mustCurrentPGUUID(t, request.ClientMessageID), ProjectID: 2, ActorUserID: 1,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("wrong durable project scope: %v", err)
		}
		projection, err := queries.ProjectCurrentAgentStop(t.Context(), sqlcgen.ProjectCurrentAgentStopParams{
			ResponseMessageGroupID: responseGroup, QuestionMessageGroupID: questionGroup,
			ExecutionID: outcome.ExecutionID, ExecutionGeneration: uuid.NewString(),
		})
		if err != nil || projection != (sqlcgen.ProjectCurrentAgentStopRow{}) {
			t.Fatalf("stale generation projected Stop: %+v %v", projection, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("denied actor, project or generation created durable effects")
		}
		if _, err := cancelRepo.CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request)); err != nil {
			t.Fatal(err)
		}
		editorEmptyStopRetire(t, jobs)
	})

	t.Run("ordinary_empty_stop_keeps_deletion_and_durable_replay", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		service, jobs := editorEmptyStopAdmissions(t, pool)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		// Change only the synthetic conversation source to exercise ordinary
		// compatibility against the same production admission and Stop path.
		if _, err := pool.Exec(t.Context(), `UPDATE p_1.chat_conversations SET source='elitea' WHERE uuid=$1`, c.UUID); err != nil {
			t.Fatal(err)
		}
		cancelRepo := editorEmptyStopCancelRepo(t, pool)
		stopped, err := cancelRepo.CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request))
		if err != nil || !stopped.Deleted || stopped.Salvaged || stopped.Replay {
			t.Fatalf("ordinary empty Stop: %+v %v", stopped, err)
		}
		var groups int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_1.chat_message_group WHERE uuid::text IN ($1,$2)`, request.ClientMessageID, request.CurrentTurn.QuestionID).Scan(&groups); err != nil || groups != 0 {
			t.Fatalf("ordinary empty pair retained: %d %v", groups, err)
		}
		editorEmptyStopRetire(t, jobs)
		before := editorLifecycleSnapshot(t, pool)
		durable := editorEmptyStopRuntime(t, pool, outcome.ExecutionID, request.IdempotencyKey)
		replayed, err := cancelRepo.CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request))
		if err != nil || !replayed.Replay || replayed.Deleted || replayed.Salvaged {
			t.Fatalf("ordinary deleted-pair replay: %+v %v", replayed, err)
		}
		if editorLifecycleSnapshot(t, pool) != before || editorEmptyStopRuntime(t, pool, outcome.ExecutionID, request.IdempotencyKey) != durable {
			t.Fatal("ordinary Stop replay recreated messages or runtime work")
		}
	})

	t.Run("successful_terminal_without_pause_cannot_be_stopped", func(t *testing.T) {
		f := newEditorLifecycleFixture(t, pool)
		c := f.create(t, f.first.ID)
		request := f.submitRequest(t, c)
		service, _ := editorEmptyStopAdmissions(t, pool)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		f.persistTerminal(t, request, outcome, false)
		// Seed terminal lifecycle only; the question, response and full-message
		// projection above are all written through their production owners.
		if _, err := pool.Exec(t.Context(), `UPDATE elitea_runtime.execution_jobs SET state='SUCCEEDED',settled_at=clock_timestamp() WHERE execution_id=$1 AND generation=1`, outcome.ExecutionID); err != nil {
			t.Fatal(err)
		}
		before := editorLifecycleSnapshot(t, pool)
		if _, err := editorEmptyStopCancelRepo(t, pool).CancelCurrentAgent(t.Context(), editorEmptyStopRequest(request)); !errors.Is(err, agentapp.ErrCurrentAgentCancelNotAllowed) {
			t.Fatalf("successful terminal Stop: %v", err)
		}
		if after := editorLifecycleSnapshot(t, pool); after != before {
			t.Fatal("terminal Stop changed successful receipt or runtime work")
		}
	})
}

func editorEmptyStopAdmissions(t *testing.T, pool *pgxpool.Pool) (*agentapp.AdmissionService, *AgentExecutionJobsRepository) {
	t.Helper()
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 1024
	// Isolate production retirement from other tests sharing this fixture.
	policy.StreamName = "elitea:runtime:editor-stop:" + uuid.NewString()
	repo, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	newID := func() (string, error) { return uuid.NewString(), nil }
	factory, err := agentapp.NewInputBundleFactory(agentapp.InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1"}, newID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentapp.NewAdmissionService(repo, factory, time.Now, newID)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

func editorEmptyStopCancelRepo(t *testing.T, pool *pgxpool.Pool) *CurrentAgentCancelRepository {
	t.Helper()
	repo, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func editorEmptyStopRequest(request agentapp.SubmitRequest) agentapp.CurrentAgentCancelRequest {
	return agentapp.CurrentAgentCancelRequest{ProjectID: 1, ActorUserID: 1, ResponseMessageID: request.ClientMessageID}
}

func editorEmptyStopRetire(t *testing.T, jobs *AgentExecutionJobsRepository) {
	t.Helper()
	for _, expected := range []int{1, 0} {
		retired, err := jobs.RetireNoAuthorityAgentExecution(t.Context(), 1)
		if err != nil || retired != expected {
			t.Fatalf("original cancellation retirement count=%d expected=%d error=%v", retired, expected, err)
		}
	}
}

func editorEmptyStopPair(t *testing.T, pool *pgxpool.Pool, request agentapp.SubmitRequest, outcome executionapp.AdmissionOutcome, streaming bool) (int32, int32) {
	t.Helper()
	var responseGroup, questionGroup int32
	var responseID, questionID, executionID, generation string
	var gotStreaming bool
	var items, traces int
	err := pool.QueryRow(t.Context(), `SELECT response.id,question.id,response.uuid::text,question.uuid::text,response.task_id,response.meta->>'execution_generation',response.is_streaming,
 (SELECT count(*) FROM p_1.chat_message_items i WHERE i.message_group_id=response.id),
 (SELECT count(*) FROM p_1.chat_message_trace_step s WHERE s.message_group_id=response.id)
 FROM p_1.chat_message_group response JOIN p_1.chat_message_group question ON question.id=response.reply_to_id AND question.conversation_id=response.conversation_id WHERE response.uuid=$1`, request.ClientMessageID).
		Scan(&responseGroup, &questionGroup, &responseID, &questionID, &executionID, &generation, &gotStreaming, &items, &traces)
	if err != nil || responseID != request.ClientMessageID || questionID != request.CurrentTurn.QuestionID || executionID != outcome.ExecutionID || generation != request.CurrentTurn.QuestionID || gotStreaming != streaming || items != 0 || traces != 0 {
		t.Fatalf("original empty pair changed: response=%s question=%s execution=%s generation=%s streaming=%t items=%d traces=%d error=%v", responseID, questionID, executionID, generation, gotStreaming, items, traces, err)
	}
	return responseGroup, questionGroup
}

func editorEmptyStopRuntime(t *testing.T, pool *pgxpool.Pool, executionID, idempotencyKey string) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'jobs',(SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY j.generation),'[]') FROM elitea_runtime.execution_jobs j WHERE j.execution_id=$1),
 'bindings',(SELECT COALESCE(jsonb_agg(to_jsonb(b) ORDER BY b.generation),'[]') FROM elitea_runtime.agent_execution_jobs b WHERE b.execution_id=$1),
 'outbox',(SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY o.outbox_id),'[]') FROM elitea_runtime.command_outbox o WHERE o.execution_id=$1),
 'bundles',(SELECT COALESCE(jsonb_agg(to_jsonb(b) ORDER BY b.input_bundle_id),'[]') FROM elitea_runtime.input_bundles b JOIN elitea_runtime.execution_jobs j ON j.input_bundle_id=b.input_bundle_id WHERE j.execution_id=$1),
 'entries',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.entry_id),'[]') FROM elitea_runtime.input_bundle_entries e JOIN elitea_runtime.execution_jobs j ON j.input_bundle_id=e.input_bundle_id WHERE j.execution_id=$1),
 'claims',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.claim_id),'[]') FROM elitea_runtime.execution_claims c WHERE c.execution_id=$1),
 'replays',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.event_id),'[]') FROM elitea_runtime.execution_replay_events e WHERE e.execution_id=$1),
 'reservations',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.capability_id),'[]') FROM elitea_runtime.agent_admission_reservations r WHERE r.idempotency_key=$2)
 )::text`, executionID, idempotencyKey).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var rows struct {
		Jobs []struct {
			State        string     `json:"state"`
			DesiredState string     `json:"desired_state"`
			SettledAt    *time.Time `json:"settled_at"`
		} `json:"jobs"`
		Claims  []json.RawMessage `json:"claims"`
		Replays []json.RawMessage `json:"replays"`
	}
	if err := json.Unmarshal([]byte(snapshot), &rows); err != nil || len(rows.Jobs) != 1 || rows.Jobs[0].State != "CANCELLED" || rows.Jobs[0].DesiredState != "CANCELLED" || rows.Jobs[0].SettledAt == nil || len(rows.Claims) != 0 || len(rows.Replays) != 1 {
		t.Fatalf("original terminal cancellation not durable: %+v %v", rows, err)
	}
	// Normalize JSON ordering before comparing repeated observations.
	var canonical any
	if err := json.Unmarshal([]byte(snapshot), &canonical); err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes)
}
