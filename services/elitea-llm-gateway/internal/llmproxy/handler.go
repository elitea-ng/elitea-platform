package llmproxy

import (
	"context"
	"encoding/json"
	"expvar"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/overhead"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/pkg/ssewriter"
)

// Handler serves the /llm dialect surface. It decodes OpenAI/Anthropic wire
// bodies into bifrost/core request structs, calls the embedded core methods
// through the LLMRouter seam, and writes a net/http SSE loop over the returned
// stream channel (design §6.3). It never calls the fasthttp-bound integrations
// factories.
type Handler struct {
	router LLMRouter
	logger *slog.Logger
	// identitySecret verifies the edge's signed identity headers. An empty
	// secret disables verification (the mTLS transport still authenticates the
	// hop) — matching the edge, which only signs when a secret is configured.
	identitySecret []byte
	// models synthesises the per-project /llm/v1/models set from Postgres
	// (design §4.2, §3.4). nil when the gateway is booted without a database:
	// the /v1/models surface then reports an empty set rather than erroring.
	models *ModelResolver
	// budgetRef publishes the whole NATS budget path — the admission gate, the
	// price calculator, the two event publishers and the budget_used reader —
	// as ONE immutable value (budget_plane.go).
	//
	// It is an atomic pointer because the wiring can arrive AFTER the handler
	// serves traffic: a gateway that starts while NATS is unreachable has no
	// gate at all, and a later re-dial installs one (issue #315). The fields it
	// replaces were plain struct fields read on the money path with no
	// synchronisation, so a late install was a data race on billing.
	//
	// Read it with h.budget(); never touch this field directly.
	budgetRef atomic.Pointer[budgetPlane]

	// installMu serialises the read-modify-write in mutateBudget. The atomic
	// pointer makes every READ safe on its own; two concurrent WRITES would
	// still lose one of the two updates.
	installMu sync.Mutex

	// policy supplies the authored governance DEFINITIONS from
	// gateway.governance_config (policy_gate.go). nil disables every authored
	// control — model allowlists, rate limits, MCP allowlists, routing rules
	// and rate policy — and is the posture of a gateway with no database.
	// It is a DIFFERENT plane from the budget gate: that one enforces spend
	// against a counter, this one enforces what an operator wrote down.
	policy PolicySource
	// rateLimiter enforces the authored per-minute ceilings against the shared
	// NATS counter. nil (or a limiter with no counter) disables rate limiting
	// while leaving the other authored controls in force.
	rateLimiter *policy.Limiter
	// routePick is the weighted-target draw for routing rules. nil selects a
	// random source; a test sets it to make a weighted rule determinate.
	routePick func(total float64) float64

	// billingWg tracks in-flight async billing goroutines (Fix round-3 #2).
	// DrainBilling() blocks until all goroutines complete. billingClosing is
	// set to 1 atomically before DrainBilling waits, preventing any new Add
	// after Wait starts (Add-after-Wait panic guard).
	billingWg      sync.WaitGroup
	billingClosing atomic.Int32

	// loopGuard is the per-(project_id, model) circular-routing circuit
	// breaker (spec §2.6 guard #2). nil = disarmed (unit-test construction);
	// production wiring arms it via WithLoopBreaker.
	loopGuard *loopBreaker

	// hopMarker is the deployment's hop marker — the actual anti-circular-
	// routing mechanism (issue #164, hopguard.go). nil = detection UNARMED
	// (unit-test construction, or a deployment with no GATEWAY_HOP_SECRET);
	// production wiring arms it via WithHopMarker.
	//
	// It is a DIFFERENT layer from loopGuard above and neither replaces the
	// other: this one detects a loop, that one bounds amplification.
	hopMarker *hopmarker.Marker

	// streamGrace is how long a stream whose SSE loop exited early may keep its
	// provider stream alive waiting for the authoritative usage trailer
	// (issue #9, DECISIONS.md 2026-08-05). 0 disables the mechanism: the
	// provider context is then bound to the request context exactly as before,
	// and an early exit bills nothing. Set via WithStreamGrace.
	streamGrace time.Duration

	// drainLimit bounds concurrently-detained streams globally AND per project:
	// each drain holds a goroutine and an open provider socket for up to
	// streamGrace. nil = unbounded (unit-test construction only); production
	// wiring always sets a limit via WithStreamDrainLimit.
	drainLimit *slotLimiter

	// drainWg tracks detached stream drains, SEPARATELY from billingWg. The two
	// must be waited on in order: drains have to settle (and spawn their
	// billing goroutines) BEFORE billing is closed, or a drain that recovered
	// an authoritative trailer has its increment refused — the deploy-time
	// spend loss found in review.
	//
	// drainMu/drainWaiting are the Add-after-Wait guard. They are deliberately
	// a MUTEX rather than an atomic: the check and the Add must be one step
	// with respect to DrainBilling's Wait, and drainWaiting must flip in phase
	// 2 (DrainBilling), NOT in phase 1 (StopStreamGrace). An earlier cut keyed
	// tracking off the phase-1 flag, which left every drain spawned during the
	// HTTP-drain window untracked — so Wait() skipped exactly the drains
	// shutdown was supposed to protect (reproduced: 0 UpdateUsage calls).
	drainWg      sync.WaitGroup
	drainMu      sync.Mutex
	drainWaiting bool

	// drainsClosing tells a NEW drain not to wait out the full grace once
	// shutdown has begun. It governs the GRACE only — never tracking.
	drainsClosing atomic.Int32

	// drainClosing is closed by DrainBilling so a detached drain stops waiting
	// for a provider trailer once graceful shutdown starts — the pod's
	// termination grace must not be held hostage to streamGrace.
	drainClosing     chan struct{}
	drainClosingOnce sync.Once

	// streamCtxHook receives every context built by newStreamContext. It is a
	// TEST SEAM ONLY (nil in production, never set by any HandlerOption): the
	// "was the stream context cancelled when the budget gate blocked the
	// request?" assertion has no other observation point, because a blocked
	// request never reaches the router.
	streamCtxHook func(*schemas.BifrostContext)

	// realtimeDialer opens the PROVIDER side of a /llm/v1/realtime session
	// (realtime.go). nil disables the route: it then answers 501 rather than
	// upgrading a socket it could never connect to anything.
	realtimeDialer RealtimeDialer

	// realtimeLimit bounds concurrent realtime sessions globally and per
	// project. It is a SEPARATE pool from drainLimit — a live call must not
	// compete for slots with abandoned SSE streams.
	realtimeLimit *slotLimiter

	// realtimeRecheck is how often a live session re-asks the budget gate. It is
	// the MANDATORY bound on a session's spend: a socket the tenant holds open
	// is not bounded by its own admission check (realtime.go, fact F3).
	realtimeRecheck time.Duration

	// realtimeKeepalive is the ping interval on a live session's two sockets.
	// It is NOT operator configuration: the value is bounded by what proxies
	// and load balancers do, not by anything a deployment chooses. It is a
	// field rather than a bare constant so a test can drive the pinger without
	// waiting 20 s for one tick.
	realtimeKeepalive time.Duration

	// realtimePingBound is how long ONE keepalive ping waits for its pong. It
	// is NOT operator configuration and it is NOT shared between the two peers:
	// each ping gets its own deadline, so a caller that is slow to pong cannot
	// spend the provider's budget and make a live provider read as a dead one.
	// It is a field rather than a bare constant so a test can drive a slow peer
	// without waiting the production bound. See realtimeSession.pingPeers.
	realtimePingBound time.Duration

	// realtimeGateBound is how long a live session waits for ONE budget-gate
	// answer before it reads the silence as an outage. It is NOT operator
	// configuration: the value follows what admissionVerdict can legitimately
	// take, not what a deployment chooses. It is a field rather than a bare
	// constant so a test can drive a STALLED budget store without waiting the
	// production bound for it. See realtimeSession.gateVerdict.
	realtimeGateBound time.Duration

	// realtimeOrigins is the accept-side Origin allowlist. Empty is the
	// library's same-origin default, which is the secure one; it is never
	// InsecureSkipVerify.
	realtimeOrigins []string

	// sessionWg tracks live realtime sessions, SEPARATELY from drainWg and
	// billingWg. sessionMu/sessionWaiting are the same Add-after-Wait guard
	// trackDrain uses, and sessionClosing is what tells a live session to end.
	// CloseRealtimeSessions states why these are not the drain's.
	sessionWg          sync.WaitGroup
	sessionMu          sync.Mutex
	sessionWaiting     bool
	sessionClosing     chan struct{}
	sessionClosingOnce sync.Once

	// egressPolicy backs the /llm/v1/check_connection endpoint's SSRF gate
	// (#319). It is the SAME operator-configured allowlist GetKeysForProvider
	// applies to persisted credentials (*account.EliteaAccount implements
	// this), so a not-yet-saved credential under test is refused on the exact
	// terms a saved one would be (issue #13). nil makes CheckConnection refuse
	// every request — fail closed rather than skip the check.
	egressPolicy EgressPolicy
}

// HandlerOption customises Handler construction. It keeps NewHandler's core
// signature stable (router/logger/identitySecret) while letting later features
// — the models resolver — be wired in without churning existing call sites.
type HandlerOption func(*Handler)

// WithModelResolver wires the synthetic /llm/v1/models resolver. A nil resolver
// leaves the models surface reporting an empty set.
func WithModelResolver(r *ModelResolver) HandlerOption {
	return func(h *Handler) { h.models = r }
}

// WithEgressPolicy wires the operator's egress allowlist into the
// /llm/v1/check_connection endpoint (#319). A nil policy is a no-op — the
// endpoint then refuses every request, matching the fail-closed default the
// rest of the gateway uses for an unconfigured Account.
func WithEgressPolicy(p EgressPolicy) HandlerOption {
	return func(h *Handler) { h.egressPolicy = p }
}

// WithBudgetGate wires the pre-LLM budget enforcement gate. When gate is nil
// the option is a no-op (enforcement is skipped). calc must be non-nil when
// gate is non-nil — the cost Calculator turns the response's token counts into
// the billed amount. It is NOT used pre-flight: admission passes no estimate.
func WithBudgetGate(gate BudgetChecker, calc CostEstimator) HandlerOption {
	return func(h *Handler) {
		if gate == nil {
			return
		}
		h.mutateBudget(func(p *budgetPlane) {
			p.gate = gate
			p.calc = calc
		})
	}
}

// WithLoopBreaker arms the per-(project_id, model) amplification backstop with
// the default numbers. The composition root
// (cmd/elitea-llm-gateway/main.go) MUST arm this in production wiring —
// guarded by TestMainWiring — and does so via WithLoopBreakerParams so the
// operator's settings apply.
func WithLoopBreaker() HandlerOption {
	return WithLoopBreakerParams(LoopBreakerParams{})
}

// WithLoopBreakerParams arms the backstop with operator-supplied numbers
// (issue #12). A NEGATIVE Threshold disarms it entirely: the handler then has
// no loopGuard and admits every request. That is a legitimate operator choice —
// the layer cannot detect a loop (see loopbreaker.go) — but it must never be
// silent, so main() logs the resulting mode at startup.
func WithLoopBreakerParams(p LoopBreakerParams) HandlerOption {
	return func(h *Handler) {
		if p.Threshold < 0 {
			h.loopGuard = nil
			return
		}
		h.loopGuard = newLoopBreaker(p)
	}
}

// WithLoopBreakerClock arms the breaker exactly like WithLoopBreaker but reads
// time from now instead of time.Now. It exists so tests outside this package
// (internal/preflight) can drive the 1 s sliding window from a frozen clock
// instead of racing the wall clock — a burst that must land inside one second
// is otherwise flaky on a loaded CI box. Production wiring uses WithLoopBreaker;
// a nil now falls back to time.Now.
func WithLoopBreakerClock(p LoopBreakerParams, now func() time.Time) HandlerOption {
	return func(h *Handler) {
		h.loopGuard = newLoopBreaker(p)
		if now != nil {
			h.loopGuard.now = now
		}
	}
}

// WithAlertEventPublisher wires the gateway.events.* publisher used to emit
// the budget.soft_alert event when the 80% threshold alert fires (spec §8.3:
// "a soft-alert is recorded on gateway.events.*"). nil is a no-op (the alert
// still logs; nothing is published).
func WithAlertEventPublisher(p AlertEventPublisher) HandlerOption {
	return func(h *Handler) {
		h.mutateBudget(func(bp *budgetPlane) { bp.alerts = p })
	}
}

// WithOpsEventPublisher wires the operator-only gateway.events.ops.* publisher
// used for budget.unbilled_stream (issue #9). nil is a no-op (the loss still
// logs at WARN; nothing is published).
func WithOpsEventPublisher(p OpsEventPublisher) HandlerOption {
	return func(h *Handler) {
		h.mutateBudget(func(bp *budgetPlane) { bp.ops = p })
	}
}

// WithStreamGrace sets how long an early-exiting stream may keep its provider
// stream alive waiting for the authoritative usage trailer (issue #9). The
// value is clamped to [0, MaxStreamGrace]; 0 disables the mechanism entirely
// (provider context stays bound to the request context, early exits bill
// nothing). The composition root wires this from LLM_STREAM_GRACE_MS.
func WithStreamGrace(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d < 0 {
			d = 0
		}
		if d > MaxStreamGrace {
			d = MaxStreamGrace
		}
		h.streamGrace = d
	}
}

// WithStreamDrainLimit bounds how many abandoned streams may be drained
// concurrently. n <= 0 leaves the pool unbounded (test construction); the
// composition root always passes a positive limit.
func WithStreamDrainLimit(n int) HandlerOption {
	return func(h *Handler) {
		h.drainLimit = newDrainLimiter(n)
	}
}

// WithRealtimeDialer wires the provider side of the /llm/v1/realtime route. A
// nil dialer leaves the route answering 501: the alternative would be to accept
// a caller's socket and then have nothing to connect it to.
func WithRealtimeDialer(d RealtimeDialer) HandlerOption {
	return func(h *Handler) { h.realtimeDialer = d }
}

// WithRealtimeSessionLimit bounds concurrent realtime sessions on this replica.
// n <= 0 leaves the pool unbounded (test construction only); the composition
// root always passes a positive limit, because nothing else bounds a session —
// a hijacked connection has no server-side timeout (realtime.go, fact F6).
func WithRealtimeSessionLimit(n int) HandlerOption {
	return func(h *Handler) { h.realtimeLimit = realtimeSessionLimit(n) }
}

// WithRealtimeBudgetRecheck sets how often a live realtime session re-asks the
// budget gate. A non-positive value leaves the default in place: a session with
// no re-check is gated exactly once, for its whole life, which is the hole this
// mechanism exists to close.
func WithRealtimeBudgetRecheck(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.realtimeRecheck = d
		}
	}
}

// WithRealtimeOrigins sets the accept-side Origin allowlist for
// /llm/v1/realtime. An empty list keeps the same-origin default; the values come
// from the operator (LLM_REALTIME_ALLOWED_ORIGINS), never from a request.
func WithRealtimeOrigins(origins []string) HandlerOption {
	return func(h *Handler) { h.realtimeOrigins = origins }
}

// NewHandler builds a /llm Handler over the given router. logger may be nil
// (a discarding logger is substituted). identitySecret may be empty to disable
// HMAC verification of the forwarded identity headers. Optional features (the
// models resolver) are supplied via HandlerOption.
func NewHandler(router LLMRouter, logger *slog.Logger, identitySecret []byte, opts ...HandlerOption) *Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	h := &Handler{
		router:         router,
		logger:         logger,
		identitySecret: identitySecret,
		// Issue #9: the grace-period drain is ON by default so a Handler built
		// without the option (tests, embedders) still bills a disconnected
		// stream rather than silently dropping it. Production overrides the
		// duration and the concurrency bound from config.
		streamGrace:  DefaultStreamGrace,
		drainLimit:   newDrainLimiter(DefaultStreamDrainLimit),
		drainClosing: make(chan struct{}),
		// The realtime defaults are ON for the same reason the stream grace is:
		// a Handler built without the options must still bound a session rather
		// than gate it once and hold an unbounded socket.
		realtimeLimit:     realtimeSessionLimit(DefaultRealtimeMaxSessions),
		realtimeRecheck:   DefaultRealtimeBudgetRecheck,
		realtimeKeepalive: realtimeKeepalivePeriod,
		realtimePingBound: realtimeWriteTimeout,
		realtimeGateBound: realtimeGateTimeout,
		sessionClosing:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// StopStreamGrace is PHASE 1 of the shutdown sequence: it tells every in-flight
// stream drain to stop waiting for a provider usage trailer, WITHOUT closing
// billing. Streams that already hold (or are about to hold) authoritative usage
// can still bill.
//
// It exists because the two things DrainBilling used to do have opposite timing
// requirements (gateway-review blocker 1). "Stop waiting for trailers" must
// happen EARLY, before srv.Shutdown, so the stream grace cannot extend the
// pod's termination window. "Refuse new billing goroutines and wait" must
// happen LATE, after srv.Shutdown has quiesced the HTTP surface — otherwise
// billingClosing is set while SSE handlers are still live, and a drain that
// successfully recovered a trailer has its increment refused and its spend
// dropped on every rolling deploy.
//
// Safe to call more than once.
func (h *Handler) StopStreamGrace() {
	h.drainsClosing.Store(1)
	h.drainClosingOnce.Do(func() {
		if h.drainClosing != nil {
			close(h.drainClosing)
		}
	})
}

// DrainBilling is PHASE 2: it marks the handler as draining (no new billing
// goroutines will be spawned) and blocks until all in-flight async billing
// goroutines — including detached stream drains — complete.
//
// Call StopStreamGrace() first and srv.Shutdown() in between; see
// drainForShutdown in the composition root, whose ordering TestMainWiring
// asserts. Calling DrainBilling alone is still correct, just less forgiving:
// any stream still settling at that moment has its spend refused (and metered
// as billing_refused).
//
// Sequence mandated by the design:
//  1. handler.StopStreamGrace() — drains stop waiting for provider trailers.
//  2. srv.Shutdown()            — HTTP surface quiesces; live streams settle.
//  3. handler.DrainBilling()    — waits for updateUsage/drain goroutines.
//  4. govStore.Drain()          — waits for PersistOutageDelta goroutines.
func (h *Handler) DrainBilling() {
	// Idempotent: a caller that skipped phase 1 still gets the old semantics.
	h.StopStreamGrace()

	// Close the drain group and wait for it BEFORE closing billing. Drains have
	// already been told to stop waiting for trailers, so this is bounded by
	// drainShutdownTimeout — and it is what lets a drain holding recovered
	// provider usage spawn its billing goroutine instead of being refused.
	h.drainMu.Lock()
	h.drainWaiting = true
	h.drainMu.Unlock()
	h.drainWg.Wait()

	h.billingClosing.Store(1)
	h.billingWg.Wait()
}

// trackDrain registers a detached drain with the drain group, atomically with
// respect to DrainBilling's Wait. Returns false once the group is closed, in
// which case the caller must run untracked (it has already been cancelled, so
// it settles promptly).
func (h *Handler) trackDrain() bool {
	h.drainMu.Lock()
	defer h.drainMu.Unlock()
	if h.drainWaiting {
		return false
	}
	h.drainWg.Add(1)
	return true
}

// discard is an io.Writer that drops everything; used for the nil-logger case.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// newContext builds a BifrostContext for a request, trusting the edge's signed
// identity headers on the mTLS-internal network and injecting the resolved
// projectID as the Bifrost virtual-key value (design §5.3). It returns false
// (after writing a 403) when a configured identity secret does not verify.
func (h *Handler) newContext(w http.ResponseWriter, r *http.Request) (*schemas.BifrostContext, bool) {
	ctx, _, ok := h.buildContext(w, r, false)
	return ctx, ok
}

// newStreamContext builds the BifrostContext for a STREAMING request. Unlike
// newContext it decouples the provider stream from the client's request
// context: the parent is context.WithoutCancel(r.Context()), so a client
// disconnect no longer makes net/http cancel the context that bifrost's stream
// goroutine is watching (issue #9).
//
// That cancellation is what destroyed the authoritative usage trailer — the
// very record needed to bill the tokens the provider had already generated —
// and it is why simply draining the channel after a disconnect billed nothing.
// The returned streamCancel hands that decision to us instead: the SSE loop
// cancels on clean completion, and on an early exit the detached drain cancels
// once the trailer arrives or the grace period expires.
//
// The caller MUST ensure cancel is eventually invoked on every path (it is
// idempotent). Values — the virtual-key/project handle and user ID, on which
// per-project credential resolution depends — survive WithoutCancel unchanged.
func (h *Handler) newStreamContext(w http.ResponseWriter, r *http.Request) (*schemas.BifrostContext, *streamCancel, bool) {
	ctx, sc, ok := h.buildContext(w, r, h.streamGrace > 0)
	if ok && h.streamCtxHook != nil {
		h.streamCtxHook(ctx)
	}
	return ctx, sc, ok
}

// requestContext picks the streaming or unary context for a handler that serves
// both from one body (Chat, Responses, Messages). The caller owns the returned
// cancel until it hands it to a stream loop.
//
// It is also the one place that knows whether a request streams, for all three
// dialects, so it stamps the request log here. Before this, `SetStreaming` had
// no call site at all: every row said `streaming = false`, and the health view
// groups by that column, so a streamed 40-second call and a buffered 200 ms
// call shared one mean that described neither.
func (h *Handler) requestContext(w http.ResponseWriter, r *http.Request, streaming bool) (*schemas.BifrostContext, *streamCancel, bool) {
	requestlog.FromContext(r.Context()).SetStreaming(streaming)
	if streaming {
		return h.newStreamContext(w, r)
	}
	return h.buildContext(w, r, false)
}

// buildContext is the shared body of newContext / newStreamContext. When
// detach is false the returned context inherits request cancellation exactly as
// before; the cancel is still returned so callers can release the context's
// watcher deterministically.
func (h *Handler) buildContext(w http.ResponseWriter, r *http.Request, detach bool) (*schemas.BifrostContext, *streamCancel, bool) {
	if !verifySignature(r.Header, h.identitySecret) {
		writeError(w, http.StatusForbidden, "permission_error", "invalid identity signature", "")
		return nil, nil, false
	}

	// Unary (and grace-disabled streaming) requests inherit the request's
	// cancellation so a client disconnect propagates into core; no deadline
	// (the SSE path is long-lived, §9.5).
	parent := r.Context()
	if detach {
		parent = context.WithoutCancel(r.Context())
	}
	ctx, cancel := schemas.NewBifrostContextWithCancel(parent)
	sc := &streamCancel{fn: cancel}

	// vk = the resolved projectID handle (never the raw key). Only set when
	// present; a missing project is fatal at the gateway only when
	// IsVkMandatory is on (governance, BF0.4), not here.
	//
	// FIX #24: also propagate the caller's user ID so usage attribution and
	// audit trails carry the originating user, not just the project.
	if id := identityFromHeaders(r.Header); id.projectID != "" {
		ctx.SetValue(schemas.BifrostContextKeyVirtualKey, id.projectID)
		if id.userID != "" {
			ctx.SetValue(schemas.BifrostContextKeyUserID, id.userID)
		}
		// The tenant is the governance CEL `customer_id`. It has been forwarded
		// by the edge since the identity path was written and, until the
		// authored-governance plane landed, nothing read it — the same shape of
		// gap as the user id before issue #321. A routing rule scoped to a
		// customer needs it, so it goes on the context with the rest of the
		// identity rather than being re-read from headers later.
		if id.tenantID != "" {
			ctx.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, id.tenantID)
		}
		// The runtime execution this call was made from. It rides the context
		// with the rest of the identity rather than being re-read from the
		// headers at the billing site, because the billing path is reached from
		// a detached context (stream_drain) and from a goroutine, where the
		// request is long gone.
		if id.executionID != "" {
			ctx.SetValue(contextKeyExecutionID, id.executionID)
		}
	}
	return ctx, sc, true
}

// finish applies the response-header hygiene every /llm response gets: strip
// provider/litellm leakage and stamp the platform server name (design §2,
// §6.3). It must run before the status line is written.
func finish(h http.Header) {
	for k := range h {
		lk := canonicalLower(k)
		if hasPrefix(lk, "x-litellm-") || hasPrefix(lk, "llm_provider-") {
			h.Del(k)
		}
	}
	h.Set("Server", "Centry")
}

// ---- OpenAI dialect (catch-all /llm/v1/*) ----

// Chat handles POST /llm/v1/chat/completions. It streams when the body sets
// "stream": true, else returns a unary chat completion.
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	// t0 anchors the hop-overhead measurement (design §10.2 / gate BFF.9d).
	// X-Elapsed-Ms reports the gateway's overhead up to the instant core holds
	// the provider credential: body decode, identity verification, loop
	// breaker, the NATS budget check, the wait in the core provider queue, core
	// routing, and the per-request credential resolution (the Account's
	// Postgres read and Fernet decrypt).
	//
	// The credential half runs INSIDE the router call, so an overhead.Meter on
	// the context carries it back out: account.GetKeysForProvider marks the
	// Meter, and the stamp below reads the mark (issue #17).
	//
	// The header still EXCLUDES the provider round-trip — core dials the
	// provider only after it holds the key — and it excludes the gateway's own
	// response marshalling. A request that resolves no credential (a plugin
	// short-circuit, a direct key, a failure before the provider worker)
	// reports the pre-dispatch time alone. The k6 overhead script consumes this
	// header.
	t0 := time.Now()
	var req openai.OpenAIChatRequest
	raw, ok0 := decodeJSONRaw(w, r, &req)
	if !ok0 {
		return
	}
	streaming := isStream(req.Stream)
	ctx, sc, ok := h.requestContext(w, r, streaming)
	if !ok {
		return
	}
	// The Meter must reach the context BEFORE dispatch: the account marks it
	// from inside the router call.
	meter := overhead.Attach(ctx, t0)
	bifReq := req.ToBifrostChatRequest(ctx)

	// Map the caller's model id onto the provider's own model name (issue #317)
	// BEFORE the budget gate, so the gate and the provider see the same name.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	provider, model := providerModelFromChatReq(bifReq)
	// The authored MCP allowlist judges the servers the caller named, before
	// the budget gate: a refused request must not move a counter.
	if !h.checkMCPAllowlist(w, ctx, raw, provider, model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	// Pre-flight budget check (admission only; the cost is billed post-response).
	if !h.checkBudget(w, ctx, model) {
		sc.cancel() // blocked before dispatch: nothing owns the context
		return
	}

	// preDispatch is what the Meter reports when the router resolved no
	// credential. Take the snapshot HERE: after the router call, time.Since(t0)
	// carries the provider round-trip.
	preDispatch := time.Since(t0)

	if streaming {
		ch, bErr := h.router.ChatCompletionStreamRequest(ctx, bifReq)
		// Stamp before streamOpenAI: beginStream writes the response head, and
		// a header set after that never reaches the client.
		setElapsedHeader(w, meter.Overhead(preDispatch))
		// FIX #5: pass billing context so streamOpenAI can call updateUsage
		// after the channel drains with the final usage-carrying chunk.
		// Issue #9: streamOpenAI takes ownership of sc — it settles the stream
		// inline on a clean close, or hands both channel and cancel to the
		// detached drain on an early exit.
		h.streamOpenAI(w, r.Context().Done(), ctx, sc, provider, model, ch, bErr)
		return
	}
	defer sc.cancel()
	resp, bErr := h.router.ChatCompletionRequest(ctx, bifReq)
	// Stamp AFTER the router returns: credential resolution is the part of the
	// overhead that runs inside that call (issue #17). writeUnary below writes
	// the first body byte, so the header is still in time.
	setElapsedHeader(w, meter.Overhead(preDispatch))
	// Write the response FIRST (client-visible), then bill asynchronously.
	// updateUsage spawns a bounded goroutine so the HTTP response latency does
	// not include the NATS billing round-trip (FIX #18).
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		in, out := usageFromChatResponse(resp)
		h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
	}
}

// TextCompletion handles POST /llm/v1/completions (legacy text completions).
func (h *Handler) TextCompletion(w http.ResponseWriter, r *http.Request) {
	var req openai.OpenAITextCompletionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostTextCompletionRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// FIX #4: enforce the budget gate before calling the provider.
	provider, model := providerModelFromTextReq(bifReq)
	if !h.checkBudget(w, ctx, model) {
		return
	}

	resp, bErr := h.router.TextCompletionRequest(ctx, bifReq)
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		in, out := usageFromTextCompletionResponse(resp)
		h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
	}
}

// Embeddings handles POST /llm/v1/embeddings.
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	var req openai.OpenAIEmbeddingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostEmbeddingRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// FIX #4: enforce the budget gate before calling the provider.
	provider, model := providerModelFromEmbeddingReq(bifReq)
	if !h.checkBudget(w, ctx, model) {
		return
	}

	resp, bErr := h.router.EmbeddingRequest(ctx, bifReq)
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		in, out := usageFromEmbeddingResponse(resp)
		h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
	}
}

// Responses handles POST /llm/v1/responses (OpenAI Responses API). It streams
// when the body sets "stream": true.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	var req openai.OpenAIResponsesRequest
	raw, ok0 := decodeJSONRaw(w, r, &req)
	if !ok0 {
		return
	}
	streaming := isStream(req.Stream)
	ctx, sc, ok := h.requestContext(w, r, streaming)
	if !ok {
		return
	}
	bifReq := req.ToBifrostResponsesRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	// FIX #3: enforce the budget gate before calling the provider (mirrors Messages).
	provider, model := providerModelFromResponsesReq(bifReq)
	// The authored MCP allowlist judges the tools[] entries of type "mcp".
	if !h.checkMCPAllowlist(w, ctx, raw, provider, model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	if !h.checkBudget(w, ctx, model) {
		sc.cancel() // blocked before dispatch: nothing owns the context
		return
	}

	if streaming {
		ch, bErr := h.router.ResponsesStreamRequest(ctx, bifReq)
		// FIX #5: pass billing context so streamResponses can call updateUsage
		// after the channel drains with the final usage chunk.
		h.streamResponses(w, r.Context().Done(), ctx, sc, provider, model, ch, bErr, false)
		return
	}
	defer sc.cancel()
	resp, bErr := h.router.ResponsesRequest(ctx, bifReq)
	h.writeUnary(w, resp, bErr)
	// FIX #3: bill the unary response after writing to the client.
	if bErr == nil && resp != nil {
		in, out := usageFromResponsesResponse(resp)
		h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
	}
}

// ImageGeneration handles POST /llm/v1/images/generations (JSON body).
func (h *Handler) ImageGeneration(w http.ResponseWriter, r *http.Request) {
	var req openai.OpenAIImageGenerationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostImageGenerationRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// FIX #26: enforce the budget gate before calling the image provider.
	// Image generation can be expensive; an over-budget project must be
	// blocked before any provider call incurs real cost.
	provider, model := providerModelFromImageGenReq(bifReq)
	if !h.checkBudget(w, ctx, model) {
		return
	}

	resp, bErr := h.router.ImageGenerationRequest(ctx, bifReq)
	// Write the response first, then bill asynchronously (FIX #18).
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		// Fix round-3 #8: bill image responses that carry no token Usage by
		// falling back to a fixed per-image cost (perImageFallbackNano).
		// usageFromImageResponse returns (in, out, imgCount):
		//   - Usage != nil  →  in/out populated, imgCount = 0  →  normal token path.
		//   - Usage == nil  →  in=out=0, imgCount = len(Data)  →  direct billing path.
		in, out, imgCount := usageFromImageResponse(resp)
		if in > 0 || out > 0 {
			h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
		} else if imgCount > 0 {
			h.updateUsageDirect(ctx, identityProjectFromCtx(ctx), identityUserFromCtx(ctx), provider, model, imgCount*perImageFallbackNano)
		}
	}
}

// ---- Anthropic dialect (exact /llm/v1/messages) ----

// Messages handles POST /llm/v1/messages. In bifrost/core v1.7.3 the Anthropic
// messages surface routes through the Responses API
// (RouteConfigTypeAnthropic): the body converts via ToBifrostResponsesRequest
// and streaming uses ResponsesStreamRequest with the Anthropic stream-event
// framing (design §6.2; corrects the stale "uses Chat" table).
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	var req anthropic.AnthropicMessageRequest
	raw, ok0 := decodeJSONRaw(w, r, &req)
	if !ok0 {
		return
	}
	streaming := isStream(req.Stream)
	ctx, sc, ok := h.requestContext(w, r, streaming)
	if !ok {
		return
	}
	bifReq := req.ToBifrostResponsesRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	provider, model := providerModelFromResponsesReq(bifReq)
	// The authored MCP allowlist judges the mcp_servers[] entries.
	if !h.checkMCPAllowlist(w, ctx, raw, provider, model) {
		sc.cancel() // rejected before dispatch: nothing owns the context
		return
	}
	// Pre-flight budget check (see Chat handler comment).
	if !h.checkBudget(w, ctx, model) {
		sc.cancel() // blocked before dispatch: nothing owns the context
		return
	}

	if streaming {
		ch, bErr := h.router.ResponsesStreamRequest(ctx, bifReq)
		// FIX #5: pass billing context so streamAnthropic can call updateUsage
		// after the channel drains with the final usage-carrying chunk.
		h.streamAnthropic(w, r.Context().Done(), ctx, sc, provider, model, ch, bErr)
		return
	}
	defer sc.cancel()
	resp, bErr := h.router.ResponsesRequest(ctx, bifReq)
	if bErr != nil {
		// Fix round-3 #4: spec §2.5 mandates OpenAI-shaped errors on ALL /llm
		// routes, including /llm/v1/messages. Use writeOpenAIError, not
		// writeAnthropicError, so the error body is {"error":{message,type,code}}.
		h.writeOpenAIError(w, bErr)
		return
	}
	// Write the response first, then bill asynchronously (FIX #18).
	writeJSON(w, http.StatusOK, anthropic.ToAnthropicResponsesResponse(ctx, resp))
	in, out := usageFromResponsesResponse(resp)
	h.updateUsage(ctx, provider, model, in, out, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
}

// CountTokens handles POST /llm/v1/messages/count_tokens — a synchronous
// (non-SSE) Anthropic token count backed by CountTokensRequest.
func (h *Handler) CountTokens(w http.ResponseWriter, r *http.Request) {
	var req anthropic.AnthropicMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostResponsesRequest(ctx)
	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// Budget gate BEFORE the provider — count_tokens is a provider call and must
	// be admission-gated like every other /llm endpoint (uniform gating,
	// DECISIONS.md). No updateUsage after: CountTokensResponse carries no billable
	// usage, so there is nothing to meter post-response.
	_, model := providerModelFromResponsesReq(bifReq)
	if !h.checkBudget(w, ctx, model) {
		return
	}
	resp, bErr := h.router.CountTokensRequest(ctx, bifReq)
	if bErr != nil {
		// Fix round-3 #4: spec §2.5 — OpenAI-shaped errors on ALL /llm routes.
		h.writeOpenAIError(w, bErr)
		return
	}
	writeJSON(w, http.StatusOK, anthropic.ToAnthropicCountTokensResponse(resp))
}

// MessagesSubPath handles unknown POST /llm/v1/messages/{suffix} paths. Only
// count_tokens is a real Anthropic sub-path; everything else is 404 rather than
// being misrouted to the OpenAI catch-all.
func (h *Handler) MessagesSubPath(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error", "unknown messages sub-path", "")
}

// ---- synthetic models surface (GET /llm/v1/models, not routed through core) ----

// Models handles GET /llm/v1/models. The set is synthesised from the calling
// project's Postgres configuration (section 'llm'), NOT routed through
// bifrost/core (design §4.2, §3.4). Response is the OpenAI list envelope.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.modelsProjectID(w, r)
	if !ok {
		return
	}
	list := modelsList{Object: modelsListType, Data: h.modelList(r.Context(), projectID)}
	writeJSON(w, http.StatusOK, list)
}

// Model handles GET /llm/v1/models/{name}: a single-model lookup returning 200
// with the model object when the calling project has it, else 404.
func (h *Handler) Model(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.modelsProjectID(w, r)
	if !ok {
		return
	}
	name := modelNameFromPath(r.URL.Path)
	if name == "" {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown model", "")
		return
	}
	if h.models == nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown model", "")
		return
	}
	mo, found := h.models.Get(r.Context(), projectID, name)
	if !found {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown model", "")
		return
	}
	writeJSON(w, http.StatusOK, mo)
}

// modelsProjectID verifies the edge's signed identity and returns the resolved
// projectID. It writes a 403 and returns ok=false on an invalid signature
// (matching newContext); a missing project id resolves to "" (an empty model
// set), never an error.
func (h *Handler) modelsProjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !verifySignature(r.Header, h.identitySecret) {
		writeError(w, http.StatusForbidden, "permission_error", "invalid identity signature", "")
		return "", false
	}
	return identityFromHeaders(r.Header).projectID, true
}

// modelList resolves the project's synthesised model set, tolerating a nil
// resolver (gateway booted without a database ⇒ empty set).
func (h *Handler) modelList(ctx context.Context, projectID string) []modelObject {
	if h.models == nil {
		return []modelObject{}
	}
	return h.models.List(ctx, projectID)
}

// modelNameFromPath extracts the {name} segment from a /llm/v1/models/{name}
// path. Model ids may themselves contain slashes (e.g. "openai/gpt-4o"), so the
// whole remainder after the "/models/" prefix is the id, URL-unescaped.
func modelNameFromPath(path string) string {
	const prefix = "/llm/v1/models/"
	i := strings.Index(path, prefix)
	if i < 0 {
		return ""
	}
	name := path[i+len(prefix):]
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	return strings.Trim(name, "/")
}

// NotFound writes an OpenAI-shaped 404 for any unmounted /llm path so the
// surface returns a structured error body rather than chi's bare 404 text.
func (h *Handler) NotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error", "unknown route", "")
}

// MethodNotAllowed writes an OpenAI-shaped 405 for a known path hit with the
// wrong method.
func (h *Handler) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
}

// ---- shared response helpers ----

// writeUnary marshals a successful bifrost response as JSON, or maps a
// *schemas.BifrostError to an OpenAI-shaped error body with the right status.
func (h *Handler) writeUnary(w http.ResponseWriter, resp interface{}, bErr *schemas.BifrostError) {
	if bErr != nil {
		h.writeOpenAIError(w, bErr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamOpenAI writes the OpenAI SSE framing: each chunk is a data-only frame,
// then a terminal "data: [DONE]" marker on normal completion. A mid-stream
// error is emitted as a data frame carrying the OpenAI-shaped error and ends
// the stream (no [DONE]).
//
// FIX #5: the final usage-carrying chunk (BifrostChatResponse.Usage != nil)
// is captured; after the channel drains updateUsage is called with the real
// streamed token counts.
//
// Issue #9: every early exit hands the still-open channel AND the stream
// cancel to a detached drain instead of returning, so a client that
// disconnects mid-stream is still billed from the provider's own usage trailer
// if it arrives within the grace period — and the loss is metered if it does
// not. sc is owned by this function from here on and is settled exactly once.
func (h *Handler) streamOpenAI(
	w http.ResponseWriter,
	clientGone <-chan struct{},
	ctx *schemas.BifrostContext,
	sc *streamCancel,
	provider, model string,
	ch chan *schemas.BifrostStreamChunk,
	bErr *schemas.BifrostError,
) {
	if bErr != nil {
		sc.cancel()
		h.writeOpenAIError(w, bErr)
		return
	}
	if ch == nil {
		// A nil channel with a nil error is a router contract violation
		// (observed from bifrost's responses-stream path); ranging over it
		// would hang the request forever. Surface it as a 502 instead.
		sc.cancel()
		h.logger.Error("stream router returned nil channel with nil error", "provider", provider, "model", model)
		writeError(w, http.StatusBadGateway, "api_error",
			"upstream stream could not be established", "bad_gateway")
		return
	}
	s := h.newChatSettler("streamOpenAI", ctx, sc, provider, model, ch)
	// A panic below would otherwise leak the provider stream forever: with the
	// context decoupled from the request (issue #9), nothing else will ever
	// cancel it. Only fires when the stream was never settled, so it cannot
	// kill a drain that legitimately owns the context.
	defer func() {
		if !s.settled {
			s.sc.cancel()
		}
	}()
	sw, endStream, err := h.beginStream(w)
	defer endStream()
	if err != nil {
		// The provider stream is already open; it must be drained (and billed)
		// even though we can never write it to this client.
		s.settleEarly(lossReasonBeginStreamFail)
		return
	}
	for {
		chunk, exit := nextChunk(ch, clientGone)
		if exit == streamExitClient {
			// The client vanished while the provider was silent. Without this
			// the loop would park here until the provider's own idle timeout,
			// because the provider stream no longer dies with the request
			// (issue #9).
			s.settleEarly(lossReasonClientGone)
			return
		}
		if exit == streamExitChanDone {
			break
		}
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			data, _ := json.Marshal(openAIErrorBody(chunk.BifrostError))
			_ = sw.Data(string(data))
			s.settleEarly(lossReasonProviderError)
			return
		}
		// Capture usage from the final usage-carrying chunk (providers send
		// usage in the last chunk before [DONE]; earlier chunks have Usage=nil).
		s.observe(chunk)
		data, mErr := json.Marshal(chunk)
		if mErr != nil {
			h.logger.Warn("marshal stream chunk", "err", mErr)
			continue
		}
		if writeErr := sw.Data(string(data)); writeErr != nil {
			s.settleEarly(lossReasonWriteError) // client disconnected
			return
		}
	}
	_ = sw.Data("[DONE]")
	// Bill after the channel drains successfully.
	s.settleClean()
}

// streamResponses writes the OpenAI Responses-API SSE framing: each chunk
// carries its own event type (resp.Type) and no [DONE] marker. sendDone is
// kept for symmetry but is false for the Responses API.
//
// FIX #5: the "response.completed" event carries Response.Usage; usage is
// captured from that chunk and updateUsage is called after the channel drains.
// Issue #9: see streamOpenAI — every early exit hands the open channel and the
// stream cancel to the detached drain.
func (h *Handler) streamResponses(
	w http.ResponseWriter,
	clientGone <-chan struct{},
	ctx *schemas.BifrostContext,
	sc *streamCancel,
	provider, model string,
	ch chan *schemas.BifrostStreamChunk,
	bErr *schemas.BifrostError,
	sendDone bool,
) {
	if bErr != nil {
		sc.cancel()
		h.writeOpenAIError(w, bErr)
		return
	}
	if ch == nil {
		// A nil channel with a nil error is a router contract violation
		// (observed from bifrost's responses-stream path); ranging over it
		// would hang the request forever. Surface it as a 502 instead.
		sc.cancel()
		h.logger.Error("stream router returned nil channel with nil error", "provider", provider, "model", model)
		writeError(w, http.StatusBadGateway, "api_error",
			"upstream stream could not be established", "bad_gateway")
		return
	}
	s := h.newResponsesSettler("streamResponses", ctx, sc, provider, model, ch)
	// A panic below would otherwise leak the provider stream forever: with the
	// context decoupled from the request (issue #9), nothing else will ever
	// cancel it. Only fires when the stream was never settled, so it cannot
	// kill a drain that legitimately owns the context.
	defer func() {
		if !s.settled {
			s.sc.cancel()
		}
	}()
	sw, endStream, err := h.beginStream(w)
	defer endStream()
	if err != nil {
		s.settleEarly(lossReasonBeginStreamFail)
		return
	}
	for {
		chunk, exit := nextChunk(ch, clientGone)
		if exit == streamExitClient {
			s.settleEarly(lossReasonClientGone)
			return
		}
		if exit == streamExitChanDone {
			break
		}
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			data, _ := json.Marshal(openAIErrorBody(chunk.BifrostError))
			_ = sw.Event("error", string(data))
			s.settleEarly(lossReasonProviderError)
			return
		}
		if chunk.BifrostResponsesStreamResponse == nil {
			continue
		}
		// Capture usage from the response.completed event (carries Response.Usage).
		s.observe(chunk)
		sr := chunk.BifrostResponsesStreamResponse
		event := string(sr.Type)
		data, mErr := json.Marshal(sr)
		if mErr != nil {
			h.logger.Warn("marshal responses chunk", "err", mErr)
			continue
		}
		if writeErr := sw.Event(event, string(data)); writeErr != nil {
			s.settleEarly(lossReasonWriteError)
			return
		}
	}
	if sendDone {
		_ = sw.Data("[DONE]")
	}
	// Bill after the channel drains successfully.
	s.settleClean()
}

// streamAnthropic writes the Anthropic SSE framing: each Responses stream chunk
// is converted to one or more AnthropicStreamEvents ("event: <type>\ndata:
// ...") with NO [DONE] marker. A mid-stream error is emitted as the Anthropic
// "event: error" frame and ends the stream.
//
// FIX #5: usage is captured from the response.completed event (Response.Usage)
// and updateUsage is called after the channel drains successfully.
// Issue #9: see streamOpenAI — every early exit hands the open channel and the
// stream cancel to the detached drain.
func (h *Handler) streamAnthropic(
	w http.ResponseWriter,
	clientGone <-chan struct{},
	ctx *schemas.BifrostContext,
	sc *streamCancel,
	provider, model string,
	ch chan *schemas.BifrostStreamChunk,
	bErr *schemas.BifrostError,
) {
	if bErr != nil {
		// Fix round-3 #4: spec §2.5 — OpenAI-shaped errors on ALL /llm routes,
		// including the Anthropic /v1/messages streaming pre-error path.
		sc.cancel()
		h.writeOpenAIError(w, bErr)
		return
	}
	if ch == nil {
		// Router contract violation (nil chan + nil error) — see streamResponses.
		sc.cancel()
		h.logger.Error("stream router returned nil channel with nil error", "provider", provider, "model", model)
		writeError(w, http.StatusBadGateway, "api_error",
			"upstream stream could not be established", "bad_gateway")
		return
	}
	s := h.newResponsesSettler("streamAnthropic", ctx, sc, provider, model, ch)
	// A panic below would otherwise leak the provider stream forever: with the
	// context decoupled from the request (issue #9), nothing else will ever
	// cancel it. Only fires when the stream was never settled, so it cannot
	// kill a drain that legitimately owns the context.
	defer func() {
		if !s.settled {
			s.sc.cancel()
		}
	}()
	sw, endStream, err := h.beginStream(w)
	defer endStream()
	if err != nil {
		s.settleEarly(lossReasonBeginStreamFail)
		return
	}
	for {
		chunk, exit := nextChunk(ch, clientGone)
		if exit == streamExitClient {
			s.settleEarly(lossReasonClientGone)
			return
		}
		if exit == streamExitChanDone {
			break
		}
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			// ToAnthropicResponsesStreamError returns a complete
			// "event: error\ndata: ...\n\n" frame.
			_ = sw.Raw(anthropic.ToAnthropicResponsesStreamError(chunk.BifrostError))
			s.settleEarly(lossReasonProviderError)
			return
		}
		if chunk.BifrostResponsesStreamResponse == nil {
			continue
		}
		// Capture usage from the response.completed event.
		s.observe(chunk)
		sr := chunk.BifrostResponsesStreamResponse
		events := anthropic.ToAnthropicResponsesStreamResponse(ctx, sr)
		for _, ev := range events {
			if ev == nil {
				continue
			}
			data, mErr := json.Marshal(ev)
			if mErr != nil {
				h.logger.Warn("marshal anthropic event", "err", mErr)
				continue
			}
			if writeErr := sw.Event(string(ev.Type), string(data)); writeErr != nil {
				s.settleEarly(lossReasonWriteError)
				return
			}
		}
	}
	// Bill after the channel drains successfully.
	s.settleClean()
}

// beginStream applies header hygiene, then constructs the SSE writer (which
// sets the streaming headers and clears the write deadline). On failure it
// writes a 500 and returns the error so the caller aborts.
//
// The SSE loop is only correct if the ResponseWriter supports per-chunk
// flushing: without http.Flusher the net/http server buffers the whole
// response and every stream chunk arrives at end-of-request, defeating the
// streaming contract (design §6.3). The precondition is asserted here (the
// handler owns the streaming decision) before delegating the framing to
// ssewriter, which re-checks and clears the write deadline.
// It also counts the stream. The returned release function decrements, and
// every caller must defer it — see MetricSSEActiveConnections for why the
// number matters and what it was worth before it existed.
func (h *Handler) beginStream(w http.ResponseWriter) (*ssewriter.Writer, func(), error) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported", "")
		return nil, func() {}, errStreamingUnsupported
	}
	finish(w.Header())
	sw, err := ssewriter.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported", "")
		return nil, func() {}, err
	}
	// Incremented only after the stream is genuinely open. Counting the
	// refused cases would make the gauge drift upward forever, and a gauge
	// that only rises is one an autoscaler reads as permanent load.
	sseActiveConnections.Add(1)
	var once sync.Once
	return sw, func() { once.Do(func() { sseActiveConnections.Add(-1) }) }, nil
}

const (
	// MetricSSEActiveConnections is the number of /llm SSE streams this
	// process is serving right now.
	//
	// IT IS A GAUGE, AND THE ONLY ONE ON THIS SURFACE. Every other gateway
	// metric is a monotonic counter, which is why this one carries no `_total`
	// suffix: a counter that goes down is a broken counter, and a Prometheus
	// Adapter rule written for a counter would rate() this into nonsense.
	//
	// WHY IT EXISTS. deploy/helm/elitea/values.yaml has named this exact
	// string as llmGateway.autoscaling.sseConnectionMetric since the HPA was
	// written, and NOTHING published it — the /metrics surface is an explicit
	// allowlist (cmd/elitea-llm-gateway/main.go gatewayMetrics) and no entry
	// produced it. An operator enabling that HPA got
	// `ScalingActive=False / FailedGetPodsMetric`, which does not scale and
	// does not complain: the deployment silently pinned at minReplicas while
	// its dashboard said autoscaling was on.
	//
	// Concurrent streams are the right signal for this process because the
	// cost of an SSE connection is almost entirely time spent holding it open,
	// not CPU — a gateway saturated on streams looks idle to a CPU-target HPA.
	MetricSSEActiveConnections = "gateway_llm_sse_active_connections"
)

var sseActiveConnections = expvar.NewInt(MetricSSEActiveConnections)

// SSEMetricNames returns the gauge names this package publishes, for the
// /metrics allowlist. Separate from the counter lists because the two are
// exported with different Prometheus types and mixing them is how a gauge
// ends up declared as a counter.
func SSEMetricNames() []string {
	return []string{MetricSSEActiveConnections}
}
