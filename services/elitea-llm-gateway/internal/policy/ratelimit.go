package policy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// RateCounter is the shared counter the limiter runs on. *nats.Client satisfies
// it; tests inject a fake. It is an interface here rather than a concrete
// dependency so this package never imports the NATS client and stays testable
// without a live server.
type RateCounter interface {
	// IncrRateLimit adds delta to the bucket and returns the new total.
	IncrRateLimit(ctx context.Context, subject string, delta int64) (int64, error)
	// ReadRateLimit returns the bucket's current total, 0 when unset.
	ReadRateLimit(ctx context.Context, subject string) (int64, error)
}

// SubjectBuilder builds a counter subject for (kind, key, window). It is
// injected so the policy package does not import the NATS package. Production
// passes nats.RateLimitSubject.
type SubjectBuilder func(kind, key string, windowUnix int64) string

// Counter kinds. These MUST match the nats package's RateKind constants; the
// two are separate declarations because neither package may import the other.
const (
	kindRequests = "req"
	kindTokens   = "tok"
)

// rateLimitOpTimeout bounds one counter round-trip on the admission path. The
// NATS client applies its own OpTimeout too; this is the outer bound so a
// wedged client cannot hold an /llm request open.
const rateLimitOpTimeout = 2 * time.Second

// Limiter enforces authored per-minute rate limits against a shared counter.
//
// # Two semantics worth being explicit about
//
// REQUESTS are counted on arrival: the limiter increments first and refuses if
// the new total exceeds the ceiling. A refused request therefore still counts
// toward its window. That is the standard fixed-window behaviour and it is
// self-limiting — a caller hammering a closed window keeps it closed, which is
// the intent — but it does mean the observed count can exceed the ceiling.
//
// TOKENS cannot be counted on arrival, because the token cost of a request is
// unknown until the provider answers. The limiter therefore READS the window's
// token total at admission and refuses when it has already reached the ceiling,
// then adds the actual usage after the completion. The consequence, stated
// plainly: a token ceiling is enforced on the request AFTER the one that
// crossed it, so a single very large request can overshoot by its own size.
// There is no way to do better without pre-tokenising every request in the
// gateway, which would put a tokeniser for every provider dialect on the hot
// path.
//
// # When the counter is unavailable
//
// The limiter FAILS OPEN, loudly. A rate limit is a protection against
// overload, not a billing control: refusing all traffic because the counter is
// unreachable converts a NATS outage into a total outage, and the budget gate
// — which IS the spend control — has its own fail-mode FSM for exactly that
// decision. Every fail-open is counted in Degraded() and logged, so the gap is
// measurable rather than assumed.
type Limiter struct {
	// counter is bound late: a gateway that boots while NATS is down starts
	// with none, and the background recovery loop calls Bind when NATS comes
	// up. Read atomically because Admit runs on every /llm request goroutine.
	counter atomic.Pointer[counterBox]
	subject SubjectBuilder
	log     *slog.Logger
	now     func() time.Time

	// degraded counts admissions that skipped the limit because the counter was
	// unreachable. It is a gauge of how much enforcement was actually lost.
	degraded atomic.Int64
	// refused counts admissions the limiter blocked.
	refused atomic.Int64
	// failing is true while the most recent counter round-trip failed. It is
	// cleared by the next success, so it tracks a NATS drop AND its recovery
	// without any background probing.
	failing atomic.Bool
}

// counterBox lets an interface value live in an atomic.Pointer.
type counterBox struct{ c RateCounter }

// RateStatus is the limiter's operator-facing state.
type RateStatus string

const (
	// RateStatusOK: bound to a counter and the last round-trip succeeded.
	RateStatusOK RateStatus = "ok"
	// RateStatusDisabled: no counter has been bound; limits are NOT enforced.
	RateStatusDisabled RateStatus = "disabled"
	// RateStatusDegraded: bound, but the counter is failing; requests are being
	// admitted without their ceiling (fail open).
	RateStatusDegraded RateStatus = "degraded"
)

// Bind installs the counter after construction. It is how a gateway that
// started without NATS begins enforcing when NATS appears, without a restart.
// A nil counter is ignored.
func (l *Limiter) Bind(c RateCounter) {
	if l == nil || c == nil {
		return
	}
	l.counter.Store(&counterBox{c: c})
	l.failing.Store(false)
}

// Status reports whether per-minute limits are actually being kept.
func (l *Limiter) Status() RateStatus {
	switch {
	case !l.Enabled():
		return RateStatusDisabled
	case l.failing.Load():
		return RateStatusDegraded
	default:
		return RateStatusOK
	}
}

func (l *Limiter) rateCounter() RateCounter {
	if b := l.counter.Load(); b != nil {
		return b.c
	}
	return nil
}

// LimiterConfig configures a Limiter.
type LimiterConfig struct {
	Counter RateCounter
	Subject SubjectBuilder
	Logger  *slog.Logger
	Now     func() time.Time
}

// NewLimiter builds a Limiter. A nil Counter or Subject builds a limiter that
// enforces nothing and says so through Enabled, which is the posture of a
// gateway booted without NATS.
func NewLimiter(cfg LimiterConfig) *Limiter {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	l := &Limiter{subject: cfg.Subject, log: log, now: nowFn}
	l.Bind(cfg.Counter)
	return l
}

// Enabled reports whether the limiter can enforce anything at all.
func (l *Limiter) Enabled() bool {
	return l != nil && l.rateCounter() != nil && l.subject != nil
}

// Degraded returns how many admissions skipped enforcement because the counter
// was unreachable.
func (l *Limiter) Degraded() int64 {
	if l == nil {
		return 0
	}
	return l.degraded.Load()
}

// Refused returns how many admissions the limiter blocked.
func (l *Limiter) Refused() int64 {
	if l == nil {
		return 0
	}
	return l.refused.Load()
}

// RateDecision is the limiter's admission answer.
type RateDecision struct {
	// Allowed is false when a ceiling was reached.
	Allowed bool
	// Rule names the authored row that refused, for the log and the message.
	Rule string
	// Bucket is "requests" or "tokens".
	Bucket string
	// Limit and Observed describe the refusal in the operator's own units.
	Limit    int64
	Observed int64
	// RetryAfter is the time until the current window closes. It is always the
	// remainder of the window, never a fixed backoff: telling a caller to retry
	// before the window resets guarantees a second refusal.
	RetryAfter time.Duration
}

// allowed is the decision every admitted request gets.
var rateAllowed = RateDecision{Allowed: true}

// Admit applies the rate limit that governs sub, if any.
//
// countRequest says whether this is an ARRIVAL. An arrival increments the
// request bucket; a re-check of work already admitted — a live realtime
// session re-asking on its ticker — only observes it. The distinction is the
// same one the loop breaker draws, and for the same reason: a long session
// re-checking every few seconds would otherwise spend the project's whole
// request allowance on itself while making no requests at all. The token
// bucket is READ either way, so a session that has burned the token ceiling is
// still stopped.
//
// It returns rateAllowed when no definition applies, when the definition limits
// nothing, or when the counter is unavailable. def must come from
// Snapshot.RateLimit.
func (l *Limiter) Admit(ctx context.Context, def RateLimitDef, sub Subject, countRequest bool) RateDecision {
	if !l.Enabled() || !def.Limited() {
		return rateAllowed
	}
	now := l.now()
	window := windowStart(now)
	key := RateLimitKey(def.Name, sub.ProjectID)
	retry := windowRemaining(now)

	// The token bucket is checked FIRST, and by reading rather than
	// incrementing. Checking it after the request increment would burn a
	// request slot on a call the token ceiling was going to refuse anyway.
	if def.TokensPerMin > 0 {
		total, err := l.read(ctx, kindTokens, key, window)
		if err != nil {
			l.failOpen("tokens", def, sub, err)
			return rateAllowed
		}
		if total >= def.TokensPerMin {
			l.refused.Add(1)
			return RateDecision{
				Rule: def.Name, Bucket: "tokens",
				Limit: def.TokensPerMin, Observed: total, RetryAfter: retry,
			}
		}
	}

	if def.RequestsPerMin > 0 {
		if !countRequest {
			total, err := l.read(ctx, kindRequests, key, window)
			if err != nil {
				l.failOpen("requests", def, sub, err)
				return rateAllowed
			}
			if total > def.RequestsPerMin {
				l.refused.Add(1)
				return RateDecision{
					Rule: def.Name, Bucket: "requests",
					Limit: def.RequestsPerMin, Observed: total, RetryAfter: retry,
				}
			}
			return rateAllowed
		}
		total, err := l.incr(ctx, kindRequests, key, window, 1)
		if err != nil {
			l.failOpen("requests", def, sub, err)
			return rateAllowed
		}
		if total > def.RequestsPerMin {
			l.refused.Add(1)
			return RateDecision{
				Rule: def.Name, Bucket: "requests",
				Limit: def.RequestsPerMin, Observed: total, RetryAfter: retry,
			}
		}
	}
	return rateAllowed
}

// RecordTokens adds a completed request's token usage to the window that was
// current when the request was ADMITTED, not the window that is current now.
//
// admittedAt is the caller's admission time. Using it matters: a streamed
// response can finish minutes after it started, and attributing its tokens to
// the window it finished in would charge them to a window that never saw the
// request — inflating a quiet minute and letting the busy one look clean. When
// the admission window has already expired from the stream, the increment
// lands on a subject nothing will read, which is the correct outcome: the
// usage is historical and no live ceiling should move for it.
func (l *Limiter) RecordTokens(ctx context.Context, def RateLimitDef, sub Subject, admittedAt time.Time, tokens int64) {
	if !l.Enabled() || def.TokensPerMin <= 0 || tokens <= 0 {
		return
	}
	key := RateLimitKey(def.Name, sub.ProjectID)
	if _, err := l.incr(ctx, kindTokens, key, windowStart(admittedAt), tokens); err != nil {
		l.degraded.Add(1)
		l.log.Warn("policy: rate limit token usage was NOT recorded; the token ceiling is under-counting",
			"rule", def.Name, "project_id", sub.ProjectID, "tokens", tokens, "err", err)
	}
}

func (l *Limiter) read(ctx context.Context, kind, key string, window int64) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, rateLimitOpTimeout)
	defer cancel()
	n, err := l.rateCounter().ReadRateLimit(opCtx, l.subject(kind, key, window))
	l.failing.Store(err != nil)
	return n, err
}

func (l *Limiter) incr(ctx context.Context, kind, key string, window, delta int64) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, rateLimitOpTimeout)
	defer cancel()
	n, err := l.rateCounter().IncrRateLimit(opCtx, l.subject(kind, key, window), delta)
	l.failing.Store(err != nil)
	return n, err
}

func (l *Limiter) failOpen(bucket string, def RateLimitDef, sub Subject, err error) {
	l.degraded.Add(1)
	l.log.Warn("policy: rate-limit counter unavailable; ADMITTING the request without enforcing the limit",
		"bucket", bucket, "rule", def.Name, "project_id", sub.ProjectID, "err", err)
}

// RateLimitKey builds the bucket identity for a rule and a project.
//
// The project is part of the key even for a rule that names no project. A
// global "60 requests per minute" row means sixty per TENANT, not sixty shared
// across every tenant on the platform — the latter would let one project's
// traffic refuse another's, which is not a limit an operator would author on
// purpose. A rule that genuinely needs one shared ceiling is not expressible
// today, and that gap is better than silently picking the surprising reading.
func RateLimitKey(rule string, projectID int) string {
	return sanitiseToken(rule) + "-" + fmt.Sprint(projectID)
}

// sanitiseToken maps an operator-authored rule name onto the NATS token
// charset. A name is free text — it can hold dots, spaces and wildcards, each
// of which would otherwise split one bucket into several or, worse, make one
// rule's subject match another's.
func sanitiseToken(s string) string {
	if s == "" {
		return "unnamed"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// A name that sanitises to nothing usable would collide with every other
	// such name; the fallback at least keeps it a legal token.
	if strings.Trim(out, "_") == "" {
		return "unnamed"
	}
	// Bound the token so a very long name cannot push the subject past what the
	// server accepts.
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func windowStart(t time.Time) int64 {
	return t.UTC().Truncate(time.Minute).Unix()
}

func windowRemaining(t time.Time) time.Duration {
	u := t.UTC()
	return u.Truncate(time.Minute).Add(time.Minute).Sub(u)
}
