package policy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
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
// toward the window of the limit that refused it (and of no other limit; see
// AdmitAll). That is the standard fixed-window behaviour and it is
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
	// agents resolves a request's agent for the agent throttles. nil resolves
	// nothing, and then no agent-scoped row matches.
	agents *AgentResolver

	// degraded counts admissions that skipped the limit because the counter was
	// unreachable. It is a gauge of how much enforcement was actually lost.
	degraded atomic.Int64
	// refused counts admissions the limiter blocked.
	refused atomic.Int64
	// state is the operator-facing health (RateStatus). It has ONE writer
	// family — Bind and Probe, both called only from the gateway's rate-limit
	// monitor goroutine. Request-path outcomes never write it: a client that
	// cancels mid-call, or one slow request, must not make the gateway claim
	// its limits are down. Degraded() still counts per-request fail-opens.
	state atomic.Int32
}

// counterBox lets an interface value live in an atomic.Pointer.
type counterBox struct{ c RateCounter }

// RateStatus is the limiter's operator-facing state.
type RateStatus string

const (
	// RateStatusUnbound: no counter has been bound; limits are NOT enforced.
	RateStatusUnbound RateStatus = "unbound"
	// RateStatusDegraded: bound, but the last health probe failed; requests are
	// admitted without their ceiling (fail open).
	RateStatusDegraded RateStatus = "degraded"
	// RateStatusEnforced: bound and the last health probe succeeded.
	RateStatusEnforced RateStatus = "enforced"
)

const (
	stateUnbound int32 = iota
	stateDegraded
	stateEnforced
)

// Bind installs the counter after construction. It is how a gateway that
// started without NATS begins enforcing when NATS appears, without a restart.
//
// Bind never reports enforced on faith: the state starts degraded and one
// Probe runs immediately, so "enforced" always means a read succeeded. A nil
// counter is ignored. Call it only from the monitor goroutine (single writer),
// or before the limiter is shared.
func (l *Limiter) Bind(c RateCounter) {
	if l == nil || c == nil {
		return
	}
	l.state.Store(stateDegraded)
	l.counter.Store(&counterBox{c: c})
	l.Probe(context.Background())
}

// Probe performs one cheap counter read with the caller's own context and
// records the result as the limiter's health. A failure caused by ctx itself
// being cancelled or timing out is ignored entirely — that is the prober
// shutting down, not NATS failing. Single writer: monitor goroutine only.
func (l *Limiter) Probe(ctx context.Context) {
	if l == nil {
		return
	}
	c := l.rateCounter()
	if c == nil || l.subject == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, rateLimitOpTimeout)
	defer cancel()
	_, err := c.ReadRateLimit(pctx, l.subject(kindRequests, "_health", 0))
	switch {
	case err == nil:
		l.state.Store(stateEnforced)
	case ctx.Err() != nil:
		// our own shutdown; say nothing
	default:
		l.state.Store(stateDegraded)
	}
}

// Status reports whether per-minute limits are actually being kept.
func (l *Limiter) Status() RateStatus {
	if !l.Enabled() {
		return RateStatusUnbound
	}
	if l.state.Load() == stateDegraded {
		return RateStatusDegraded
	}
	return RateStatusEnforced
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
	// Agents reads the agent of a runtime execution, for the agent throttles
	// (ADR-0041 decision 4). nil disables agent throttles only.
	Agents AgentLookup
	// Resolver is a resolver the caller shares with other users (the
	// guardrail selection). It takes precedence over Agents. Production
	// passes one resolver over NewPostgresAgentLookup.
	Resolver *AgentResolver
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
	switch {
	case cfg.Resolver != nil:
		l.agents = cfg.Resolver
	case cfg.Agents != nil:
		l.agents = NewAgentResolver(cfg.Agents, log, nowFn)
	}
	if cfg.Counter != nil {
		// Configured counters start unprobed (degraded) and do not read at
		// construction; the monitor's first Probe promotes them to enforced.
		l.counter.Store(&counterBox{c: cfg.Counter})
		l.state.Store(stateDegraded)
	}
	return l
}

// Enabled reports whether the limiter can enforce anything at all.
func (l *Limiter) Enabled() bool {
	return l != nil && l.rateCounter() != nil && l.subject != nil
}

// Degraded returns how many admissions skipped enforcement because the counter
// was unreachable, plus the failed agent lookups of the resolver. The
// resolver is shared with the guardrail selection, so a failed lookup there
// counts here too.
func (l *Limiter) Degraded() int64 {
	if l == nil {
		return 0
	}
	return l.degraded.Load() + l.agents.Failures()
}

// Agent returns the agent of a runtime execution, or the zero AgentRef when
// the request has none, or when it cannot be read (agent throttles then fail
// open, and Degraded counts it).
func (l *Limiter) Agent(ctx context.Context, executionID string) AgentRef {
	if l == nil {
		return AgentRef{}
	}
	return l.agents.Resolve(ctx, executionID)
}

// Agents returns the limiter's agent resolver, or nil.
func (l *Limiter) Agents() *AgentResolver {
	if l == nil {
		return nil
	}
	return l.agents
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
	// Class is what the refusing row limits: a project, an agent or a user.
	Class RateClass
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

// Admit applies the rate limit that governs sub, if any. It is AdmitAll with
// one definition; see AdmitAll for the semantics.
//
// It returns rateAllowed when no definition applies, when the definition limits
// nothing, or when the counter is unavailable. def must come from
// Snapshot.RateLimit.
func (l *Limiter) Admit(ctx context.Context, def RateLimitDef, sub Subject, countRequest bool) RateDecision {
	return l.AdmitAll(ctx, []RateLimitDef{def}, sub, countRequest)
}

// AdmitAll applies every rate limit that governs sub (Snapshot.RateLimits) as
// ONE admission: a request that one limit refuses consumes no allowance of the
// others.
//
// countRequest says whether this is an ARRIVAL. An arrival increments the
// request buckets; a re-check of work already admitted — a live realtime
// session re-asking on its ticker — only observes them. The distinction is the
// same one the loop breaker draws, and for the same reason: a long session
// re-checking every few seconds would otherwise spend the project's whole
// request allowance on itself while making no requests at all. The token
// buckets are READ either way, so a session that has burned the token ceiling
// is still stopped.
//
// # Order and compensation
//
//  1. Every token bucket is read first. Reads change nothing, so a refusal here
//     costs no request slot in any bucket.
//  2. The request buckets are incremented narrowest first: the agent throttle,
//     then the user throttle, then the project rate limit. A throttled agent
//     that retries after its 429 is therefore refused before its request
//     reaches the project bucket.
//  3. When a request bucket refuses, the limiter decrements each bucket that
//     this call already incremented (a compensating Nats-Incr of -1 on the same
//     window subject). The bucket that refused keeps its count: that is the
//     fixed-window behaviour documented on Limiter.
//
// The counter has no multi-subject transaction, so this is
// increment-then-compensate, not all-or-nothing. The remaining race: between an
// increment and its compensation, a concurrent request on that earlier bucket
// sees a count one higher and can be refused. The error is always toward
// refusing, never toward admitting over a ceiling, and it lasts one counter
// round-trip. When the compensating decrement fails, the earlier bucket keeps
// the extra count until its window closes; Degraded counts it and it is logged.
//
// A counter failure on one limit FAILS OPEN for that limit only; the other
// limits still apply.
func (l *Limiter) AdmitAll(ctx context.Context, defs []RateLimitDef, sub Subject, countRequest bool) RateDecision {
	if !l.Enabled() {
		return rateAllowed
	}
	limited := make([]RateLimitDef, 0, len(defs))
	for _, def := range defs {
		if def.Limited() {
			limited = append(limited, def)
		}
	}
	if len(limited) == 0 {
		return rateAllowed
	}
	// Narrowest first. RateLimits returns project, agent, user; a stable sort
	// on the class keeps that contract out of this function.
	sort.SliceStable(limited, func(i, j int) bool {
		return admitRank(limited[i].Class) < admitRank(limited[j].Class)
	})

	now := l.now()
	window := windowStart(now)
	retry := windowRemaining(now)

	// 1. Token buckets: read only. A limit whose counter fails here is skipped
	// for the rest of the admission, so one outage counts once in Degraded.
	failed := make([]bool, len(limited))
	for i, def := range limited {
		if def.TokensPerMin <= 0 {
			continue
		}
		total, err := l.read(ctx, kindTokens, BucketKey(def, sub), window)
		if err != nil {
			l.failOpen("tokens", def, sub, err)
			failed[i] = true
			continue
		}
		if total >= def.TokensPerMin {
			l.refused.Add(1)
			return RateDecision{
				Rule: def.Name, Class: def.Class, Bucket: "tokens",
				Limit: def.TokensPerMin, Observed: total, RetryAfter: retry,
			}
		}
	}

	// 2. Request buckets.
	var counted []RateLimitDef
	for i, def := range limited {
		if def.RequestsPerMin <= 0 || failed[i] {
			continue
		}
		key := BucketKey(def, sub)
		var (
			total int64
			err   error
		)
		if countRequest {
			total, err = l.incr(ctx, kindRequests, key, window, 1)
		} else {
			total, err = l.read(ctx, kindRequests, key, window)
		}
		if err != nil {
			l.failOpen("requests", def, sub, err)
			continue
		}
		if total > def.RequestsPerMin {
			l.refused.Add(1)
			// 3. Give back the slots this refused request took in the
			// buckets that admitted it.
			l.compensate(ctx, counted, sub, window)
			return RateDecision{
				Rule: def.Name, Class: def.Class, Bucket: "requests",
				Limit: def.RequestsPerMin, Observed: total, RetryAfter: retry,
			}
		}
		if countRequest {
			counted = append(counted, def)
		}
	}
	return rateAllowed
}

// admitRank orders the classes for AdmitAll: agent, user, project.
func admitRank(c RateClass) int {
	switch c {
	case RateClassAgent:
		return 0
	case RateClassUser:
		return 1
	}
	return 2
}

// compensate decrements the request bucket of each def in window by one. It
// runs on a request that a later bucket refused.
func (l *Limiter) compensate(ctx context.Context, defs []RateLimitDef, sub Subject, window int64) {
	for _, def := range defs {
		if _, err := l.incr(ctx, kindRequests, BucketKey(def, sub), window, -1); err != nil {
			l.degraded.Add(1)
			l.log.Warn("policy: rate limit slot of a refused request was NOT given back; the bucket over-counts until its window closes",
				"rule", def.Name, "class", def.Class.String(), "project_id", sub.ProjectID, "err", err)
		}
	}
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
	key := BucketKey(def, sub)
	if _, err := l.incr(ctx, kindTokens, key, windowStart(admittedAt), tokens); err != nil {
		l.degraded.Add(1)
		l.log.Warn("policy: rate limit token usage was NOT recorded; the token ceiling is under-counting",
			"rule", def.Name, "class", def.Class.String(), "project_id", sub.ProjectID, "tokens", tokens, "err", err)
	}
}

func (l *Limiter) read(ctx context.Context, kind, key string, window int64) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, rateLimitOpTimeout)
	defer cancel()
	return l.rateCounter().ReadRateLimit(opCtx, l.subject(kind, key, window))
}

func (l *Limiter) incr(ctx context.Context, kind, key string, window, delta int64) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, rateLimitOpTimeout)
	defer cancel()
	return l.rateCounter().IncrRateLimit(opCtx, l.subject(kind, key, window), delta)
}

func (l *Limiter) failOpen(bucket string, def RateLimitDef, sub Subject, err error) {
	l.degraded.Add(1)
	l.log.Warn("policy: rate-limit counter unavailable; ADMITTING the request without enforcing the limit",
		"bucket", bucket, "rule", def.Name, "class", def.Class.String(), "project_id", sub.ProjectID, "err", err)
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

// BucketKey builds the counter identity for a rule and the request it governs.
//
// A project rate limit counts per project (RateLimitKey). A throttle counts per
// principal (ADR-0041 decision 4):
//
//   - an agent throttle counts per agent, keyed on the agent's own project and
//     application id, so two agents of one project never share a counter. When
//     the row also names users, it counts per agent and user.
//   - a user throttle counts per user, across every project the row selects.
//     It is a limit on the person, not on one of their projects.
//
// The class letter ("a", "u") keeps the three key families apart: a project
// key ends in "-<project>", which no throttle key does.
func BucketKey(def RateLimitDef, sub Subject) string {
	switch def.Class {
	case RateClassAgent:
		key := fmt.Sprintf("%s-a%d_%d", sanitiseToken(def.Name), sub.Agent.ProjectID, sub.Agent.ApplicationID)
		if len(def.Scope.UserIDs) > 0 {
			key += fmt.Sprintf("-u%d", sub.UserID)
		}
		return key
	case RateClassUser:
		return fmt.Sprintf("%s-u%d", sanitiseToken(def.Name), sub.UserID)
	}
	return RateLimitKey(def.Name, sub.ProjectID)
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
