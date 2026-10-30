package repos

// Run caps against a REAL migrated database (shared 0212, ADR-0041
// decision 3): the per-execution snapshot, the override grant and its
// consumption by the conversation's next run, the spend routine, and the
// wall cap doubled for a run that consumed a wall override. Requires
// ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func newRunCapsRepo(t *testing.T, h *interruptHarness) *RunCapsRepository {
	t.Helper()
	repo, err := NewRunCapsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// capResponse marks a response as stopped at cap, as the projection writes it.
func (h *interruptHarness) capResponse(responseID, cap string) {
	h.exec(`UPDATE p_1.chat_message_group SET is_streaming = false,
meta = coalesce(meta, '{}'::jsonb) || jsonb_build_object('error_code', 'CAP_REACHED', 'cap', $2::text),
task_id = $3 WHERE uuid = $1::uuid`, responseID, cap, h.executions[responseID])
}

// bindToConversation makes the response's run a run of the harness
// conversation (client_stream_id is chat_conversations.uuid in production).
func (h *interruptHarness) bindToConversation(responseID string) string {
	h.t.Helper()
	var conversation string
	if err := h.pool.QueryRow(h.t.Context(), `SELECT uuid::text FROM p_1.chat_conversations WHERE id = $1`,
		h.conversationID).Scan(&conversation); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $2 WHERE execution_id = $1`,
		h.executions[responseID], conversation)
	return conversation
}

func readRunCapsRecord(t *testing.T, h *interruptHarness, executionID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT run_caps FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`, executionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{}
	if raw != nil {
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestRunCaps_OverrideGrantIsConsumedOnceByTheNextRun(t *testing.T) {
	h := newInterruptHarness(t)
	repo := newRunCapsRepo(t, h)
	ctx := t.Context()

	capped := h.newResponse()
	conversation := h.bindToConversation(capped)
	h.capResponse(capped, "max_tool_calls")

	// A worker-side cap needs the saved per-tier caps.
	if _, err := repo.GrantRunCapOverride(ctx, workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: capped, ExpiresIn: time.Hour,
	}); !errors.Is(err, workerruncaps.ErrOverrideNeedsTierCaps) {
		t.Fatalf("grant without tier caps = %v", err)
	}
	// A response that did not stop at a cap, or one of another conversation.
	plain := h.newResponse()
	if _, err := repo.GrantRunCapOverride(ctx, workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: plain, AllowWorkerCaps: true, ExpiresIn: time.Hour,
	}); !errors.Is(err, workerruncaps.ErrOverrideNotCapped) {
		t.Fatalf("grant on a plain response = %v", err)
	}
	if _, err := repo.GrantRunCapOverride(ctx, workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: capped, AllowWorkerCaps: true, ExpiresIn: time.Hour,
		ConversationUUID: "0190f2a6-7d1c-7cc1-9a35-3f4b4d2a1b11",
	}); !errors.Is(err, workerruncaps.ErrOverrideNotCapped) {
		t.Fatalf("grant through another conversation = %v", err)
	}

	grant, err := repo.GrantRunCapOverride(ctx, workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: capped, AllowWorkerCaps: true, ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.Cap != "max_tool_calls" || grant.SourceExecutionID != h.executions[capped] || !grant.ExpiresAt.After(grant.GrantedAt) {
		t.Fatalf("grant = %+v", grant)
	}
	var stored string
	if err := h.pool.QueryRow(ctx, `SELECT conversation_uuid FROM elitea_runtime.run_cap_overrides WHERE id = $1`,
		grant.ID).Scan(&stored); err != nil || stored != conversation {
		t.Fatalf("grant conversation = %q, %v", stored, err)
	}
	// The security audit event, in the same transaction.
	var events int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = 'ai.elitea.security.v1.agent.cap.override' AND event->'data'->>'resource_id' = $1`,
		capped).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit events = %d, %v", events, err)
	}
	// Once per response.
	if _, err := repo.GrantRunCapOverride(ctx, workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: capped, AllowWorkerCaps: true, ExpiresIn: time.Hour,
	}); !errors.Is(err, workerruncaps.ErrOverrideAlreadyGranted) && !errors.Is(err, workerruncaps.ErrOverrideOpenGrant) {
		t.Fatalf("second grant = %v", err)
	}

	// The stopped response's own run (a regenerate) does not consume it.
	caps := platformconfig.TierRunCaps{MaxToolCalls: 50, MaxModelTurns: 20}
	bound, err := repo.BindRunCaps(ctx, workerruncaps.Binding{ExecutionID: h.executions[capped], Generation: 1, ProjectID: 1, Caps: caps})
	if err != nil || bound != caps {
		t.Fatalf("regenerate bound %+v, %v", bound, err)
	}

	next := h.newResponse()
	h.bindToConversation(next)
	bound, err = repo.BindRunCaps(ctx, workerruncaps.Binding{ExecutionID: h.executions[next], Generation: 1, ProjectID: 1, Caps: caps})
	if err != nil {
		t.Fatal(err)
	}
	if bound != (platformconfig.TierRunCaps{MaxToolCalls: 100, MaxModelTurns: 20}) {
		t.Fatalf("next run bound %+v, want the tool calls doubled", bound)
	}
	if record := readRunCapsRecord(t, h, h.executions[next]); record["override"] != "max_tool_calls" || record["max_tool_calls"] != float64(100) {
		t.Fatalf("record = %v", record)
	}
	// A redispatch of the same execution reuses the snapshot.
	again, err := repo.BindRunCaps(ctx, workerruncaps.Binding{ExecutionID: h.executions[next], Generation: 1, ProjectID: 1, Caps: caps})
	if err != nil || again != bound {
		t.Fatalf("rebind = %+v, %v", again, err)
	}
	// The run after that gets the plain caps: the grant is spent.
	third := h.newResponse()
	h.bindToConversation(third)
	if bound, err := repo.BindRunCaps(ctx, workerruncaps.Binding{ExecutionID: h.executions[third], Generation: 1, ProjectID: 1, Caps: caps}); err != nil || bound != caps {
		t.Fatalf("third run bound %+v, %v", bound, err)
	}
	var consumed string
	if err := h.pool.QueryRow(ctx, `SELECT consumed_execution_id FROM elitea_runtime.run_cap_overrides WHERE id = $1`,
		grant.ID).Scan(&consumed); err != nil || consumed != h.executions[next] {
		t.Fatalf("consumed by %q, %v", consumed, err)
	}
}

func TestRunCaps_ExpiredGrantIsNotConsumed(t *testing.T) {
	h := newInterruptHarness(t)
	repo := newRunCapsRepo(t, h)
	capped := h.newResponse()
	h.bindToConversation(capped)
	h.capResponse(capped, "max_wall_seconds")
	if _, err := repo.GrantRunCapOverride(t.Context(), workerruncaps.OverrideRequest{
		ProjectID: 1, ActorUserID: 7, MessageID: capped, ExpiresIn: time.Millisecond,
	}); err != nil {
		t.Fatalf("a Main-side cap needs no tier caps: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	next := h.newResponse()
	h.bindToConversation(next)
	caps := platformconfig.DefaultTierRunCaps()
	if bound, err := repo.BindRunCaps(t.Context(), workerruncaps.Binding{ExecutionID: h.executions[next], Generation: 1, ProjectID: 1, Caps: caps}); err != nil || bound != caps {
		t.Fatalf("bound %+v, %v", bound, err)
	}
	if record := readRunCapsRecord(t, h, h.executions[next]); record["override"] != nil {
		t.Fatalf("an expired grant was consumed: %v", record)
	}
}

// A run that consumed a wall override gets twice the wall cap; the cancel
// names its cap.
func TestRunCaps_WallOverrideDoublesTheWallCap(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	walls, err := NewRunWallCapRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	seedAgentRun(t, pool, "wall-plain", 1, 31)
	seedAgentRun(t, pool, "wall-overridden", 1, 31)
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET admitted_at = clock_timestamp() - interval '90 minutes'
WHERE execution_id IN ('wall-plain', 'wall-overridden');
UPDATE elitea_runtime.execution_jobs SET run_caps = '{"override": "max_wall_seconds"}'::jsonb
WHERE execution_id = 'wall-overridden'`); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"wall-plain": true, "wall-overridden": false} {
		if got, err := walls.EnforceWallCap(t.Context(), id, 1, time.Hour); err != nil || got != want {
			t.Errorf("%s cancelled = %t (%v), want %t", id, got, err, want)
		}
	}
	var cap *string
	if err := pool.QueryRow(t.Context(), `SELECT cancel_cap FROM elitea_runtime.execution_jobs
WHERE execution_id = 'wall-plain'`).Scan(&cap); err != nil || cap == nil || *cap != "max_wall_seconds" {
		t.Fatalf("cancel_cap = %v, %v", cap, err)
	}
}

func TestRunCaps_SpendRoutineCancelsRunsPastTheirSpendCap(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewRunCapsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"spend-over", "spend-under", "spend-uncapped", "spend-tool"} {
		seedAgentRun(t, pool, id, 1, 31)
	}
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET run_caps = '{"max_tool_calls": 200, "max_spend": 0.01}'::jsonb
WHERE execution_id IN ('spend-over', 'spend-under', 'spend-tool');
UPDATE elitea_runtime.execution_jobs SET run_caps = '{"max_tool_calls": 200, "max_spend": 0}'::jsonb
WHERE execution_id = 'spend-uncapped';
INSERT INTO gateway.gateway_models (model_name, provider, input_cost_per_1m_tokens, output_cost_per_1m_tokens)
VALUES ('m-test', 'p-test', 1, 10);
-- 2000 in at $1/M + 1000 out at $10/M = $0.012, past $0.01.
INSERT INTO gateway.llm_request_logs (project_id, provider, model, prompt_tokens, completion_tokens, execution_id)
VALUES (1, 'p-test', 'm-test', 2000, 1000, 'spend-over'),
       (1, 'p-test', 'm-test', 1000, 100, 'spend-under'),
       (1, 'p-test', 'm-test', 900000, 900000, 'spend-uncapped'),
       -- Another project's row never counts.
       (2, 'p-test', 'm-test', 900000, 900000, 'spend-under');
INSERT INTO gateway.llm_request_logs (project_id, provider, model, operation, cost_usd, execution_id)
VALUES (1, 'search', 'web', 'tool', 0.02, 'spend-tool')`); err != nil {
		t.Fatal(err)
	}
	cancelled, err := repo.EnforceSpendCaps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SpendCapCancel{}
	for _, run := range cancelled {
		got[run.ExecutionID] = run
	}
	if len(got) != 2 || got["spend-over"].Spent < 0.0119 || got["spend-over"].MaxSpend != 0.01 || got["spend-tool"].Spent != 0.02 {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	for id, want := range map[string]string{
		"spend-over": "CANCELLED", "spend-tool": "CANCELLED", "spend-under": "RUNNING", "spend-uncapped": "RUNNING",
	} {
		if state := desiredState(t, pool, id); state != want {
			t.Errorf("%s desired = %s, want %s", id, state, want)
		}
	}
	var reason, cap string
	if err := pool.QueryRow(t.Context(), `SELECT cancel_reason, cancel_cap FROM elitea_runtime.execution_jobs
WHERE execution_id = 'spend-over'`).Scan(&reason, &cap); err != nil || reason != CapReasonWallCap || cap != "max_spend" {
		t.Fatalf("cancel = %q/%q, %v", reason, cap, err)
	}
	// Idempotent: a second pass cancels nothing new.
	if again, err := repo.EnforceSpendCaps(t.Context()); err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
}

// The response of a run stopped at a cap names the cap: cancel_cap for a
// Main-side cap (NULL before 0212 is the wall cap), and the worker's cap on
// a worker-side failure.
func TestRunCaps_StoppedResponseNamesItsCap(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.bindToConversation(response)
	executionID := h.executions[response]
	h.exec(`UPDATE p_1.chat_message_group SET task_id = $2,
meta = coalesce(meta, '{}'::jsonb) || '{"execution_generation": "1"}'::jsonb WHERE uuid = $1::uuid`, response, executionID)

	ctx := t.Context()
	if cap, err := capInTx(t, h, func(tx sqlExecutor) (string, error) { return cancelledAtCap(ctx, tx, executionID, 1) }); err != nil || cap != "" {
		t.Fatalf("a running job read cap %q, %v", cap, err)
	}
	h.exec(`UPDATE elitea_runtime.execution_jobs SET desired_state = 'CANCELLED', cancel_reason = 'cap_reached' WHERE execution_id = $1`, executionID)
	if cap, err := capInTx(t, h, func(tx sqlExecutor) (string, error) { return cancelledAtCap(ctx, tx, executionID, 1) }); err != nil || cap != CapMaxWallSeconds {
		t.Fatalf("a pre-0212 cap row read %q, %v", cap, err)
	}
	h.exec(`UPDATE elitea_runtime.execution_jobs SET cancel_cap = 'max_spend' WHERE execution_id = $1`, executionID)
	cap, err := capInTx(t, h, func(tx sqlExecutor) (string, error) { return cancelledAtCap(ctx, tx, executionID, 1) })
	if err != nil || cap != "max_spend" {
		t.Fatalf("cap = %q, %v", cap, err)
	}
	record := outputRecord{ExecutionID: executionID, Generation: 1, ProjectionProjectID: 1}
	if _, err := capInTx(t, h, func(tx sqlExecutor) (string, error) {
		return "", presentCapReachedAgentResponse(ctx, tx, record, cap)
	}); err != nil {
		t.Fatal(err)
	}
	meta := h.responseMeta(response)
	if meta["error_code"] != "CAP_REACHED" || meta["cap"] != "max_spend" ||
		meta["error"] != "The run stopped because it reached the maximum spend an administrator set. The task may be incomplete. Check what it completed before you continue." {
		t.Fatalf("meta = %v", meta)
	}
	if _, err := capInTx(t, h, func(tx sqlExecutor) (string, error) {
		return "", presentWorkerCapReached(ctx, tx, record, "max_model_turns")
	}); err != nil {
		t.Fatal(err)
	}
	if meta := h.responseMeta(response); meta["cap"] != "max_model_turns" {
		t.Fatalf("worker cap meta = %v", meta)
	}
	browser, err := capReachedBrowserData("max_records_read")
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(browser, &event); err != nil || event["cap"] != "max_records_read" || event["code"] != "CAP_REACHED" {
		t.Fatalf("browser data = %s, %v", browser, err)
	}
}

func capInTx(t *testing.T, h *interruptHarness, run func(sqlExecutor) (string, error)) (string, error) {
	t.Helper()
	var out string
	err := pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		out, err = run(pgxExecutor{queryer: tx})
		return err
	})
	return out, err
}
