package llmproxy

// budget_gate.go — pre-LLM admission check and post-completion billing hooks
// wired into the /llm handler layer (design §8.5, BF0.9b).
//
// The gate is nil-safe: when the published budget plane carries no gate
// (budget_plane.go) every helper returns immediately, so existing call sites
// that build a Handler without governance continue to work unchanged.
//
// Every helper here takes ONE snapshot of the plane with h.budget() and reads
// the fields of that snapshot. Do not read h.budget() twice in one operation:
// budget enforcement can be installed while the gateway serves traffic (issue
// #315), and two loads can straddle the install.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// billingCtxTimeout is the deadline for the background billing context used
// in updateUsage (FIX #18). A client disconnect must not cancel the billing
// increment, so we detach from the request context. 10 s is generous for a
// NATS KV operation but bounded so a stuck NATS connection does not hold the
// goroutine forever.
const billingCtxTimeout = 10 * time.Second

// budgetGateTimeout bounds the pre-LLM admission read. It exists because the
// streaming path passes a context.WithoutCancel-derived context (issue #9): the
// gate must stay bounded even when nothing upstream can cancel it.
const budgetGateTimeout = 10 * time.Second

// billOutcome distinguishes the three ways a billing attempt can end. Only
// billRefused means real, known spend was dropped and must be alarmed on.
type billOutcome int

const (
	billBilled      billOutcome = iota // increment accepted (goroutine spawned)
	billNotBillable                    // nothing to bill: no gate, no project, or zero cost
	billRefused                        // billing is closing — a known amount was DROPPED
)

// budgetScopeProject is the scope string used for project-level budget checks.
const budgetScopeProject = failmode.ScopeProject

// budgetScopeUser is the scope string used for per-member budget checks
// (issue #321). Its scope_id is "{project_id}:{user_id}"; see
// failmode.UserScopeID for why that shape is fixed.
const budgetScopeUser = failmode.ScopeUser

// The budget refusal wire contract. Every budget refusal this gateway writes
// uses these three constants, and the SDK's reader is the reason they are what
// they are.
//
// elitea-sdk `runtime/exceptions.py::budget_exceeded_from` is the ONE place any
// SDK caller decides whether a 402 is a budget rejection. It does two things,
// in this order:
//
//  1. It matches on error.TYPE only: `detail.get("type") == "budget_exceeded"`.
//     A body whose type is anything else returns None from the same branch —
//     it does NOT fall through to the message-text path below it.
//  2. It reads the SCOPE out of error.CODE, and accepts exactly two values:
//     "project_budget_exceeded" and "member_budget_exceeded". Any other code
//     resolves to the default scope, which is the project one.
//
// So the type carries "this is a budget refusal" and the code carries "which
// budget". A refusal that puts the scope in the type is not recognised as a
// budget refusal at all: the SDK returns None, the handler treats the 402 as an
// ordinary provider error, and the policy rejection is fed back to the model as
// message content. The SDK's own docstring names that outcome as the thing the
// typed exception exists to prevent.
//
// The scope also survives past the SDK: BudgetExceededError.scope becomes the
// agent event's `budget_error_code`, which is what the front end keys its
// member-versus-project message on (EliteaUI budgetError.constants.js). The
// front end never sees this HTTP body.
const (
	// budgetErrorType is the ONLY error type a budget refusal may carry. It is
	// the SDK's match key; see above.
	budgetErrorType = "budget_exceeded"
	// budgetCodeProject is the project-ceiling code. It stays the OpenAI
	// canonical "insufficient_quota" rather than "project_budget_exceeded":
	// a generic OpenAI client understands it, spec §2.5 and the cutover gate
	// (cutover-ctl budget-check, BFF.9E) both assert it, and the SDK resolves
	// an unrecognised code to the project scope anyway — which is the correct
	// scope for this refusal. TestBudgetRefusalMatchesSDKContract pins that
	// reliance so it cannot become accidental.
	budgetCodeProject = "insufficient_quota"
	// budgetCodeMember is the member-ceiling code. The member cap is an Elitea
	// concept with no OpenAI equivalent, so there is no canonical code to keep
	// here, and the SDK needs this exact spelling to report the member scope.
	budgetCodeMember = "member_budget_exceeded"
	// budgetScopeFieldProject and budgetScopeFieldMember are the values of the
	// error.scope field. Only a refusal this gateway's own gate decided carries
	// error.scope. A provider's own quota refusal also arrives as
	// budget_exceeded/insufficient_quota (statusAndType), so the code alone
	// cannot say that the PROJECT ceiling refused the call. The workers read
	// error.scope to name the refusing budget, and treat its absence as an
	// unknown scope, for example provider billing (#6732). The SDK ignores the
	// field, so its type/code contract above does not change.
	budgetScopeFieldProject = "project"
	budgetScopeFieldMember  = "member"
)

// perImageFallbackNano is the fixed per-image billing cost in nano-USD used
// when an image-generation response carries no token-based Usage field.
// $0.040 per image (40_000_000 nano-USD) matches the DALL·E 3 Standard
// 1024×1024 list price and is a conservative floor for image models that do
// not report token usage. This constant is intentionally NOT a catalog lookup:
// image models whose pricing is token-based will report Usage and go through
// the normal cost.Calculator path; this path only fires when Usage==nil.
const perImageFallbackNano int64 = 40_000_000

// billingPeriodStart returns the first second of the current calendar month in
// UTC as a Unix timestamp. Budget counters are keyed by this value (design §8,
// NATS counter subject format). A monthly period is a safe conservative
// assumption that aligns with the pylon accumulator design.
func billingPeriodStart(now time.Time) int64 {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Unix()
}

// billingPeriodEnd returns the last second of the current calendar month in UTC.
func billingPeriodEnd(now time.Time) int64 {
	y, m, _ := now.UTC().Date()
	// First second of the next month, minus one second.
	next := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	return next.Unix() - 1
}

// parseProjectID converts the string project ID from the identity headers into
// an int required by GovernanceStore. Returns -1 if the value is empty or
// non-numeric.
func parseProjectID(s string) int {
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

// checkBudget performs the pre-LLM admission check (design §8.5). It:
//  1. Returns (true, nil) immediately when the gate is disabled (nil).
//  2. Parses the project identity from the request; skips enforcement for
//     anonymous/unresolved projects (no ID ⇒ no budget row ⇒ treat as
//     unlimited, consistent with GovernanceStore's ErrNoBudgetRow path).
//  3. Calls BudgetChecker.CheckBudget with reqCostNano=0; on Block402 → writes
//     HTTP 402; on Block503 → writes HTTP 503; on Allow → returns (true, nil).
//
// reqCostNano is 0 because the gateway has no pre-flight token estimate: the
// wire formats do not carry a pre-counted prompt token count, and computing one
// from the marshalled body would over-count inline base64 image/audio payloads
// by orders of magnitude. 0 is the conservative value — the FSM uses
// reqCostNano only for the degraded-path FRESH_NEAR per-replica cap, so it can
// never over-gate. The actual cost is billed after the response arrives.
//
// Issue #10: an in-process per-project "in-flight reservation" counter used to
// be added here, claiming to bound the concurrent-admission overshoot. Every
// call site passed promptTokenEst=0, so the reservation was never incremented
// while the billing path always decremented — the counter only ever drifted
// negative and its sync.Map entries were never reaped. The mechanism was
// deleted rather than repaired: bounding the overshoot for real needs a token
// estimator nobody has asked for, and the NATS counter remains ground truth.
//
// Returns (proceed=true) when the caller should continue; (proceed=false) means
// the response has already been written and the caller must return immediately.
//
// It is a thin HTTP wrapper over admissionVerdict, which holds the whole
// decision and touches no http.ResponseWriter. The split exists for the
// realtime route (realtime.go): that route hijacks the connection, so after the
// upgrade there is no ResponseWriter left to refuse a turn with, and it needs
// the SAME verdict this function writes. Keeping one decision function is what
// stops the realtime re-check from drifting into a second, weaker gate.
func (h *Handler) checkBudget(
	w http.ResponseWriter,
	ctx context.Context,
	model string,
) bool {
	v := h.admissionVerdict(ctx, model)
	if v.allow {
		return true
	}
	if v.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(v.retryAfter/time.Second)+1, 10))
	}
	writeBudgetRefusal(w, v)
	return false
}

// writeBudgetRefusal writes a refusal verdict. A gate-decided budget refusal
// adds error.scope; every other refusal is the plain writeError body.
func writeBudgetRefusal(w http.ResponseWriter, v budgetVerdict) {
	if v.scope == "" {
		writeError(w, v.status, v.errType, v.message, v.code)
		return
	}
	if sink, ok := w.(requestlog.ErrorCodeSetter); ok {
		sink.SetErrorCode(v.errType)
	}
	writeJSON(w, v.status, openAIError{Error: openAIErrorFields{
		Message: v.message, Type: v.errType, Code: v.code, Scope: v.scope,
	}})
}

// budgetVerdict is one admission decision, with no dependency on how it is
// delivered. allow=true means dispatch; otherwise the four refusal fields carry
// exactly what writeError would have written, so an HTTP caller and a WebSocket
// caller refuse on identical terms.
//
// retryAfter is non-zero only for the loop-breaker refusal, which is the one
// refusal that carries a Retry-After header on the HTTP path.
type budgetVerdict struct {
	allow   bool
	status  int
	errType string
	message string
	code    string
	// scope is set only on a budget refusal the gate decided: "project" or
	// "member". See budgetScopeFieldProject.
	scope      string
	retryAfter time.Duration
	// budgeted is set only on an ALLOWED verdict: the project has a hard
	// ceiling (failmode.Decision.Budgeted). The audio routes read it.
	budgeted bool
}

// budgetAllowed is the verdict every admitted request gets.
var budgetAllowed = budgetVerdict{allow: true}

// admissionMode says whether an admission question comes from an ARRIVAL or
// from a RE-CHECK of work already admitted.
//
// The two differ in ONE place: the amplification backstop. An arrival is a
// request, so it is counted. A re-check is not, so it is only observed. See
// loopBreaker.observe for the defect that split them.
type admissionMode int

const (
	// admissionArrival is a request that just arrived. It COUNTS toward the
	// per-(project, model) backstop.
	admissionArrival admissionMode = iota
	// admissionRecheck re-asks the question for work already admitted, such as
	// a live realtime session's periodic budget re-check. It respects an open
	// circuit but records no hit.
	admissionRecheck
)

// admissionVerdict is checkBudget's decision, with the response writing removed.
// The order of the three checks — loop breaker, project ceiling, member ceiling
// — and every log line are unchanged from the single function this was split
// out of.
func (h *Handler) admissionVerdict(ctx context.Context, model string) budgetVerdict {
	return h.admissionVerdictFor(ctx, model, admissionArrival)
}

// recheckVerdict answers the SAME admission question for work that is already
// running, and it does NOT count as a request.
//
// It exists for the realtime route: a live session re-asks the budget on a
// ticker, and that ticker used to feed the amplification backstop's sliding
// window. Long sessions on one (project, model) pair could then open the
// circuit for the project's real /llm traffic. The budget half of the answer is
// identical; only the backstop half changes.
func (h *Handler) recheckVerdict(ctx context.Context, model string) budgetVerdict {
	return h.admissionVerdictFor(ctx, model, admissionRecheck)
}

func (h *Handler) admissionVerdictFor(ctx context.Context, model string, mode admissionMode) budgetVerdict {
	// Circular-routing guard #2 (spec §2.6) runs BEFORE the budget gate and
	// regardless of whether budget enforcement is wired: a routing loop must
	// be contained even on a deployment without governance. The tuple key is
	// the caller-visible project + model; requests without a resolvable
	// project are not tracked (they cannot form a stable loop tuple).
	if h.loopGuard != nil && model != "" {
		if projectID := identityProjectFromCtx(ctx); projectID != "" {
			// observe READS the circuit; allow reads it AND records an arrival.
			// A re-check is not an arrival, so it must never take the second
			// path. See loopBreaker.observe.
			consult := h.loopGuard.allow
			if mode == admissionRecheck {
				consult = h.loopGuard.observe
			}
			if ok, retryAfter := consult(projectID, model); !ok {
				h.logger.Warn("loop breaker: circuit open for (project, model) tuple — possible circular routing",
					"project_id", projectID, "model", model, "retry_after", retryAfter)
				return budgetVerdict{
					status:     http.StatusTooManyRequests,
					errType:    "rate_limit_error",
					message:    "Too many requests for this (project, model) pair; possible circular routing. Retry later.",
					code:       "rate_limit_exceeded",
					retryAfter: retryAfter,
				}
			}
		}
	}

	// The authored per-minute ceilings run next, and BEFORE the budget gate.
	// Both are reads, so the order is not about cost: a request over its rate
	// limit must be refused as a rate limit, and letting the budget gate answer
	// first would report the wrong reason whenever a project is over both.
	if v := h.rateVerdict(ctx, model, mode); !v.allow {
		return v
	}

	// ONE snapshot of the budget path for this whole admission decision — the
	// project ceiling below and the member ceiling in memberVerdict. See
	// budget_plane.go.
	bp := h.budget()
	if bp.gate == nil {
		return budgetAllowed
	}

	pid := parseProjectID(identityProjectFromCtx(ctx))
	if pid < 0 {
		// No resolvable project — treat as unlimited (no row = no cap).
		return budgetAllowed
	}
	scopeID := strconv.Itoa(pid)

	now := time.Now()
	periodStart := billingPeriodStart(now)

	// Bound the admission read. Streaming requests now hand this function a
	// context that is deliberately decoupled from the client (issue #9), so it
	// has neither a deadline nor a cancellation path: without this timeout a
	// stalled Postgres pool would park the handler goroutine forever, where it
	// previously unwound on client hangup. Fail-closed semantics are preserved
	// — a timeout surfaces as the existing 503 branch below.
	gateCtx, gateCancel := context.WithTimeout(ctx, budgetGateTimeout)
	dec, err := bp.gate.CheckBudget(gateCtx, pid, budgetScopeProject, scopeID, periodStart, 0)
	gateCancel()
	if err != nil {
		// A hard error from the gate is unexpected (the gate is designed to
		// degrade gracefully); treat it as a 503 to avoid silently bypassing
		// enforcement.
		h.logger.Error("budget gate: CheckBudget error; blocking request",
			"project_id", pid, "err", err)
		return budgetVerdict{
			status:  http.StatusServiceUnavailable,
			errType: "service_unavailable",
			message: "budget service error; try again shortly",
			code:    "nats_unavailable",
		}
	}

	switch dec.Verdict {
	case failmode.Allow:
		// Fix round-3 #12: log at Info when the Allow decision comes from a
		// degraded (NATS-down) FSM state so operators can see that the gateway
		// is operating in fallback mode on a per-request basis. Only logged when
		// Degraded is set to avoid a log entry on every healthy-path request.
		if dec.Degraded {
			h.logger.Info("budget gate: degraded allow (NATS unavailable, fallback tier used)",
				"project_id", pid,
				"state", dec.State.String(),
			)
		}
		// The project has room. The member cap is a SECOND ceiling inside it,
		// so it is asked only after the project admits (issue #321).
		v := h.memberVerdict(ctx, bp.gate, pid, periodStart)
		v.budgeted = v.allow && dec.Budgeted
		return v
	case failmode.Block402:
		return budgetVerdict{
			status:  http.StatusPaymentRequired,
			errType: budgetErrorType,
			message: "project budget exhausted for this billing period",
			code:    budgetCodeProject,
			scope:   budgetScopeFieldProject,
		}
	case failmode.Block503:
		return budgetVerdict{
			status:  http.StatusServiceUnavailable,
			errType: "service_unavailable",
			message: "budget service temporarily unavailable; try again shortly",
			code:    "nats_unavailable",
		}
	default:
		// Unknown verdict: fail open on the PROJECT ceiling (log and proceed).
		// Should never happen. The member ceiling is still applied — a verdict
		// this code does not recognise is not a reason to skip a second,
		// independent limit.
		h.logger.Warn("budget gate: unknown verdict; allowing request",
			"verdict", fmt.Sprintf("%v", dec.Verdict))
		v := h.memberVerdict(ctx, bp.gate, pid, periodStart)
		v.budgeted = v.allow && dec.Budgeted
		return v
	}
}

// memberVerdict is the per-member half of the admission check (issue #321).
//
// Until this existed, a project admin could set a member's monthly cap, get a
// 200 back, watch the value round-trip through the API, and that member could
// still spend the entire project budget. The limit was authored, served and
// rendered; nothing read it.
//
// It runs on the SAME machinery as the project check — the same FSM, the same
// tiered-hybrid fallback, the same NATS counter and write-back — because the
// accumulator has always been keyed by (scope, scope_id, period) and
// elitea-main has always read the user scope. Only the gateway's read and write
// of that scope were missing.
//
// A request with no resolvable member id is admitted: an integration
// authenticating with a project token has no member to charge, and refusing it
// would break every non-interactive caller. Those calls remain bounded by the
// project ceiling, which is the ceiling they have always been bounded by.
//
// The refusal carries the member scope in error.CODE, and the shared
// `budget_exceeded` type. The front end has had a distinct message for
// `member_budget_exceeded` since before the Go port (EliteaUI
// budgetError.constants.js), and it deep-links to the member's own Usage tab;
// collapsing the two would send a member who is over THEIR cap to a project
// budget screen they cannot act on.
//
// The scope moved from the type to the code in the SDK-compatibility pass. It
// was in the type, and the front end never received it: EliteaUI does not read
// this HTTP body. It reads the agent event's `budget_error_code`, which is
// elitea-sdk's BudgetExceededError.scope, and the SDK derives that scope from
// error.CODE after matching error.TYPE against `budget_exceeded` alone. A
// member refusal typed `member_budget_exceeded` failed that match, so
// budget_exceeded_from returned None, no typed exception was raised, and the
// refusal reached the model as ordinary message content. See budgetErrorType.
//
// gate is the SAME snapshot the project ceiling used (budget_plane.go). It is
// a parameter and not a second h.budget() load, so one admission decision can
// never ask two different gates.
func (h *Handler) memberVerdict(
	ctx context.Context,
	gate BudgetChecker,
	projectID int,
	periodStart int64,
) budgetVerdict {
	raw := identityUserFromCtx(ctx)
	uid := parseUserID(raw)
	if uid < 0 {
		// "No member" and "a member id we could not read" are different, and
		// only the second is a fault. Without this line they look identical in
		// production, and a member cap that quietly stops applying is the #321
		// shape all over again.
		if raw != "" {
			h.logger.Warn("budget gate: member id is present but unusable; the member cap is not applied",
				"project_id", projectID, "user_id_header", raw)
		}
		return budgetAllowed
	}
	scopeID := failmode.UserScopeID(projectID, uid)

	gateCtx, gateCancel := context.WithTimeout(ctx, budgetGateTimeout)
	dec, err := gate.CheckBudget(gateCtx, projectID, budgetScopeUser, scopeID, periodStart, 0)
	gateCancel()
	if err != nil {
		// Same reasoning as the project gate: a hard error is not a licence to
		// skip the ceiling.
		h.logger.Error("budget gate: member CheckBudget error; blocking request",
			"project_id", projectID, "user_id", uid, "err", err)
		return budgetVerdict{
			status:  http.StatusServiceUnavailable,
			errType: "service_unavailable",
			message: "budget service error; try again shortly",
			code:    "nats_unavailable",
		}
	}

	switch dec.Verdict {
	case failmode.Block402:
		h.logger.Info("budget gate: member budget exhausted",
			"project_id", projectID, "user_id", uid, "state", dec.State.String())
		return budgetVerdict{
			status:  http.StatusPaymentRequired,
			errType: budgetErrorType,
			message: "member budget exhausted for this billing period",
			code:    budgetCodeMember,
			scope:   budgetScopeFieldMember,
		}
	case failmode.Block503:
		return budgetVerdict{
			status:  http.StatusServiceUnavailable,
			errType: "service_unavailable",
			message: "budget service temporarily unavailable; try again shortly",
			code:    "nats_unavailable",
		}
	default:
		return budgetAllowed
	}
}

// identityProjectFromCtx extracts the project ID string set on the
// BifrostContext by newContext (via schemas.BifrostContextKeyVirtualKey).
func identityProjectFromCtx(ctx context.Context) string {
	return bifrostCtxString(ctx, schemas.BifrostContextKeyVirtualKey)
}

// contextKeyExecutionID carries the runtime execution id set by newContext
// from the X-Elitea-Execution-Id header.
//
// A gateway-local key rather than a Bifrost one: Bifrost has no notion of an
// Elitea execution, and the value is never handed to a provider.
const contextKeyExecutionID schemas.BifrostContextKey = "elitea-execution-id"

// identityExecutionFromCtx extracts the runtime execution id, empty when the
// request was not made from one.
func identityExecutionFromCtx(ctx context.Context) string {
	return bifrostCtxString(ctx, contextKeyExecutionID)
}

// identityUserFromCtx extracts the member ID string set on the BifrostContext
// by newContext from the X-Elitea-User-Id header. elitea-main has forwarded
// that header since the llmproxy identity path was written; until issue #321
// the gateway carried it and read it for nothing.
func identityUserFromCtx(ctx context.Context) string {
	return bifrostCtxString(ctx, schemas.BifrostContextKeyUserID)
}

func bifrostCtxString(ctx context.Context, key any) string {
	type bifrostCtx interface {
		Value(key any) any
	}
	if bc, ok := ctx.(bifrostCtx); ok {
		if v, ok2 := bc.Value(key).(string); ok2 {
			return v
		}
	}
	return ""
}

// parseUserID converts the member ID string from the identity headers into an
// int. It returns -1 for an absent or unusable value, which every caller reads
// as "no member to charge" — the same convention parseProjectID uses.
func parseUserID(s string) int {
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

// updateUsage records the billed cost for a completed request onto the
// authoritative counter and publishes a write-behind delta. It is
// best-effort fire-and-forget: errors are logged but do not cause the
// response to fail (the provider has already been called; double-billing
// is worse than a missed update).
//
// FIX #18 (async off critical path): the actual billing increment is
// launched in a bounded goroutine on a fresh context so:
//  1. A client disconnect before the increment does not cancel it.
//  2. The HTTP response is written BEFORE waiting on the NATS round-trip.
//
// The counter increment MUST still happen even if the client disconnects —
// the goroutine uses context.Background() bounded by billingCtxTimeout, not
// the request context. This matches the streaming path, which already bills
// post-drain.
//
// FIX #15: after a successful UpdateUsage, the post-increment ratio is
// compared against the pre-increment snapshot's SoftAlertPct (default 80).
// The soft alert fires ONLY when the spend CROSSES the threshold (was below,
// now at-or-above) — not on every request. TryAlertCooldown deduplicates
// within the cooldown window.
//
// The caller supplies the usage from the response; tokens default to 0 when
// the response carries no usage field (e.g. streaming partial responses).
// It reports WHY nothing was billed, not merely that nothing was. The stream
// drain alarms on a refused increment (real, known spend dropped) and must stay
// silent for "there was nothing to bill here" — a gateway with no budget gate
// wired, an unresolvable project, or a zero-priced model. Collapsing the two
// made every clean stream on an ungoverned deployment publish a
// billing_refused alarm, desensitising operators to the one signal that
// detects real loss.
func (h *Handler) updateUsage(
	ctx context.Context,
	provider string,
	model string,
	inputTokens, outputTokens int64,
	projectIDStr string,
	userIDStr string,
) billOutcome {
	return h.updateUsageUnits(ctx, surfaceTokens, provider, model,
		cost.Units{InputTokens: inputTokens, OutputTokens: outputTokens},
		projectIDStr, userIDStr)
}

// billingSurface names which /llm surface a billed request came from.
//
// It exists because the two non-token counters below are AUDIO controls, and a
// realtime turn bills through this same function. Without the distinction a
// live realtime session moved gateway_audio_non_token_basis_total and
// gateway_audio_default_priced_total, and logged itself as "audio: …" — so an
// operator alarming on the audio controls could not tell a whisper-1
// transcription from a realtime session, and the realtime counters
// under-reported the same events. Realtime publishes its own counters
// (RealtimeMetricNames); these two stay the audio routes' own.
type billingSurface int

const (
	// surfaceTokens is the token routes (chat, completions, responses,
	// messages, embeddings, images) that reach here through updateUsage. They
	// carry no audio units, so the audio counters and the "audio: …" log lines
	// do not apply to them. Before this value existed updateUsage passed
	// surfaceAudio, and every plain text chat priced from the fallback table
	// logged "audio: billed a token price the catalog did not supply" and moved
	// gateway_audio_default_priced_total.
	surfaceTokens billingSurface = iota
	// surfaceAudio is the unary /llm/v1/audio/* routes only.
	surfaceAudio
	// surfaceRealtime is a turn of a /llm/v1/realtime session.
	surfaceRealtime
)

// updateUsageUnits is updateUsage over any denomination the catalog can price:
// tokens, seconds (carried as milliseconds) or characters (issue #323). Only
// the two audio routes call it directly. updateUsage is the token-only form,
// and its eleven call sites are unchanged.
//
// It is one function and not two because everything after the price lookup —
// the period bounds, the ledger dimensions, the drain guard, the member scope,
// the soft alert — is identical for every basis. A second copy of that path is
// a second place for the money to go missing.
// recordLoggedUsage attaches the request's token counts to the log.
//
// Called from updateUsageUnits, which every billed surface reaches — the same
// argument mapModel's enrichment makes for provider and model. Tokens are
// enrichment and not the record itself: a request that failed before the
// provider answered has none, and zero is the honest value there.
func recordLoggedUsage(ctx context.Context, units cost.Units) {
	enrichment := requestlog.FromContext(ctx)
	enrichment.SetTokens(units.InputTokens, units.OutputTokens)
	recordLoggedCredentialOwner(ctx, enrichment)
}

// recordLoggedCredentialOwner attaches who owns the credential that served the
// request (legacy issue 6709). It runs where the usage is recorded, because a
// request that reaches the billing path is one a credential served, and ctx
// there is the request's BifrostContext, where bifrost/core left the selected
// key. A ctx without a selected key leaves the field empty.
func recordLoggedCredentialOwner(ctx context.Context, enrichment *requestlog.Enrichment) {
	enrichment.SetCredentialOwner(account.SelectedCredentialOwner(ctx))
}

// recordLoggedCacheTokens attaches the prompt-cache counts of a chat or text
// completion usage block. Nil usage records nothing.
func recordLoggedCacheTokens(ctx context.Context, usage *schemas.BifrostLLMUsage) {
	if read, write, ok := cacheTokensFromLLMUsage(usage); ok {
		requestlog.FromContext(ctx).SetCacheTokens(read, write)
	}
}

// recordLoggedResponsesCacheTokens is recordLoggedCacheTokens for the
// Responses-API usage block, which /llm/v1/responses and /llm/v1/messages use.
func recordLoggedResponsesCacheTokens(ctx context.Context, usage *schemas.ResponsesResponseUsage) {
	if read, write, ok := cacheTokensFromResponsesUsage(usage); ok {
		requestlog.FromContext(ctx).SetCacheTokens(read, write)
	}
}

// cacheTokensFromLLMUsage reads the cache-read and cache-write counts from a
// chat usage block. ok is false when the block carries no prompt details.
func cacheTokensFromLLMUsage(usage *schemas.BifrostLLMUsage) (read, write int64, ok bool) {
	if usage == nil || usage.PromptTokensDetails == nil {
		return 0, 0, false
	}
	details := usage.PromptTokensDetails
	return int64(details.CachedReadTokens), int64(details.CachedWriteTokens), true
}

// cacheTokensFromResponsesUsage reads the same two counts from a Responses-API
// usage block. bifrost maps OpenAI's cached_tokens into CachedReadTokens and
// Anthropic's cache_creation_input_tokens into CachedWriteTokens.
func cacheTokensFromResponsesUsage(usage *schemas.ResponsesResponseUsage) (read, write int64, ok bool) {
	if usage == nil || usage.InputTokensDetails == nil {
		return 0, 0, false
	}
	details := usage.InputTokensDetails
	return int64(details.CachedReadTokens), int64(details.CachedWriteTokens), true
}

func (h *Handler) updateUsageUnits(
	ctx context.Context,
	surface billingSurface,
	provider string,
	model string,
	u cost.Units,
	projectIDStr string,
	userIDStr string,
) billOutcome {
	// BEFORE the billable check: a request whose tokens are known but which
	// this deployment does not bill (no budget gate wired) still used them, and
	// the log is about what happened rather than about what was charged.
	recordLoggedUsage(ctx, u)

	bp := h.budget()
	if bp.gate == nil || bp.calc == nil {
		return billNotBillable
	}
	pid := parseProjectID(projectIDStr)
	if pid < 0 {
		return billNotBillable
	}
	now := time.Now()
	periodStart := billingPeriodStart(now)
	periodEnd := billingPeriodEnd(now)

	// Compute cost on the caller's goroutine using the cost calculator (no I/O).
	// Use a fresh context for the cost lookup so a client disconnect doesn't
	// abort even the cheap in-process price lookup.
	costCtx, costCancel := context.WithTimeout(context.Background(), billingCtxTimeout)
	defer costCancel()

	actualCost := bp.calc.CostUnits(costCtx, provider, model, u)

	// Report which rate paid, and refuse to let an UNPRICED audio request look
	// like a cheap one. The two counters live in audio.go, where their names
	// are published to /metrics.
	//
	// The test is written against the basis the UNITS ask for, not against
	// Cost.Basis alone, and that is deliberate. A token-billed request whose
	// price is zero is PRICED and costs nothing; reading Cost.Basis alone would
	// make every zero-cost estimator stub look unpriced and would put the
	// eleven token call sites on a path they never take today.
	if surface == surfaceAudio && u.Basis() != cost.BasisTokens {
		if actualCost.Basis == "" {
			// The provider reported seconds or characters and the catalog holds
			// no rate for them. Billing zero here is unavoidable — inventing a
			// rate would put a made-up figure on the authoritative counter — but
			// it must not be silent. This is the number an operator alarms on.
			audioUnpriced.Add(1)
			h.logger.WarnContext(ctx, "audio: the catalog carries no rate for the units this response reported; the request bills zero",
				"provider", provider, "model", model,
				"unit_basis", u.Basis(), "metric", MetricAudioUnpriced)
			return billNotBillable
		}
		audioNonTokenBasis.Add(1)
		h.logger.InfoContext(ctx, "audio: a non-token rate priced this request",
			"provider", provider, "model", model,
			"basis", actualCost.Basis, "source", actualCost.Source,
			"cost_nano", actualCost.TotalNanoUSD, "metric", MetricAudioNonTokenBasis)
	} else if surface == surfaceAudio && actualCost.TotalNanoUSD > 0 && !actualCost.FromCatalog() {
		// The audio response reported TOKENS, and the token price did not come
		// from the catalog.
		//
		// The seconds and characters bases cannot reach here: audioCost refuses
		// a rate that is not from the catalog, so they bill a real price or
		// nothing. The token basis is different — it falls back to the pylon
		// default table like every other route, which is longstanding and
		// disclosed. The consequence for AUDIO is what was silent: the amount is
		// non-zero and plausible, so MetricAudioUnpriced cannot fire (a price
		// was produced) and MetricAudioNonTokenBasis cannot fire (the basis is
		// tokens). An invented figure reached the authoritative counter and left
		// no trace.
		//
		// This does NOT refuse the request. Refusing would change the pricing
		// policy of the token basis for one route, and that is a [human
		// decision]. It makes the condition alarmable, which is what was
		// missing.
		audioDefaultPriced.Add(1)
		h.logger.WarnContext(ctx, "audio: billed a token price the catalog did not supply",
			"provider", provider, "model", model,
			"source", actualCost.Source, "cost_nano", actualCost.TotalNanoUSD,
			"metric", MetricAudioDefaultPriced)
	}

	// The catalog row has an input price and no output price, so the output
	// tokens were billed at the pylon input x 3 estimate. That is the shape
	// issue #6719 hid: gpt-image-2 billed its image output at half its real
	// rate, and every log line looked like a catalog price. The bill does not
	// change here; the estimate becomes visible.
	if actualCost.OutputDerived {
		h.logger.WarnContext(ctx, "cost: the catalog has no output price for this model; output tokens billed at the input x 3 estimate",
			"provider", provider, "model", model,
			"output_tokens", u.OutputTokens, "output_nano", actualCost.OutputNanoUSD)
	}

	// The authored credential rate policy decides whether this cost reaches the
	// counter at all (policy_gate.go). It runs AFTER pricing on purpose: the
	// price is what an operator sees in the ledger for a zero-rated request,
	// and skipping the lookup would make a zero-rate-metered row indistinguishable
	// from a genuinely free one.
	ratePolicy := h.ratePolicyFor(ctx, provider, model)
	switch ratePolicy {
	case policy.RatePolicyExcluded:
		h.logger.DebugContext(ctx, "governance: the authored rate policy excludes this usage from accounting",
			"provider", provider, "model", model, "cost_nano", actualCost.TotalNanoUSD)
		// The tokens still count toward the rate limit. `excluded` is a BILLING
		// treatment, not an exemption from the ceilings that protect the
		// platform from load.
		h.recordPolicyTokens(ctx, provider, model, u.InputTokens+u.OutputTokens, now)
		return billNotBillable
	case policy.RatePolicyZeroRateMetered:
		h.logger.DebugContext(ctx, "governance: the authored rate policy meters this usage at zero cost",
			"provider", provider, "model", model, "priced_nano", actualCost.TotalNanoUSD)
		actualCost.TotalNanoUSD = 0
	}

	// The completed request's tokens go onto its rate-limit window. This is the
	// only place the authoritative token count is known.
	h.recordPolicyTokens(ctx, provider, model, u.InputTokens+u.OutputTokens, now)

	// A zero-rate-metered request continues past this guard deliberately: it
	// must still produce a ledger row, which is the whole difference between
	// `zero-rate-metered` and `excluded`. The counter moves by zero.
	if actualCost.TotalNanoUSD <= 0 && ratePolicy != policy.RatePolicyZeroRateMetered {
		return billNotBillable // nothing to bill
	}

	// The dimensions the usage ledger records for this request (issue #320).
	// They are the values the billing path ALREADY has — the resolved provider
	// and model it just priced, and the token counts the provider reported. No
	// count is derived or estimated: an estimated token is not a billed one.
	// The token columns stay the TOKEN counts. A seconds-billed or
	// characters-billed request reports none, so they stay zero: writing a
	// millisecond count into a column named prompt_tokens would put a duration
	// on a page of token figures, and nothing downstream would say so.
	dims := &failmode.UsageDimensions{
		UserID:           optionalUserID(userIDStr),
		Provider:         provider,
		Model:            model,
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		// The instant THIS gateway billed the request, taken from the same
		// `now` the period bounds come from. The ledger row is written by the
		// scheduler, minutes later or more, so a column default would date the
		// request to whenever the consumer got to it.
		OccurredAtUnix: now.Unix(),
		// The runtime execution this request was made from, so the ledger can be
		// grouped by agent the same way the request log can. Empty for every
		// caller that is not a runtime execution, which is most of them.
		ExecutionID: identityExecutionFromCtx(ctx),
	}

	if h.spawnBillingGoroutine(bp, pid, userIDStr, periodStart, periodEnd, actualCost.TotalNanoUSD, dims) {
		return billBilled
	}
	return billRefused
}

// optionalUserID renders the identity header's member id as a *int for the
// ledger: nil when there is no member to attribute the call to.
func optionalUserID(userIDStr string) *int {
	uid := parseUserID(userIDStr)
	if uid < 0 {
		return nil
	}
	return &uid
}

// updateUsageDirect bills a pre-computed costNano amount (nano-USD) for the
// given project, bypassing the cost.Calculator. Used for image-generation
// responses where the provider does not report token usage (Fix round-3 #8):
// the caller has already counted the generated images and multiplied by
// perImageFallbackNano.
//
// If costNano <= 0 or the gate/store is absent the call is a no-op.
func (h *Handler) updateUsageDirect(
	ctx context.Context,
	projectIDStr string,
	userIDStr string,
	provider string,
	model string,
	costNano int64,
) billOutcome {
	// A credential served this response too, though it reported no usage.
	// Before the gate check, for the reason updateUsageUnits records first:
	// the log is about what happened, not about what was charged.
	recordLoggedCredentialOwner(ctx, requestlog.FromContext(ctx))

	bp := h.budget()
	if bp.gate == nil || costNano <= 0 {
		return billNotBillable
	}
	pid := parseProjectID(projectIDStr)
	if pid < 0 {
		return billNotBillable
	}
	now := time.Now()
	periodStart := billingPeriodStart(now)
	periodEnd := billingPeriodEnd(now)

	// An image response that reports no Usage still has a provider, a model and
	// a cost, so it still belongs in the ledger and in the per-model table. Its
	// token counts stay 0 — the provider reported none, and inventing one to
	// fill a column would put an estimate on a page of billed figures.
	dims := &failmode.UsageDimensions{
		UserID:         optionalUserID(userIDStr),
		Provider:       provider,
		Model:          model,
		OccurredAtUnix: now.Unix(),
		ExecutionID:    identityExecutionFromCtx(ctx),
	}

	if h.spawnBillingGoroutine(bp, pid, userIDStr, periodStart, periodEnd, costNano, dims) {
		return billBilled
	}
	return billRefused
}

// spawnBillingGoroutine is the shared inner billing path: it guards against
// Add-after-Wait (Fix round-3 #2), spawns the billing goroutine, and runs
// the soft-alert crossing check after a successful increment.
//
// FIX #27 (github issue #15): the pre-increment CheckBudget snapshot (needed
// only for the soft-alert crossing comparison in trySoftAlert) is read INSIDE
// the goroutine, after billCtx is created and before UpdateUsage runs. It used
// to run synchronously on the caller's (request) goroutine, which — despite
// FIX #18 moving the increment itself off the critical path — still added up
// to billingCtxTimeout of client-visible latency when the budget store was slow.
//
// Callers must have already validated costNano > 0.
// It returns false when the goroutine was NOT spawned (drain in progress), so a
// caller holding known, provider-reported spend can meter the drop rather than
// letting it vanish into a log line.
// The member scope is billed in the SAME goroutine, with its OWN event id.
// Two ids, not one, because gateway.processed_event_ids has event_id as its
// primary key: a member delta reusing the project delta's id would be seen as
// an already-applied redelivery and silently contribute nothing to the member's
// accumulator. The member cap would then admit forever while appearing enforced
// — the same defect as #321, one layer down.
//
// The member increment carries NO usage dimensions. The ledger row written for
// the project delta already names the member in its user_id column; a second
// row would double every token count, request count and cost figure the
// per-day and per-model views report.
//
// The member scope also gets its own soft-alert crossing check (issue #510),
// with its own pre-increment snapshot taken beside the project one. The member
// cap refuses calls, so a member must be told they are near it before it does;
// until this, the 402 was the first thing that told them a cap applied.
//
// bp is the caller's budget-plane snapshot. It is a parameter and not a fresh
// h.budget() load, so the goroutine bills through the SAME gate that admitted
// and priced the request (budget_plane.go).
func (h *Handler) spawnBillingGoroutine(
	bp *budgetPlane,
	pid int,
	userIDStr string,
	periodStart, periodEnd int64,
	costNano int64,
	dims *failmode.UsageDimensions,
) bool {
	scopeID := strconv.Itoa(pid)
	eventID := uuid.NewString()
	uid := parseUserID(userIDStr)

	// Fix round-3 #2: guard against Add-after-Wait (billingClosing already set
	// by DrainBilling) and track in-flight goroutines so DrainBilling can wait.
	if h.billingClosing.Load() != 0 {
		// Drain in progress — skip spawning and log so spend is not silently
		// dropped.
		h.logger.Warn("budget gate: billing goroutine skipped (drain in progress); spend may be under-counted",
			"project_id", pid, "cost_nano", costNano, "event_id", eventID)
		return false
	}
	h.billingWg.Add(1)
	go func() {
		defer h.billingWg.Done()

		// FIX #27: pre-increment snapshot, read here (detached goroutine) instead
		// of on the request goroutine. Used only by the soft-alert crossing check
		// below; never gates the request (admission already happened in checkBudget).
		// It gets its OWN timeout budget, separate from billCtx below, so a
		// slow/degraded read can never shrink the money-critical UpdateUsage
		// call's deadline (gateway-review: sharing one budget across both would
		// silently starve UpdateUsage under DB/NATS degradation).
		alertCtx, alertCancel := context.WithTimeout(context.Background(), billingCtxTimeout)
		preDec, preErr := bp.gate.CheckBudget(alertCtx, pid, budgetScopeProject, scopeID, periodStart, 0)
		alertCancel()

		// The MEMBER pre-increment snapshot (issue #510). Read it here, beside
		// the project one and before any increment. A crossing is a comparison
		// of the counter before this request with the counter after it. After
		// the increment the "before" value is gone.
		//
		// This is one more gate read for each request that bills a member. It
		// costs the caller nothing: FIX #27 moved this whole block off the
		// request goroutine, so the request has already been answered.
		//
		// Read it only when a member id resolved. A request with no member bills
		// no member scope, thus there is no member crossing to find.
		var (
			memberScopeID string
			memberPreDec  failmode.Decision
			memberPreErr  error
		)
		if uid > 0 {
			memberScopeID = failmode.UserScopeID(pid, uid)
			memberPreCtx, memberPreCancel := context.WithTimeout(context.Background(), billingCtxTimeout)
			memberPreDec, memberPreErr = bp.gate.CheckBudget(memberPreCtx, pid, budgetScopeUser,
				memberScopeID, periodStart, 0)
			memberPreCancel()
			if memberPreErr != nil {
				h.logger.Warn("budget gate: member CheckBudget error before the increment; the member soft alert is skipped",
					"project_id", pid, "user_id", uid, "err", memberPreErr)
			}
		}

		billCtx, cancel := context.WithTimeout(context.Background(), billingCtxTimeout)
		defer cancel()

		projectErr := bp.gate.UpdateUsage(billCtx, pid, budgetScopeProject, scopeID, eventID,
			costNano, periodStart, periodEnd, dims)
		if projectErr != nil {
			h.logger.Warn("budget gate: UpdateUsage failed; spend may be under-counted",
				"project_id", pid, "cost_nano", costNano,
				"event_id", eventID, "err", projectErr)
		}

		// Bill the member scope even when the project increment failed: the two
		// counters are independent, and skipping the member increment because
		// the project one erred would leave a member cap under-counted for
		// reasons that have nothing to do with that member.
		//
		// It gets its OWN timeout budget rather than sharing billCtx, for the
		// reason FIX #27 gives one to the pre-increment snapshot: a slow project
		// increment would otherwise spend the whole 10 s and hand this call an
		// already-expired context. That is not a missed alert, it is member
		// spend dropped — and the member cap that admits forever afterwards is
		// the defect #321 exists about, one layer down.
		memberBilled := false
		if uid > 0 {
			memberEventID := uuid.NewString()
			memberCtx, memberCancel := context.WithTimeout(context.Background(), billingCtxTimeout)
			err := bp.gate.UpdateUsage(memberCtx, pid, budgetScopeUser,
				memberScopeID, memberEventID,
				costNano, periodStart, periodEnd, nil)
			memberCancel()
			if err != nil {
				h.logger.Warn("budget gate: member UpdateUsage failed; member spend may be under-counted",
					"project_id", pid, "user_id", uid, "cost_nano", costNano,
					"event_id", memberEventID, "err", err)
			} else {
				memberBilled = true
			}
		}

		// FIX #15: soft-alert threshold crossing check.
		// The alert must fire ONLY when the pre-increment spend was BELOW the
		// soft threshold and the post-increment spend is AT OR ABOVE it.
		//
		// Each scope is tested against ITS OWN pair of snapshots, and the two
		// tests are independent (issue #510). They are conditions and not early
		// returns on purpose: an unreadable project snapshot, or a project
		// increment that failed, must not silence the member's alert. The
		// member's counter moved, and the member is the person the alert is for.
		//
		//   - preErr / memberPreErr: the "before" value is unknown, so no
		//     crossing can be established. Skip that scope only.
		//   - Verdict != Allow: the scope was already at or over its hard limit
		//     before this request. The refusal is the signal at that point; a
		//     soft alert adds nothing.
		if projectErr == nil && preErr == nil && preDec.Verdict == failmode.Allow {
			h.trySoftAlert(billCtx, bp, pid, budgetScopeProject, scopeID, periodStart, costNano, preDec)
		}

		// The member soft alert (issue #510). Before this, a member crossed
		// their threshold in silence, and the 402 from their own cap was the
		// first signal that a cap applied to them.
		//
		// Give it its OWN context, for the reason the member increment above
		// gets one: billCtx has already paid for the project increment and can
		// hold no time at all. A deadline spent elsewhere is not a reason to
		// drop this member's warning.
		if memberBilled && memberPreErr == nil && memberPreDec.Verdict == failmode.Allow {
			memberAlertCtx, memberAlertCancel := context.WithTimeout(context.Background(), billingCtxTimeout)
			h.trySoftAlert(memberAlertCtx, bp, pid, budgetScopeUser, memberScopeID,
				periodStart, costNano, memberPreDec)
			memberAlertCancel()
		}
	}()
	return true
}

// trySoftAlert fires the 80% soft-alert ONLY when the running counter has
// CROSSED the SoftAlertPct threshold with this billing increment.
//
// It is SCOPE-GENERIC (issue #510). scope + scopeID name one budget: the
// project ("project", "42") or one member inside it ("user", "42:7"). Both
// ceilings refuse a call, so both owe their owner a warning before they do, and
// everything the warning needs is already per-scope — CheckBudget reads the
// member's own limit and threshold, and TryAlertCooldown derives its key from
// (scope, scopeID, period), so the two scopes claim different cooldowns and
// neither can suppress the other.
//
// Crossing detection: compare the pre-increment FSM state (preDec) with the
// post-increment FSM state (from a fresh CheckBudget). The alert fires when:
//   - preDec was Allow (spend was below the soft threshold), AND
//   - postDec is Block402 (hard limit crossed, definitely past 80%) OR
//     postDec.State is StateDownPGFreshNear (degraded-path near-threshold).
//
// On the NATS_HEALTHY path the post-increment CheckBudget re-reads the
// authoritative counter, so it correctly reflects the updated spend. On the
// degraded path the Snapshot AccumulatedNano doesn't change mid-request, so
// StateDownPGFreshNear signals the pre-computed threshold was already exceeded.
//
// TryAlertCooldown deduplicates: once an alert fires it is suppressed within
// the cooldown window (typically hours) even if every subsequent request also
// crosses the threshold.
//
// bp is the caller's budget-plane snapshot: the alert re-reads the counter
// through the same gate the increment used, and publishes through the same
// event publisher (budget_plane.go).
func (h *Handler) trySoftAlert(
	ctx context.Context,
	bp *budgetPlane,
	pid int,
	scope, scopeID string,
	periodStart, costJustBilled int64,
	preDec failmode.Decision,
) {
	// Re-read the budget state AFTER the increment. reqCostNano=costJustBilled
	// lets the FSM account for the just-billed amount when evaluating thresholds
	// (particularly the per-replica FRESH_NEAR cap on the degraded path).
	postDec, postErr := bp.gate.CheckBudget(ctx, pid, scope, scopeID, periodStart, costJustBilled)
	if postErr != nil {
		h.logger.Warn("budget gate: CheckBudget error during post-increment soft-alert check; skipping",
			"project_id", pid, "scope", scope, "scope_id", scopeID, "err", postErr)
		return
	}

	// Determine whether a threshold was crossed on this billing increment.
	// The pre-increment state was Allow (enforced by the caller).
	//
	//   - postDec.Verdict == Block402: hard limit (100%) was just crossed.
	//     The soft threshold (80%) is necessarily also crossed.
	//   - postDec.State == StateDownPGFreshNear: degraded-path 80%..100% range
	//     entered; the FSM already enforces the per-replica NEAR cap.
	//   - postDec.State == StateNATSHealthy && postDec.SoftThresholdNear &&
	//     !preDec.SoftThresholdNear: NATS_HEALTHY path just crossed into the
	//     soft-alert zone (Fix round-3 #6: previously the NATS_HEALTHY path
	//     never fired the soft alert because it didn't track the 80% threshold).
	//
	// Only fire in these cases to avoid alerting on every request.
	crossed := postDec.Verdict == failmode.Block402 ||
		postDec.State == failmode.StateDownPGFreshNear ||
		(postDec.State == failmode.StateNATSHealthy &&
			postDec.SoftThresholdNear && !preDec.SoftThresholdNear)

	h.logger.Debug("soft-alert crossing check",
		"project_id", pid,
		"scope", scope, "scope_id", scopeID,
		"crossed", crossed,
		"pre_state", preDec.State.String(), "pre_near", preDec.SoftThresholdNear,
		"post_state", postDec.State.String(), "post_near", postDec.SoftThresholdNear,
		"post_verdict", int(postDec.Verdict),
		"cost_just_billed_nano", costJustBilled)

	if !crossed {
		return
	}

	// The platform soft-alert switch (issue #322). An operator who turns alert
	// emission off through PUT /admin/gateway/budget-alerts used to get 200 OK,
	// a changed GET, and alerts that kept firing until the pod restarted and the
	// GET silently flipped back. The switch now lives in a row the gateway
	// reads, and this is where it takes effect.
	//
	// It is checked AFTER the crossing test and BEFORE the cooldown claim, on
	// purpose: not claiming the cooldown means the first crossing after an
	// operator re-enables alerts still fires, rather than being suppressed by a
	// claim made silently while alerts were off.
	if postDec.SoftAlertsDisabled {
		h.logger.Debug("budget gate: soft alert suppressed by platform switch",
			"project_id", pid, "scope", scope, "scope_id", scopeID)
		return
	}

	fired, err := bp.gate.TryAlertCooldown(ctx, scope, scopeID, periodStart)
	if err != nil {
		h.logger.Warn("budget gate: TryAlertCooldown error; soft-alert suppressed",
			"project_id", pid, "scope", scope, "scope_id", scopeID, "err", err)
		return
	}
	if fired {
		h.logger.Warn("budget soft-alert: the budget has crossed the spend threshold",
			"project_id", pid,
			"scope", scope,
			"scope_id", scopeID,
			"cost_just_billed_nano", costJustBilled,
			"period_start", periodStart,
			"pre_state", preDec.State.String(),
			"post_state", postDec.State.String(),
		)
		h.publishSoftAlertEvent(ctx, bp.alerts, pid, scope, scopeID, costJustBilled, periodStart)
	}
}

// softAlertPayload is the budget.soft_alert event body. Field names are part
// of the platform event contract consumed by elitea-main subscribers and the
// BFF.9e live gate — change only with a spec update.
type softAlertPayload struct {
	// ProjectID is ALWAYS the numeric project id, on both scopes. It names the
	// project the alert belongs to, and it matches the subject the event is
	// published on. A member alert does NOT put "42:7" here: a subscriber that
	// read this field as a project id would then be looking up a project that
	// does not exist.
	ProjectID string `json:"project_id"`
	// Scope is which ceiling crossed: "project" or "user" (issue #510).
	Scope string `json:"scope"`
	// ScopeID is the accumulator key of that ceiling — "42" for a project,
	// "42:7" for a member. It is what identifies the budget the alert is about.
	ScopeID string `json:"scope_id"`
	// UserID names the member on a user-scope alert, so a subscriber does not
	// have to parse ScopeID to know who to tell. It is absent on a project
	// alert, which belongs to no one member.
	UserID             int   `json:"user_id,omitempty"`
	PeriodStartUnix    int64 `json:"period_start_unix"`
	CostJustBilledNano int64 `json:"cost_just_billed_nano"`
}

// softAlertEnvelope mirrors elitea-main's natsbus Event envelope (the shape
// natsbus subscribers decode): {type, source, payload, timestamp}.
type softAlertEnvelope struct {
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

// softAlertEventType matches elitea-main events.EventBudgetSoftAlert.
const softAlertEventType = "budget.soft_alert"

// publishSoftAlertEvent emits budget.soft_alert onto gateway.events.* so the
// alert is externally observable (spec §8.3 / BFF.9e: "a soft-alert is
// recorded on gateway.events.* within S seconds"). Best-effort: a publish
// failure is logged, never fatal — the authoritative alert record remains the
// NATS cooldown claim; the event is the notification channel.
//
// pub is the publisher from the caller's budget-plane snapshot. A nil pub
// disables publishing; the alert still logs.
//
// The SUBJECT is the project's, for a member alert as well as a project one
// (issue #510). PublishSoftAlertEvent turns its first argument into the NATS
// subject token gateway.events.project.<id>.events, which is the channel
// elitea-main relays to the members of that project; a member scope_id such as
// "42:7" would build a subject nothing subscribes to, and the warning would
// reach nobody. Which budget crossed is carried in the payload instead.
func (h *Handler) publishSoftAlertEvent(
	ctx context.Context,
	pub AlertEventPublisher,
	pid int,
	scope, scopeID string,
	costJustBilled, periodStart int64,
) {
	if pub == nil {
		return
	}
	projectID := strconv.Itoa(pid)
	body := softAlertPayload{
		ProjectID:          projectID,
		Scope:              scope,
		ScopeID:            scopeID,
		PeriodStartUnix:    periodStart,
		CostJustBilledNano: costJustBilled,
	}
	if scope == budgetScopeUser {
		// A malformed member key leaves UserID absent rather than reporting
		// member 0. ScopeID still carries the raw key, so the alert stays
		// diagnosable instead of naming the wrong person.
		if uid, ok := failmode.UserIDFromScopeID(scopeID); ok {
			body.UserID = uid
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		h.logger.Warn("budget soft-alert: marshal event payload failed", "err", err)
		return
	}
	env, err := json.Marshal(softAlertEnvelope{
		Type:      softAlertEventType,
		Source:    "elitea-llm-gateway",
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	})
	if err != nil {
		h.logger.Warn("budget soft-alert: marshal event envelope failed", "err", err)
		return
	}

	pubCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := pub.PublishSoftAlertEvent(pubCtx, projectID, env); err != nil {
		h.logger.Warn("budget soft-alert: event publish failed",
			"project_id", projectID, "scope", scope, "scope_id", scopeID, "err", err)
	}
}

// usageFromChatResponse extracts (inputTokens, outputTokens) from a
// BifrostChatResponse. Returns (0, 0) when the usage field is absent.
func usageFromChatResponse(resp *schemas.BifrostChatResponse) (int64, int64) {
	if resp == nil || resp.Usage == nil {
		return 0, 0
	}
	return int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens)
}

// usageFromResponsesResponse extracts (inputTokens, outputTokens) from a
// BifrostResponsesResponse. Returns (0, 0) when the usage field is absent.
func usageFromResponsesResponse(resp *schemas.BifrostResponsesResponse) (int64, int64) {
	if resp == nil || resp.Usage == nil {
		return 0, 0
	}
	return int64(resp.Usage.InputTokens), int64(resp.Usage.OutputTokens)
}

// providerModelFromChatReq extracts provider and model strings from a
// BifrostChatRequest, converting the ModelProvider type to string.
func providerModelFromChatReq(req *schemas.BifrostChatRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromResponsesReq extracts provider and model from a
// BifrostResponsesRequest.
func providerModelFromResponsesReq(req *schemas.BifrostResponsesRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromTextReq extracts provider and model from a
// BifrostTextCompletionRequest.
func providerModelFromTextReq(req *schemas.BifrostTextCompletionRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromEmbeddingReq extracts provider and model from a
// BifrostEmbeddingRequest.
func providerModelFromEmbeddingReq(req *schemas.BifrostEmbeddingRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// usageFromTextCompletionResponse extracts (inputTokens, outputTokens) from a
// BifrostTextCompletionResponse. Returns (0, 0) when the usage field is absent.
func usageFromTextCompletionResponse(resp *schemas.BifrostTextCompletionResponse) (int64, int64) {
	if resp == nil || resp.Usage == nil {
		return 0, 0
	}
	return int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens)
}

// usageFromEmbeddingResponse extracts (inputTokens, outputTokens) from a
// BifrostEmbeddingResponse. Embeddings only have prompt tokens; output is 0.
func usageFromEmbeddingResponse(resp *schemas.BifrostEmbeddingResponse) (int64, int64) {
	if resp == nil || resp.Usage == nil {
		return 0, 0
	}
	return int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens)
}

// usageFromImageResponse extracts (inputTokens, outputTokens, imageCount)
// from a BifrostImageGenerationResponse.
//
// When the provider populates Usage (e.g. OpenAI gpt-image-1), the token
// counts are returned and imageCount is 0 — the normal cost.Calculator path
// applies.
//
// When Usage is nil (e.g. DALL·E 2, Stability AI), token counts are 0 and
// imageCount is set to len(resp.Data) so the caller can bill a fixed per-image
// fallback cost via perImageFallbackNano (Fix round-3 #8). This ensures image
// models that do not report token usage still get billed.
func usageFromImageResponse(resp *schemas.BifrostImageGenerationResponse) (inputTokens, outputTokens, imageCount int64) {
	if resp == nil {
		return 0, 0, 0
	}
	if resp.Usage != nil {
		return int64(resp.Usage.InputTokens), int64(resp.Usage.OutputTokens), 0
	}
	return 0, 0, int64(len(resp.Data))
}

// providerModelFromImageGenReq extracts provider and model from a
// BifrostImageGenerationRequest.
func providerModelFromImageGenReq(req *schemas.BifrostImageGenerationRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromImageEditReq extracts provider and model from a
// BifrostImageEditRequest.
func providerModelFromImageEditReq(req *schemas.BifrostImageEditRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromImageVariationReq extracts provider and model from a
// BifrostImageVariationRequest.
func providerModelFromImageVariationReq(req *schemas.BifrostImageVariationRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}
