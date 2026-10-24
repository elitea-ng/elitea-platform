package llmproxy

// metered.go — the metered-call API (#1238): POST /llm/v1/metered/admit and
// POST /llm/v1/metered/settle.
//
// WHAT IT IS FOR. The agent worker's web family (web search, page fetch,
// host-intel queries) spends money at third-party vendors. That spend goes
// through the SAME budget engine as model calls: the same project and member
// ceilings, NATS counters, fail-mode FSM, write-behind ledger, refusal bodies
// and Usage screens. The worker never decides budget and never writes to NATS;
// it asks here before the provider is called, and reports the units afterwards.
//
// WHO IS BILLED. Identity comes from elitea-main's signed X-Elitea-* tuple,
// exactly as for a model call; the edge refuses a metered call whose execution
// id it does not admit. The worker states only units, never a scope, a member,
// an execution or a price: the price is this gateway's table row
// (gateway.metered_operation_prices, internal/cost/metered.go), and an
// operation with no row is refused before the provider is called. There is no
// default price and no silent zero.
//
// ADMIT runs admissionVerdictFor unchanged under the model name
// "<provider>/<operation>": the loop breaker, the authored per-minute rate
// ceilings (so an operator rate-limits `tavily/web_search` with an ordinary
// governance rate row), then the project and member ceilings. Its answer is a
// SIGNED, STATELESS admission: an HMAC over the identity, the operation, the
// price and a nonce, valid for ten minutes. No replica needs it to verify a
// settle, and a settle can name no price, scope or execution its admission did
// not. The signing key is derived from GATEWAY_IDENTITY_SECRET, which every
// replica shares; a gateway without that secret refuses every metered call
// (503 metered_signing_unconfigured) rather than sign with a key no other
// replica knows.
//
// A NATS-DOWN ADMISSION RESERVES. In a degraded state the admission's
// CheckBudget reserves against this replica's share (failmode.Holds). The
// provider is called AFTER the admit handler returns, so the request's own pin
// would release that hold before any spend is on the counter. The admit
// therefore pins the holds itself (meteredHolds) until the settle of that
// admission has put its cost on the counter, or until the admission expires
// when no settle comes to this replica.
//
// SETTLE verifies the admission against the settle request's own signed
// identity, prices the units at the admitted price in integers, and bills
// through spawnBillingGoroutine with the CALLER's event id (the member scope
// gets a UUIDv5 of it). A retried settle re-sends the same id, and every dedup
// layer collapses it: the NATS Nats-Msg-Id window, processed_event_ids, and the
// ledger's event_id key. An admission that is never settled bills nothing —
// the same exposure as a model response whose billing goroutine is skipped on
// drain.
//
// The admission's healthy-path refusal is the model rule: spend >= ceiling.
// admissionVerdictFor passes no estimate to CheckBudget, as for model calls,
// so the estimate the worker sends is priced and echoed but does not tighten
// the degraded per-replica cap.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// MeteredPricer prices a metered tool operation. *cost.Calculator implements
// it.
type MeteredPricer interface {
	MeteredPrice(ctx context.Context, provider, operation string) (cost.MeteredPrice, bool)
}

// WithMeteredPrices sets the pricer the metered-call API uses when the budget
// plane's calculator is absent (a gateway with a database and no NATS).
func WithMeteredPrices(p MeteredPricer) HandlerOption {
	return func(h *Handler) { h.meteredPrices = p }
}

// meteredAdmissionTTL is how long an admission may be settled. It sits inside
// the NATS dedup window (at least 12 minutes), so a settle retried until the
// admission expires is still deduplicated on the counter.
const meteredAdmissionTTL = 10 * time.Minute

// meteredLedgerLookupTimeout bounds the duplicate lookup on the usage ledger.
// The lookup only decides whether the settle answer says "duplicate"; a slow
// database must not hold the settle.
const meteredLedgerLookupTimeout = 2 * time.Second

// maxMeteredBody caps an admit or settle body; both are a few hundred bytes.
const maxMeteredBody = 16 << 10

// meteredName is the shape of a provider or operation name: what the price
// table's VARCHAR(64) key holds.
var meteredName = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// The settle outcomes a worker reports.
const (
	meteredOutcomeOK              = "ok"
	meteredOutcomeProviderError   = "provider_error"
	meteredOutcomeRefusedUpstream = "refused_upstream"
)

// meteredFailureTargetFailed is the one failure class of a provider_error
// settle that the provider charges for: the provider reached the target, and
// the target answered an error (a 404 page). The worker also reports
// not_reached (no I/O: an egress or URL refusal), refused (the provider
// refused the call: key, rate limit, request shape) and failed (timeout,
// network, 5xx, unreadable answer); none of those bills a failure charge.
const meteredFailureTargetFailed = "target_failed"

type meteredAdmitRequest struct {
	Operation string            `json:"operation"`
	Provider  string            `json:"provider"`
	Estimate  cost.MeteredUnits `json:"estimate"`
}

type meteredPriceWire struct {
	Unit           string `json:"unit"`
	NanoUSDPerUnit int64  `json:"nano_usd_per_unit"`
}

type meteredAdmitResponse struct {
	AdmissionID     string           `json:"admission_id"`
	Price           meteredPriceWire `json:"price"`
	EstimateNanoUSD int64            `json:"estimate_nano_usd"`
}

type meteredSettleRequest struct {
	AdmissionID string            `json:"admission_id"`
	EventID     string            `json:"event_id"`
	Operation   string            `json:"operation"`
	Provider    string            `json:"provider"`
	Units       cost.MeteredUnits `json:"units"`
	Outcome     string            `json:"outcome"`
	// Failure classes a provider_error (meteredFailureTargetFailed). Absent
	// or unknown, no failure charge is billed.
	Failure string `json:"failure,omitempty"`
}

type meteredSettleResponse struct {
	BilledNanoUSD int64 `json:"billed_nano_usd"`
	// Duplicate is true when this event id was already settled: by this
	// replica within the admission's lifetime, or on the usage ledger. The
	// bill is handed off again either way (every dedup layer collapses it),
	// and the edge then writes no second audit row. False is not a proof of
	// a first settle: a retry that reaches another replica before the first
	// settle's ledger row is written reads false.
	Duplicate bool `json:"duplicate"`
}

// meteredAdmission is the signed content of an admission id.
type meteredAdmission struct {
	Version int `json:"v"`
	// ID is a random nonce: two admissions of the same operation in the same
	// second are distinct, and it keys this replica's held reservation.
	ID          string `json:"id"`
	ProjectID   string `json:"p"`
	UserID      string `json:"u,omitempty"`
	ExecutionID string `json:"x"`
	Provider    string `json:"pr"`
	Operation   string `json:"op"`
	Unit        string `json:"unit"`
	NanoPerUnit int64  `json:"n"`
	ExpiresUnix int64  `json:"exp"`
}

// meteredAdmissionKey derives the admission signing key. It is a separate key
// from the identity secret (an HMAC of a fixed label under it), so an
// admission can never be presented as an identity signature or the reverse.
// With no identity secret it returns nil, and the metered routes refuse: a
// per-process key would sign admissions that no other replica can verify, so
// a settle that lands elsewhere would fail for a call the provider already
// billed.
func meteredAdmissionKey(identitySecret []byte) []byte {
	if len(identitySecret) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, identitySecret)
	mac.Write([]byte("elitea-metered-admission/v1"))
	return mac.Sum(nil)
}

func (h *Handler) signAdmission(a meteredAdmission) string {
	payload, _ := json.Marshal(a)
	mac := hmac.New(sha256.New, h.admissionKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// openAdmission verifies an admission id and returns its content. ok is false
// for a malformed, forged or expired one.
func (h *Handler) openAdmission(id string, now time.Time) (meteredAdmission, bool) {
	encoded, signature, found := strings.Cut(id, ".")
	if !found || len(id) > 2048 {
		return meteredAdmission{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return meteredAdmission{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return meteredAdmission{}, false
	}
	mac := hmac.New(sha256.New, h.admissionKey)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(mac.Sum(nil), got) != 1 {
		return meteredAdmission{}, false
	}
	var a meteredAdmission
	if json.Unmarshal(payload, &a) != nil || a.Version != 1 || now.Unix() > a.ExpiresUnix {
		return meteredAdmission{}, false
	}
	return a, true
}

// meteredSigningReady refuses a metered call when this gateway has no shared
// admission key. It writes the refusal and returns false.
func (h *Handler) meteredSigningReady(w http.ResponseWriter) bool {
	if len(h.admissionKey) != 0 {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "service_unavailable",
		"metered calls need GATEWAY_IDENTITY_SECRET, which signs admissions every replica can verify",
		"metered_signing_unconfigured")
	return false
}

// newAdmissionNonce returns 16 random bytes, base64url.
func newAdmissionNonce() string {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(nonce)
}

// meteredPricer is the pricer for this request: the budget plane's
// calculator when it prices metered operations, else the configured one.
func (h *Handler) meteredPricer(bp *budgetPlane) MeteredPricer {
	if pricer, ok := bp.calc.(MeteredPricer); ok && pricer != nil {
		return pricer
	}
	return h.meteredPrices
}

// meteredContext verifies the signed identity and the body, and returns the
// request context. It writes the refusal and returns false on any failure.
func (h *Handler) meteredContext(w http.ResponseWriter, r *http.Request, dst any) (context.Context, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMeteredBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid metered request body", "invalid_request")
		return nil, false
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return nil, false
	}
	// The edge already refuses a metered call without an admitted execution
	// id; this is the gateway's own check that the tuple it verified names a
	// project and a run, because tool spend with neither has nowhere to land.
	if identityProjectFromCtx(ctx) == "" || identityExecutionFromCtx(ctx) == "" {
		writeError(w, http.StatusForbidden, "permission_error",
			"a metered call needs a project and an execution", "execution_not_attributable")
		return nil, false
	}
	return ctx, true
}

// MeteredAdmit handles POST /llm/v1/metered/admit.
func (h *Handler) MeteredAdmit(w http.ResponseWriter, r *http.Request) {
	if !h.meteredSigningReady(w) {
		return
	}
	var req meteredAdmitRequest
	ctx, ok := h.meteredContext(w, r, &req)
	if !ok {
		return
	}
	if !meteredName.MatchString(req.Provider) || !meteredName.MatchString(req.Operation) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid metered provider or operation", "invalid_request")
		return
	}
	requestlog.FromContext(r.Context()).SetModel(req.Provider, req.Operation)

	bp := h.budget()
	pricer := h.meteredPricer(bp)
	if pricer == nil {
		writeMeteredUnpriced(w)
		return
	}
	price, priced := pricer.MeteredPrice(ctx, req.Provider, req.Operation)
	if !priced {
		writeMeteredUnpriced(w)
		return
	}

	// The same admission question a model call asks, under the model name an
	// operator writes a rate row for.
	if v := h.admissionVerdict(ctx, req.Provider+"/"+req.Operation); !v.allow {
		if v.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(v.retryAfter/time.Second)+1, 10))
		}
		writeBudgetRefusal(w, v)
		return
	}

	estimate, ok := cost.PriceMetered(price, req.Estimate)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the metered estimate is out of range", "invalid_request")
		return
	}
	expires := time.Now().Add(meteredAdmissionTTL)
	nonce := newAdmissionNonce()
	// A degraded-mode reservation of this admission must outlive the
	// handler: the provider is called after it returns (see the file header).
	if holds := failmode.HoldsFrom(ctx); holds.Len() > 0 {
		h.meteredHolds.keep(nonce, holds.Pin(), time.Until(expires))
	}
	admission := h.signAdmission(meteredAdmission{
		Version:     1,
		ID:          nonce,
		ProjectID:   identityProjectFromCtx(ctx),
		UserID:      identityUserFromCtx(ctx),
		ExecutionID: identityExecutionFromCtx(ctx),
		Provider:    req.Provider,
		Operation:   req.Operation,
		Unit:        price.Unit,
		NanoPerUnit: price.NanoUSDPerUnit,
		ExpiresUnix: expires.Unix(),
	})
	writeJSON(w, http.StatusOK, meteredAdmitResponse{
		AdmissionID:     admission,
		Price:           meteredPriceWire{Unit: price.Unit, NanoUSDPerUnit: price.NanoUSDPerUnit},
		EstimateNanoUSD: estimate.TotalNanoUSD,
	})
}

// MeteredSettle handles POST /llm/v1/metered/settle.
func (h *Handler) MeteredSettle(w http.ResponseWriter, r *http.Request) {
	if !h.meteredSigningReady(w) {
		return
	}
	var req meteredSettleRequest
	ctx, ok := h.meteredContext(w, r, &req)
	if !ok {
		return
	}
	eventID, err := uuid.Parse(req.EventID)
	if err != nil || req.EventID != eventID.String() {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "event_id must be a canonical UUID", "invalid_request")
		return
	}
	switch req.Outcome {
	case meteredOutcomeOK, meteredOutcomeProviderError, meteredOutcomeRefusedUpstream:
	default:
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown metered outcome", "invalid_request")
		return
	}
	admission, ok := h.openAdmission(req.AdmissionID, time.Now())
	if !ok || admission.ProjectID != identityProjectFromCtx(ctx) ||
		admission.UserID != identityUserFromCtx(ctx) ||
		admission.ExecutionID != identityExecutionFromCtx(ctx) ||
		admission.Provider != req.Provider || admission.Operation != req.Operation {
		writeError(w, http.StatusForbidden, "permission_error",
			"the admission is not valid for this settlement", "metered_admission_invalid")
		return
	}

	units := billableMeteredUnits(req)
	price := cost.MeteredPrice{Unit: admission.Unit, NanoUSDPerUnit: admission.NanoPerUnit}
	billed, ok := cost.PriceMetered(price, units)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the metered units are out of range", "invalid_request")
		return
	}
	enrichment := requestlog.FromContext(r.Context())
	enrichment.SetModel(req.Provider, req.Operation)

	duplicate := h.meteredEventSettled(ctx, eventID.String())
	// The admit's held reservation (if this replica holds one) goes once the
	// cost is on the counter, or at once when nothing is billed here.
	release := h.meteredHolds.take(admission.ID)
	switch h.billMetered(ctx, req.Provider, req.Operation, billed, eventID.String(), release) {
	case billRefused:
		// The gateway is draining: nothing was billed, and the worker's retry
		// with the same event id lands on a replica that is not.
		writeBudgetRefusal(w, budgetEnforcementPendingVerdict)
		return
	case billNotBillable:
		// No budget gate on this deployment: the operation was admitted
		// ungoverned, exactly like a model call there, and nothing was billed.
		h.meteredSettled.mark(eventID.String(), time.Now())
		enrichment.SetMetered(billed.Units, price.Unit, 0)
		writeJSON(w, http.StatusOK, meteredSettleResponse{Duplicate: duplicate})
		return
	}
	h.meteredSettled.mark(eventID.String(), time.Now())
	enrichment.SetMetered(billed.Units, price.Unit, billed.TotalNanoUSD)
	writeJSON(w, http.StatusOK, meteredSettleResponse{BilledNanoUSD: billed.TotalNanoUSD, Duplicate: duplicate})
}

// meteredEventSettled reports whether eventID was already settled: by this
// replica within the admission's lifetime, else on the usage ledger. A ledger
// error reads as "not settled": the answer only steers the edge's audit row,
// and the bill is idempotent either way.
func (h *Handler) meteredEventSettled(ctx context.Context, eventID string) bool {
	if h.meteredSettled.seen(eventID, time.Now()) {
		return true
	}
	if h.meteredLedger == nil {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), meteredLedgerLookupTimeout)
	defer cancel()
	settled, err := h.meteredLedger.MeteredEventBilled(lookupCtx, eventID)
	if err != nil {
		h.logger.Warn("metered settle: the usage ledger lookup failed; the settle is answered as new",
			"event_id", eventID, "err", err)
		return false
	}
	return settled
}

// billableMeteredUnits are the units a settlement bills. A provider error that
// reported no units bills the provider's documented failure charge only when
// the provider reached the target and charges for the outcome
// (meteredFailureTargetFailed): Firecrawl charges a page for a scrape whose
// page answered 404. A failure before any I/O (an egress refusal), a connect
// failure, a timeout, or a call the provider refused (401, 429) bills nothing.
func billableMeteredUnits(req meteredSettleRequest) cost.MeteredUnits {
	units := req.Units
	reported := units.Requests > 0 || units.Credits > 0 || units.Pages > 0
	if req.Outcome == meteredOutcomeProviderError && !reported &&
		req.Failure == meteredFailureTargetFailed &&
		req.Provider == "firecrawl" && req.Operation == "web_fetch" {
		units.Pages = 1
	}
	return units
}

// billMetered bills one priced metered operation through the model billing
// path. A zero-priced operation is billed too: it still writes its ledger row
// and moves the counter by zero, which is what makes a free operation visible
// in Usage and analytics (the model path's zero-rate-metered branch).
//
// release drops the admission's held reservation: after the cost is on the
// counter when the bill is handed off, at once otherwise.
func (h *Handler) billMetered(ctx context.Context, provider, operation string, billed cost.MeteredCost, eventID string, release func()) billOutcome {
	settlePin := failmode.HoldsFrom(ctx).Pin()
	unpin := func() {
		settlePin()
		release()
	}
	handed := false
	defer func() {
		if !handed {
			unpin()
		}
	}()
	bp := h.budget()
	if bp.gate == nil {
		return billNotBillable
	}
	projectID := identityProjectFromCtx(ctx)
	pid := parseProjectID(projectID)
	if pid < 0 {
		return billNotBillable
	}
	userID := identityUserFromCtx(ctx)
	now := time.Now()
	dims := &failmode.UsageDimensions{
		UserID:         optionalUserID(userID),
		Provider:       provider,
		Model:          operation,
		OccurredAtUnix: now.Unix(),
		ExecutionID:    identityExecutionFromCtx(ctx),
		Operation:      failmode.UsageOperationTool,
		Units:          billed.Units,
		Unit:           billed.Price.Unit,
	}
	if h.spawnBillingGoroutine(bp, pid, userID, billingPeriodStart(now), billingPeriodEnd(now),
		billed.TotalNanoUSD, dims, unpin, eventID) {
		handed = true
		return billBilled
	}
	return billRefused
}

// writeMeteredUnpriced refuses an operation the price table does not price.
func writeMeteredUnpriced(w http.ResponseWriter) {
	writeError(w, http.StatusUnprocessableEntity, "invalid_request_error",
		"this metered operation has no price; an administrator must set one", "metered_operation_unpriced")
}

// meteredHolds keeps the degraded-mode reservations of admissions until their
// settle, keyed by the admission nonce (see the file header). A reservation
// whose settle never reaches this replica is released when its admission
// expires; until then it bounds this replica's share, which over-counts but
// never lets the fleet spend past the cap.
type meteredHolds struct {
	mu   sync.Mutex
	held map[string]*meteredHold
}

type meteredHold struct {
	unpin func()
	timer *time.Timer
}

func newMeteredHolds() *meteredHolds {
	return &meteredHolds{held: map[string]*meteredHold{}}
}

// keep holds unpin until take(id) or ttl, whichever comes first.
func (m *meteredHolds) keep(id string, unpin func(), ttl time.Duration) {
	if m == nil {
		unpin()
		return
	}
	entry := &meteredHold{unpin: unpin}
	m.mu.Lock()
	defer m.mu.Unlock()
	if previous := m.held[id]; previous != nil {
		previous.timer.Stop()
		previous.unpin()
	}
	m.held[id] = entry
	entry.timer = time.AfterFunc(ttl, func() {
		m.mu.Lock()
		if m.held[id] == entry {
			delete(m.held, id)
		}
		m.mu.Unlock()
		entry.unpin()
	})
}

// take removes id's hold and returns its release, or a no-op when this
// replica holds none for it. The release is idempotent.
func (m *meteredHolds) take(id string) func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	entry := m.held[id]
	delete(m.held, id)
	m.mu.Unlock()
	if entry == nil {
		return func() {}
	}
	entry.timer.Stop()
	var once sync.Once
	return func() { once.Do(entry.unpin) }
}

// len reports how many admissions hold a reservation (tests).
func (m *meteredHolds) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}

// meteredSettledTTL is how long this replica remembers a settled event id: an
// admission cannot be settled after it expires, so a retry never comes later.
const meteredSettledTTL = meteredAdmissionTTL + time.Minute

// meteredSettledSweepAt is the size at which mark sweeps expired entries.
const meteredSettledSweepAt = 4096

// meteredSettledSet remembers the event ids this replica settled within the
// admission lifetime, for the settle answer's duplicate flag.
type meteredSettledSet struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newMeteredSettledSet() *meteredSettledSet {
	return &meteredSettledSet{at: map[string]time.Time{}}
}

func (s *meteredSettledSet) mark(eventID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.at) >= meteredSettledSweepAt {
		for id, at := range s.at {
			if now.Sub(at) > meteredSettledTTL {
				delete(s.at, id)
			}
		}
	}
	s.at[eventID] = now
}

func (s *meteredSettledSet) seen(eventID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.at[eventID]
	return ok && now.Sub(at) <= meteredSettledTTL
}

// MeteredLedger answers whether a metered event id is on the usage ledger
// (gateway.llm_usage_events, keyed by event_id).
type MeteredLedger interface {
	MeteredEventBilled(ctx context.Context, eventID string) (bool, error)
}

// WithMeteredLedger sets the ledger the settle reads its duplicate flag from,
// beside this replica's own memory. nil leaves only that memory.
func WithMeteredLedger(l MeteredLedger) HandlerOption {
	return func(h *Handler) { h.meteredLedger = l }
}
