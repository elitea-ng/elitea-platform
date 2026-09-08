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

	ensurer := newSeamEnsurer(t, maxConcurrentProvisions, maxQueuedProvisions,
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
	awaitIdle(t, ensurer)
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
	ensurer := newSeamEnsurer(t, 2, maxQueuedProvisions, func(_ context.Context, userID int64) (int64, error) {
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

// A FULL BUDGET QUEUES, IT DOES NOT DROP (issue 843).
//
// THE DEFECT. The slot was taken before any goroutine existed, and a full
// budget ABANDONED the attempt. Three E2E personas signing in together got one
// personal project between them. That was safe only for an account that is a
// member of nothing, because resolvePersonalProjectID answers "" for it and
// the next request asks again; a member of a shared project was answered that
// shared project, so nothing ever asked again.
//
// The bound survives — see maxQueuedProvisions — and the outcome changes: the
// caller beyond the slot goes on a bounded queue that the RUNNING worker
// drains, oldest entry first, and its channel closes when ITS attempt ends
// rather than immediately.
func TestAFullBudgetQueuesTheCallInsteadOfDroppingIt(t *testing.T) {
	var attempts atomic.Int64
	entered := make(chan int64, 8)
	release := make(chan struct{})

	ensurer := newSeamEnsurer(t, maxConcurrentProvisions, maxQueuedProvisions,
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

	queued := ensurer.EnsureStarted(10)
	if queued == nil {
		t.Fatal("the second caller was dropped; a shared-project member dropped here " +
			"is told the shared project is their personal one, for good")
	}

	// A QUEUED ATTEMPT IS PENDING, NOT FINISHED. The channel must stay open,
	// because the endpoint reads its close as "re-resolve now" — and it must
	// stay MARKED in flight, so the caller's own polls join this entry instead
	// of filling the queue with copies of one account.
	select {
	case <-queued:
		t.Fatal("the queued caller was released before its attempt ever ran")
	default:
	}
	if _, pending := ensurer.inFlight.Load(int64(10)); !pending {
		t.Fatal("the queued call left the user unmarked, so its own next request " +
			"would take a second place in the queue")
	}
	if again := ensurer.EnsureStarted(10); again != queued {
		t.Fatal("a second call for a QUEUED user did not join its entry, so one user " +
			"can occupy two places in the queue")
	}

	// The budget still bounds the WORK: nothing beyond the one slot is running.
	if got := attempts.Load(); got != 1 {
		t.Fatalf("%d attempts are running with a one-deep budget, want 1", got)
	}

	// A third account queues behind the second, and the queue is drained
	// OLDEST FIRST. Without the order, a burst starves whoever arrived first.
	third := ensurer.EnsureStarted(11)
	if third == nil {
		t.Fatal("the third caller was dropped")
	}

	close(release)
	awaitClosed(t, 9, holder)
	awaitAttempt(t, entered, 10)
	awaitClosed(t, 10, queued)
	awaitAttempt(t, entered, 11)
	awaitClosed(t, 11, third)

	if got := attempts.Load(); got != 3 {
		t.Fatalf("%d attempts ran for three accounts, want 3: every caller must be "+
			"provisioned, not just the one that took the slot", got)
	}
	awaitIdle(t, ensurer)
}

// THE DRAIN IS OLDEST-FIRST, which is the half the test above cannot see while
// its attempts all block on one channel. Ten accounts arrive behind a held
// slot; they must be provisioned in arrival order.
func TestTheQueueIsDrainedOldestFirst(t *testing.T) {
	entered := make(chan int64, 16)
	release := make(chan struct{})

	ensurer := newSeamEnsurer(t, maxConcurrentProvisions, maxQueuedProvisions,
		func(_ context.Context, userID int64) (int64, error) {
			entered <- userID
			if userID == 1 {
				<-release
			}
			return 0, nil
		})

	// User 1 takes the slot and holds it, so the rest are enqueued in the
	// order these calls are made — one goroutine per caller would make the
	// arrival order itself a race.
	if ensurer.EnsureStarted(1) == nil {
		t.Fatal("the first caller was refused")
	}
	awaitAttempt(t, entered, 1)

	var waits []<-chan struct{}
	for userID := int64(2); userID <= 11; userID++ {
		wait := ensurer.EnsureStarted(userID)
		if wait == nil {
			t.Fatalf("user %d was dropped with a queue of %d", userID, maxQueuedProvisions)
		}
		waits = append(waits, wait)
	}

	close(release)
	for index, wait := range waits {
		awaitClosed(t, index, wait)
	}
	for want := int64(2); want <= 11; want++ {
		select {
		case got := <-entered:
			if got != want {
				t.Fatalf("the queue ran user %d where user %d was waiting longer", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("user %d was never provisioned", want)
		}
	}
	awaitIdle(t, ensurer)
}

// THE QUEUE IS BOUNDED, and the bound behaves exactly as the old drop did: the
// caller is refused with a closed channel and nothing about it stays pending,
// so its next authenticated request queues it again. This is what keeps a
// burst from costing unbounded memory.
func TestAFullQueueRefusesRatherThanGrowing(t *testing.T) {
	entered := make(chan int64, 8)
	release := make(chan struct{})

	// One slot, one waiting place.
	ensurer := newSeamEnsurer(t, 1, 1, func(_ context.Context, userID int64) (int64, error) {
		entered <- userID
		<-release
		return 0, nil
	})

	if ensurer.EnsureStarted(9) == nil {
		t.Fatal("the first caller was refused")
	}
	awaitAttempt(t, entered, 9)
	if ensurer.EnsureStarted(10) == nil {
		t.Fatal("the second caller was refused with a free place in the queue")
	}

	refused := ensurer.EnsureStarted(11)
	if refused != nil {
		select {
		case <-refused:
		case <-time.After(5 * time.Second):
			t.Fatal("the refused caller holds a channel nothing closes")
		}
	}
	if _, pending := ensurer.inFlight.Load(int64(11)); pending {
		t.Fatal("the refused call left the user marked as being provisioned, so its " +
			"next request would join an entry no worker holds")
	}

	close(release)
	awaitAttempt(t, entered, 10)
	awaitIdle(t, ensurer)

	// And the bound is a bound, not a fuse: the queue is usable again.
	if after := ensurer.EnsureStarted(11); after == nil {
		t.Fatal("the drained queue never took another caller")
	}
	awaitAttempt(t, entered, 11)
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// newSeamEnsurer builds a real ensurer and replaces the one seam.
//
// NewEnsurer, not a struct literal: the constructor is what wires the slot
// budget, the logger and `attempt`, and a literal here would let those defaults
// rot with no test noticing. The pool is a zero value nothing dereferences,
// because `attempt` replaces the only method that reads it.
func newSeamEnsurer(
	t *testing.T, budget, queued int, attempt func(context.Context, int64) (int64, error),
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
	WithBounds(budget, queued)(ensurer)
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

// awaitIdle waits until no worker is running and nothing is queued.
//
// It POLLS rather than reading the counters once, because the last worker
// retires just AFTER it closes the channel that released the test: the close is
// the last deferred action of the attempt, and the retirement happens on the
// next turn of the drain loop. Reading the counters immediately would be a race
// against that turn, not an assertion about the design.
func awaitIdle(t *testing.T, ensurer *Ensurer) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		ensurer.queueMu.Lock()
		running, waiting := ensurer.running, len(ensurer.waiting)
		ensurer.queueMu.Unlock()
		if running == 0 && waiting == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%d workers and %d queued entries outlived every attempt; "+
				"a slot that is never returned stops the queue for good", running, waiting)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
