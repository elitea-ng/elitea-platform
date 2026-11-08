package repos

// Idle compaction per (conversation, agent) against a real PostgreSQL
// (ADR-0050 decision 9b2, issue 1610): arm on settle, due -> one ADMITTED
// job, the same participant's turn cancels while a peer's does not, and the
// idle run's quiet projection and terminal. Requires
// ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/idlecompaction"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

type idleHarness struct {
	*wakeHarness
	idle     *AgentIdleCompactionsRepository
	admitter *agentexecutionapp.IdleCompactionAdmitter
	agent    int64
	peer     int64
}

func newIdleHarness(t *testing.T) *idleHarness {
	t.Helper()
	w := newWakeHarness(t)
	idle, err := NewAgentIdleCompactionsRepository(w.pool)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := agentexecutionapp.NewIdleCompactionAdmitter(idle, w.admissions, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	h := &idleHarness{wakeHarness: w, idle: idle, admitter: admitter}
	if err := w.pool.QueryRow(t.Context(), `
SELECT participant.id FROM p_1.chat_participants participant
JOIN p_1.chat_participant_mapping mapping ON mapping.participant_id = participant.id AND mapping.conversation_id = $1
WHERE participant.entity_name = 'application'`, w.conversationID).Scan(&h.agent); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(t.Context(), `
WITH participant AS (
  INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
  VALUES (gen_random_uuid(), 'application', '{"id": 2}'::jsonb) RETURNING id),
mapped AS (
  INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
  SELECT $1, id, now() AT TIME ZONE 'UTC' FROM participant)
SELECT id FROM participant`, w.conversationID).Scan(&h.peer); err != nil {
		t.Fatal(err)
	}
	return h
}

const idleTestMeasurement = `{"version":1,"phase":"compacting","budget_mode":"balanced","total_tokens":272000,` +
	`"usable_input_tokens":205280,"reserved_output_tokens":64000,"safety_margin_tokens":2720,` +
	`"estimated_input_tokens":190000,"compaction_trigger_tokens":184752,"compaction_target_tokens":143696}`

// settledAnswer turns a harness response into an agent's settled answer: a
// SUCCEEDED job on a native Anthropic model with its exact frozen request, a
// binding to the conversation as participant, and an answer that measured a
// large context and recorded its last request's start.
func (h *idleHarness) settledAnswer(responseID string, participant int64) string {
	h.t.Helper()
	executionID := h.executions[responseID]
	var generation string
	if err := h.pool.QueryRow(h.t.Context(), `SELECT gen_random_uuid()::text`).Scan(&generation); err != nil {
		h.t.Fatal(err)
	}
	h.generations[responseID] = generation
	input := agentCapacityInput(generation)
	input.Llm = []byte(`{"kwargs":{"model":"claude-sonnet-4-5","openai_compatible":false}}`)
	input.Meta = []byte(`{"keep":true}`)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		h.t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	h.exec(`UPDATE elitea_runtime.input_bundle_entries
SET content_bytes = $2, content_digest = $3, content_size = $4, semantic_role = 'agent.execution_request'
WHERE input_bundle_id = $1`, "bundle-"+executionID, encoded, digest[:], len(encoded))
	h.exec(`UPDATE elitea_runtime.agent_execution_jobs
SET client_stream_id = $2, client_execution_generation = $3, participant_id = $4, addressed_to = '{}', application_id = 7
WHERE execution_id = $1`, executionID, h.conversation, generation, participant)
	h.exec(`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp()
WHERE execution_id = $1`, executionID)
	h.exec(`UPDATE p_1.chat_message_group SET is_streaming = FALSE, task_id = $2::text,
updated_at = clock_timestamp() AT TIME ZONE 'UTC',
meta = jsonb_build_object('thread_id', 'thread-' || $2::text, 'execution_generation', $3::text,
    'runtime_context', jsonb_build_object('execution_id', $2::text, 'generation', 1,
        'execution_generation', $3::text, 'response_message_id', $1::text, 'measurement', $4::jsonb),
    'runtime_request_started_at', to_jsonb(clock_timestamp()))
WHERE uuid = $1::uuid`, responseID, executionID, generation, idleTestMeasurement)
	return executionID
}

func (h *idleHarness) arm(executionID string) bool {
	h.t.Helper()
	armed, err := h.idle.ArmIdleCompaction(h.t.Context(), executionID, 1, idlecompaction.Arm)
	if err != nil {
		h.t.Fatalf("arm: %v", err)
	}
	return armed
}

func (h *idleHarness) checkState(sourceExecution string) string {
	h.t.Helper()
	var state string
	var skip *string
	err := h.pool.QueryRow(h.t.Context(), `SELECT state, skip_code FROM elitea_runtime.agent_idle_compactions
WHERE idempotency_key = $1`, agentexecutionapp.IdleCompactionIdempotencyKey(sourceExecution)).Scan(&state, &skip)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	if skip != nil {
		return state + ":" + *skip
	}
	return state
}

// makeDue moves the check's last request one minute into the past and its due
// time to now, inside the cache lifetime.
func (h *idleHarness) makeDue(sourceExecution string) {
	h.exec(`UPDATE elitea_runtime.agent_idle_compactions
SET request_started_at = clock_timestamp() - interval '1 minute', due_at = clock_timestamp()
WHERE idempotency_key = $1`, agentexecutionapp.IdleCompactionIdempotencyKey(sourceExecution))
}

func (h *idleHarness) responseSnapshot(responseID string) string {
	h.t.Helper()
	var snapshot string
	if err := h.pool.QueryRow(h.t.Context(), `
SELECT meta::text || '|' || COALESCE(updated_at::text, '') || '|' || is_streaming::text || '|' || COALESCE(task_id, '')
FROM p_1.chat_message_group WHERE uuid = $1::uuid`, responseID).Scan(&snapshot); err != nil {
		h.t.Fatal(err)
	}
	return snapshot
}

func TestIdleCompactionArmsAdmitsOnceAndProjectsQuietly(t *testing.T) {
	h := newIdleHarness(t)
	response := h.newResponse()
	source := h.settledAnswer(response, h.agent)
	if !h.arm(source) || h.checkState(source) != "QUEUED" {
		t.Fatalf("not armed: %q", h.checkState(source))
	}
	// A replayed settlement arms nothing new.
	if h.arm(source) {
		t.Fatal("a replayed settlement queued a second check")
	}
	// Not due yet: the sweep admits nothing.
	if created, err := h.admitter.Sweep(t.Context()); err != nil || created != 0 {
		t.Fatalf("early sweep: %d %v", created, err)
	}
	h.makeDue(source)
	before := h.responseSnapshot(response)
	if created, err := h.admitter.Sweep(t.Context()); err != nil || created != 1 {
		t.Fatalf("due sweep: %d %v", created, err)
	}
	if created, err := h.admitter.Sweep(t.Context()); err != nil || created != 0 {
		t.Fatalf("second sweep: %d %v", created, err)
	}
	if got := h.checkState(source); got != "ADMITTED" {
		t.Fatalf("state = %q", got)
	}
	if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE idempotency_key = $1`,
		agentexecutionapp.IdleCompactionIdempotencyKey(source)); n != 1 {
		t.Fatalf("idle jobs = %d", n)
	}
	var idleExecution string
	var participant int64
	var stream, message, sioEvent string
	if err := h.pool.QueryRow(t.Context(), `
SELECT row.idle_execution_id, binding.participant_id, binding.client_stream_id, binding.client_message_id, binding.sio_event
FROM elitea_runtime.agent_idle_compactions row
JOIN elitea_runtime.agent_execution_jobs binding ON binding.execution_id = row.idle_execution_id
WHERE row.idempotency_key = $1`, agentexecutionapp.IdleCompactionIdempotencyKey(source)).Scan(
		&idleExecution, &participant, &stream, &message, &sioEvent); err != nil {
		t.Fatal(err)
	}
	if participant != h.agent || stream != h.conversation || message != response || sioEvent != "chat_continue_predict" {
		t.Fatalf("binding = %d %s %s %s", participant, stream, message, sioEvent)
	}
	if after := h.responseSnapshot(response); after != before {
		t.Fatalf("the admission wrote the transcript:\n%s\n%s", before, after)
	}

	// The idle run's root measurement goes to its row, never the answer.
	store, err := newPostgresSharedStore(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	measurement := strings.Replace(idleTestMeasurement, `"estimated_input_tokens":190000`, `"estimated_input_tokens":40000`, 1)
	frame := outputapp.NodeEventFrame{
		Fence: runtimedomain.Fence{ExecutionID: idleExecution, Generation: 1},
		BrowserData: []byte(fmt.Sprintf(`{"type":"agent_context_status","stream_id":%q,"message_id":%q,`+
			`"execution_generation":%q,"sio_event":"chat_continue_predict",`+
			`"response_metadata":{"model_scope":"agent","context_status":%s}}`,
			h.conversation, response, h.generations[response], measurement)),
	}
	err = store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
		idle, err := isIdleCompactionRun(t.Context(), tx, idleExecution)
		if err != nil || !idle {
			return fmt.Errorf("idle run not detected: %v %v", idle, err)
		}
		if source, err := isIdleCompactionRun(t.Context(), tx, source); err != nil || source {
			return fmt.Errorf("source detected as idle: %v %v", source, err)
		}
		return projectIdleCompactionContext(t.Context(), tx, 1, frame)
	})
	if err != nil {
		t.Fatal(err)
	}
	// The terminal ends the row DONE; the transcript is untouched.
	if err := h.idle.SettleIdleCompactionRun(t.Context(), idleExecution, "SUCCEEDED"); err != nil {
		t.Fatal(err)
	}
	if got := h.checkState(source); got != "DONE" {
		t.Fatalf("state after the terminal = %q", got)
	}
	if after := h.responseSnapshot(response); after != before {
		t.Fatalf("the idle run wrote the transcript:\n%s\n%s", before, after)
	}
	// The idle run is no run of the agent's: a sweep still admits nothing,
	// and the meter reads the compacted measurement.
	rows, err := h.idle.LatestAgentContexts(t.Context(), 1, h.conversation)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ParticipantID != h.agent || !rows[0].Idle {
		t.Fatalf("meter rows = %+v", rows)
	}
	var context struct {
		ExecutionID string `json:"execution_id"`
		Active      bool   `json:"active"`
		Measurement struct {
			EstimatedInputTokens uint64 `json:"estimated_input_tokens"`
		} `json:"measurement"`
	}
	if err := json.Unmarshal(rows[0].RuntimeContext, &context); err != nil {
		t.Fatal(err)
	}
	if context.ExecutionID != idleExecution || context.Active || context.Measurement.EstimatedInputTokens != 40000 {
		t.Fatalf("meter context = %+v", context)
	}
	// The idle run's settlement never arms a check of its own.
	if h.arm(idleExecution) {
		t.Fatal("an idle run armed an idle check")
	}
}

func TestIdleCompactionIsCancelledByTheSameAgentOnly(t *testing.T) {
	h := newIdleHarness(t)
	first := h.settledAnswer(h.newResponse(), h.agent)
	if !h.arm(first) {
		t.Fatal("not armed")
	}
	// A peer agent's turn never touches the row.
	err := pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
		return cancelParticipantIdleCompactions(t.Context(), pgxExecutor{queryer: tx}, 1, h.conversation, h.peer)
	})
	if err != nil || h.checkState(first) != "QUEUED" {
		t.Fatalf("peer turn: %v %q", err, h.checkState(first))
	}
	// The same agent's turn cancels it.
	err = pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
		return cancelParticipantIdleCompactions(t.Context(), pgxExecutor{queryer: tx}, 1, h.conversation, h.agent)
	})
	if err != nil || h.checkState(first) != "CANCELLED:superseded_by_agent_turn" {
		t.Fatalf("own turn: %v %q", err, h.checkState(first))
	}

	// An admitted idle run of the agent is asked to stop by its next turn.
	second := h.settledAnswer(h.newResponse(), h.agent)
	if !h.arm(second) {
		t.Fatal("second answer not armed")
	}
	h.makeDue(second)
	if created, err := h.admitter.Sweep(t.Context()); err != nil || created != 1 {
		t.Fatalf("sweep: %d %v", created, err)
	}
	var idleExecution string
	if err := h.pool.QueryRow(t.Context(), `SELECT idle_execution_id FROM elitea_runtime.agent_idle_compactions
WHERE idempotency_key = $1`, agentexecutionapp.IdleCompactionIdempotencyKey(second)).Scan(&idleExecution); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
		return cancelParticipantIdleCompactions(t.Context(), pgxExecutor{queryer: tx}, 1, h.conversation, h.agent)
	})
	if err != nil || h.checkState(second) != "CANCELLED:superseded_by_agent_turn" {
		t.Fatalf("own turn over a running idle run: %v %q", err, h.checkState(second))
	}
	var desired string
	if err := h.pool.QueryRow(t.Context(), `SELECT desired_state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
		idleExecution).Scan(&desired); err != nil || desired != "CANCELLED" {
		t.Fatalf("idle job desired_state = %q %v", desired, err)
	}
	// Its late terminal moves nothing.
	if err := h.idle.SettleIdleCompactionRun(t.Context(), idleExecution, "CANCELLED"); err != nil ||
		h.checkState(second) != "CANCELLED:superseded_by_agent_turn" {
		t.Fatalf("late terminal: %v %q", err, h.checkState(second))
	}
}

func TestIdleCompactionNewerAnswerSupersedesTheQueuedCheck(t *testing.T) {
	h := newIdleHarness(t)
	firstResponse := h.newResponse()
	first := h.settledAnswer(firstResponse, h.agent)
	if !h.arm(first) {
		t.Fatal("not armed")
	}
	second := h.settledAnswer(h.newResponse(), h.agent)
	// The first answer is no longer the agent's latest: a replay arms nothing.
	if h.arm(first) {
		t.Fatal("an older answer armed")
	}
	if !h.arm(second) || h.checkState(first) != "CANCELLED:superseded_by_agent_turn" || h.checkState(second) != "QUEUED" {
		t.Fatalf("states = %q %q", h.checkState(first), h.checkState(second))
	}
	// A peer's answer is armed alongside, independently.
	peer := h.settledAnswer(h.newResponse(), h.peer)
	if !h.arm(peer) || h.checkState(second) != "QUEUED" {
		t.Fatalf("peer arm: %q %q", h.checkState(peer), h.checkState(second))
	}
	// A small context, a non-Anthropic model or a paused answer is not armed.
	small := h.settledAnswer(h.newResponse(), h.agent)
	h.exec(`UPDATE p_1.chat_message_group
SET meta = jsonb_set(meta, '{runtime_context,measurement,estimated_input_tokens}', '1000')
WHERE task_id = $1`, small)
	if h.arm(small) {
		t.Fatal("a small context armed")
	}
	if got := h.checkState(second); got != "QUEUED" {
		t.Fatalf("an unarmed answer touched the earlier check: %q", got)
	}
	// The sweep sees a newer turn of the agent and skips the old check.
	h.makeDue(second)
	if created, err := h.admitter.Sweep(t.Context()); err != nil || created != 0 ||
		h.checkState(second) != "SKIPPED:superseded_by_agent_turn" {
		t.Fatalf("sweep: %d %v %q", created, err, h.checkState(second))
	}
}
