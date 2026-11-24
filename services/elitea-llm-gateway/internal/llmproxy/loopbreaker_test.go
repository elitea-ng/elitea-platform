package llmproxy

import (
	"testing"
	"time"
)

// testClock is an injectable manual clock for loopBreaker tests.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// The breaker's numbers became operator-settable in issue #12. These tests pin
// its SEMANTICS — trip at the threshold, sliding window, cooldown, tuple-map
// bounding — not the production defaults, so they construct the breaker with
// fixed test values and assert exactly what they asserted before.
const (
	testLoopThreshold = 5
	testLoopWindow    = time.Second
	testLoopOpenFor   = 30 * time.Second
)

// testLoopParams are the fixed numbers the behavioural tests below run against.
func testLoopParams() LoopBreakerParams {
	return LoopBreakerParams{Threshold: testLoopThreshold, Window: testLoopWindow, OpenFor: testLoopOpenFor}
}

func newTestBreaker() (*loopBreaker, *testClock) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(testLoopParams())
	b.now = clk.now
	return b, clk
}

// TestLoopBreaker_TripsAtThreshold asserts the spec §2.6 numbers exactly:
// 4 requests in 1 s are allowed, the 5th trips the circuit.
func TestLoopBreaker_TripsAtThreshold(t *testing.T) {
	b, clk := newTestBreaker()

	for i := 0; i < testLoopThreshold-1; i++ {
		ok, _ := b.allow("42", "openai/gpt-4o")
		if !ok {
			t.Fatalf("request %d: allow = false, want true (below threshold)", i+1)
		}
		clk.advance(100 * time.Millisecond) // 4 requests inside 1 s
	}

	ok, retryAfter := b.allow("42", "openai/gpt-4o")
	if ok {
		t.Fatal("5th request within 1 s: allow = true, want false (circuit must open)")
	}
	if retryAfter != testLoopOpenFor {
		t.Errorf("retryAfter = %v, want %v", retryAfter, testLoopOpenFor)
	}
}

// TestLoopBreaker_SlidingWindow asserts 5 requests spread over MORE than 1 s
// do not trip (the window slides; old hits expire).
func TestLoopBreaker_SlidingWindow(t *testing.T) {
	b, clk := newTestBreaker()

	for i := 0; i < 10; i++ {
		ok, _ := b.allow("42", "openai/gpt-4o")
		if !ok {
			t.Fatalf("request %d: allow = false, want true (300 ms apart → max 4 in any 1 s window)", i+1)
		}
		clk.advance(300 * time.Millisecond)
	}
}

// TestLoopBreaker_OpenCircuitRejectsUntilCooldown asserts requests during the
// 30 s open period are rejected and the circuit closes afterwards.
func TestLoopBreaker_OpenCircuitRejectsUntilCooldown(t *testing.T) {
	b, clk := newTestBreaker()

	for i := 0; i < testLoopThreshold; i++ {
		b.allow("42", "openai/gpt-4o")
	}

	clk.advance(29 * time.Second)
	if ok, _ := b.allow("42", "openai/gpt-4o"); ok {
		t.Fatal("allow = true at t+29s, want false (circuit open for 30 s)")
	}

	clk.advance(2 * time.Second) // t+31s > 30 s cooldown
	if ok, _ := b.allow("42", "openai/gpt-4o"); !ok {
		t.Fatal("allow = false after cooldown elapsed, want true (circuit closes)")
	}
}

// TestLoopBreaker_TuplesAreIndependent asserts a tripped (project, model) does
// not affect a different project or a different model.
func TestLoopBreaker_TuplesAreIndependent(t *testing.T) {
	b, _ := newTestBreaker()

	for i := 0; i < testLoopThreshold; i++ {
		b.allow("42", "openai/gpt-4o")
	}
	if ok, _ := b.allow("42", "openai/gpt-4o"); ok {
		t.Fatal("tripped tuple must be open")
	}
	if ok, _ := b.allow("43", "openai/gpt-4o"); !ok {
		t.Error("different project must be unaffected")
	}
	if ok, _ := b.allow("42", "anthropic/claude-sonnet-5"); !ok {
		t.Error("different model must be unaffected")
	}
}

// TestLoopBreaker_KeyCollisionResistance asserts the tuple key cannot be forged
// across the project/model boundary (separator is not a legal ID character).
func TestLoopBreaker_KeyCollisionResistance(t *testing.T) {
	if loopKey("1", "2/model") == loopKey("12", "/model") {
		t.Fatal("loopKey must not collide across the project/model boundary")
	}
}

// TestLoopBreaker_MapBounded asserts the tuple table stays within
// loopBreakerMaxTuples and that overflow degrades to admit (never blocks
// honest traffic on a full table).
func TestLoopBreaker_MapBounded(t *testing.T) {
	b, clk := newTestBreaker()

	// Fill the table with live tuples (all inside the window).
	for i := 0; i < loopBreakerMaxTuples; i++ {
		b.allow("p", "model-"+itoa(i))
	}
	if len(b.hits) > loopBreakerMaxTuples {
		t.Fatalf("hits table = %d tuples, must stay <= %d", len(b.hits), loopBreakerMaxTuples)
	}

	// One more NEW tuple while the table is full of live entries → admitted.
	if ok, _ := b.allow("p", "overflow-model"); !ok {
		t.Fatal("overflow tuple must be admitted (degrade to inactive, not block)")
	}

	// After the window passes, stale tuples are prunable and new tuples track.
	clk.advance(2 * time.Second)
	if ok, _ := b.allow("p", "post-prune-model"); !ok {
		t.Fatal("post-prune tuple must be admitted")
	}
	if len(b.hits) > loopBreakerMaxTuples {
		t.Fatalf("hits table = %d tuples after prune, must stay <= %d", len(b.hits), loopBreakerMaxTuples)
	}
}

// TestLoopBreaker_OpenUntilBounded asserts the open-circuit table is bounded and
// that elapsed cooldowns are reclaimed. openUntil is the map that survives a
// trip (hits is dropped), so an attacker cycling model names grows it — and the
// fall-through in allow only reclaims an entry when that exact tuple is
// requested again after its cooldown, which a cycling attacker never does.
func TestLoopBreaker_OpenUntilBounded(t *testing.T) {
	b, clk := newTestBreaker()

	// trip drives one tuple past the threshold, leaving it open, and reports
	// what the tripping request itself returned.
	trip := func(model string) bool {
		var ok bool
		for i := 0; i < testLoopThreshold; i++ {
			ok, _ = b.allow("p", model)
		}
		return ok
	}

	// Cycle enough distinct models to reach the cap. hits stays near-empty
	// (every trip deletes its entry), so only the openUntil cap can bound this.
	for i := 0; i < loopBreakerMaxTuples; i++ {
		trip("model-" + itoa(i))
	}
	if len(b.openUntil) > loopBreakerMaxTuples {
		t.Fatalf("openUntil = %d entries, must stay <= %d", len(b.openUntil), loopBreakerMaxTuples)
	}

	// Every circuit is live: a further new tuple is still rejected, and the
	// table does not grow past the cap.
	if trip("overflow-model") {
		t.Error("threshold burst on a new tuple with a full table: allow = true, want false")
	}
	if len(b.openUntil) > loopBreakerMaxTuples {
		t.Fatalf("openUntil = %d entries after overflow, must stay <= %d", len(b.openUntil), loopBreakerMaxTuples)
	}

	// Once the cooldowns elapse, the next trip prunes them all: the expired
	// entries are reclaimed rather than pinned forever.
	clk.advance(testLoopOpenFor + time.Second)
	if trip("post-cooldown-model") {
		t.Error("threshold burst after the table was reclaimed: allow = true, want false")
	}
	if len(b.openUntil) != 1 {
		t.Fatalf("openUntil = %d entries after cooldown + trip, want 1 (expired entries must be reclaimed)",
			len(b.openUntil))
	}
	if ok, _ := b.allow("p", "post-cooldown-model"); ok {
		t.Error("the freshly tripped tuple must still be open after the prune")
	}
}

// TestLoopBreaker_PruneReclaimsExpiredOpenCircuits asserts pruneLocked itself
// drops elapsed cooldowns while keeping live ones (the unit behind the bound).
func TestLoopBreaker_PruneReclaimsExpiredOpenCircuits(t *testing.T) {
	b, clk := newTestBreaker()

	for i := 0; i < testLoopThreshold; i++ {
		b.allow("42", "openai/gpt-4o")
	}
	clk.advance(testLoopOpenFor + time.Second)
	for i := 0; i < testLoopThreshold; i++ {
		b.allow("42", "anthropic/claude-sonnet-5") // still live after the prune
	}

	b.mu.Lock()
	b.pruneLocked(b.now().UnixNano())
	b.mu.Unlock()

	if _, open := b.openUntil[loopKey("42", "openai/gpt-4o")]; open {
		t.Error("expired open circuit survived pruneLocked")
	}
	if _, open := b.openUntil[loopKey("42", "anthropic/claude-sonnet-5")]; !open {
		t.Error("live open circuit was pruned — the 30 s cooldown must be honoured")
	}
}

// itoa is a tiny helper to avoid strconv in the hot loop above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// ─── issue #12: the backstop's numbers are operator settings ────────────────

// TestLoopBreaker_DefaultDoesNotTripOrdinaryBurst is the issue #12 regression
// guard. The shipped numbers (5 requests / 1 s / 30 s lockout) made this a
// hardcoded 5 req/s per-(project, model) rate limiter armed in production: a
// 50-VU k6 run against one tuple measured 99.96% HTTP 429. Ordinary bursty
// traffic must pass at the default.
//
// 50 is not arbitrary: it is GATEWAY_PROVIDER_CONCURRENCY, the per-replica
// provider worker pool, i.e. the most concurrent work one replica can actually
// have in flight for a tuple. If the backstop rejects THAT, it is rejecting
// traffic the gateway was built to serve.
//
// Mutation: set DefaultLoopBreakerThreshold back to 5 — this test MUST fail.
func TestLoopBreaker_DefaultDoesNotTripOrdinaryBurst(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(LoopBreakerParams{}) // production defaults
	b.now = clk.now

	const ordinaryBurst = 50
	for i := 0; i < ordinaryBurst; i++ {
		if ok, _ := b.allow("42", "openai/gpt-4o"); !ok {
			t.Fatalf("request %d of an ordinary %d-request burst was rejected at the DEFAULT threshold — "+
				"the backstop is a de-facto rate limiter on production traffic (issue #12)", i+1, ordinaryBurst)
		}
		clk.advance(time.Millisecond)
	}
}

// TestLoopBreaker_DefaultStillContainsRunawayAmplification is the other side of
// the same trade-off: raising the threshold must not make the layer decorative.
// Volume beyond what any replica could serve is still contained.
func TestLoopBreaker_DefaultStillContainsRunawayAmplification(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(LoopBreakerParams{})
	b.now = clk.now

	tripped := false
	for i := 0; i < DefaultLoopBreakerThreshold*2; i++ {
		if ok, retryAfter := b.allow("42", "openai/gpt-4o"); !ok {
			tripped = true
			if retryAfter != DefaultLoopBreakerOpenFor {
				t.Errorf("retryAfter = %v, want the default open duration %v", retryAfter, DefaultLoopBreakerOpenFor)
			}
			break
		}
		// All inside one window: no clock advance.
	}
	if !tripped {
		t.Fatalf("%d requests in a single window did not trip the backstop — the layer is decorative",
			DefaultLoopBreakerThreshold*2)
	}
}

// TestLoopBreaker_OperatorParamsApply proves the numbers are genuinely settings
// and not a knob that is parsed and then ignored — the failure mode that left a
// previous guard disarmed in every install.
func TestLoopBreaker_OperatorParamsApply(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(LoopBreakerParams{Threshold: 3, Window: 2 * time.Second, OpenFor: 7 * time.Second})
	b.now = clk.now

	for i := 0; i < 2; i++ {
		if ok, _ := b.allow("1", "m"); !ok {
			t.Fatalf("request %d below the operator threshold of 3 was rejected", i+1)
		}
		clk.advance(500 * time.Millisecond) // still inside the 2 s window
	}
	ok, retryAfter := b.allow("1", "m")
	if ok {
		t.Fatal("the 3rd request did not trip the operator-set threshold of 3")
	}
	if retryAfter != 7*time.Second {
		t.Fatalf("retryAfter = %v, want the operator-set 7s", retryAfter)
	}
}

// TestWithLoopBreakerParams_NegativeThresholdDisarms pins the explicit
// disarm sentinel. main() logs this mode loudly (logLoopBreakerMode); what
// matters here is that it really does admit everything rather than falling
// back to a default the operator did not ask for.
func TestWithLoopBreakerParams_NegativeThresholdDisarms(t *testing.T) {
	h := newTestHandler(&trackingRouter{}, nil, nil, WithLoopBreakerParams(LoopBreakerParams{Threshold: -1}))
	if h.loopGuard != nil {
		t.Fatal("a negative threshold must disarm the backstop entirely, not fall back to the default")
	}
}

// TestLoopBreaker_DefaultsMatchPublishedNumbers pins all three defaults to the
// numbers published to operators, so a change here cannot land without also
// updating the pages on-call reads. Issue #164: #12 raised these numbers and the
// docs kept saying 5 / 1 s / 30 s, which is the state that misleads someone
// diagnosing a 429 at 03:00.
//
// The two behavioural tests above pin only the threshold (via a 50-request burst
// that must pass and a 2×threshold burst that must trip); window and open
// duration were provably unpinned — mutating them to 30 s left the whole suite
// green. This test closes that.
//
// If you are changing a default: change it here, in loopbreaker.go, AND in
// elitea-docs — spec-bifrost-migration.mdx §2.6 and
// runbook-bifrost-cutover.mdx ("Incident response: circular-routing failure
// mode" + Prevention invariant 2).
func TestLoopBreaker_DefaultsMatchPublishedNumbers(t *testing.T) {
	if DefaultLoopBreakerThreshold != 1000 {
		t.Errorf("DefaultLoopBreakerThreshold = %d, want 1000 — elitea-docs publishes 1000 (issue #164)",
			DefaultLoopBreakerThreshold)
	}
	if DefaultLoopBreakerWindow != time.Second {
		t.Errorf("DefaultLoopBreakerWindow = %v, want 1s — elitea-docs publishes a 1 s window (issue #164)",
			DefaultLoopBreakerWindow)
	}
	if DefaultLoopBreakerOpenFor != 5*time.Second {
		t.Errorf("DefaultLoopBreakerOpenFor = %v, want 5s — elitea-docs publishes a 5 s open duration "+
			"(issue #164); 30 s is the pre-#12 value that caused the incident", DefaultLoopBreakerOpenFor)
	}
}

// TestLoopBreaker_DefaultOpenDurationIsEnforced is the behavioural half: the
// published 5 s is what a tripped tuple actually gets as Retry-After, not just a
// constant nobody reads.
func TestLoopBreaker_DefaultOpenDurationIsEnforced(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(LoopBreakerParams{}) // production defaults
	b.now = clk.now

	var retryAfter time.Duration
	for i := 0; i < DefaultLoopBreakerThreshold; i++ {
		var ok bool
		ok, retryAfter = b.allow("42", "openai/gpt-4o")
		if !ok {
			break
		}
	}
	if retryAfter != 5*time.Second {
		t.Fatalf("Retry-After on a default-config trip = %v, want the published 5s", retryAfter)
	}

	// Still open just before the 5 s elapses, closed after.
	clk.advance(5*time.Second - time.Millisecond)
	if ok, _ := b.allow("42", "openai/gpt-4o"); ok {
		t.Fatal("tuple admitted before the published 5 s open duration elapsed")
	}
	clk.advance(2 * time.Millisecond)
	if ok, _ := b.allow("42", "openai/gpt-4o"); !ok {
		t.Fatal("tuple still rejected after the published 5 s open duration elapsed")
	}
}

// TestLoopBreaker_DefaultWindowIsEnforced pins the published 1 s sliding window:
// threshold-many requests spread over MORE than 1 s must not trip.
func TestLoopBreaker_DefaultWindowIsEnforced(t *testing.T) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	b := newLoopBreaker(LoopBreakerParams{}) // production defaults
	b.now = clk.now

	// 2× the threshold, one every 2 ms => 2 s+ of wall clock, never more than
	// ~500 inside any 1 s window. A window longer than the published 1 s would
	// accumulate all of them and trip.
	for i := 0; i < DefaultLoopBreakerThreshold*2; i++ {
		if ok, _ := b.allow("42", "openai/gpt-4o"); !ok {
			t.Fatalf("request %d tripped the backstop although it arrived outside the published 1 s "+
				"window — the window is wider than documented", i+1)
		}
		clk.advance(2 * time.Millisecond)
	}
}

// TestLoopBreaker_ObserveReportsTheCircuitAndRecordsNothing pins the read-only
// half of the breaker.
//
// WHY IT EXISTS. The realtime budget re-check runs on a ticker for the whole
// life of every live session, and it used to call allow(). Every tick then
// appended a timestamp to the tuple's sliding window, exactly as an arriving
// request does, so long sessions on one (project, model) pair could open the
// circuit for that project's REAL traffic. observe() must answer the same
// question and leave the window alone.
func TestLoopBreaker_ObserveReportsTheCircuitAndRecordsNothing(t *testing.T) {
	c := &testClock{t: time.Unix(0, 0)}
	b := newLoopBreaker(LoopBreakerParams{Threshold: 3, Window: time.Second, OpenFor: 5 * time.Second})
	b.now = c.now

	// A closed circuit admits, and 100 observations do not move it toward the
	// threshold of 3.
	for i := 0; i < 100; i++ {
		if ok, retry := b.observe("p1", "m1"); !ok {
			t.Fatalf("observe #%d refused a tuple with an empty window (retry=%v)", i, retry)
		}
	}
	if got := len(b.hits["p1\x00m1"]); got != 0 {
		t.Fatalf("the window holds %d hits after 100 observations, want 0: an observation is not an arrival", got)
	}

	// An arrival still counts, and the circuit still opens on the threshold.
	for i := 0; i < 3; i++ {
		b.allow("p1", "m1")
	}
	// An open circuit is REPORTED by observe. Losing that would leave a live
	// session ungated by the backstop it is meant to respect.
	ok, retry := b.observe("p1", "m1")
	if ok {
		t.Fatal("observe admitted a tuple whose circuit is open")
	}
	if retry <= 0 {
		t.Fatalf("observe returned retryAfter=%v on an open circuit, want a positive wait", retry)
	}

	// A different tuple is untouched.
	if ok, _ := b.observe("p1", "m2"); !ok {
		t.Fatal("observe refused a tuple whose own circuit is closed")
	}
}
