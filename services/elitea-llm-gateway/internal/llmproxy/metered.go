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
// SIGNED, STATELESS admission: an HMAC over the identity, the operation and the
// price, valid for ten minutes. No replica stores it, and a settle can name no
// price, scope or execution its admission did not.
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
}

type meteredSettleResponse struct {
	BilledNanoUSD int64 `json:"billed_nano_usd"`
	// Duplicate is always false: the bill is handed to the billing goroutine,
	// and whether a dedup layer collapsed it is known only there. A retried
	// settle is safe either way.
	Duplicate bool `json:"duplicate"`
}

// meteredAdmission is the signed content of an admission id.
type meteredAdmission struct {
	Version     int    `json:"v"`
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
func meteredAdmissionKey(identitySecret []byte) []byte {
	if len(identitySecret) == 0 {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		return key
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
	admission := h.signAdmission(meteredAdmission{
		Version:     1,
		ProjectID:   identityProjectFromCtx(ctx),
		UserID:      identityUserFromCtx(ctx),
		ExecutionID: identityExecutionFromCtx(ctx),
		Provider:    req.Provider,
		Operation:   req.Operation,
		Unit:        price.Unit,
		NanoPerUnit: price.NanoUSDPerUnit,
		ExpiresUnix: time.Now().Add(meteredAdmissionTTL).Unix(),
	})
	writeJSON(w, http.StatusOK, meteredAdmitResponse{
		AdmissionID:     admission,
		Price:           meteredPriceWire{Unit: price.Unit, NanoUSDPerUnit: price.NanoUSDPerUnit},
		EstimateNanoUSD: estimate.TotalNanoUSD,
	})
}

// MeteredSettle handles POST /llm/v1/metered/settle.
func (h *Handler) MeteredSettle(w http.ResponseWriter, r *http.Request) {
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

	switch h.billMetered(ctx, req.Provider, req.Operation, billed, eventID.String()) {
	case billRefused:
		// The gateway is draining: nothing was billed, and the worker's retry
		// with the same event id lands on a replica that is not.
		writeBudgetRefusal(w, budgetEnforcementPendingVerdict)
		return
	case billNotBillable:
		// No budget gate on this deployment: the operation was admitted
		// ungoverned, exactly like a model call there, and nothing was billed.
		enrichment.SetMetered(billed.Units, price.Unit, 0)
		writeJSON(w, http.StatusOK, meteredSettleResponse{})
		return
	}
	enrichment.SetMetered(billed.Units, price.Unit, billed.TotalNanoUSD)
	writeJSON(w, http.StatusOK, meteredSettleResponse{BilledNanoUSD: billed.TotalNanoUSD})
}

// billableMeteredUnits are the units a settlement bills. A provider error that
// reported no units bills the provider's documented failure charge: Firecrawl
// charges a credit for a scraped page even when the page answered 403 or 404.
// Every other failure with no units bills nothing.
func billableMeteredUnits(req meteredSettleRequest) cost.MeteredUnits {
	units := req.Units
	reported := units.Requests > 0 || units.Credits > 0 || units.Pages > 0
	if req.Outcome == meteredOutcomeProviderError && !reported &&
		req.Provider == "firecrawl" && req.Operation == "web_fetch" {
		units.Pages = 1
	}
	return units
}

// billMetered bills one priced metered operation through the model billing
// path. A zero-priced operation is billed too: it still writes its ledger row
// and moves the counter by zero, which is what makes a free operation visible
// in Usage and analytics (the model path's zero-rate-metered branch).
func (h *Handler) billMetered(ctx context.Context, provider, operation string, billed cost.MeteredCost, eventID string) billOutcome {
	unpin := failmode.HoldsFrom(ctx).Pin()
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
