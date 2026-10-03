// realtime.go — the /llm/v1/realtime WebSocket route: a long-lived, bidirectional
// audio session that the gateway gates, meters and bills exactly like every
// other /llm route (issue #323 follow-up).
//
// WHY THIS FILE EXISTS. pylon-indexer's realtime ASR relay
// (indexer_asr_realtime.py) opens a WebSocket to `/v1/realtime?model=…&intent=
// transcription` and pumps PCM16 audio at it. The retired LiteLLM proxy answered
// that path. Nothing replaced it, so that image keeps a LiteLLM process of its
// own — a second LLM data plane that applies no budget and bills nothing. The
// unary audio routes (audio.go) removed two thirds of that reason; this route
// removes the last third.
//
// WHY IT IS NOT "audio.go WITH A SOCKET". Every other route on this gateway is
// a request that ENDS, so one admission check bounds it. A realtime session does
// not end: the tenant holds the socket, the turns keep coming, and — because a
// hijacked connection has its deadlines cleared — no server timeout can ever
// reap it. Four things follow, and each is a rule rather than a preference:
//
//  1. ADMISSION RUNS BEFORE THE UPGRADE. mapModel, the price gate and the full
//     budget gate all need an http.ResponseWriter to refuse with, and the
//     hijack destroys it. They run in that order, before websocket.Accept. A
//     client frame can still ASK for another model after the upgrade, and that
//     frame re-runs the whole sequence; see admitFrameModels.
//  2. THE BUDGET IS RE-ASKED PERIODICALLY. bifrost reports a turn start for
//     exactly `response.create` and the SERVER-side `input_audio_buffer.
//     committed`, and the only known caller sends neither. A re-check armed on
//     turn start alone would never fire for it. The ticker is the mandatory
//     mechanism; the turn trigger is an extra one.
//  3. THE MODEL MUST CARRY A REAL CATALOG PRICE. See realtimePricedModel.
//  4. THE SESSION IS ITS OWN SHUTDOWN PHASE. It is not a stream drain; see
//     CloseRealtimeSessions.
//
// WHO OWNS WHAT. The session owns one context, and cancelling it is the ONE way
// every goroutine stops:
//
//	uplink    reads the CLIENT socket, writes the PROVIDER socket. Exits on a
//	          client read error or a cancelled context; cancels on the way out.
//	          It is also the only writer of the session's two model slots.
//	downlink  reads the PROVIDER socket, writes the CLIENT socket, then bills
//	          the turn. Exits on a provider read error or a cancelled context;
//	          cancels on the way out.
//	keepalive pings BOTH sockets. Exits on a cancelled context. It is not
//	          optional: hijack clears the connection deadlines, so an idle
//	          session is invisible to every server timeout.
//	regate    re-asks the budget gate on a ticker. Exits on a cancelled context.
//	watchdog  cancels when the process starts to shut down.
//
// Neither socket is read by two goroutines, so no read is racy. Writes are
// serialised by github.com/coder/websocket itself, which is why the keepalive
// pinger cannot corrupt a frame the uplink or downlink is writing.
//
// HOW A SESSION ENDS. Through end(), and in ONE order: refuse, then close, then
// cancel. Each step is there because the other order broke something a test now
// holds; end() states which.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
)

// The operator numbers this route ships with. The config package holds the same
// two values for the env-var defaults; TestRealtimeConstantsInSync keeps the
// pairs together, exactly as it does for the stream grace.
const (
	// DefaultRealtimeBudgetRecheck is how often a live session re-asks the
	// budget gate. See internal/config for the derivation of 15 s.
	DefaultRealtimeBudgetRecheck = 15 * time.Second
	// DefaultRealtimeMaxSessions bounds concurrent sessions on one replica.
	DefaultRealtimeMaxSessions = 128
)

const (
	// realtimeSessionPerProjectDivisor derives the per-project session cap from
	// the global one, the way drainPerProjectDivisor does for stream drains. One
	// tenant must not be able to hold every session slot on a replica and lock
	// every other tenant out of the route.
	realtimeSessionPerProjectDivisor = 8

	// realtimeReadLimit caps ONE WebSocket message on either socket.
	//
	// The library default is 32 KiB and a realtime frame carries base64 audio
	// well past it. The failure that default produces is not an error at setup:
	// the session opens, works, and then dies mid-call with close status 1009,
	// minutes in and only for the callers whose audio chunks happen to be large.
	// 1 MiB is 20-100x a normal OpenAI realtime audio delta, and it still bounds
	// what one hostile frame can make this process allocate.
	realtimeReadLimit int64 = 1 << 20

	// realtimeKeepalivePeriod is the ping interval on both sockets. It is well
	// inside the 60 s idle timeout that load balancers and reverse proxies
	// commonly apply, and F6 is the reason it exists at all: after the hijack no
	// server-side ReadHeaderTimeout or IdleTimeout applies to this connection,
	// so nothing else would ever notice a peer that went away without a FIN.
	realtimeKeepalivePeriod = 20 * time.Second

	// realtimeWriteTimeout bounds ONE frame write (and one ping round trip). A
	// peer that stops reading must not park a pump goroutine for the life of the
	// session context, which is unbounded by design.
	realtimeWriteTimeout = 10 * time.Second

	// realtimeDialTimeout bounds the provider handshake. It runs before the
	// upgrade, so it is still on the caller's HTTP request.
	realtimeDialTimeout = 15 * time.Second

	// maxBudgetOutagesBeforeClose is the N of decision H1: after this many
	// re-checks that could not reach the budget store, the session is closed
	// instead of merely refusing turns.
	//
	// IT COUNTS OUTAGES SINCE THE LAST ALLOW, not adjacent ones, and the name
	// says so because an earlier one said "consecutive" and the code never did.
	// Only an Allow resets the counter: a verdict that is neither an Allow nor
	// an outage — the amplification backstop's 429 — leaves it untouched, on the
	// reasoning that such a verdict says nothing about whether the store is
	// reachable, so it neither proves an outage nor ends one. A 503, 429, 503
	// sequence therefore reaches 2, which is the intended behaviour and not an
	// accident of the name.
	//
	// 4 × DefaultRealtimeBudgetRecheck is about a minute. That is deliberately
	// longer than a NATS reconnect or a leader election, which are seconds, so a
	// blip does not drop a live call; and it is far shorter than the FSM's own
	// continuous-outage ceiling (LLM_BUDGET_NATS_DEGRADED_MAX_DURATION_MIN,
	// default 10 minutes), so a real outage cannot leave an un-gated socket open
	// for as long as that ceiling would allow. Turns are refused throughout, so
	// nothing reaches the provider during the minute either way — what the
	// window buys is that the caller's session survives a blip.
	maxBudgetOutagesBeforeClose = 4

	// RealtimeCloseTimeout bounds CloseRealtimeSessions during shutdown. A
	// session that has been told to stop only has to finish the frame it is
	// writing and settle its last turn's billing, so this is a close budget and
	// not a drain budget.
	RealtimeCloseTimeout = 5 * time.Second

	// realtimeGateTimeout bounds ONE mid-session budget-gate call.
	//
	// admissionVerdict wraps each of its two store reads (the project ceiling,
	// then the member ceiling) in a context that carries budgetGateTimeout. That
	// bound is a context DEADLINE, and a store that STALLS while it ignores its
	// context returns nothing at all. The session context has no deadline by
	// design, so the re-check goroutine then parks for ever: the ticker never
	// fires again and a live session runs un-gated for the life of the process.
	// This is the bound that does not depend on the store's own manners.
	//
	// It is larger than 2 x budgetGateTimeout on purpose. A store that is merely
	// SLOW, and that honours its context, still answers inside it; only a store
	// that answers nothing at all reaches it. A timeout is an OUTAGE, so
	// decision H1 applies: refuse the turn, keep the socket open, and count the
	// tick toward maxBudgetOutagesBeforeClose.
	// KEEP realtimePingBound LARGER THAN THIS. gateTurn runs this bound on the
	// uplink, which is the goroutine that reads the client socket and therefore
	// the only one that can process a pong.
	realtimeGateTimeout = 2*budgetGateTimeout + time.Second

	// realtimeCloseHandshakeBudget bounds how long a session waits for the
	// PEER's close reply.
	//
	// coder/websocket's Close writes the close frame and then waits up to a
	// HARDCODED 5 s to read the peer's reply. To read it, it must take the
	// connection's readMu, which the uplink holds while it is parked in Read. A
	// caller that stops reading therefore costs the full 5 s, and
	// CloseRealtimeSessions has only RealtimeCloseTimeout for every session on
	// the replica. Measured before this bound existed: one silent peer burned
	// 5.001 s of the 5 s budget, and the wait returned with the session STILL
	// LIVE — so DrainBilling ran while that session's last turn was in flight.
	//
	// The close FRAME is written before the wait, so a peer that reads still
	// gets the close status. The abandoned library goroutine ends on its own
	// 5 s bound, and the cancel that follows closes the connection under it.
	realtimeCloseHandshakeBudget = time.Second

	// realtimePingBound is the deadline for ONE keepalive ping, and it MUST stay
	// larger than realtimeGateTimeout.
	//
	// The uplink is the only goroutine that reads the CLIENT socket, and
	// coder/websocket processes a PONG on that read path — there is no separate
	// control-frame reader. gateTurn runs on the uplink and parks it for up to
	// realtimeGateTimeout while it asks the budget store. A ping bound shorter
	// than that reports a perfectly healthy caller as dead, and keepalive tears
	// the whole session down for it: a stalled budget store would end every live
	// call rather than merely refusing its turns, which is the opposite of what
	// decision H1 chose.
	//
	// The margin is what keeps the two from meeting exactly. A genuinely dead
	// peer is still detected, one keepalive period later.
	realtimePingBound = realtimeGateTimeout + 5*time.Second

	// maxRealtimeCloseReasonBytes is the close-reason budget. RFC 6455 allows
	// 123 bytes; the margin leaves room for the rune-boundary back-off in
	// wsSocket.Close without ever reaching the limit the library enforces.
	maxRealtimeCloseReasonBytes = 120
)

// The realtime money-path and capacity counters.
//
// gateway_realtime_turns_unpriced_total is the important one. A realtime session
// with a TRANSCRIPTION intent produces no usage that bifrost's own extractor can
// read: ExtractRealtimeTurnUsage parses `response.usage` off `response.done`
// only, and a transcription-intent session emits neither. This gateway parses
// the transcription event's own top-level usage instead — and when even that is
// absent, the turn is UNPRICED and counted here. It is never billed as zero and
// never billed from an invented number.
const (
	// MetricRealtimeSessionsOpened counts sessions that passed admission and
	// completed the upgrade.
	MetricRealtimeSessionsOpened = "gateway_realtime_sessions_opened_total"
	// MetricRealtimeRefusedUnpricedModel counts upgrades refused because the
	// price catalog holds no rate for the requested model (decision H2).
	MetricRealtimeRefusedUnpricedModel = "gateway_realtime_refused_unpriced_model_total"
	// MetricRealtimeRefusedCapacity counts upgrades refused because the global
	// or per-project session bound was already full.
	MetricRealtimeRefusedCapacity = "gateway_realtime_refused_capacity_total"
	// MetricRealtimeTurnsBilled counts turns whose usage reached the
	// authoritative counter.
	MetricRealtimeTurnsBilled = "gateway_realtime_turns_billed_total"
	// MetricRealtimeTurnsUnpriced counts terminal turn events that carried no
	// usable usage at all, so nothing was billed for that turn.
	MetricRealtimeTurnsUnpriced = "gateway_realtime_turns_unpriced_total"
	// MetricRealtimeTurnsRefused counts TURN STARTS the gateway did NOT forward
	// to the provider because the budget gate did not admit them (decision H1).
	// The only known caller sends no event bifrost reads as a turn start, so
	// read MetricRealtimeFramesDropped beside it: that is the traffic a refusal
	// really stopped.
	MetricRealtimeTurnsRefused = "gateway_realtime_turns_refused_total"
	// MetricRealtimeSessionsClosedBudget counts sessions closed by the budget
	// gate: an exhausted budget, or too many consecutive gate outages.
	MetricRealtimeSessionsClosedBudget = "gateway_realtime_sessions_closed_budget_total"
	// MetricRealtimeFramesDropped counts CLIENT frames the gateway did NOT
	// forward to the provider because the session was refusing (decision H1).
	//
	// It is the number that says how much of a caller's traffic a refusal
	// actually stopped. MetricRealtimeTurnsRefused cannot say it: the only known
	// caller never sends an event bifrost reads as a turn start, so its whole
	// session is dropped frame by frame with that counter at zero.
	MetricRealtimeFramesDropped = "gateway_realtime_frames_dropped_total"
	// MetricRealtimeTurnBasisMismatch counts turns whose usage arrived on a
	// basis the H2 price probe did NOT accept for that model. The probe admits
	// either basis, so a token-priced model can be admitted and then report a
	// duration for every turn — which bills zero, and which no other counter
	// can see. See realtimePricing.
	MetricRealtimeTurnBasisMismatch = "gateway_realtime_turn_basis_mismatch_total"
	// MetricRealtimeTurnsUnbilled counts turns whose provider-reported spend was
	// DROPPED because billing was already draining. It is real, known spend that
	// went nowhere, and it is the realtime twin of budget.unbilled_stream.
	MetricRealtimeTurnsUnbilled = "gateway_realtime_turns_unbilled_total"
	// MetricRealtimeSessionsClosedModel counts sessions closed because a client
	// frame asked the provider for a model that admission refused (decision H2
	// applied after the upgrade).
	MetricRealtimeSessionsClosedModel = "gateway_realtime_sessions_closed_model_total"
)

var (
	realtimeSessionsOpened       = expvar.NewInt(MetricRealtimeSessionsOpened)
	realtimeRefusedUnpricedModel = expvar.NewInt(MetricRealtimeRefusedUnpricedModel)
	realtimeRefusedCapacity      = expvar.NewInt(MetricRealtimeRefusedCapacity)
	realtimeTurnsBilled          = expvar.NewInt(MetricRealtimeTurnsBilled)
	realtimeTurnsUnpriced        = expvar.NewInt(MetricRealtimeTurnsUnpriced)
	realtimeTurnsRefused         = expvar.NewInt(MetricRealtimeTurnsRefused)
	realtimeSessionsClosedBudget = expvar.NewInt(MetricRealtimeSessionsClosedBudget)
	realtimeFramesDropped        = expvar.NewInt(MetricRealtimeFramesDropped)
	realtimeTurnBasisMismatch    = expvar.NewInt(MetricRealtimeTurnBasisMismatch)
	realtimeTurnsUnbilled        = expvar.NewInt(MetricRealtimeTurnsUnbilled)
	realtimeSessionsClosedModel  = expvar.NewInt(MetricRealtimeSessionsClosedModel)
)

// RealtimeMetricNames returns the names of this file's counters, in a fixed
// order, for the composition root's /metrics allowlist. It mirrors
// AudioMetricNames and ModelMapMetricNames: a counter this package publishes
// reaches the scrape surface through ONE named path, never a name copied into a
// second file.
//
// An expvar variable that is not listed here has NO route on this process's mux
// (CLAUDE.md, issue #465). Add a counter here when you publish one.
func RealtimeMetricNames() []string {
	return []string{
		MetricRealtimeSessionsOpened,
		MetricRealtimeRefusedUnpricedModel,
		MetricRealtimeRefusedCapacity,
		MetricRealtimeTurnsBilled,
		MetricRealtimeTurnsUnpriced,
		MetricRealtimeTurnsRefused,
		MetricRealtimeSessionsClosedBudget,
		MetricRealtimeFramesDropped,
		MetricRealtimeTurnBasisMismatch,
		MetricRealtimeTurnsUnbilled,
		MetricRealtimeSessionsClosedModel,
	}
}

// RealtimeCloseCode is a RFC 6455 close status, kept as this package's own type
// so the pump's ports do not depend on the WebSocket library.
type RealtimeCloseCode int

const (
	// RealtimeCloseNormal ends a session that finished for an ordinary reason.
	RealtimeCloseNormal RealtimeCloseCode = 1000
	// RealtimeCloseGoingAway ends a session because this replica is shutting down.
	RealtimeCloseGoingAway RealtimeCloseCode = 1001
	// RealtimeClosePolicy ends a session the budget gate refused.
	RealtimeClosePolicy RealtimeCloseCode = 1008
	// RealtimeCloseInternal ends a session on a gateway-side failure.
	RealtimeCloseInternal RealtimeCloseCode = 1011
)

// RealtimeSocket is the minimal WebSocket surface the pump uses. Both sockets —
// the caller's and the provider's — are behind it, so the pump can be driven by
// a fake provider socket in a test while the caller's side is a real one.
type RealtimeSocket interface {
	// Read returns the next complete message. It must fail when ctx is done.
	Read(ctx context.Context) ([]byte, error)
	// Write sends one complete text message.
	Write(ctx context.Context, frame []byte) error
	// Ping sends a ping and waits for the pong.
	Ping(ctx context.Context) error
	// Close closes the socket. It is best effort and never returns an error:
	// there is nothing a caller could do with one at this point.
	Close(code RealtimeCloseCode, reason string)
}

// RealtimeCodec is the provider's realtime event translation surface. A bifrost
// provider satisfies it through schemas.RealtimeProvider plus
// schemas.RealtimeUsageExtractor.
type RealtimeCodec interface {
	// ToBifrostRealtimeEvent decodes a PROVIDER event into bifrost's canonical
	// envelope.
	ToBifrostRealtimeEvent(providerEvent json.RawMessage) (*schemas.BifrostRealtimeEvent, error)
	// ToProviderRealtimeEvent encodes a canonical event back into the provider's
	// own wire format.
	ToProviderRealtimeEvent(event *schemas.BifrostRealtimeEvent) (json.RawMessage, error)
	// ShouldStartRealtimeTurn reports whether a client event begins a turn.
	ShouldStartRealtimeTurn(event *schemas.BifrostRealtimeEvent) bool
	// ExtractRealtimeTurnUsage reads a terminal turn event's usage. It takes the
	// RAW frame, which is why the downlink keeps the undecoded bytes.
	ExtractRealtimeTurnUsage(terminalEventRaw []byte) *schemas.BifrostLLMUsage
}

// RealtimeUpstream is one opened provider side of a session.
type RealtimeUpstream struct {
	// Socket is the provider WebSocket. The session owns it and closes it.
	Socket RealtimeSocket
	// Codec translates events for this provider.
	Codec RealtimeCodec
	// Subprotocol is the subprotocol the provider socket NEGOTIATED, read off
	// the opened connection ("realtime" for OpenAI and Azure, and "" for a
	// provider that answers with none).
	//
	// It is NOT the value the provider DECLARED. A declared value is only what
	// the dial offered, so reporting it claims a negotiation that may not have
	// happened. The two sides of a session negotiate separately — the caller's
	// socket is accepted before the provider is dialled — so this is a fact an
	// operator has to be able to read, and it must be the real one.
	Subprotocol string
}

// RealtimeDialer opens the provider side of a realtime session. bifrost/core
// owns no socket at all — it supplies the URL, the headers and the subprotocol,
// and the caller does the dial — so this port is where that sequence lives.
// NewBifrostRealtimeDialer is the production implementation.
//
// params carries the caller query parameters that the gateway forwards onto the
// provider URL. The handler passes ONLY the allowlisted set
// (realtimeForwardedQuery); the dialer applies the allowlist a second time,
// because this port must not become a way to reach the provider URL with
// caller-authored text.
type RealtimeDialer interface {
	DialRealtime(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string, params url.Values) (*RealtimeUpstream, error)
}

// realtimeForwardedParams is the ALLOWLIST of caller query parameters that
// reach the provider's realtime URL.
//
// WHY AN ALLOWLIST AND NOT PASSTHROUGH. The caller's query string is attacker
// controlled. bifrost builds the provider URL from the resolved credential's
// base URL, so a copied `model` would undo mapModel and the price gate, and a
// copied `api-key`, `api-version` or `deployment` would let the caller pick the
// credential or the Azure deployment the gateway dials. Nothing outside this
// list travels.
//
// WHY `intent` IS ON IT. The one client this route has —
// indexer_asr_realtime.py — dials
// `/v1/realtime?model=<m>&intent=transcription`, and `intent` is what selects
// the provider's TRANSCRIPTION session mode. bifrost's RealtimeWebSocketURL
// builds `<base>/v1/realtime?model=<model>` and core holds no occurrence of
// "intent" at all, so without this the provider opens a CONVERSATIONAL session
// and the only caller gets the wrong session for every call.
//
// Add a parameter here only when it selects a provider session MODE. A
// parameter that selects a model, a credential, a deployment or an endpoint is
// gateway state, and the gateway owns it.
var realtimeForwardedParams = []string{"intent"}

// realtimeForwardedQuery returns the allowlisted subset of a caller's query.
// It is the ONE place the allowlist is applied, and it copies values rather
// than the map, so no caller-owned slice reaches the dial.
func realtimeForwardedQuery(in url.Values) url.Values {
	out := url.Values{}
	for _, name := range realtimeForwardedParams {
		for _, v := range in[name] {
			if v = strings.TrimSpace(v); v != "" {
				out.Add(name, v)
			}
		}
	}
	return out
}

// Realtime handles GET (and POST) /llm/v1/realtime.
//
// The whole admission sequence runs while an http.ResponseWriter still exists,
// because after websocket.Accept there is no way left to write a status line or
// an OpenAI-shaped body. The order is the order every /llm route uses, with one
// step added:
//
//	upgrade check   — a request that is not a WebSocket handshake gets an
//	                  OpenAI-shaped 426, never the library's plain-text one
//	decode (the model rides in the query string)
//	mapModel        — the provider must never see a caller-authored title
//	price gate      — decision H2, see realtimePricedModel
//	checkBudget     — project ceiling, then member ceiling
//	capacity gate   — the session bound
//	Accept          — the upgrade
//	dial provider   — LAST, and after Accept on purpose
//
// THE DIAL IS THE LAST STEP. It used to run before websocket.Accept, so that a
// provider failure could still be an HTTP status. The price of that order was
// paid by every request that never becomes a session: a plain GET, a scan, and
// — because the Origin allowlist lives INSIDE Accept — every refused cross-site
// handshake each opened one outbound WebSocket to the provider first. That is
// an unauthenticated caller driving connections to a paid upstream. Accept now
// runs first, and a dial failure is reported on the socket as the same
// OpenAI-shaped error object every refusal on this route carries.
func (h *Handler) Realtime(w http.ResponseWriter, r *http.Request) {
	if h.realtimeDialer == nil {
		h.logger.Error("realtime: the route is mounted but no dialer is wired; refusing the session")
		writeError(w, http.StatusNotImplemented, "api_error",
			"the realtime surface is not available on this gateway", "realtime_unavailable")
		return
	}

	ctx, sc, ok := h.buildContext(w, r, true)
	if !ok {
		return
	}
	// A request that is not a WebSocket handshake never reaches websocket.Accept,
	// so the library's plain-text "426 Upgrade Required" would be the answer —
	// and an /llm route must answer an OpenAI-shaped body on EVERY refusal
	// (spec 2.5). A caller that gets `websocket: ...` as text has to guess
	// whether the gateway refused it or a proxy did.
	if !isWebSocketUpgrade(r) {
		sc.cancel()
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error",
			"`/llm/v1/realtime` serves a WebSocket session; the request must be a WebSocket upgrade",
			"upgrade_required")
		return
	}
	// ONE ownership rule for the whole function: everything acquired below is
	// released by cleanup, and cleanup runs EXACTLY ONCE, on every path,
	// including after the pump returns. Splitting this into per-resource guards
	// is how a slot or a wait-group entry leaks on the one refusal path somebody
	// forgets — and releasing the wait-group entry twice panics with a negative
	// counter, so "exactly once" is the property to keep, not "at least once".
	acquiredSlot := false
	tracked := false
	cleanup := func() {
		if acquiredSlot {
			h.realtimeLimit.release(identityProjectFromCtx(ctx))
		}
		if tracked {
			h.sessionWg.Done()
		}
		sc.cancel()
	}
	defer cleanup()

	// The model rides in the query string, not in a body: a WebSocket handshake
	// is a GET. The legacy relay sends `?model=…&intent=transcription`. `model`
	// is the gateway's — mapModel and the price gate own it — and `intent` is
	// forwarded to the provider, because it selects the provider's session
	// MODE. See realtimeForwardedParams for the whole allowlist.
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"the `model` query parameter is required", "")
		return
	}
	var provider schemas.ModelProvider
	if !h.mapModel(w, ctx, &provider, &model) {
		return
	}
	pricing, priced := h.realtimePricedModel(w, ctx, string(provider), model)
	if !priced {
		return
	}
	if !h.checkBudget(w, ctx, model) {
		return
	}

	// The pool bound. An anonymous caller has no project id, so every
	// unauthenticated handshake shares the "" bucket and is capped by the
	// per-project cap — which is the right answer for a caller that cannot be
	// attributed or billed.
	projectID := identityProjectFromCtx(ctx)
	if !h.realtimeLimit.acquire(projectID) {
		realtimeRefusedCapacity.Add(1)
		inUse, total, per := h.realtimeLimit.snapshot()
		h.logger.WarnContext(ctx, "realtime: the session pool is full; refusing the upgrade",
			"project_id", projectID, "in_use", inUse, "total", total, "per_project", per,
			"metric", MetricRealtimeRefusedCapacity)
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"too many concurrent realtime sessions; retry later", "rate_limit_exceeded")
		return
	}
	acquiredSlot = true

	// Register with the shutdown group BEFORE the provider is dialled, so a
	// session that is opening while shutdown starts is either tracked or
	// refused — never half-open and invisible to CloseRealtimeSessions.
	if !h.trackSession() {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"the gateway is shutting down; retry against another replica", "shutting_down")
		return
	}
	tracked = true

	client, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The caller may ask for a subprotocol or for none. RFC 6455 always
		// allows the empty one, which is what the pylon relay uses. The
		// provider's own subprotocol is not known yet — the dial happens after
		// this — so the gateway offers the ONE subprotocol this surface uses.
		Subprotocols: realtimeSubprotocols(realtimeSubprotocol),
		// SECURITY: a WebSocket handshake is NOT subject to CORS, so without an
		// origin check any web page could open this route with the browser's
		// ambient credentials. An empty OriginPatterns is the library's
		// same-origin default, NOT "allow everything" — and InsecureSkipVerify
		// is deliberately never set. See config.RealtimeAllowedOrigins.
		//
		// This check runs INSIDE Accept, which is the second reason the dial
		// moved below it: a refused cross-site handshake must not have opened a
		// provider socket first.
		OriginPatterns: h.realtimeOrigins,
	})
	if aerr != nil {
		// Accept has already written its own HTTP error response.
		h.logger.WarnContext(ctx, "realtime: the client upgrade was refused",
			"project_id", projectID, "origin", r.Header.Get("Origin"), "err", aerr)
		return
	}
	client.SetReadLimit(realtimeReadLimit)
	clientSock := &wsSocket{conn: client}

	// The dialer applies realtimeDialTimeout itself: the handshake deadline
	// belongs to the dial, and the session context must outlive it. Only the
	// allowlisted query parameters travel; see realtimeForwardedParams.
	up, derr := h.realtimeDialer.DialRealtime(ctx, provider, model, realtimeForwardedQuery(r.URL.Query()))
	if derr != nil {
		h.logger.ErrorContext(ctx, "realtime: the provider socket could not be opened",
			"project_id", projectID, "provider", provider, "model", model, "err", derr)
		// The connection is hijacked, so there is no status line left. The same
		// OpenAI-shaped error object rides the socket instead, and the close
		// status says the fault was the gateway's side of the session.
		writeRealtimeFrame(ctx, clientSock, realtimeRefusalFrame(
			"api_error", "upstream_unavailable", "the realtime provider is unavailable"))
		clientSock.Close(RealtimeCloseInternal, "the realtime provider is unavailable")
		return
	}

	realtimeSessionsOpened.Add(1)
	// Both subprotocols are logged. The two sides of a session negotiate
	// separately now that the client socket is accepted before the provider is
	// dialled, so a provider that answers something other than "realtime" is a
	// fact an operator has to be able to see.
	h.logger.InfoContext(ctx, "realtime: session opened",
		"project_id", projectID, "provider", provider, "model", model,
		"client_subprotocol", client.Subprotocol(), "provider_subprotocol", up.Subprotocol,
		"metric", MetricRealtimeSessionsOpened)

	admitted := realtimeModel{provider: string(provider), model: model, pricing: pricing}
	s := &realtimeSession{
		h:      h,
		ctx:    ctx,
		cancel: sc.cancel,
		client: clientSock,
		up:     up,
		// Both slots start at the model admission approved. A client frame can
		// move either one, and only through the full admission sequence — see
		// admitFrameModels.
		resp:      admitted,
		asr:       admitted,
		projectID: projectID,
		userID:    identityUserFromCtx(ctx),
	}
	// Blocking here is what keeps the hijacked connection owned by a live
	// goroutine: net/http will not touch it again, so nothing else would. The
	// single deferred cleanup above releases the slot, the wait-group entry and
	// the context once run returns.
	s.run()
}

// realtimeSubprotocol is the ONE WebSocket subprotocol this surface uses.
// OpenAI and Azure both negotiate "realtime"; RFC 6455 always allows the empty
// one, which is what the pylon relay asks for. It is a constant rather than a
// value read from the provider because the client socket is now accepted BEFORE
// the provider is dialled, so the provider's answer is not known yet. A
// provider that negotiates something else still works — the two sides of a
// session negotiate separately — and the dial logs what it got.
const realtimeSubprotocol = "realtime"

// realtimeSubprotocols returns the subprotocols to offer on a handshake.
func realtimeSubprotocols(sub string) []string {
	if sub == "" {
		return nil
	}
	return []string{sub}
}

// isWebSocketUpgrade reports whether the request is a WebSocket handshake.
//
// It repeats what websocket.Accept checks, because Accept answers a plain-text
// 426 and this route must answer an OpenAI-shaped body on every refusal. RFC
// 6455 makes `Connection` a comma-separated token LIST, so a substring test on
// the whole header is the correct one here (browsers send "keep-alive, Upgrade").
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// realtimePricedModel enforces decision H2: a realtime session opens only for a
// model the price CATALOG carries a rate for.
//
// Why this route and no other. cost.Calculator's default table prefix-matches,
// so "gpt-4o-realtime-preview" resolves onto the plain "gpt-4o" text row and
// bills a confident, roughly 10x-too-small number that no "unpriced" counter can
// ever fire on. For a single audio request the answer to an unpriced model is
// "bill zero and count it", because the request ends and the loss is bounded by
// one call. A socket the tenant holds open has no natural bound at all, so the
// same answer would cap nothing. The session is refused instead.
//
// The probe accepts EITHER basis, because both are real realtime billing shapes:
// a conversational session is sold by the token, and a transcription session can
// be sold by the second. cost.Cost.FromCatalog is the test in both cases — it
// reports the provenance of the rate that WOULD pay, so a default-table or
// fallback price answers false.
//
// A gateway with no budget gate wired has no catalog to read and meters nothing
// on any route; the check is skipped there for the same reason checkBudget is,
// and this is not a second policy.
func (h *Handler) realtimePricedModel(w http.ResponseWriter, ctx context.Context, provider, model string) (realtimePricing, bool) {
	bp := h.budget()
	if bp.gate == nil || bp.calc == nil {
		// Nothing is metered on this deployment, so every basis is "as priced as
		// any other". Reporting both keeps the mismatch counter silent here
		// rather than firing on every turn of an ungoverned gateway.
		return realtimePricing{tokens: true, seconds: true}, true
	}
	p := realtimePricing{
		tokens:  bp.calc.Cost(ctx, provider, model, 1, 1).FromCatalog(),
		seconds: bp.calc.CostUnits(ctx, provider, model, cost.Units{InputMillis: 1}).FromCatalog(),
	}
	if p.tokens || p.seconds {
		return p, true
	}
	realtimeRefusedUnpricedModel.Add(1)
	h.logger.ErrorContext(ctx, "realtime: the price catalog carries no rate for this model; refusing the session",
		"provider", provider, "model", model, "metric", MetricRealtimeRefusedUnpricedModel)
	// 502 and `api_error` are the status and type writeModelCatalogueUnavailable
	// already uses for the neighbouring condition — a piece of gateway-side
	// model configuration that is missing. It is deliberately NOT 402: a 402 on
	// this surface must carry the budget contract elitea-sdk matches on, and
	// this refusal is not a budget refusal.
	writeError(w, http.StatusBadGateway, "api_error",
		"the model `"+model+"` has no realtime price in the catalogue; the session was refused", "model_not_priced")
	return realtimePricing{}, false
}

// realtimePricing records WHICH price basis admission accepted for one model.
//
// H2's probe accepts EITHER basis, because both are real realtime billing
// shapes. That leaves a gap the probe alone cannot close: a model the catalogue
// prices by TOKENS is admitted, and if the provider then reports a DURATION for
// every turn, updateUsageUnits finds no per-second rate, bills nothing and
// answers billNotBillable. The turn is not billed, and it is not counted as
// unpriced either, because the realtime unpriced counter only fires when the
// gateway could read NO quantity at all. The session bills zero, silently, for
// its whole life.
//
// Binding the admission to ONE basis is not the fix: the gateway cannot know
// which shape a provider will report until it reports one, and refusing a model
// that carries only one of the two rates would refuse every whisper-style row.
// The mismatch is made ALARMABLE instead — see MetricRealtimeTurnBasisMismatch.
type realtimePricing struct {
	tokens  bool
	seconds bool
}

// admits reports whether a catalogue rate exists for the basis a turn reported.
func (p realtimePricing) admits(basis string) bool {
	switch basis {
	case cost.BasisTokens:
		return p.tokens
	case cost.BasisSeconds:
		return p.seconds
	default:
		// Characters are not a realtime basis; treat an unknown one as
		// unpriced so it is counted rather than assumed.
		return false
	}
}

// realtimeModel is one model a live session serves, with the price basis its
// admission accepted. A session holds TWO of them, because a realtime session
// can serve two models at once: the response model and the input-transcription
// model. Billing each turn against the model that PRODUCED it is the whole
// point — pricing a transcription turn with the conversation model's rate is a
// wrong number, not a missing one.
type realtimeModel struct {
	provider string
	model    string
	pricing  realtimePricing
}

// realtimeSession is one live session and everything it owns.
type realtimeSession struct {
	h   *Handler
	ctx *schemas.BifrostContext
	// cancel is THE stop signal. Every goroutine below exits once it fires, and
	// it is idempotent.
	cancel func()

	client RealtimeSocket
	up     *RealtimeUpstream

	// modelMu guards the two model slots. They are NOT immutable: a client
	// frame can move either one, and the uplink writes them while the downlink
	// reads them to price a turn.
	modelMu sync.RWMutex
	resp    realtimeModel // billed for response.done turns
	asr     realtimeModel // billed for input-transcription turns

	projectID string
	userID    string

	// refusing is decision H1's state: while it is set the uplink forwards
	// NOTHING to the provider, so no un-gated turn can reach it, and the socket
	// stays open.
	//
	// THREE GOROUTINES WRITE IT, not one. regate writes it on a re-check that
	// the gate did not admit. The UPLINK writes it too, through gateTurn and
	// through end() on a refused mid-session model change. watchShutdown writes
	// it through end() as well. atomic.Bool is what makes those writes safe, so
	// this is a documentation fact and not a race — but the comment used to name
	// the re-check goroutine as the only writer, and a false ownership claim is
	// how the next change turns a safe field into a plain bool and creates a
	// real race.
	refusing atomic.Bool

	// ending is set by end() BEFORE it closes the client socket, and it is what
	// makes the refusal that end() set permanent.
	//
	// It cannot be replaced by a check on the session context. end() runs in the
	// order refuse, close, cancel, and the close waits up to
	// realtimeCloseHandshakeBudget for the peer's close reply. Throughout that
	// window s.ctx.Err() is still nil. A gate call that was already in flight
	// when the session was refused can therefore return Allow inside that
	// window, and a resumeTurns() guarded only on the context would clear the
	// refusal and let the uplink forward to the provider again — on the budget
	// that just refused it. Reordering end() to cancel first does NOT fix this
	// and breaks something else: coder/websocket arms a context.AfterFunc that
	// closes the connection ABRUPTLY when the context is cancelled, so the close
	// FRAME is lost and the caller cannot tell a budget refusal from a crash
	// (measured: 22 of 30 runs returned EOF with no close status).
	ending atomic.Bool

	// endOnce makes the FIRST reason a session ends the reason its caller sees.
	// Without it run()'s tidy close races the budget gate's 1008 and the caller
	// reads "session ended" for a refusal.
	endOnce sync.Once
}

// currentResponseModel returns the model a response turn bills against.
func (s *realtimeSession) currentResponseModel() realtimeModel {
	return s.slotModel(realtimeSlotResponse)
}

// currentTranscriptionModel returns the model a transcription turn bills against.
func (s *realtimeSession) currentTranscriptionModel() realtimeModel {
	return s.slotModel(realtimeSlotTranscription)
}

// end is the ONE way a session ends from the inside, and the ORDER of its three
// steps is the whole reason it exists.
//
//  1. REFUSE FIRST. Until this flag is set the uplink keeps forwarding client
//     events to the provider, and step 2 blocks. An exhausted budget that
//     closed the socket first therefore paid for a whole close handshake of
//     further inference. Setting the flag stops the spend at once.
//  2. CLOSE, WITH THE FRAME. The close STATUS is how the caller learns WHY the
//     session ended (1008 budget, 1001 shutdown), so the frame has to be
//     written. wsSocket.Close bounds the wait for the peer's reply; it does not
//     skip the frame.
//  3. CANCEL LAST. Cancelling first looks tidier and is wrong: the session
//     context is the context the uplink reads the CLIENT socket with, and
//     coder/websocket arms a context.AfterFunc that closes the connection
//     ABRUPTLY when it fires (conn.go, setupReadTimeout). Measured on this
//     code: cancel-then-close delivered the close status on 8 runs out of 30
//     and an unexplained EOF on the other 22.
func (s *realtimeSession) end(code RealtimeCloseCode, reason string) {
	s.endOnce.Do(func() {
		// ending BEFORE refusing: a concurrent resumeTurns reads ending, and it
		// must never observe the refusal without also observing that the
		// session is on its way out.
		s.ending.Store(true)
		s.refusing.Store(true)
		s.client.Close(code, reason)
		s.cancel()
	})
}

// run drives the session and returns when every goroutine has exited.
func (s *realtimeSession) run() {
	defer s.cancel()

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		// The uplink owns the CLIENT socket's reads. Cancelling on the way out
		// is what tears the other four goroutines down when the caller hangs up.
		defer s.cancel()
		s.uplink()
	}()
	go func() {
		defer wg.Done()
		// The downlink owns the PROVIDER socket's reads, and cancels for the
		// same reason when the provider hangs up.
		defer s.cancel()
		s.downlink()
	}()
	go func() {
		defer wg.Done()
		s.keepalive()
	}()
	go func() {
		defer wg.Done()
		s.regate()
	}()
	go func() {
		defer wg.Done()
		s.watchShutdown()
	}()

	wg.Wait()
	// end() is a no-op when the session already ended for a stated reason, so
	// the caller keeps the close status that says WHY.
	s.end(RealtimeCloseNormal, "session ended")
	s.up.Socket.Close(RealtimeCloseNormal, "session ended")
	m := s.currentResponseModel()
	s.h.logger.Info("realtime: session closed",
		"project_id", s.projectID, "provider", m.provider, "model", m.model)
}

// watchShutdown cancels the session when the process begins to shut down. It is
// the reason CloseRealtimeSessions terminates: nothing else is watching that
// channel on a session's behalf.
func (s *realtimeSession) watchShutdown() {
	select {
	case <-s.h.sessionClosing:
		s.end(RealtimeCloseGoingAway, "the gateway is shutting down")
	case <-s.ctx.Done():
	}
}

// uplink pumps caller events to the provider.
//
// Every frame passes through bifrost's canonical envelope and back out through
// the provider's own encoder, so a caller's event is normalised exactly once. A
// frame the envelope cannot represent is forwarded VERBATIM rather than dropped:
// this route exists to carry a wire protocol the gateway does not own, and an
// event bifrost has no case for (the relay's `transcription_session.update`, for
// one) must still reach the provider.
func (s *realtimeSession) uplink() {
	for {
		raw, err := s.client.Read(s.ctx)
		if err != nil {
			return
		}
		if s.refusing.Load() {
			// DECISION H1. The turn is not forwarded. The socket stays open, and
			// the client already has the error event the transition sent.
			s.countDrop(raw)
			continue
		}
		// A client frame can ask the provider for a DIFFERENT model. That has to
		// be admitted before it is forwarded, or admission means nothing after
		// the upgrade; see admitFrameModels.
		rewritten, admitted := s.admitFrameModels(raw)
		if !admitted {
			return
		}
		out := raw
		switch {
		case rewritten != nil:
			// The frame carried a caller-authored model title and now carries
			// the mapped provider name. It is forwarded AS REWRITTEN and not
			// through the canonical envelope: the round trip is what put the
			// name back, and transcription_session.update is an event bifrost
			// has no case for at all.
			out = rewritten
		default:
			if ev, perr := schemas.ParseRealtimeEvent(raw); perr == nil && ev != nil {
				// The ADDITIONAL turn-start trigger. It fires for the events
				// bifrost recognises as a turn start; the periodic re-check is
				// what covers every caller that sends none of them.
				if s.up.Codec.ShouldStartRealtimeTurn(ev) && !s.gateTurn() {
					continue
				}
				if conv, cerr := s.up.Codec.ToProviderRealtimeEvent(ev); cerr == nil && len(conv) > 0 {
					out = conv
				}
			}
		}
		if err := s.write(s.up.Socket, out); err != nil {
			return
		}
	}
}

// countDrop counts ONE client frame the refusing state stopped.
//
// This is where the turns a refusal really drops are dropped. refuseTurns used
// to hold the only Add, and regate calls it once per re-check TICK — so at the
// shipped 15 s interval an operator read "1" while a caller streaming 20 audio
// frames a second had 300 of them thrown away. The tick count and the drop
// count are different quantities and both are now published.
func (s *realtimeSession) countDrop(raw []byte) {
	realtimeFramesDropped.Add(1)
	// Decode the TYPE alone, not the whole event. This runs for every frame a
	// refusing session drops, which for the relay is roughly twenty a second,
	// and each of those frames is mostly base64 audio. ParseRealtimeEvent
	// materialises that payload — it keeps every unknown top-level field in
	// ExtraParams — purely so a counter can be incremented. The classifier only
	// ever reads Type, so that is all this builds. The same cheap decode is what
	// realtimeTerminalEventType falls back to.
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Type == "" {
		return
	}
	if s.up.Codec.ShouldStartRealtimeTurn(&schemas.BifrostRealtimeEvent{
		Type: schemas.RealtimeEventType(env.Type),
	}) {
		realtimeTurnsRefused.Add(1)
	}
}

// ── the mid-session model change (decision H2, after the upgrade) ───────────

// realtimeModelSlot names which of a session's two models a frame field sets.
type realtimeModelSlot int

const (
	// realtimeSlotResponse is the model that serves response turns.
	realtimeSlotResponse realtimeModelSlot = iota
	// realtimeSlotTranscription is the model that transcribes input audio.
	realtimeSlotTranscription
)

// realtimeAskedModel is one model name a client frame asks the provider for.
type realtimeAskedModel struct {
	slot realtimeModelSlot
	name string
}

// realtimeSessionUpdateFrame is the part of session.update and
// transcription_session.update this route has to read. Both events carry a
// `session` object, and both can name a model inside it.
type realtimeSessionUpdateFrame struct {
	Type    string `json:"type"`
	Session *struct {
		Model                   *string `json:"model"`
		InputAudioTranscription *struct {
			Model *string `json:"model"`
		} `json:"input_audio_transcription"`
	} `json:"session"`
}

// realtimeFrameModels returns the models a client frame asks the provider for.
//
// Only two event types can carry one. Everything else — audio appends, commits,
// response.create — is read as naming no model, and the fast path in
// admitFrameModels then costs one JSON decode of a small prefix.
func realtimeFrameModels(raw []byte) []realtimeAskedModel {
	var f realtimeSessionUpdateFrame
	if err := json.Unmarshal(raw, &f); err != nil || f.Session == nil {
		return nil
	}
	if f.Type != string(schemas.RTEventSessionUpdate) && f.Type != realtimeTranscriptionSessionUpdate {
		return nil
	}
	var out []realtimeAskedModel
	if m := f.Session.Model; m != nil && strings.TrimSpace(*m) != "" {
		out = append(out, realtimeAskedModel{slot: realtimeSlotResponse, name: strings.TrimSpace(*m)})
	}
	if t := f.Session.InputAudioTranscription; t != nil && t.Model != nil && strings.TrimSpace(*t.Model) != "" {
		out = append(out, realtimeAskedModel{slot: realtimeSlotTranscription, name: strings.TrimSpace(*t.Model)})
	}
	return out
}

// realtimeTranscriptionSessionUpdate is the transcription-mode session update.
// bifrost has NO case for it — ParseRealtimeEvent does not recognise it — which
// is exactly why the model it carries would otherwise pass through untouched.
// It is also the FIRST frame indexer_asr_realtime.py sends on every session.
const realtimeTranscriptionSessionUpdate = "transcription_session.update"

// realtimeRewriteFrameModels returns the frame with the named slots' model
// fields replaced. The generic decode keeps every field the gateway does not
// understand, and json.Number keeps a threshold like 0.7 from becoming 0.7000001
// on the way through.
func realtimeRewriteFrameModels(raw []byte, repl map[realtimeModelSlot]string) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	sess, _ := doc["session"].(map[string]any)
	if sess == nil {
		return nil, errRealtimeFrameShape
	}
	if name, ok := repl[realtimeSlotResponse]; ok {
		sess["model"] = name
	}
	if name, ok := repl[realtimeSlotTranscription]; ok {
		iat, _ := sess["input_audio_transcription"].(map[string]any)
		if iat == nil {
			return nil, errRealtimeFrameShape
		}
		iat["model"] = name
	}
	return json.Marshal(doc)
}

// errRealtimeFrameShape means a frame that named a model could not be rewritten
// to carry the MAPPED name. The session is refused rather than the frame
// forwarded, because forwarding it would send the provider a caller-authored
// title — the one thing mapModel exists to prevent.
var errRealtimeFrameShape = errors.New("realtime: the session frame has no writable model field")

// slotModel reads one model slot.
func (s *realtimeSession) slotModel(slot realtimeModelSlot) realtimeModel {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	if slot == realtimeSlotTranscription {
		return s.asr
	}
	return s.resp
}

// setSlotModel writes one model slot. Only the uplink calls it, and only after
// the full admission sequence has admitted the new model.
func (s *realtimeSession) setSlotModel(slot realtimeModelSlot, m realtimeModel) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	if slot == realtimeSlotTranscription {
		s.asr = m
		return
	}
	s.resp = m
}

// admitFrameModels is decision H2 applied AFTER the upgrade.
//
// THE HOLE IT CLOSES. mapModel, the price gate and checkBudget all run against
// the `model` query parameter — but the model the provider actually SERVES is
// changed by a client frame: session.update carries session.model, and
// transcription_session.update carries session.input_audio_transcription.model.
// The uplink forwarded both verbatim, so an admitted, priced model became an
// unpriced one mid-session while billTurn kept pricing the original. Everything
// H2 refuses at the upgrade was reachable one frame later.
//
// WHY RE-ADMISSION AND NOT REFUSAL OF THE FRAME. Refusing the frame outright
// breaks the ONE client this route has: indexer_asr_realtime.py opens every
// session by sending transcription_session.update, and that frame carries the
// transcription model AND the audio format, the language and the VAD settings.
// Dropping it leaves the session with no transcription configuration at all — a
// silent, total failure. So the new model goes through the FULL sequence
// instead: mapModel, then the price gate, then the budget gate. On success the
// session adopts it, so billTurn prices what the provider serves, and the frame
// is rewritten to carry the MAPPED name. On failure the session is CLOSED, not
// merely stripped: a session that quietly kept serving the old model would
// answer a request the caller never made, and the caller could not tell.
//
// It returns (rewritten, true) when the frame must be forwarded as rewritten,
// (nil, true) when the frame may be forwarded unchanged, and (nil, false) when
// the session has been refused and the uplink must stop.
func (s *realtimeSession) admitFrameModels(raw []byte) ([]byte, bool) {
	asked := realtimeFrameModels(raw)
	if len(asked) == 0 {
		return nil, true
	}
	repl := make(map[realtimeModelSlot]string, len(asked))
	for _, a := range asked {
		cur := s.slotModel(a.slot)
		if a.name == cur.model {
			// The caller named the model already admitted, by its provider
			// name. Nothing changes, and the budget gate is not re-asked.
			continue
		}
		next, v, ok := s.readmitModel(cur, a.name)
		if !ok {
			realtimeSessionsClosedModel.Add(1)
			s.h.logger.WarnContext(s.ctx, "realtime: a client frame asked for a model admission refused; closing the session",
				"project_id", s.projectID, "admitted_model", cur.model, "asked_model", a.name,
				"status", v.status, "code", v.code, "metric", MetricRealtimeSessionsClosedModel)
			s.sendRefusalEvent(v)
			s.end(RealtimeClosePolicy, v.message)
			return nil, false
		}
		if next != cur {
			s.setSlotModel(a.slot, next)
			s.h.logger.InfoContext(s.ctx, "realtime: the session model changed mid-session and was re-admitted",
				"project_id", s.projectID, "from", cur.model, "to", next.model, "provider", next.provider)
		}
		if next.model != a.name {
			repl[a.slot] = next.model
		}
	}
	if len(repl) == 0 {
		return nil, true
	}
	out, err := realtimeRewriteFrameModels(raw, repl)
	if err != nil {
		realtimeSessionsClosedModel.Add(1)
		s.h.logger.ErrorContext(s.ctx, "realtime: the mapped model could not be written back into the session frame; closing the session",
			"project_id", s.projectID, "err", err, "metric", MetricRealtimeSessionsClosedModel)
		v := budgetVerdict{
			status: http.StatusBadGateway, errType: "api_error", code: "model_not_found",
			message: "the session frame could not carry the mapped model name",
		}
		s.sendRefusalEvent(v)
		s.end(RealtimeClosePolicy, v.message)
		return nil, false
	}
	return out, true
}

// readmitModel runs the FULL admission sequence for a model a client frame
// asked for. It is the same three steps the upgrade runs, in the same order,
// against the same functions — a second, weaker copy of admission is how the
// two drift apart.
//
// The budget gate is asked only when the model really CHANGES. A caller that
// spells the admitted model by its advertised title costs one model-set read
// (cached) and no budget read.
func (s *realtimeSession) readmitModel(cur realtimeModel, want string) (realtimeModel, budgetVerdict, bool) {
	rec := &realtimeRefusalRecorder{}
	prov := schemas.ModelProvider(cur.provider)
	mapped := want
	if !s.h.mapModel(rec, s.ctx, &prov, &mapped) {
		return cur, rec.verdict(), false
	}
	pricing, priced := s.h.realtimePricedModel(rec, s.ctx, string(prov), mapped)
	if !priced {
		return cur, rec.verdict(), false
	}
	next := realtimeModel{provider: string(prov), model: mapped, pricing: pricing}
	if next.model == cur.model && next.provider == cur.provider {
		return next, budgetAllowed, true
	}
	if v := s.gateVerdict(mapped); !v.allow {
		return cur, v, false
	}
	return next, budgetAllowed, true
}

// realtimeRefusalRecorder captures the OpenAI-shaped body an admission step
// WOULD have written. After the upgrade there is no http.ResponseWriter left,
// and the admission functions need one; this is that writer.
//
// It exists so a mid-session refusal carries the SAME type, code and message
// the HTTP path writes for the same condition. A second set of refusal strings
// for the same conditions is a second thing to keep in step with elitea-sdk.
type realtimeRefusalRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *realtimeRefusalRecorder) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *realtimeRefusalRecorder) Write(b []byte) (int, error) { return w.body.Write(b) }

func (w *realtimeRefusalRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

// verdict renders what was captured as a refusal verdict.
func (w *realtimeRefusalRecorder) verdict() budgetVerdict {
	v := budgetVerdict{
		status:  w.status,
		errType: "api_error",
		message: "the realtime session was refused",
	}
	if v.status == 0 {
		v.status = http.StatusBadGateway
	}
	var body openAIError
	if err := json.Unmarshal(w.body.Bytes(), &body); err == nil && body.Error.Type != "" {
		v.errType, v.code, v.message = body.Error.Type, body.Error.Code, body.Error.Message
		v.scope = body.Error.Scope
	}
	return v
}

// gateVerdict asks the SAME admission question the HTTP routes ask, under a
// bound this route owns, and as a RE-CHECK rather than as a request.
//
// WHY recheckVerdict AND NOT admissionVerdict. admissionVerdict records a hit
// in the amplification backstop's sliding window, because an arriving request
// is what that window measures. This function runs on a ticker for the whole
// life of every live session, so it made the gateway's own housekeeping look
// like tenant traffic: enough long sessions on one (project, model) pair opened
// the circuit for that project's REAL /llm requests. recheckVerdict observes
// the circuit and records nothing. See loopBreaker.observe.
//
// admissionVerdict already wraps each store read in budgetGateTimeout, but that
// is a context DEADLINE: a store that stalls while it ignores its context
// returns nothing, and the session context has no deadline by design. The
// re-check goroutine then parks for ever, the ticker never fires again, and the
// session runs un-gated until the process dies. The timer below is the bound
// that holds whatever the store does.
//
// The channel is BUFFERED, so the abandoned goroutine can deliver its answer
// and exit rather than leaking on a receiver that walked away. It writes no
// session state, so a late answer changes nothing. A stalled store therefore
// holds at most maxBudgetOutagesBeforeClose goroutines per session, because the
// session closes after that many silent re-checks.
//
// A timeout is an OUTAGE and reads as one: decision H1 refuses the turn, keeps
// the socket, and counts it toward maxBudgetOutagesBeforeClose.
func (s *realtimeSession) gateVerdict(model string) budgetVerdict {
	bound := s.h.realtimeGateBound
	if bound <= 0 {
		bound = realtimeGateTimeout
	}
	out := make(chan budgetVerdict, 1)
	go func() { out <- s.h.recheckVerdict(s.ctx, model) }()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case v := <-out:
		return v
	case <-timer.C:
		s.h.logger.Error("realtime: the budget gate did not answer within the session bound; treating it as an outage",
			"project_id", s.projectID, "model", model, "timeout", bound)
		return budgetVerdict{
			status:  http.StatusServiceUnavailable,
			errType: "service_unavailable",
			message: "budget service did not answer in time; try again shortly",
			code:    "nats_unavailable",
		}
	}
}

// downlink pumps provider events to the caller and bills each completed turn.
//
// The provider frame is forwarded UNCHANGED. Re-encoding it through the
// canonical envelope would be a lossy round trip in service of nothing: the
// caller speaks the provider's dialect, which is the dialect the frame already
// carries. The decoded envelope is used to classify the event, and the RAW bytes
// are kept because that is what the usage extractor takes.
func (s *realtimeSession) downlink() {
	for {
		raw, err := s.up.Socket.Read(s.ctx)
		if err != nil {
			return
		}
		var ev *schemas.BifrostRealtimeEvent
		if decoded, cerr := s.up.Codec.ToBifrostRealtimeEvent(raw); cerr == nil {
			ev = decoded
		}
		// FORWARD FIRST. Billing must never delay the caller's audio, and a
		// billing bug must never be able to swallow a frame.
		werr := s.write(s.client, raw)
		// BILL EVEN WHEN THE FORWARD FAILED. The provider has already done the
		// work by the time it reports a completed turn; a client that went away
		// between the turn and its terminal frame does not make that work free.
		// Returning on the write error before this line was the
		// free-inference-on-disconnect class stream_drain.go exists to prevent,
		// and no counter moved for it either.
		s.billTurn(raw, ev)
		if werr != nil {
			return
		}
	}
}

// write sends one frame under a bounded deadline. The session context has none
// by design, so without this a peer that stops reading parks the pump for the
// life of the session.
func (s *realtimeSession) write(sock RealtimeSocket, frame []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, realtimeWriteTimeout)
	defer cancel()
	return sock.Write(ctx, frame)
}

// keepalive pings both peers. F6: the hijack CLEARS the connection deadlines, so
// no server ReadHeaderTimeout or IdleTimeout can ever reap an idle session and
// the liveness check is this gateway's own job. A failed ping ends the session.
func (s *realtimeSession) keepalive() {
	period := s.h.realtimeKeepalive
	if period <= 0 {
		period = realtimeKeepalivePeriod
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		clientErr, providerErr := s.pingPeers()
		if clientErr != nil || providerErr != nil {
			s.h.logger.Info("realtime: a keepalive ping failed; ending the session",
				"project_id", s.projectID, "client_err", clientErr, "provider_err", providerErr)
			s.cancel()
			return
		}
	}
}

// pingPeers pings the two peers AT THE SAME TIME, and gives each ping its OWN
// deadline. It reports one result per peer.
//
// THE FAILURE IT PREVENTS: a false liveness verdict on a healthy provider. The
// two pings shared one deadline and ran one after the other. coder/websocket's
// Ping waits for the PONG, so a caller that was slow to answer spent the shared
// budget, and the provider ping then started on a context that was already
// expired. The provider answered "context deadline exceeded" although it was
// live, and keepalive tore the whole session down for it. A deadline per peer
// makes each verdict a statement about that peer alone.
//
// The two sockets are different connections, so a concurrent ping on each is
// safe: coder/websocket serialises the writes on ONE connection.
func (s *realtimeSession) pingPeers() (clientErr, providerErr error) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		clientErr = s.ping(s.client)
	}()
	go func() {
		defer wg.Done()
		providerErr = s.ping(s.up.Socket)
	}()
	wg.Wait()
	return clientErr, providerErr
}

// ping sends one ping under a deadline of its own.
func (s *realtimeSession) ping(sock RealtimeSocket) error {
	bound := s.h.realtimePingBound
	if bound <= 0 {
		bound = realtimePingBound
	}
	ctx, cancel := context.WithTimeout(s.ctx, bound)
	defer cancel()
	return sock.Ping(ctx)
}

// regate is the MANDATORY budget re-check (decision H1, fact F3).
//
// It re-asks the SAME admission verdict the HTTP routes ask — one decision
// function, so this cannot drift into a second, weaker gate — and applies the
// two outcomes a human chose:
//
//	402 (any ceiling exhausted) → send the refusal event, CLOSE the session.
//	503 (the store did not      → refuse turns, KEEP the socket open. After
//	     answer)                  maxBudgetOutagesBeforeClose consecutive ones,
//	                             close. This is decision H1, and 503 is the ONLY
//	                             verdict H1 was written for.
//	anything else refused       → refuse turns, KEEP the socket open, and count
//	                             NOTHING. See the branch for why H1 must not
//	                             cover a tripped amplification backstop.
//
// An Allow clears the refusal, so a session that rode out a blip resumes.
func (s *realtimeSession) regate() {
	interval := s.h.realtimeRecheck
	if interval <= 0 {
		// Only a Handler built by hand can reach this. It is logged rather than
		// silent, because a session with no re-check is a session with one
		// admission check and no bound at all.
		s.h.logger.Warn("realtime: the budget re-check interval is not positive; this session is gated ONCE",
			"project_id", s.projectID, "model", s.currentResponseModel().model)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	outages := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		model := s.currentResponseModel().model
		v := s.gateVerdict(model)
		switch {
		case v.allow:
			outages = 0
			s.resumeTurns()
		case v.status == http.StatusPaymentRequired:
			realtimeSessionsClosedBudget.Add(1)
			s.h.logger.Info("realtime: the budget is exhausted; closing the session",
				"project_id", s.projectID, "model", model, "code", v.code,
				"metric", MetricRealtimeSessionsClosedBudget)
			// refusing is set by end() BEFORE the close, so no further client
			// event reaches the provider while the close handshake runs. The
			// refusal frame is written first, because a cancelled session
			// cannot write one.
			// refuseTurns, not a bare Store: its compare-and-swap is what keeps
			// a caller that is ALREADY refusing (an earlier outage tick sent it
			// one error event) from receiving a second one. The close frame
			// below still carries this verdict's own message, so the reason is
			// not lost by sending one event instead of two.
			s.refuseTurns(v)
			s.end(RealtimeClosePolicy, v.message)
			return
		case v.status == http.StatusServiceUnavailable:
			// AN OUTAGE, and the ONLY verdict decision H1's counter counts. H1
			// was authored for one condition: the budget store could not answer.
			// The turn is refused, the socket stays open, and N consecutive
			// silences close the session.
			outages++
			s.refuseTurns(v)
			if outages >= maxBudgetOutagesBeforeClose {
				realtimeSessionsClosedBudget.Add(1)
				s.h.logger.Error("realtime: the budget gate has been unreachable for too long; closing the session",
					"project_id", s.projectID, "model", model, "consecutive", outages,
					"metric", MetricRealtimeSessionsClosedBudget)
				s.end(RealtimeClosePolicy, "the budget service is unavailable")
				return
			}
		default:
			// A refusal that is NEITHER an exhausted budget NOR a store outage.
			// The amplification backstop's 429 is the one that reaches here.
			//
			// It refuses turns and keeps the socket, but it must NOT count
			// toward H1's outage budget. H1 says what to do when the gateway
			// cannot READ the budget; a tripped circuit breaker is a different
			// condition, and applying H1 to it closes live calls under a policy
			// nobody wrote for them. A backstop circuit opens for 5 s by
			// default, which is shorter than the whole outage window, so a
			// session that rode one out would have been closed for a condition
			// that had already cleared.
			//
			// The counter is left UNCHANGED rather than reset. This verdict
			// says nothing about whether the store is reachable, so it neither
			// proves an outage nor ends one; only an Allow ends one.
			s.h.logger.Warn("realtime: the gate refused for a reason that is not an outage; refusing turns and keeping the session",
				"project_id", s.projectID, "model", model, "status", v.status, "code", v.code)
			s.refuseTurns(v)
		}
	}
}

// gateTurn is the ADDITIONAL turn-start trigger. It reports whether the turn may
// be forwarded. It answers from the same verdict function regate uses, so it is
// a RE-CHECK too: the session was already counted against the amplification
// backstop at the upgrade, and counting each turn of one admitted session again
// would open the circuit on the gateway's own gating work.
func (s *realtimeSession) gateTurn() bool {
	model := s.currentResponseModel().model
	v := s.gateVerdict(model)
	if v.allow {
		return true
	}
	// This turn is NOT forwarded, whichever branch follows. It is a turn start
	// the gate refused, so it is counted here — the uplink's refusing check
	// never sees it.
	realtimeTurnsRefused.Add(1)
	realtimeFramesDropped.Add(1)
	if v.status == http.StatusPaymentRequired {
		realtimeSessionsClosedBudget.Add(1)
		s.h.logger.Info("realtime: the budget is exhausted at turn start; closing the session",
			"project_id", s.projectID, "model", model, "code", v.code,
			"metric", MetricRealtimeSessionsClosedBudget)
		// See regate's 402 branch: refuseTurns deduplicates the error event, and
		// the close frame still carries this verdict's message.
		s.refuseTurns(v)
		s.end(RealtimeClosePolicy, v.message)
		return false
	}
	s.refuseTurns(v)
	return false
}

// refuseTurns enters the refusing state and tells the caller ONCE. Re-entering
// on every frame would flood the socket with error events, which is its own
// denial of service.
// It counts NOTHING. The counter used to live here, and regate calls this once
// per re-check TICK — so gateway_realtime_turns_refused_total reported the
// number of ticks that found the gate down, not the number of turns the refusal
// dropped. The drops are counted where they happen, in countDrop and gateTurn.
func (s *realtimeSession) refuseTurns(v budgetVerdict) {
	if !s.refusing.CompareAndSwap(false, true) {
		return
	}
	s.h.logger.Warn("realtime: refusing turns; the budget gate did not admit them",
		"project_id", s.projectID, "model", s.currentResponseModel().model,
		"status", v.status, "code", v.code, "metric", MetricRealtimeFramesDropped)
	s.sendRefusalEvent(v)
}

// resumeTurns leaves the refusing state.
func (s *realtimeSession) resumeTurns() {
	// A session that is ENDING never resumes. end() sets the refusal to stop
	// spend at once, and a late Allow — from a gate call that was already in
	// flight when the session was refused — must not clear it.
	//
	// The test is `ending`, NOT the session context. end() cancels LAST, after a
	// close handshake that can take realtimeCloseHandshakeBudget, so a context
	// check passes for that whole window and a late Allow would resume spend on
	// an exhausted budget. See the `ending` field for why end() cannot simply
	// cancel first instead.
	if s.ending.Load() || s.ctx.Err() != nil {
		return
	}
	if s.refusing.CompareAndSwap(true, false) {
		s.h.logger.Info("realtime: the budget gate admitted again; resuming turns",
			"project_id", s.projectID, "model", s.currentResponseModel().model)
	}
}

// realtimeErrorEvent is the mid-session refusal frame.
//
// It is a NEW contract: no elitea-sdk realtime client exists, so nothing here is
// a compatibility requirement. It is shaped as the provider's own `error` event
// so an existing client's error branch handles it without a change, and its
// error object carries the EXACT three fields the HTTP path's budget refusal
// carries — `type` is always budget_exceeded and the scope rides in `code` (see
// budgetErrorType in budget_gate.go). A future SDK reader can therefore reuse
// the one matcher it already has. See DECISIONS.md, 2026-08-20.
type realtimeErrorEvent struct {
	Type  string                   `json:"type"`
	Error realtimeErrorEventFields `json:"error"`
}

type realtimeErrorEventFields struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// Scope is the same error.scope the HTTP budget refusal carries.
	Scope string `json:"scope,omitempty"`
}

// sendRefusalEvent writes the refusal to the caller. It is best effort: a caller
// that has already gone away is about to end the session anyway.
func (s *realtimeSession) sendRefusalEvent(v budgetVerdict) {
	frame := realtimeRefusalFrameScoped(v.errType, v.code, v.message, v.scope)
	if frame == nil {
		return
	}
	if werr := s.write(s.client, frame); werr != nil {
		s.h.logger.Debug("realtime: the refusal event could not be delivered",
			"project_id", s.projectID, "err", werr)
	}
}

// realtimeRefusalFrame renders one refusal as the provider's `error` event.
func realtimeRefusalFrame(errType, code, message string) []byte {
	return realtimeRefusalFrameScoped(errType, code, message, "")
}

// realtimeRefusalFrameScoped is realtimeRefusalFrame with the gate's
// error.scope, which is empty for every refusal the gate did not decide.
func realtimeRefusalFrameScoped(errType, code, message, scope string) []byte {
	frame, err := json.Marshal(realtimeErrorEvent{
		Type:  string(schemas.RTEventError),
		Error: realtimeErrorEventFields{Type: errType, Code: code, Message: message, Scope: scope},
	})
	if err != nil {
		return nil
	}
	return frame
}

// writeRealtimeFrame sends one frame on a socket the session does not own yet.
// It is used on the ONE path that refuses AFTER the upgrade but BEFORE the pump
// starts: the provider dial failed, so there is no session and no session
// context to write under.
func writeRealtimeFrame(ctx context.Context, sock RealtimeSocket, frame []byte) {
	if frame == nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, realtimeWriteTimeout)
	defer cancel()
	_ = sock.Write(wctx, frame)
}

// billTurn bills ONE completed turn, per turn and never once per session.
//
// It calls updateUsageUnits, which calls spawnBillingGoroutine, which ALREADY
// mints a fresh uuid per call — so one call per turn already yields one event id
// per turn and spawnBillingGoroutine needs NO change for this route. Stating it
// plainly here so nobody "extends" it: one id per SESSION would make turns 2..N
// look like redeliveries to the NATS duplicate window and to
// gateway.processed_event_ids' primary key, and they would contribute nothing.
//
// streamSettler is deliberately NOT reused. It ASSIGNS the usage it sees,
// because SSE token counts are cumulative over a response. Realtime
// `response.done` usage is PER RESPONSE, so it must be summed across turns, and
// assigning would bill only the last one.
//
// Two terminal events carry usage, and they need different readers (fact F2):
//
//	response.done                 bifrost's own extractor reads it.
//	…input_audio_transcription.
//	  completed                   bifrost's extractor returns nil for it — it
//	                              only ever looks at `response.usage` — so this
//	                              file parses the event's own top-level usage.
func (s *realtimeSession) billTurn(raw []byte, ev *schemas.BifrostRealtimeEvent) {
	// The TYPE comes from the raw frame when the codec could not decode it.
	// `if ev == nil { return }` was a silent hole: a terminal frame the codec
	// choked on was forwarded to the caller, billed nowhere, and counted on NO
	// counter — neither turns_billed nor turns_unpriced. Every other path in
	// this file is explicit that a turn it cannot price must be counted.
	typ, terminal := realtimeTerminalEventType(raw, ev)
	if !terminal {
		return
	}
	var (
		units cost.Units
		ok    bool
		m     realtimeModel
	)
	switch typ {
	case schemas.RTEventResponseDone:
		m = s.currentResponseModel()
		units, ok = realtimeResponseUnits(s.up.Codec.ExtractRealtimeTurnUsage(raw))
	default: // schemas.RTEventInputAudioTransCompleted
		m = s.currentTranscriptionModel()
		units, ok = realtimeTranscriptionUnits(raw)
	}
	if !ok {
		// The turn ENDED and reported nothing this gateway can bill. It is not
		// billed as zero and no number is invented for it; it is counted, which
		// is the only honest answer and the one an operator can alarm on.
		realtimeTurnsUnpriced.Add(1)
		s.h.logger.WarnContext(s.ctx, "realtime: the turn carries no usable usage; nothing is billed for it",
			"project_id", s.projectID, "provider", m.provider, "model", m.model,
			"event", string(typ), "decoded", ev != nil, "metric", MetricRealtimeTurnsUnpriced)
		return
	}
	// DECISION H2 admitted this model on EITHER price basis, and the turn bills
	// on whatever the provider reported — so the two can disagree.
	//
	// THE TURN IS NOT BILLED WHEN THEY DO, and that is the whole point. Only the
	// seconds and character arms refuse a rate the catalogue did not supply; the
	// TOKEN arm falls back to the pylon default table like every other route. So
	// a model admitted on a per-second rate alone, reporting tokens, used to
	// reach tokenCost and bill the invented 1.0/3.0 USD-per-1M fallback. H2
	// exists because a socket the tenant holds open has no natural bound, and
	// billing it from a made-up number is exactly what H2 refuses at the
	// upgrade; letting a turn do it one frame later reinstates it.
	//
	// Refusing to bill is the honest answer: the catalogue prices this model,
	// but not in the units this turn reported, so the gateway does not know what
	// the turn cost. It is counted, never invented.
	if !m.pricing.admits(units.Basis()) {
		realtimeTurnBasisMismatch.Add(1)
		s.h.logger.ErrorContext(s.ctx, "realtime: the turn reported a basis the catalogue does not price for this model; it bills nothing",
			"project_id", s.projectID, "provider", m.provider, "model", m.model,
			"reported_basis", units.Basis(), "priced_tokens", m.pricing.tokens,
			"priced_seconds", m.pricing.seconds, "metric", MetricRealtimeTurnBasisMismatch)
		return
	}
	switch s.h.updateUsageUnits(s.ctx, surfaceRealtime, m.provider, m.model, units, s.projectID, s.userID) {
	case billBilled:
		realtimeTurnsBilled.Add(1)
	case billRefused:
		// Real, provider-reported spend was DROPPED because billing is already
		// draining. streamSettler treats the identical condition as alarmable,
		// and a session is not different: the tenant used the model and nothing
		// charged for it. The event is the same one, with the drain outcome
		// naming this route, so the existing alarm covers both.
		realtimeTurnsUnbilled.Add(1)
		s.h.logger.Warn("realtime: the turn's usage was recovered but the billing increment was refused; spend dropped",
			"project_id", s.projectID, "provider", m.provider, "model", m.model,
			"event", string(typ), "metric", MetricRealtimeTurnsUnbilled)
		s.h.publishUnbilledStreamEvent(s.projectID, m.provider, m.model,
			lossReasonBillingRefused, realtimeDrainOutcome, int64(len(raw)))
	case billNotBillable:
	}
}

// realtimeDrainOutcome is the `drain_outcome` dimension a realtime turn puts on
// budget.unbilled_stream. The reason set stays the CLOSED set stream_drain.go
// declares; only the outcome says which surface lost the spend.
const realtimeDrainOutcome = "realtime_turn"

// realtimeTerminalEventType reports whether a provider frame ends a turn, and
// which of the two terminal events it is.
//
// The decoded envelope is preferred, because a provider's own event name is
// translated into the canonical one by the codec. When the codec could not
// decode the frame the RAW `type` is read instead: an undecodable terminal
// frame is still a turn, ExtractRealtimeTurnUsage takes the raw bytes anyway,
// and a turn that ends must never leave every counter still.
func realtimeTerminalEventType(raw []byte, ev *schemas.BifrostRealtimeEvent) (schemas.RealtimeEventType, bool) {
	typ := schemas.RealtimeEventType("")
	if ev != nil {
		typ = ev.Type
	} else {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return "", false
		}
		typ = schemas.RealtimeEventType(env.Type)
	}
	switch typ {
	case schemas.RTEventResponseDone, schemas.RTEventInputAudioTransCompleted:
		return typ, true
	default:
		return typ, false
	}
}

// realtimeResponseUnits converts a `response.done` usage into billable units.
//
// FACT F2, and the reason this does not test for nil. bifrost's extractor
// returns a NON-NIL, ALL-ZERO BifrostLLMUsage for a usage envelope whose shape
// it does not understand — a duration-shaped one, for instance. `if u != nil {
// bill(u) }` would then bill zero and report success, which is the exact shape
// of a silent under-bill. The test is on the QUANTITY, never on the pointer.
func realtimeResponseUnits(u *schemas.BifrostLLMUsage) (cost.Units, bool) {
	if u == nil {
		return cost.Units{}, false
	}
	in, out := int64(u.PromptTokens), int64(u.CompletionTokens)
	if in <= 0 && out <= 0 {
		return cost.Units{}, false
	}
	return cost.Units{InputTokens: in, OutputTokens: out}, true
}

// realtimeTranscriptionUsage is the top-level `usage` object a transcription
// event carries. It is NOT nested under `response`, which is precisely why
// bifrost's extractor cannot see it.
type realtimeTranscriptionUsage struct {
	Usage *struct {
		Type         string   `json:"type"`
		InputTokens  int64    `json:"input_tokens"`
		OutputTokens int64    `json:"output_tokens"`
		TotalTokens  int64    `json:"total_tokens"`
		Seconds      *float64 `json:"seconds"`
	} `json:"usage"`
}

// realtimeTranscriptionUnits reads the usage off a transcription-completed
// event. The API reports one of two shapes, and both are handled:
//
//	{"type":"tokens","input_tokens":N,"output_tokens":M,…}
//	{"type":"duration","seconds":3.2}
//
// A `total_tokens`-only envelope is treated as UNPRICED on purpose. Input and
// output are priced at different rates, so splitting a total between them is a
// number the gateway would have made up, and no made-up number may reach the
// authoritative counter.
func realtimeTranscriptionUnits(raw []byte) (cost.Units, bool) {
	var env realtimeTranscriptionUsage
	if err := json.Unmarshal(raw, &env); err != nil || env.Usage == nil {
		return cost.Units{}, false
	}
	if env.Usage.InputTokens > 0 || env.Usage.OutputTokens > 0 {
		return cost.Units{InputTokens: env.Usage.InputTokens, OutputTokens: env.Usage.OutputTokens}, true
	}
	if env.Usage.Seconds != nil {
		// secondsToMillis is the ONE crossing from the provider's fractional
		// second count to the int64 the money path requires (audio.go). It
		// refuses NaN, either infinity, a non-positive value, an absurd one, and
		// a duration that rounds to zero milliseconds.
		if millis, ok := secondsToMillis(*env.Usage.Seconds); ok {
			return cost.Units{InputMillis: millis}, true
		}
	}
	return cost.Units{}, false
}

// trackSession registers a session with the shutdown group, atomically with
// respect to CloseRealtimeSessions' Wait. It is the same Add-after-Wait guard
// trackDrain uses, on a SEPARATE group: see CloseRealtimeSessions for why the
// two must not share one.
func (h *Handler) trackSession() bool {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if h.sessionWaiting {
		return false
	}
	h.sessionWg.Add(1)
	return true
}

// CloseRealtimeSessions ends every live realtime session and waits for them,
// bounded by ctx.
//
// IT IS NOT A STREAM DRAIN, and it deliberately does not reuse drainWg,
// drainClosing or StopStreamGrace. Those three mean "stop waiting for a usage
// trailer on a response that already finished", their shutdown cut is one
// second, and their pool is sized for abandoned SSE streams. A live call is none
// of those things: it has a caller on the other end, its close budget is a close
// budget and not a trailer budget, and sharing the pool would make sessions
// compete for slots with streams nobody is reading any more.
//
// WHERE IT SITS IN THE SEQUENCE, and why:
//
//	ShutdownHTTP → StopStreamGrace → CloseRealtimeSessions → DrainBilling →
//	govStore.Drain → Close
//
// AFTER ShutdownHTTP because http.Server.Shutdown neither closes nor waits for a
// hijacked connection, so this is the only thing that ends a session at all.
// BEFORE DrainBilling for the same reason a recovered stream trailer is billed
// before it: a session's LAST turn spawns a billing goroutine as it closes, and
// that goroutine needs billing open and NATS live. Move this after DrainBilling
// and every session's final turn is refused with billing_refused on every
// rolling deploy.
func (h *Handler) CloseRealtimeSessions(ctx context.Context) {
	h.sessionClosingOnce.Do(func() {
		if h.sessionClosing != nil {
			close(h.sessionClosing)
		}
	})
	h.sessionMu.Lock()
	h.sessionWaiting = true
	h.sessionMu.Unlock()

	done := make(chan struct{})
	go func() {
		h.sessionWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// A session that will not settle must not hold the pod's termination
		// grace hostage. Its billing goroutines are already spawned per turn, so
		// what is abandoned here is the socket, not a pending increment.
		//
		// Reaching this branch is a REAL loss and not merely untidy: the next
		// shutdown step closes billing, so a turn that settles after this point
		// is refused with billing_refused. wsSocket.Close bounds the per-session
		// close so an unresponsive peer cannot spend the whole budget on its
		// own; if this still fires, sessions are wedged somewhere else.
		h.logger.Warn("realtime: sessions did not all close within the shutdown budget; continuing",
			"budget", RealtimeCloseTimeout)
	}
}

// wsSocket adapts github.com/coder/websocket to RealtimeSocket. Both the
// caller's socket and the provider's use it.
type wsSocket struct {
	conn *websocket.Conn
}

func (s *wsSocket) Read(ctx context.Context) ([]byte, error) {
	_, frame, err := s.conn.Read(ctx)
	return frame, err
}

func (s *wsSocket) Write(ctx context.Context, frame []byte) error {
	// Realtime events are JSON text on every provider this route serves.
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

func (s *wsSocket) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

func (s *wsSocket) Close(code RealtimeCloseCode, reason string) {
	// RFC 6455 caps the close reason at 123 BYTES and requires it to be valid
	// UTF-8; the library refuses a reason that breaks either rule, which turns
	// the tidy close this function exists to perform into an abrupt one.
	//
	// The cut is therefore taken back to a rune boundary. The reason carries
	// caller-influenced text — a client frame naming an unknown model reaches
	// writeModelNotFound, whose message embeds that name, and it arrives here as
	// v.message — so a name in any multi-byte script could otherwise put the
	// 120th byte in the middle of a rune and produce exactly the abrupt close
	// the truncation was added to avoid.
	if len(reason) > maxRealtimeCloseReasonBytes {
		cut := maxRealtimeCloseReasonBytes
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	// The library's Close writes the close FRAME and then waits, for a
	// HARDCODED 5 s, to read the peer's reply — and it needs the connection's
	// readMu, which the pump holds while it is parked in Read. A caller that
	// stops reading therefore costs the full 5 s, per session, out of
	// CloseRealtimeSessions' whole budget. Measured before this bound: one
	// silent peer burned 5.001 s of a 5 s budget, and the shutdown wait
	// returned with the session still live, so the billing drain ran while that
	// session's last turn was in flight.
	//
	// Waiting on our own timer instead does NOT skip the frame: the frame is
	// written on the first step, before the wait. The abandoned goroutine ends
	// on the library's own 5 s bound, and the cancel that follows a close
	// closes the connection under it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.conn.Close(websocket.StatusCode(code), reason)
	}()
	timer := time.NewTimer(realtimeCloseHandshakeBudget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// realtimeSessionLimit builds the session pool. It is separate from the stream
// drain pool on purpose; see CloseRealtimeSessions.
func realtimeSessionLimit(n int) *slotLimiter {
	return newSlotLimiter(n, realtimeSessionPerProjectDivisor)
}

// compile-time proof that the socket adapter keeps its shape.
var _ RealtimeSocket = (*wsSocket)(nil)
