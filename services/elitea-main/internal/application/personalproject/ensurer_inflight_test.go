package personalproject

// WHO WAITS FOR THE ATTEMPT, AND WHO IS TOLD THERE IS NONE.
//
// THE DEFECT. The SPA sends two `GET /social/author` requests at boot, inside
// the same second. Both find no personal project, so both ask for one.
// EnsureStarted gave the first the channel of the attempt it started, and gave
// the SECOND a nil — "another attempt already owns this user" — which
// internal/api/v2/social/handler.go reads as "nothing to wait for". The first
// request therefore answered the real `personal_project_id` and the second
// answered "", from one boot, on the field routes/-guards/indexRoute.ts routes
// on. Which of the two the SPA believed decided whether a first-time user
// landed on the product or on `/onboarding`.
//
// These tests are IN-PACKAGE and drive `attempt`, the one seam. What has to be
// right here is who joins, who owns and who is dropped — none of which is
// about PostgreSQL. ensurer_postgres_integration_test.go covers Ensure itself,
// and social/personal_project_postgres_integration_test.go covers the endpoint.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

// TWO CONCURRENT CALLS FOR ONE USER GET ONE ATTEMPT AND BOTH WAIT FOR IT.
//
// The assertions are the whole contract: neither caller is refused, both hold
// the SAME channel, the provisioning runs exactly once, nothing is signalled
// while the work is still running, and both are released when it ends.
func TestTwoConcurrentCallsForOneUserJoinOneAttempt(t *testing.T) {
	var attempts atomic.Int64
	entered := make(chan int64, 8)
	release := make(chan struct{})

	ensurer := newSeamEnsurer(t, maxConcurrentProvisions,
		func(_ context.Context, userID int64) (int64, error) {
			attempts.Add(1)
			entered <- userID
			<-release
			return 0, nil
		})

	// Started together on purpose. The attempt blocks until this test releases
	// it, so the second call always arrives while the first is still running —
	// which is the boot the browser produces, made deterministic.
	waits := make([]<-chan struct{}, 2)
	var callers sync.WaitGroup
	begin := make(chan struct{})
	for index := range waits {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-begin
			waits[index] = ensurer.EnsureStarted(9)
		}()
	}
	close(begin)
	callers.Wait()

	for index, wait := range waits {
		if wait == nil {
			t.Fatalf("caller %d was refused a channel; it would answer "+
				"personal_project_id \"\" while its twin answered the real id", index)
		}
	}
	if waits[0] != waits[1] {
		t.Fatal("the two callers hold different channels, so they are not waiting " +
			"for the same attempt")
	}

	awaitAttempt(t, entered, 9)

	// The signal must mean "finished", not "started".
	for index, wait := range waits {
		select {
		case <-wait:
			t.Fatalf("caller %d was released while the provisioning was still running", index)
		default:
		}
	}

	close(release)
	for index, wait := range waits {
		awaitClosed(t, index, wait)
	}

	if got := attempts.Load(); got != 1 {
		t.Fatalf("%d provisioning attempts ran for one user, want 1", got)
	}

	// The close is the LAST action, after the in-flight entry is released and
	// the slot is returned. A caller woken by the channel therefore re-reads a
	// world in which nothing about this user is still pending.
	if _, pending := ensurer.inFlight.Load(int64(9)); pending {
		t.Fatal("the in-flight entry outlived the channel close")
	}
	if held := len(ensurer.slots); held != 0 {
		t.Fatalf("%d slots are still held after the attempt ended", held)
	}
}

// A SECOND USER IS A SECOND ATTEMPT. The join is keyed by user id, so it must
// not swallow the provisioning of anybody else.
func TestAnotherUserTakesAnAttemptOfItsOwn(t *testing.T) {
	var attempts atomic.Int64
	entered := make(chan int64, 8)
	release := make(chan struct{})

	// A budget of two, because the production budget is one and this test is
	// about the KEY rather than about the budget. The case where the budget is
	// what refuses the second user is the next test.
	ensurer := newSeamEnsurer(t, 2, func(_ context.Context, userID int64) (int64, error) {
		attempts.Add(1)
		entered <- userID
		<-release
		return 0, nil
	})

	first := ensurer.EnsureStarted(9)
	second := ensurer.EnsureStarted(10)
	if first == nil || second == nil {
		t.Fatalf("EnsureStarted(9) = %v, EnsureStarted(10) = %v; both must start", first, second)
	}
	if first == second {
		t.Fatal("two users share one channel, so one of them waits for the other's project")
	}

	awaitAttempt(t, entered, 9, 10)
	close(release)
	awaitClosed(t, 0, first)
	awaitClosed(t, 1, second)

	if got := attempts.Load(); got != 2 {
		t.Fatalf("%d attempts ran for two users, want 2", got)
	}
}

// A FULL BUDGET STILL DROPS. The slot is taken before any goroutine exists, so
// a burst of first logins cannot pile goroutines onto a one-deep budget. The
// dropped caller gets nil, which the endpoint reads as "answer as before and
// let the poll ask again".
func TestAFullBudgetDropsTheCallAndLeavesNothingPending(t *testing.T) {
	var attempts atomic.Int64
	entered := make(chan int64, 8)
	release := make(chan struct{})

	ensurer := newSeamEnsurer(t, maxConcurrentProvisions,
		func(_ context.Context, userID int64) (int64, error) {
			attempts.Add(1)
			entered <- userID
			<-release
			return 0, nil
		})

	holder := ensurer.EnsureStarted(9)
	if holder == nil {
		t.Fatal("the first call was refused, so nothing holds the budget")
	}
	awaitAttempt(t, entered, 9)

	if dropped := ensurer.EnsureStarted(10); dropped != nil {
		t.Fatal("a call was accepted with a full budget; a burst of first logins " +
			"would queue goroutines that each suppress their own later polls")
	}
	// A DROPPED ATTEMPT MUST NOT LOOK LIKE A RUNNING ONE. The in-flight entry
	// is taken before the slot, so the drop has to release it — otherwise the
	// next poll would join an attempt nobody is running and wait for a channel
	// nothing closes.
	if _, pending := ensurer.inFlight.Load(int64(10)); pending {
		t.Fatal("the dropped call left the user marked as being provisioned")
	}
	if again := ensurer.EnsureStarted(10); again != nil {
		t.Fatal("the retry was not dropped, so the budget stopped bounding anything")
	}

	// Nor may a caller that JOINED inside the drop window wait for ever. Every
	// channel handed out here belongs to an attempt that was dropped before it
	// started, so every one of them must already be closed.
	var joiners sync.WaitGroup
	joined := make(chan (<-chan struct{}), 8)
	for range cap(joined) {
		joiners.Add(1)
		go func() {
			defer joiners.Done()
			if wait := ensurer.EnsureStarted(11); wait != nil {
				joined <- wait
			}
		}()
	}
	joiners.Wait()
	close(joined)
	for wait := range joined {
		awaitClosed(t, 11, wait)
	}

	close(release)
	awaitClosed(t, 9, holder)

	if got := attempts.Load(); got != 1 {
		t.Fatalf("%d attempts ran, want 1: only the call that took the slot may provision", got)
	}

	// And the budget is a budget, not a fuse: the slot is free again.
	after := ensurer.EnsureStarted(10)
	if after == nil {
		t.Fatal("the returned slot was never reusable")
	}
	awaitAttempt(t, entered, 10)
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// newSeamEnsurer builds a real ensurer and replaces the one seam.
//
// NewEnsurer, not a struct literal: the constructor is what wires the slot
// budget, the logger and `attempt`, and a literal here would let those defaults
// rot with no test noticing. The pool is a zero value nothing dereferences,
// because `attempt` replaces the only method that reads it.
func newSeamEnsurer(
	t *testing.T, budget int, attempt func(context.Context, int64) (int64, error),
) *Ensurer {
	t.Helper()
	ensurer, err := NewEnsurer(&pgxpool.Pool{}, refusingProvisioner{})
	if err != nil {
		t.Fatalf("build ensurer: %v", err)
	}
	if ensurer.attempt == nil {
		t.Fatal("NewEnsurer wired no provisioning attempt, so EnsureStarted refuses every call")
	}
	ensurer.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ensurer.attempt = attempt
	if budget != maxConcurrentProvisions {
		ensurer.slots = make(chan struct{}, budget)
	}
	return ensurer
}

// refusingProvisioner satisfies the constructor. Nothing calls it: the seam
// stands in for the whole attempt, provisioner included, so a call here means
// the seam was not honoured.
type refusingProvisioner struct{}

func (refusingProvisioner) Provision(
	context.Context, projectprovisioning.Request,
) (projectprovisioning.Result, error) {
	panic("the provisioning seam was bypassed")
}

func (refusingProvisioner) Deprovision(
	context.Context, int64,
) (projectprovisioning.Result, error) {
	panic("the provisioning seam was bypassed")
}

// awaitAttempt waits for the named users to enter the seam, in any order.
func awaitAttempt(t *testing.T, entered <-chan int64, want ...int64) {
	t.Helper()
	pending := make(map[int64]bool, len(want))
	for _, userID := range want {
		pending[userID] = true
	}
	deadline := time.After(5 * time.Second)
	for len(pending) > 0 {
		select {
		case userID := <-entered:
			if !pending[userID] {
				t.Fatalf("user %d was provisioned, and no test asked for it", userID)
			}
			delete(pending, userID)
		case <-deadline:
			t.Fatalf("no provisioning attempt started for %v", want)
		}
	}
}

func awaitClosed(t *testing.T, label int, wait <-chan struct{}) {
	t.Helper()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatalf("the channel of caller %d never closed, so a request waits its whole bound "+
			"for an attempt that already ended", label)
	}
}
