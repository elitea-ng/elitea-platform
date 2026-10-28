package repos

// Cards that outlive their run, and wakes that cannot continue it, against a
// real PostgreSQL (fanout-interrupt-decisions §5, §8a; REVIEW-1470 #2, #3).

import (
	"errors"
	"strings"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

func (h *interruptHarness) cardState(responseID, key string) string {
	h.t.Helper()
	var state string
	if err := h.pool.QueryRow(h.t.Context(), `SELECT state FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, responseID, key).Scan(&state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *interruptHarness) settleJob(responseID, state string) {
	h.exec(`UPDATE elitea_runtime.execution_jobs SET state = $2, settled_at = clock_timestamp() WHERE execution_id = $1`,
		h.executions[responseID], state)
}

// TestARunThatEndsWithoutParkingClosesItsCards: a succeeded, failed or
// cancelled run that did not park leaves no execution that could apply a
// decision, so the sweep closes its open cards (PENDING → CANCELLED, DECIDED →
// SUPERSEDED, audited with the run's claim, resolved frames written), and a
// decision afterwards is refused, never a 200 that does nothing.
func TestARunThatEndsWithoutParkingClosesItsCards(t *testing.T) {
	h := newInterruptHarness(t)
	for _, terminal := range []string{"SUCCEEDED", "FAILED", "CANCELLED"} {
		response := h.newResponse()
		fence := h.bindLiveClaim(response)
		pending := h.raise(response, "fanout-interrupt-card-v1.json")
		decided := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
		if _, err := h.decide(interruptOwner, response, decided, "end-"+terminal, "answer", "yes"); err != nil {
			t.Fatal(err)
		}
		// Still running: nothing is closed.
		if n, err := h.repo.CloseStrandedCards(t.Context()); err != nil || n != 0 {
			t.Fatalf("%s while running: %d %v", terminal, n, err)
		}
		h.settleJob(response, terminal)
		if n, err := h.repo.CloseStrandedCards(t.Context()); err != nil || n != 1 {
			t.Fatalf("%s: closed %d %v", terminal, n, err)
		}
		if got := h.cardState(response, pending); got != "CANCELLED" {
			t.Fatalf("%s: pending card = %s", terminal, got)
		}
		if got := h.cardState(response, decided); got != "SUPERSEDED" {
			t.Fatalf("%s: decided card = %s", terminal, got)
		}
		if n := h.count(`SELECT count(*) FROM elitea_runtime.execution_interrupt_audit
WHERE root_response_id = $1::uuid AND transition IN ('CANCELLED', 'SUPERSEDED') AND claim_id = $2 AND actor_id IS NULL`,
			response, fence.ClaimID); n != 2 {
			t.Fatalf("%s: %d close audit rows by the claim", terminal, n)
		}
		if frames := h.resolvedFrames(response); len(frames) != 3 { // the decision's, then the two closes
			t.Fatalf("%s: %d resolved frames", terminal, len(frames))
		}
		if _, err := h.decide(interruptOwner, response, pending, "late-"+terminal, "approve", ""); !errors.Is(err, domain.ErrAlreadyResolved) {
			t.Fatalf("%s: a decision after the run ended = %v", terminal, err)
		}
		// Idempotent: a second pass finds nothing.
		if n, err := h.repo.CloseStrandedCards(t.Context()); err != nil || n != 0 {
			t.Fatalf("%s second pass: %d %v", terminal, n, err)
		}
	}
}

// TestADecisionAfterTheRunEndedClosesTheCardsAndIsRefused covers the window
// before the sweep: Decide itself finds the run ended, closes the cards in
// its committed transaction and answers 409.
func TestADecisionAfterTheRunEndedClosesTheCardsAndIsRefused(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.bindLiveClaim(response)
	card := h.raise(response, "fanout-interrupt-card-v1.json")
	h.settleJob(response, "FAILED")
	if _, err := h.decide(interruptOwner, response, card, "after-end", "approve", ""); !errors.Is(err, domain.ErrAlreadyResolved) {
		t.Fatalf("decide after the run ended = %v", err)
	}
	if got := h.cardState(response, card); got != "CANCELLED" {
		t.Fatalf("card = %s, want CANCELLED (committed with the refusal)", got)
	}
}

// TestAParkedOrWakingRunKeepsItsCards: a run that parked, or whose park
// queued a wake, still has a continuation, so its cards stay open.
func TestAParkedOrWakingRunKeepsItsCards(t *testing.T) {
	h := newInterruptHarness(t)
	parked := h.newResponse()
	h.bindLiveClaim(parked)
	parkedCard := h.raise(parked, "fanout-interrupt-card-v1.json")
	if _, err := h.park(parked, 0); err != nil {
		t.Fatal(err)
	}
	h.settleJob(parked, "SUCCEEDED")
	waking := h.newResponse()
	h.bindLiveClaim(waking)
	wakingCard := h.raise(waking, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, waking, wakingCard, "waking", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if s, err := h.park(waking, 0); err != nil || !s.WakeQueued {
		t.Fatalf("park with a decision = %+v, %v", s, err)
	}
	h.settleJob(waking, "SUCCEEDED")
	if n, err := h.repo.CloseStrandedCards(t.Context()); err != nil || n != 0 {
		t.Fatalf("closed %d %v", n, err)
	}
	if h.cardState(parked, parkedCard) != "PENDING" || h.cardState(waking, wakingCard) != "DECIDED" {
		t.Fatal("a parked or waking run lost its cards")
	}
}

func (h *interruptHarness) wakeTurn(responseID string, revision int64) agentexecutionapp.CurrentFanoutWakeTurn {
	return agentexecutionapp.CurrentFanoutWakeTurn{ProjectID: 1, ResponseMessageID: responseID, DecisionRevision: revision,
		ParkedExecutionID: h.executions[responseID], ParkedGeneration: 1}
}

// TestCapacityRefusalsNeverExhaustAWake: transient refusals give their
// attempt back, so a wake still admits once capacity returns, however long
// the pressure lasted.
func TestCapacityRefusalsNeverExhaustAWake(t *testing.T) {
	w := newWakeHarness(t)
	response := w.newResponse()
	w.queueWake(response)
	const maxAttempts = 20
	for range maxAttempts + 1 {
		targets, err := w.repo.ClaimFanoutWakes(t.Context(), 16, time.Microsecond, maxAttempts)
		if err != nil || len(targets) != 1 {
			t.Fatalf("claim under capacity pressure = %d %v (wakes %v)", len(targets), err, w.wakes(response))
		}
		if err := w.repo.DeferFanoutWake(t.Context(), targets[0].Turn); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "QUEUED:1:") {
		t.Fatalf("wake after 21 capacity refusals = %v", got)
	}
	// Capacity returns: the next sweep admits exactly one continuation.
	w.exec(`UPDATE elitea_runtime.execution_interrupt_wakes SET updated_at = clock_timestamp() - interval '1 hour'
WHERE root_response_id = $1::uuid`, response)
	if created := w.sweep(); created != 1 || w.jobsFor(response, 1) != 1 {
		t.Fatalf("admission after the pressure: created %d, jobs %d", created, w.jobsFor(response, 1))
	}
}

// TestAFailedWakeClosesTheCardsAndEndsTheResponseReadably: a wake that can
// never run ends FAILED, and in the same transaction its cards are
// CANCELLED, the park is removed from the response and the response carries
// a typed, readable error; a decision afterwards is refused.
func TestAFailedWakeClosesTheCardsAndEndsTheResponseReadably(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.bindLiveClaim(response)
	decided := h.raise(response, "fanout-interrupt-card-v1.json")
	pending := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	h.exec(`UPDATE p_1.chat_message_group SET meta = COALESCE(meta, '{}'::jsonb) || '{"fanout_parked_v1":{"x":1}}'::jsonb
WHERE uuid = $1::uuid`, response)
	if _, err := h.park(response, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(interruptOwner, response, decided, "failed-wake", "approve", ""); err != nil {
		t.Fatal(err)
	}
	h.settleJob(response, "SUCCEEDED")
	if err := h.repo.FailFanoutWake(t.Context(), h.wakeTurn(response, 1), agentexecutionapp.FanoutWakeFailureActorNotAllowed); err != nil {
		t.Fatal(err)
	}
	if got := h.wakes(response); len(got) != 1 || !strings.HasPrefix(got[0], "FAILED:1:") {
		t.Fatalf("wake = %v", got)
	}
	if h.cardState(response, decided) != "CANCELLED" || h.cardState(response, pending) != "CANCELLED" {
		t.Fatal("the failed wake left cards open")
	}
	var parkedLeft bool
	var code, message string
	if err := h.pool.QueryRow(t.Context(), `SELECT meta ? 'fanout_parked_v1', meta ->> 'error_code', meta ->> 'error'
FROM p_1.chat_message_group WHERE uuid = $1::uuid`, response).Scan(&parkedLeft, &code, &message); err != nil {
		t.Fatal(err)
	}
	if parkedLeft || code != "graph.fanout.wake_failed.actor_not_allowed" ||
		!strings.Contains(message, code) || !strings.Contains(message, "Support reference: "+h.executions[response]) {
		t.Fatalf("response after a failed wake: parked=%v code=%q message=%q", parkedLeft, code, message)
	}
	if _, err := h.decide(interruptOwner, response, pending, "after-failed-wake", "answer", "yes"); !errors.Is(err, domain.ErrAlreadyResolved) {
		t.Fatalf("a decision after a failed wake = %v", err)
	}
}
