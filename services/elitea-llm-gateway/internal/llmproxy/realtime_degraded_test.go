package llmproxy

// realtime_degraded_test.go — a live realtime session while NATS is down, with
// the REAL GovernanceStore (human decision, DECISIONS.md 2026-10-22).
//
// The session reserves against the replica's share at the upgrade and keeps
// the reservation until it ends. The re-check ticker reserves nothing and does
// not count the session's own reservation. A share that OTHER spend fills is
// a 402 at the re-check, and a 402 closes the session on the first refusal:
// that is decision H1 (DECISIONS.md 2026-08-20), unchanged by this work.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// degradedRealtimeFixture is the realtime fixture with the real degraded store
// in place of the recording gate. 4 replicas and a 2.5 USD reservation: the
// session's own reservation fills the 2.5 USD share by itself.
func degradedRealtimeFixture(t *testing.T) (*realtimeFixture, *failmode.DegradedCounters) {
	t.Helper()
	gs, degraded := buildDegradedStore(t, 4, degradedShare)
	costs := &fakeCostEstimator{inputRateNano: 1000, outputRateNano: 2000}
	f := newRealtimeFixture(t, WithBudgetGate(gs, costs))
	return f, degraded
}

const degradedShare = 10 * failmode.NanoUSD / 4

// TestRealtime_DegradedSessionIsNotRefusedByItsOwnReservation: without the
// check-only re-check, the ticker would count the session's own reservation,
// see a full share, answer 402 and close the session on its first tick.
func TestRealtime_DegradedSessionIsNotRefusedByItsOwnReservation(t *testing.T) {
	f, degraded := degradedRealtimeFixture(t)
	conn := f.dial(t)
	waitHeld(t, degraded, degradedShare)

	// Many re-check ticks (25 ms each). The session must stay open. The read
	// context expiring closes the connection, which gives the reservation back,
	// so the held amount is sampled WHILE the context is live and never after
	// it (read after the timeout it is 0 on a fast machine: a race of the test,
	// not a second reservation).
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	samples := make(chan int64, 64)
	go func() {
		defer close(samples)
		for ctx.Err() == nil {
			held := degraded.Held(degradedSubject())
			if ctx.Err() != nil {
				return // the timeout may have closed the session between the read and the check
			}
			samples <- held
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, _, err := conn.Read(ctx)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("the session received a frame or closed during the re-checks (err=%v, close=%v); "+
			"its own reservation must not refuse it", err, websocket.CloseStatus(err))
	}
	count := 0
	for got := range samples {
		count++
		if got != degradedShare {
			t.Fatalf("held = %d during the session, want exactly one reservation (%d): a re-check must not reserve",
				got, int64(degradedShare))
		}
	}
	if count == 0 {
		t.Fatal("no sample of the held amount was taken during the session")
	}

	// The session ends: its reservation goes back.
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitHeld(t, degraded, 0)
}

// TestRealtime_DegradedShareFilledByOthersClosesTheSession pins the behaviour
// change: in degraded mode a 402 can come from the per-replica share, and a
// 402 closes the session (H1, first refusal). When OTHER spend fills the share
// the next re-check refuses and the session closes with the budget reason.
func TestRealtime_DegradedShareFilledByOthersClosesTheSession(t *testing.T) {
	f, degraded := degradedRealtimeFixture(t)
	conn := f.dial(t)
	waitHeld(t, degraded, degradedShare)

	// Another request's bill fills the share.
	degraded.Add(degradedSubject(), degradedShare)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the refusal event; read on to the close
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008", got, err)
		}
		var closeErr websocket.CloseError
		if !errors.As(err, &closeErr) || !strings.Contains(closeErr.Reason, "budget exhausted") {
			t.Fatalf("close = %v, want the exhausted-budget reason", err)
		}
		break
	}
	waitHeld(t, degraded, 0)
}
