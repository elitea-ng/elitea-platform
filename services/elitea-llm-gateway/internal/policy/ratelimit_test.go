package policy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCounter is an in-memory stand-in for the NATS counter stream. It records
// every subject it saw so a test can assert the WINDOW a value landed in, which
// is the part the token-attribution rule turns on.
type fakeCounter struct {
	mu       sync.Mutex
	totals   map[string]int64
	failIncr error
	failRead error
	// failDecr fails only negative deltas: the compensation of AdmitAll.
	failDecr error
	incrs    []string
	reads    []string
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{totals: map[string]int64{}}
}

func (f *fakeCounter) IncrRateLimit(_ context.Context, subject string, delta int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrs = append(f.incrs, subject)
	if f.failIncr != nil {
		return 0, f.failIncr
	}
	if delta < 0 && f.failDecr != nil {
		return 0, f.failDecr
	}
	f.totals[subject] += delta
	return f.totals[subject], nil
}

func (f *fakeCounter) ReadRateLimit(_ context.Context, subject string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, subject)
	if f.failRead != nil {
		return 0, f.failRead
	}
	return f.totals[subject], nil
}

func testSubject(kind, key string, window int64) string {
	return kind + "|" + key + "|" + time.Unix(window, 0).UTC().Format(time.RFC3339)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestLimiter(c RateCounter, now time.Time) *Limiter {
	return NewLimiter(LimiterConfig{
		Counter: c,
		Subject: testSubject,
		Logger:  quietLogger(),
		Now:     func() time.Time { return now },
	})
}

func TestLimiterRefusesOverTheRequestCeiling(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	def := RateLimitDef{Name: "cap", RequestsPerMin: 3}
	sub := Subject{ProjectID: 7}

	for i := 1; i <= 3; i++ {
		if dec := l.Admit(context.Background(), def, sub, true); !dec.Allowed {
			t.Fatalf("request %d was refused under a ceiling of 3: %+v", i, dec)
		}
	}
	dec := l.Admit(context.Background(), def, sub, true)
	if dec.Allowed {
		t.Fatal("the fourth request was admitted under a ceiling of 3")
	}
	if dec.Bucket != "requests" || dec.Limit != 3 || dec.Observed != 4 {
		t.Errorf("refusal does not describe itself in the operator's units: %+v", dec)
	}
	if dec.RetryAfter <= 0 || dec.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want the remainder of the window", dec.RetryAfter)
	}
	if l.Refused() != 1 {
		t.Errorf("Refused() = %d, want 1", l.Refused())
	}
}

// TestRecheckDoesNotConsumeARequestSlot is the realtime case: a session that
// re-asks on a ticker must not spend the project's allowance on its own gating.
func TestRecheckDoesNotConsumeARequestSlot(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	def := RateLimitDef{Name: "cap", RequestsPerMin: 2}
	sub := Subject{ProjectID: 7}

	for i := 0; i < 10; i++ {
		if dec := l.Admit(context.Background(), def, sub, false); !dec.Allowed {
			t.Fatalf("re-check %d was refused with no arrivals recorded: %+v", i, dec)
		}
	}
	c.mu.Lock()
	incrs := len(c.incrs)
	c.mu.Unlock()
	if incrs != 0 {
		t.Errorf("re-checks incremented the request counter %d times", incrs)
	}
	// Two real arrivals still fit.
	for i := 0; i < 2; i++ {
		if dec := l.Admit(context.Background(), def, sub, true); !dec.Allowed {
			t.Fatalf("arrival %d was refused after re-checks: %+v", i, dec)
		}
	}
}

// TestTokenCeilingIsEnforcedOnTheNextRequest pins the semantic the limiter
// cannot avoid, so that a change to it is a deliberate one.
func TestTokenCeilingIsEnforcedOnTheNextRequest(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	def := RateLimitDef{Name: "tok", TokensPerMin: 1000}
	sub := Subject{ProjectID: 7}

	// The first request is admitted: nothing has been consumed yet.
	if dec := l.Admit(context.Background(), def, sub, true); !dec.Allowed {
		t.Fatalf("the first request was refused: %+v", dec)
	}
	// It then overshoots the whole window by itself.
	l.RecordTokens(context.Background(), def, sub, testNow, 5000)

	dec := l.Admit(context.Background(), def, sub, true)
	if dec.Allowed {
		t.Fatal("the request after the ceiling was crossed was admitted")
	}
	if dec.Bucket != "tokens" || dec.Observed != 5000 || dec.Limit != 1000 {
		t.Errorf("refusal = %+v", dec)
	}
}

// TestTokensLandInTheAdmissionWindow is the streaming case: a response that
// finishes minutes later must not be charged to the window it finished in.
func TestTokensLandInTheAdmissionWindow(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	admittedAt := testNow
	// The limiter's own clock has moved on by three minutes.
	l := newTestLimiter(c, testNow.Add(3*time.Minute))
	def := RateLimitDef{Name: "tok", TokensPerMin: 1000}
	sub := Subject{ProjectID: 7}

	l.RecordTokens(context.Background(), def, sub, admittedAt, 250)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.incrs) != 1 {
		t.Fatalf("want one increment, got %v", c.incrs)
	}
	wantWindow := time.Unix(windowStart(admittedAt), 0).UTC().Format(time.RFC3339)
	if !strings.Contains(c.incrs[0], wantWindow) {
		t.Errorf("tokens landed on %q, want the admission window %s", c.incrs[0], wantWindow)
	}
}

// TestLimiterFailsOpenAndCountsIt is the documented outage behaviour. The count
// is the point: an unenforced limit must be measurable, not assumed.
func TestLimiterFailsOpenAndCountsIt(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	c.failIncr = errors.New("nats: unavailable")
	c.failRead = errors.New("nats: unavailable")
	l := newTestLimiter(c, testNow)
	def := RateLimitDef{Name: "cap", RequestsPerMin: 1, TokensPerMin: 1}

	for i := 0; i < 3; i++ {
		if dec := l.Admit(context.Background(), def, Subject{ProjectID: 7}, true); !dec.Allowed {
			t.Fatalf("the limiter failed CLOSED on a counter outage: %+v", dec)
		}
	}
	if l.Degraded() != 3 {
		t.Errorf("Degraded() = %d, want 3 — the unenforced admissions must be counted", l.Degraded())
	}
	if l.Refused() != 0 {
		t.Errorf("Refused() = %d, want 0", l.Refused())
	}
}

func TestDisabledLimiterAdmitsEverything(t *testing.T) {
	t.Parallel()

	var nilLimiter *Limiter
	if nilLimiter.Enabled() {
		t.Error("a nil limiter reported itself enabled")
	}
	if dec := nilLimiter.Admit(context.Background(), RateLimitDef{RequestsPerMin: 1}, Subject{}, true); !dec.Allowed {
		t.Error("a nil limiter refused a request")
	}
	nilLimiter.RecordTokens(context.Background(), RateLimitDef{TokensPerMin: 1}, Subject{}, testNow, 10)

	noCounter := NewLimiter(LimiterConfig{Logger: quietLogger()})
	if noCounter.Enabled() {
		t.Error("a limiter with no counter reported itself enabled")
	}
	if dec := noCounter.Admit(context.Background(), RateLimitDef{RequestsPerMin: 1}, Subject{}, true); !dec.Allowed {
		t.Error("a limiter with no counter refused a request")
	}
}

// TestUnlimitedDefinitionSkipsTheCounter keeps an authored-but-empty row off
// the hot path entirely.
func TestUnlimitedDefinitionSkipsTheCounter(t *testing.T) {
	t.Parallel()

	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	if dec := l.Admit(context.Background(), RateLimitDef{Name: "empty"}, Subject{ProjectID: 1}, true); !dec.Allowed {
		t.Fatal("an unlimited definition refused a request")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.incrs) != 0 || len(c.reads) != 0 {
		t.Errorf("an unlimited definition still hit the counter: incrs=%v reads=%v", c.incrs, c.reads)
	}
}

// TestRateLimitKeyIsPerProjectAndSubjectSafe pins both properties of the key:
// one bucket per tenant, and a name that cannot break the subject grammar.
func TestRateLimitKeyIsPerProjectAndSubjectSafe(t *testing.T) {
	t.Parallel()

	if RateLimitKey("cap", 7) == RateLimitKey("cap", 8) {
		t.Error("two projects share one bucket; one project's traffic would refuse another's")
	}
	got := RateLimitKey("all traffic.>*", 7)
	for _, bad := range []string{".", " ", ">", "*"} {
		if strings.Contains(got, bad) {
			t.Errorf("key %q contains %q, which breaks the NATS subject grammar", got, bad)
		}
	}
	if RateLimitKey("", 1) != "unnamed-1" {
		t.Errorf("an unnamed rule produced %q", RateLimitKey("", 1))
	}
	if len(RateLimitKey(strings.Repeat("x", 500), 1)) > 80 {
		t.Error("a very long rule name produced an unbounded subject token")
	}
}

func TestWindowRemaining(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 23, 12, 0, 15, 0, time.UTC)
	if got := windowRemaining(at); got != 45*time.Second {
		t.Errorf("windowRemaining = %v, want 45s", got)
	}
}
