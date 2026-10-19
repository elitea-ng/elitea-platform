package repos

// Wake admission, the parked terminal projection, new-turn supersede and the
// after-commit signals of the per-interrupt decision ledger against a real
// PostgreSQL (fanout-interrupt-decisions v1.1 §5, §8, §8.1, §9). Requires
// ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// wakeHarness adds the real agent admission path to the ledger harness.
type wakeHarness struct {
	*interruptHarness
	admissions   *agentexecutionapp.AdmissionService
	admitter     *agentexecutionapp.FanoutWakeAdmitter
	logs         *lockedBuffer
	conversation string
	generations  map[string]string
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func randomExecutionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func newWakeHarness(t *testing.T) *wakeHarness {
	t.Helper()
	h := newInterruptHarness(t)
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 256
	jobs, err := NewAgentExecutionJobsRepository(h.pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := agentexecutionapp.NewInputBundleFactory(agentexecutionapp.InputProfile{
		Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, randomExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	admissions, err := agentexecutionapp.NewAdmissionService(jobs, factory, nil, randomExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	admitter, err := agentexecutionapp.NewFanoutWakeAdmitter(h.repo, admissions,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	w := &wakeHarness{interruptHarness: h, admissions: admissions, admitter: admitter, logs: logs, generations: map[string]string{}}
	if err := h.pool.QueryRow(t.Context(), `SELECT uuid::text FROM p_1.chat_conversations WHERE id = $1`, h.conversationID).Scan(&w.conversation); err != nil {
		t.Fatal(err)
	}
	return w
}

// parkable gives a harness response what a real parked execution has: its
// exact frozen request entry, a binding to the conversation, and a paused
// response row carrying fanout_parked_v1.
func (w *wakeHarness) parkable(responseID string) {
	w.t.Helper()
	executionID := w.executions[responseID]
	var generation string
	if err := w.pool.QueryRow(w.t.Context(), `SELECT gen_random_uuid()::text`).Scan(&generation); err != nil {
		w.t.Fatal(err)
	}
	w.generations[responseID] = generation
	input := agentCapacityInput(generation)
	// A static resume marker the wake must drop, and a member it must keep.
	input.Meta = []byte(`{"keep":true,"pipeline_static_resume_v1":{"revision":1}}`)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		w.t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	w.exec(`UPDATE elitea_runtime.input_bundle_entries
SET content_bytes = $2, content_digest = $3, content_size = $4, semantic_role = 'agent.execution_request'
WHERE input_bundle_id = $1`, "bundle-"+executionID, encoded, digest[:], len(encoded))
	w.exec(`UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $2, client_execution_generation = $3
WHERE execution_id = $1`, executionID, w.conversation, generation)
	w.exec(`UPDATE p_1.chat_message_group SET is_streaming = FALSE, task_id = $2::text,
meta = jsonb_build_object('thread_id', 'thread-' || $2::text, 'execution_generation', $3::text,
    'fanout_parked_v1', '{"decision_revision_seen":0,"pending":[],"schema":"elitea.pipeline.fanout-parked.v1"}'::jsonb)
WHERE uuid = $1::uuid`, responseID, executionID, generation)
}

func (w *wakeHarness) jobsFor(responseID string, revision int64) int {
	return w.count(`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE idempotency_key = $1`,
		agentexecutionapp.FanoutWakeIdempotencyKey(responseID, revision))
}

func (w *wakeHarness) sweep() int {
	w.t.Helper()
	created, err := w.admitter.Sweep(w.t.Context())
	if err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
	return created
}

// queueWake parks the response and decides one card, so the decision queues
// revision 1's wake.
func (w *wakeHarness) queueWake(responseID string) string {
	w.t.Helper()
	w.parkable(responseID)
	key := w.raise(responseID, "fanout-interrupt-card-v1.json")
	if s, err := w.park(responseID, 0); err != nil || s.WakeQueued {
		w.t.Fatalf("park: %+v %v", s, err)
	}
	if _, err := w.decide(interruptOwner, responseID, key, "wake-"+key[:8], "approve", ""); err != nil {
		w.t.Fatal(err)
	}
	if got := w.wakes(responseID); len(got) != 1 || !strings.HasPrefix(got[0], "QUEUED:1:") {
		w.t.Fatalf("wake not queued: %v", got)
	}
	return key
}

// TestDecideVersusParkAdmitsExactlyOneWakeJob extends the decide-vs-park race
// through admission: in both lock orders exactly one wake row is queued, and
// two admitters sweeping concurrently turn it into exactly one fanout_wake
// job; further sweeps admit nothing.
func TestDecideVersusParkAdmitsExactlyOneWakeJob(t *testing.T) {
	w := newWakeHarness(t)
	const rounds = 10
	for i := range rounds {
		response := w.newResponse()
		w.parkable(response)
		key := w.raise(response, "fanout-interrupt-card-v1.json")
		holder, err := w.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(t.Context(), `SELECT 1 FROM p_1.chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, response); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var parkErr, decideErr error
		park := func() { _, parkErr = w.park(response, 0) }
		decide := func() {
			_, decideErr = w.decide(interruptOwner, response, key, fmt.Sprintf("race-%d", i), "approve", "")
		}
		first, second := park, decide
		if i%2 == 1 {
			first, second = decide, park
		}
		wg.Go(first)
		w.waitForLockWaiters(1)
		wg.Go(second)
		w.waitForLockWaiters(2)
		if err := holder.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if parkErr != nil || decideErr != nil {
			t.Fatalf("round %d: park=%v decide=%v", i, parkErr, decideErr)
		}
		var created [2]int
		var sweepErr [2]error
		start := make(chan struct{})
		var sweepers sync.WaitGroup
		for s := range 2 {
			sweepers.Go(func() {
				<-start
				created[s], sweepErr[s] = w.admitter.Sweep(t.Context())
			})
		}
		close(start)
		sweepers.Wait()
		if sweepErr[0] != nil || sweepErr[1] != nil || created[0]+created[1] != 1 {
			t.Fatalf("round %d: sweeps created %v (%v)", i, created, sweepErr)
		}
		if n := w.jobsFor(response, 1); n != 1 {
			t.Fatalf("round %d: %d wake jobs", i, n)
		}
		if got := w.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "ADMITTED:1:") {
			t.Fatalf("round %d: wakes = %v", i, got)
		}
		if again := w.sweep(); again != 0 {
			t.Fatalf("round %d: a later sweep admitted %d more", i, again)
		}
	}
}

// TestLostAdmitIsSweptExactlyOnce: the after-commit kick never ran (a crash
// between commit and admit). The periodic sweep admits the queued wake once,
// as an ordinary continuation job carrying only the wake marker.
func TestLostAdmitIsSweptExactlyOnce(t *testing.T) {
	w := newWakeHarness(t)
	response := w.newResponse()
	w.queueWake(response)
	if created := w.sweep(); created != 1 {
		t.Fatalf("sweep created %d", created)
	}
	if created := w.sweep(); created != 0 {
		t.Fatalf("second sweep created %d", created)
	}
	var wakeExecution, entry string
	if err := w.pool.QueryRow(t.Context(), `SELECT wake_execution_id FROM elitea_runtime.execution_interrupt_wakes
WHERE root_response_id = $1::uuid AND state = 'ADMITTED'`, response).Scan(&wakeExecution); err != nil {
		t.Fatal(err)
	}
	var sio, stream, generation, message string
	var content []byte
	if err := w.pool.QueryRow(t.Context(), `
SELECT binding.sio_event, binding.client_stream_id, binding.client_execution_generation, binding.client_message_id,
       entry.content_bytes, entry.entry_id
FROM elitea_runtime.agent_execution_jobs binding
JOIN elitea_runtime.input_bundle_entries entry
  ON entry.input_bundle_id = binding.input_bundle_id AND entry.entry_id = binding.request_entry_id
WHERE binding.execution_id = $1`, wakeExecution).Scan(&sio, &stream, &generation, &message, &content, &entry); err != nil {
		t.Fatal(err)
	}
	if sio != "chat_continue_predict" || stream != w.conversation || generation != w.generations[response] || message != response {
		t.Fatalf("wake binding = %s %s %s %s", sio, stream, generation, message)
	}
	var input runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(content, &input); err != nil {
		t.Fatal(err)
	}
	wantMeta := `{"fanout_wake_v1":{"decision_revision":1,"response_message_id":"` + response +
		`","schema":"elitea.pipeline.fanout-wake.v1"},"keep":true}`
	if string(input.GetMeta()) != wantMeta || input.GetHitlResume() || input.GetShouldContinue() ||
		string(input.GetUserInput()) != `""` || string(input.GetHitlDecisions()) != `[]` || input.HitlAction != nil ||
		input.GetThreadId() != "thread-"+w.executions[response] || input.GetExecutionGeneration() != w.generations[response] {
		t.Fatalf("wake input: meta=%s resume=%v continue=%v user=%s decisions=%s thread=%s",
			input.GetMeta(), input.GetHitlResume(), input.GetShouldContinue(), input.GetUserInput(), input.GetHitlDecisions(), input.GetThreadId())
	}
	var streaming bool
	var task string
	var parked bool
	if err := w.pool.QueryRow(t.Context(), `SELECT is_streaming, task_id, meta ? 'fanout_parked_v1' FROM p_1.chat_message_group WHERE uuid = $1::uuid`,
		response).Scan(&streaming, &task, &parked); err != nil {
		t.Fatal(err)
	}
	if !streaming || task != wakeExecution || parked {
		t.Fatalf("response after wake admission: streaming=%v task=%s parked=%v", streaming, task, parked)
	}
	// The wake job is granted only to an opted-in Worker (claims.go).
	err := w.repo.shared.WithinTx(t.Context(), interruptWriteTx, func(tx sqlExecutor) error {
		wake, err := isFanoutWakeExecution(t.Context(), tx, wakeExecution)
		if err != nil || !wake {
			return fmt.Errorf("wake job not recognized: %v %v", wake, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTwoSweepersAndDuplicateSubmitsAdmitOnce: two admitters sweeping a batch
// concurrently admit each wake once, and two admissions of one claimed wake
// racing past the claim collapse onto one job (the row lock, the QUEUED check
// and the idempotency key in the admission transaction).
func TestTwoSweepersAndDuplicateSubmitsAdmitOnce(t *testing.T) {
	w := newWakeHarness(t)
	responses := make([]string, 6)
	for i := range responses {
		responses[i] = w.newResponse()
		w.queueWake(responses[i])
	}
	var total atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			created, err := w.admitter.Sweep(t.Context())
			if err != nil {
				t.Error(err)
			}
			total.Add(int64(created))
		})
	}
	close(start)
	wg.Wait()
	if total.Load() != int64(len(responses)) {
		t.Fatalf("two sweepers created %d jobs for %d wakes", total.Load(), len(responses))
	}
	for _, response := range responses {
		if n := w.jobsFor(response, 1); n != 1 {
			t.Fatalf("%d jobs for one wake", n)
		}
	}

	// Duplicate admissions of one claimed target.
	response := w.newResponse()
	w.queueWake(response)
	targets, err := w.repo.ClaimFanoutWakes(t.Context(), agentexecutionapp.FanoutWakeSweepBatch, time.Hour, agentexecutionapp.FanoutWakeMaxAttempts)
	if err != nil || len(targets) != 1 {
		t.Fatalf("claim: %d %v", len(targets), err)
	}
	submit := func() (executionapp.AdmissionOutcome, error) {
		target := targets[0]
		input, err := agentexecutionapp.FanoutWakeInput(target)
		if err != nil {
			return executionapp.AdmissionOutcome{}, err
		}
		return w.admissions.Submit(t.Context(), agentexecutionapp.SubmitRequest{
			Identity: executionapp.AdmissionIdentity{TenantID: target.TenantID, ResourceProjectID: target.ResourceProjectID,
				ProjectionProjectID: target.ProjectionProjectID, ActorID: target.ActorID},
			IdempotencyKey: agentexecutionapp.FanoutWakeIdempotencyKey(response, 1), CapabilityID: target.CapabilityID,
			ClientStreamID: target.Turn.ConversationUUID, ClientMessageID: response, SIOEvent: "chat_continue_predict",
			Input: input, CurrentFanoutWakeTurn: target.Turn.Clone(),
		})
	}
	var outcomes [2]executionapp.AdmissionOutcome
	var errs [2]error
	gate := make(chan struct{})
	for i := range 2 {
		wg.Go(func() {
			<-gate
			outcomes[i], errs[i] = submit()
		})
	}
	close(gate)
	wg.Wait()
	created := 0
	for i := range 2 {
		switch {
		case errs[i] == nil && outcomes[i].Created:
			created++
		case errs[i] == nil, errors.Is(errs[i], agentexecutionapp.ErrFanoutWakeNotQueued):
		default:
			t.Fatalf("duplicate submit %d: %v", i, errs[i])
		}
	}
	if created != 1 || w.jobsFor(response, 1) != 1 {
		t.Fatalf("duplicate submits created %d, jobs %d", created, w.jobsFor(response, 1))
	}
}

// TestWakeActorWhoLostAccessIsCancelled: an actor who lost the project role
// gets no continuation. The claim ends the wake FAILED actor_not_allowed;
// a revocation that lands after the claim is caught again inside the
// admission transaction.
func TestWakeActorWhoLostAccessIsCancelled(t *testing.T) {
	w := newWakeHarness(t)
	response := w.newResponse()
	w.queueWake(response)
	w.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 8`)
	if created := w.sweep(); created != 0 {
		t.Fatalf("a wake for a revoked actor created %d jobs", created)
	}
	if got := w.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "FAILED:1:") {
		t.Fatalf("wakes = %v", got)
	}
	if code := w.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_wakes WHERE root_response_id = $1::uuid AND failure_code = 'actor_not_allowed'`, response); code != 1 {
		t.Fatal("the refusal reason is not recorded")
	}
	if n := w.jobsFor(response, 1); n != 0 {
		t.Fatalf("%d jobs", n)
	}

	// Revoked between the claim and the admission transaction.
	w.exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 8, 9101)`)
	late := w.newResponse()
	w.queueWake(late)
	targets, err := w.repo.ClaimFanoutWakes(t.Context(), agentexecutionapp.FanoutWakeSweepBatch, time.Hour, agentexecutionapp.FanoutWakeMaxAttempts)
	if err != nil || len(targets) != 1 {
		t.Fatalf("claim: %d %v", len(targets), err)
	}
	w.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 8`)
	input, err := agentexecutionapp.FanoutWakeInput(targets[0])
	if err != nil {
		t.Fatal(err)
	}
	target := targets[0]
	_, err = w.admissions.Submit(t.Context(), agentexecutionapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{TenantID: target.TenantID, ResourceProjectID: target.ResourceProjectID,
			ProjectionProjectID: target.ProjectionProjectID, ActorID: target.ActorID},
		IdempotencyKey: agentexecutionapp.FanoutWakeIdempotencyKey(late, 1), CapabilityID: target.CapabilityID,
		ClientStreamID: target.Turn.ConversationUUID, ClientMessageID: late, SIOEvent: "chat_continue_predict",
		Input: input, CurrentFanoutWakeTurn: target.Turn.Clone(),
	})
	if !errors.Is(err, agentexecutionapp.ErrFanoutWakeActorNotAllowed) || w.jobsFor(late, 1) != 0 {
		t.Fatalf("admission for a revoked actor: %v, jobs %d", err, w.jobsFor(late, 1))
	}
	if got := w.wakes(late); len(got) != 1 || !strings.HasPrefix(got[0], "QUEUED:1:") {
		t.Fatalf("a refused admission changed the wake: %v", got)
	}
}

// parkedTerminal projects a parked terminal frame through the terminal
// transaction, exactly as ProjectAgentExecution does after the frame is
// accepted.
func (w *wakeHarness) parkedTerminal(responseID string, payload []byte) (*currentAgentFanoutParked, error) {
	executionID := w.executions[responseID]
	parsed, err := domain.ParseParked(payload)
	if err != nil {
		return nil, err
	}
	parked := &currentAgentFanoutParked{ThreadID: "thread-" + executionID, Parked: parsed}
	expected := outputapp.ExpectedAgentExecution{ExecutionID: executionID, Generation: 1, ProjectionProjectID: "1",
		ClientStreamID: w.conversation, ClientMessageID: responseID, ClientExecutionGeneration: w.generations[responseID]}
	err = w.repo.projects.WithinProjectTx(w.t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		return persistCurrentAgentTerminal(w.t.Context(), tx, expected, currentAgentTerminal{FanoutParked: parked})
	})
	return parked, err
}

// running makes a parkable response look like it is still streaming, before
// its terminal frame.
func (w *wakeHarness) running(responseID string) {
	w.parkable(responseID)
	w.exec(`UPDATE p_1.chat_message_group SET is_streaming = TRUE, meta = meta - 'fanout_parked_v1' WHERE uuid = $1::uuid`, responseID)
}

func parkedPayload(seen int64, cards ...[2]string) []byte {
	entries := make([]string, 0, len(cards))
	for _, card := range cards {
		entries = append(entries, `{"interrupt_id":"`+card[1]+`","interrupt_key":"`+card[0]+`"}`)
	}
	return []byte(fmt.Sprintf(`{"decision_revision_seen":%d,"pending":[%s],"schema":"elitea.pipeline.fanout-parked.v1"}`,
		seen, strings.Join(entries, ",")))
}

func (w *wakeHarness) interruptID(responseID, key string) string {
	var id string
	if err := w.pool.QueryRow(w.t.Context(), `SELECT interrupt_id FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, responseID, key).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// TestParkedTerminalSettlesInTheTerminalTransaction: the terminal projection
// stores the parked payload and settles the park in one transaction. A
// decision that landed before the park (revision > seen) queues the wake
// immediately and it is admitted; a payload naming a card that is not the
// response's, or a revision the ledger never reached, refuses the frame and
// writes nothing.
func TestParkedTerminalSettlesInTheTerminalTransaction(t *testing.T) {
	w := newWakeHarness(t)
	response := w.newResponse()
	w.running(response)
	a := w.raise(response, "fanout-interrupt-card-v1.json")
	b := w.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	if _, err := w.decide(interruptOwner, response, a, "before-park", "approve", ""); err != nil {
		t.Fatal(err)
	}
	payload := parkedPayload(0, [2]string{b, w.interruptID(response, b)})
	parked, err := w.parkedTerminal(response, payload)
	if err != nil || !parked.Settlement.WakeQueued || parked.Settlement.DecisionRevision != 1 {
		t.Fatalf("park after a decision: %+v %v", parked, err)
	}
	var stored []byte
	var streaming bool
	if err := w.pool.QueryRow(t.Context(), `SELECT convert_to((meta -> 'fanout_parked_v1')::text, 'UTF8'), is_streaming
FROM p_1.chat_message_group WHERE uuid = $1::uuid`, response).Scan(&stored, &streaming); err != nil {
		t.Fatal(err)
	}
	var storedValue, payloadValue any
	_ = json.Unmarshal(stored, &storedValue)
	_ = json.Unmarshal(payload, &payloadValue)
	if streaming || fmt.Sprint(storedValue) != fmt.Sprint(payloadValue) {
		t.Fatalf("parked response row: streaming=%v payload=%s", streaming, stored)
	}
	if created := w.sweep(); created != 1 || w.jobsFor(response, 1) != 1 {
		t.Fatalf("the settled wake created %d jobs", created)
	}

	// Refusals: a foreign card, and a revision ahead of the ledger.
	refused := w.newResponse()
	w.running(refused)
	own := w.raise(refused, "fanout-interrupt-card-v1.json")
	for name, body := range map[string][]byte{
		"foreign card":   parkedPayload(0, [2]string{b, w.interruptID(response, b)}),
		"unknown id":     parkedPayload(0, [2]string{own, "hitl:not-this-card"}),
		"seen too far":   parkedPayload(9, [2]string{own, w.interruptID(refused, own)}),
		"key is foreign": parkedPayload(0, [2]string{strings.Repeat("ab", 32), "hitl:1"}),
	} {
		if _, err := w.parkedTerminal(refused, body); !errors.Is(err, domain.ErrInvalidParked) ||
			!errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
		if n := w.count(`SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1::uuid AND is_streaming AND NOT meta ? 'fanout_parked_v1'`, refused); n != 1 {
			t.Fatalf("%s: a refused park changed the response", name)
		}
		if n := w.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $1::uuid AND parked_execution_id IS NOT NULL`, refused); n != 0 {
			t.Fatalf("%s: a refused park was recorded", name)
		}
	}
	// A clean park with nothing decided records the park, queues nothing.
	if parked, err := w.parkedTerminal(refused, parkedPayload(0, [2]string{own, w.interruptID(refused, own)})); err != nil || parked.Settlement.WakeQueued {
		t.Fatalf("clean park: %+v %v", parked, err)
	}
	if n := w.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $1::uuid AND parked_execution_id IS NOT NULL`, refused); n != 1 {
		t.Fatal("the clean park was not recorded")
	}
}

// TestNewTurnSupersedesEarlierOpenCards: the new turn's admission step closes
// every open card of the conversation's earlier responses (SUPERSEDED), fails
// their queued wake (response_superseded), emits agent_hitl_resolved for each
// card and owes one live replay wake per response; cards of the new response
// and of other conversations are untouched.
func TestNewTurnSupersedesEarlierOpenCards(t *testing.T) {
	w := newWakeHarness(t)
	parkedEarlier := w.newResponse()
	w.queueWake(parkedEarlier)
	pendingEarlier := w.newResponse()
	pendingKey := w.raise(pendingEarlier, "fanout-interrupt-card-v1.map-ask-user.json")
	newTurn := w.newResponse()
	ownKey := w.raise(newTurn, "fanout-interrupt-card-v1.json")
	otherConversation := w.seedConversation()
	saved := w.conversationID
	w.conversationID = otherConversation
	foreign := w.newResponse()
	foreignKey := w.raise(foreign, "fanout-interrupt-card-v1.json")
	w.conversationID = saved

	var notices []resolvedNotice
	err := w.repo.projects.WithinProjectTx(t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		var err error
		notices, err = supersedeEarlierTurnInterrupts(t.Context(), tx, 1, w.conversation, newTurn, interruptAsker)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || !notices[0].valid() || !notices[1].valid() {
		t.Fatalf("notices = %+v", notices)
	}
	if got := w.row(pendingEarlier, pendingKey).state; got != string(domain.StateSuperseded) {
		t.Fatalf("earlier pending card = %s", got)
	}
	if n := w.count(`SELECT count(*) FROM elitea_runtime.execution_interrupts WHERE root_response_id = $1::uuid AND state = 'SUPERSEDED'`, parkedEarlier); n != 1 {
		t.Fatal("the earlier decided card was not superseded")
	}
	if got := w.wakes(parkedEarlier); len(got) != 1 || !strings.HasPrefix(got[0], "FAILED:1:") {
		t.Fatalf("earlier wake = %v", got)
	}
	if created := w.sweep(); created != 0 {
		t.Fatalf("a superseded wake created %d jobs", created)
	}
	if w.row(newTurn, ownKey).state != string(domain.StatePending) || w.row(foreign, foreignKey).state != string(domain.StatePending) {
		t.Fatal("the new response's or another conversation's card was closed")
	}
	if frames := w.resolvedFrames(pendingEarlier); len(frames) != 1 {
		t.Fatalf("resolved frames for the earlier pending card = %d", len(frames))
	}
}

// recordingSignals checks, at the moment each signal fires, that what it
// announces is already committed: it reads through a separate connection.
type recordingSignals struct {
	t        *testing.T
	h        *interruptHarness
	mu       sync.Mutex
	notified []string
	kicks    int
}

func (s *recordingSignals) Notify(projectID, executionID string, cursor uint64) {
	var visible int
	if err := s.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_runtime.execution_replay_events
WHERE cursor = $1 AND execution_id = $2 AND event_type = $3`, int64(cursor), executionID, replayEventNodeEvent).Scan(&visible); err != nil {
		s.t.Error(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notified = append(s.notified, fmt.Sprintf("%s/%s/%d/visible=%d", projectID, executionID, cursor, visible))
}

func (s *recordingSignals) Kick() {
	s.mu.Lock()
	s.kicks++
	s.mu.Unlock()
}

// TestResolvedFrameLiveNotifyAfterCommitOnly: a decision wakes live replay
// streams once, after commit (the frame is visible to another connection when
// the signal fires); a replayed decision and a refused one send nothing. A
// decision on a parked response kicks wake admission once, after commit.
func TestResolvedFrameLiveNotifyAfterCommitOnly(t *testing.T) {
	w := newWakeHarness(t)
	signals := &recordingSignals{t: t, h: w.interruptHarness}
	w.repo.SetSignals(&ExecutionInterruptSignals{Replay: signals, Wakes: signals})
	response := w.newResponse()
	key := w.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := w.decide(interruptOwner, response, key, "notify", "approve", ""); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err := w.pool.QueryRow(t.Context(), `SELECT cursor FROM elitea_runtime.execution_replay_events WHERE event_id LIKE $1`,
		interruptResolvedEventIDPrefix+response+":%").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("1/%s/%d/visible=1", w.executions[response], cursor)
	if len(signals.notified) != 1 || signals.notified[0] != want || signals.kicks != 0 {
		t.Fatalf("notified=%v kicks=%d, want [%s] 0", signals.notified, signals.kicks, want)
	}
	// Replay of the same decision: no new frame, no signal.
	if result, err := w.decide(interruptOwner, response, key, "notify", "approve", ""); err != nil || !result.Replay {
		t.Fatalf("replay: %+v %v", result, err)
	}
	// A refused decision (another request on a decided card): nothing.
	if _, err := w.decide(interruptOwner, response, key, "other", "approve", ""); !errors.Is(err, domain.ErrAlreadyResolved) {
		t.Fatalf("second decision: %v", err)
	}
	if len(signals.notified) != 1 {
		t.Fatalf("signals after replay and refusal = %v", signals.notified)
	}

	parked := w.newResponse()
	w.parkable(parked)
	parkedKey := w.raise(parked, "fanout-interrupt-card-v1.json")
	if _, err := w.park(parked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(interruptOwner, parked, parkedKey, "kick", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if signals.kicks != 1 || len(signals.notified) != 2 || !strings.HasSuffix(signals.notified[1], "visible=1") {
		t.Fatalf("parked decision: kicks=%d notified=%v", signals.kicks, signals.notified)
	}
}

// TestWakeAdmissionLogsCarryNoValues: admission, a typed refusal and a
// deferred admission log fixed reasons only.
func TestWakeAdmissionLogsCarryNoValues(t *testing.T) {
	w := newWakeHarness(t)
	var captured lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	const answer = "the private answer text"
	admitted := w.newResponse()
	w.parkable(admitted)
	askKey := w.raise(admitted, "fanout-interrupt-card-v1.map-ask-user.json")
	if _, err := w.park(admitted, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(interruptOwner, admitted, askKey, "logs", "answer", answer); err != nil {
		t.Fatal(err)
	}
	corrupt := w.newResponse()
	corruptKey := w.queueWake(corrupt)
	w.exec(`UPDATE elitea_runtime.input_bundle_entries SET content_digest = decode(repeat('00', 32), 'hex') WHERE input_bundle_id = $1`,
		"bundle-"+w.executions[corrupt])
	if created := w.sweep(); created != 1 {
		t.Fatalf("created %d", created)
	}
	// The parked actor (8) loses the project role; the owner (7) decides.
	w.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 8`)
	revoked := w.newResponse()
	revokedKey := w.queueWake(revoked)
	if created := w.sweep(); created != 0 {
		t.Fatalf("created %d for a revoked actor", created)
	}
	logs := w.logs.String() + captured.String()
	for _, code := range []string{"parked_input_invalid", "actor_not_allowed"} {
		if !strings.Contains(logs, code) {
			t.Errorf("refusal %s was not logged: %s", code, logs)
		}
	}
	for _, secret := range []string{answer, askKey, revokedKey, corruptKey, admitted, revoked, corrupt,
		"claim-", "http://", "https://", "thread-"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log carries %q: %s", secret, logs)
		}
	}
}
