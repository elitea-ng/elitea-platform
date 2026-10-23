// Package preflight provides a shared hermetic test harness for the gateway's
// pre-flight admission gates. It is NOT a _test.go file — it is designed to be
// imported BY tests in other packages so each test suite can build a fully-wired
// in-process fixture without a live NATS server, a real Postgres database, or
// external provider credentials.
//
// Architecture:
//   - MockRouter implements llmproxy.LLMRouter — a configurable stub that can
//     stream N chunks (with optional inter-chunk delay) or return a canned unary
//     response, and records whether it was called.
//   - SignRequest applies the HMAC-signed identity headers so the mounted handler
//     accepts the forged request.
//   - NewSeededGovernance builds a real GovernanceStore over in-memory fakes
//     pre-seeded to a chosen spend vs. limit level.
//   - MountedHandler wires the handler, governance store, and router through the
//     real api.NewRouter chi mux, ready for httptest.
//   - StaticLegacyModels returns a fixed legacy /v1/models id set for parity
//     fixtures.
//
// All money values are int64 nano-USD (NanoUSD = 1e9).
package preflight

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/governance"
	natspkg "github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/maximhq/bifrost/core/schemas"
)

// ── MockRouter ────────────────────────────────────────────────────────────────

// StreamMode controls which OpenAI/Anthropic SSE dialect MockRouter uses when
// streaming.
type StreamMode int

const (
	// StreamModeOpenAI produces BifrostStreamChunk.BifrostChatResponse chunks.
	StreamModeOpenAI StreamMode = iota
	// StreamModeAnthropic produces BifrostStreamChunk.BifrostResponsesStreamResponse
	// chunks (the Anthropic /v1/messages streaming path).
	StreamModeAnthropic
)

// MockRouterConfig configures the behaviour of a MockRouter.
type MockRouterConfig struct {
	// Chunks is the number of content-carrying stream chunks to emit before the
	// final usage-bearing chunk. A value of 0 uses the default of 2.
	Chunks int
	// ChunkDelay is the inter-chunk delay. 0 means no delay (default for tests
	// that only need framing correctness; set a short duration, e.g. 5ms, to
	// exercise incremental flushing).
	ChunkDelay time.Duration
	// Mode selects the SSE dialect (OpenAI or Anthropic). Default is OpenAI.
	Mode StreamMode
	// UnaryResponse overrides the object returned by unary (non-streaming)
	// ChatCompletionRequest / ResponsesRequest calls. When nil, a minimal
	// response with ID="mock-resp" and Usage={10,5} is returned.
	UnaryResponse *schemas.BifrostChatResponse
	// UnaryResponsesResponse overrides the object returned by unary
	// ResponsesRequest / CountTokensRequest calls when Mode==StreamModeAnthropic.
	UnaryResponsesResponse *schemas.BifrostResponsesResponse
	// EmbeddingResponse overrides the object returned by EmbeddingRequest. The
	// default carries Usage and NO Data, which is enough for a billing
	// assertion but not for a client: openai-python reads data[0].embedding and
	// raises on an empty list. Set this to drive a real embedding round trip.
	EmbeddingResponse *schemas.BifrostEmbeddingResponse
	// InputTokens / OutputTokens are the usage values stamped on the final chunk
	// and the unary response. Defaults are 100 / 50.
	InputTokens  int64
	OutputTokens int64
}

// MockRouter is a hermetic test double for llmproxy.LLMRouter. It satisfies the
// full LLMRouter interface and is safe for concurrent use across subtests.
type MockRouter struct {
	cfg    MockRouterConfig
	called atomic.Bool
}

// NewMockRouter returns a MockRouter configured by cfg. Unset fields use safe
// defaults (2 OpenAI-dialect chunks, no delay, usage 100 in / 50 out).
func NewMockRouter(cfg MockRouterConfig) *MockRouter {
	if cfg.Chunks <= 0 {
		cfg.Chunks = 2
	}
	if cfg.InputTokens <= 0 {
		cfg.InputTokens = 100
	}
	if cfg.OutputTokens <= 0 {
		cfg.OutputTokens = 50
	}
	return &MockRouter{cfg: cfg}
}

// Called reports whether any LLM method was invoked on the router. Useful for
// over-budget-not-called assertions.
func (m *MockRouter) Called() bool { return m.called.Load() }

// Reset clears the called flag so a MockRouter can be reused across subtests.
func (m *MockRouter) Reset() { m.called.Store(false) }

// ─── LLMRouter implementation ────────────────────────────────────────────────

func (m *MockRouter) ChatCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	m.called.Store(true)
	if m.cfg.UnaryResponse != nil {
		return m.cfg.UnaryResponse, nil
	}
	return &schemas.BifrostChatResponse{
		ID:    "mock-resp",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{
			PromptTokens:     int(m.cfg.InputTokens),
			CompletionTokens: int(m.cfg.OutputTokens),
		},
	}, nil
}

func (m *MockRouter) ChatCompletionStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	m.called.Store(true)
	ch := make(chan *schemas.BifrostStreamChunk, m.cfg.Chunks+1)
	go func() {
		defer close(ch)
		for i := 0; i < m.cfg.Chunks; i++ {
			if m.cfg.ChunkDelay > 0 {
				time.Sleep(m.cfg.ChunkDelay)
			}
			ch <- &schemas.BifrostStreamChunk{
				BifrostChatResponse: &schemas.BifrostChatResponse{
					ID:     "mock-stream",
					Object: "chat.completion.chunk",
				},
			}
		}
		// Final usage-bearing chunk so the billing path fires.
		ch <- &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				ID:     "mock-stream-final",
				Object: "chat.completion.chunk",
				Usage: &schemas.BifrostLLMUsage{
					PromptTokens:     int(m.cfg.InputTokens),
					CompletionTokens: int(m.cfg.OutputTokens),
				},
			},
		}
	}()
	return ch, nil
}

func (m *MockRouter) TextCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostTextCompletionResponse{
		ID:    "mock-text",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: int(m.cfg.InputTokens), CompletionTokens: int(m.cfg.OutputTokens)},
	}, nil
}

func (m *MockRouter) EmbeddingRequest(_ *schemas.BifrostContext, _ *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	m.called.Store(true)
	if m.cfg.EmbeddingResponse != nil {
		return m.cfg.EmbeddingResponse, nil
	}
	return &schemas.BifrostEmbeddingResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: int(m.cfg.InputTokens)},
	}, nil
}

func (m *MockRouter) ResponsesRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	m.called.Store(true)
	if m.cfg.UnaryResponsesResponse != nil {
		return m.cfg.UnaryResponsesResponse, nil
	}
	id := "mock-responses"
	return &schemas.BifrostResponsesResponse{
		ID:    &id,
		Model: "openai/gpt-4o",
		Usage: &schemas.ResponsesResponseUsage{
			InputTokens:  int(m.cfg.InputTokens),
			OutputTokens: int(m.cfg.OutputTokens),
		},
	}, nil
}

func (m *MockRouter) ResponsesStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	m.called.Store(true)
	ch := make(chan *schemas.BifrostStreamChunk, m.cfg.Chunks+1)
	go func() {
		defer close(ch)
		respID := "mock-resp-stream"
		for i := 0; i < m.cfg.Chunks; i++ {
			if m.cfg.ChunkDelay > 0 {
				time.Sleep(m.cfg.ChunkDelay)
			}
			ch <- &schemas.BifrostStreamChunk{
				BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
					Type:     schemas.ResponsesStreamResponseTypeCreated,
					Response: &schemas.BifrostResponsesResponse{ID: &respID, Model: "openai/gpt-4o"},
				},
			}
		}
		// Final usage-bearing chunk (response.completed).
		ch <- &schemas.BifrostStreamChunk{
			BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
				Type: schemas.ResponsesStreamResponseTypeCompleted,
				Response: &schemas.BifrostResponsesResponse{
					ID:    &respID,
					Model: "openai/gpt-4o",
					Usage: &schemas.ResponsesResponseUsage{
						InputTokens:  int(m.cfg.InputTokens),
						OutputTokens: int(m.cfg.OutputTokens),
					},
				},
			},
		}
	}()
	return ch, nil
}

func (m *MockRouter) CountTokensRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostCountTokensResponse{}, nil
}

func (m *MockRouter) ImageGenerationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostImageGenerationResponse{}, nil
}

func (m *MockRouter) ImageEditRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostImageGenerationResponse{}, nil
}

func (m *MockRouter) ImageVariationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostImageGenerationResponse{}, nil
}

func (m *MockRouter) SpeechRequest(_ *schemas.BifrostContext, _ *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostSpeechResponse{}, nil
}

func (m *MockRouter) TranscriptionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	m.called.Store(true)
	return &schemas.BifrostTranscriptionResponse{}, nil
}

// compile-time assertion: MockRouter must satisfy LLMRouter.
var _ llmproxy.LLMRouter = (*MockRouter)(nil)

// ── SignRequest ───────────────────────────────────────────────────────────────

// SignRequest sets the HMAC-signed identity headers on req so the gateway
// handler's verifySignature check passes. It delegates to the exported
// llmproxy.SignIdentityHeaders helper which replicates the edge's signing logic
// exactly. An empty secret omits the signature header, matching the gateway
// config that disables verification.
// An empty executionID signs under v1, which is what every caller that is not a
// runtime execution does.
func SignRequest(req *http.Request, secret []byte, projectID, userID, tenantID string) {
	llmproxy.SignIdentityHeaders(req.Header, secret, projectID, userID, tenantID, "")
}

// SignRequestFromExecution is SignRequest for a call made FROM a runtime
// execution: it sets X-Elitea-Execution-Id and signs the v2 tuple that covers
// it.
func SignRequestFromExecution(req *http.Request, secret []byte, projectID, userID, tenantID, executionID string) {
	llmproxy.SignIdentityHeaders(req.Header, secret, projectID, userID, tenantID, executionID)
}

// ── FakeNATS ─────────────────────────────────────────────────────────────────

// FakeNATS is a thread-safe in-memory fake that satisfies the governance
// package's unexported natsClient interface (matched structurally via
// NewSeededGovernance). Tests can inspect Totals, Deltas, and configure
// error overrides to drive error paths.
type FakeNATS struct {
	mu sync.Mutex

	// Totals maps subject → current authoritative counter value (nano-USD).
	Totals map[string]int64
	// Deltas records every payload sent via PublishDelta (write-behind deltas).
	Deltas [][]byte
	// AlertEvents records every (projectID, envelope) pair sent via
	// PublishSoftAlertEvent (the gateway.events.* budget.soft_alert surface).
	AlertEvents []AlertEventRecord
	// OpsEvents captures PublishOpsEvent (gateway.events.ops.*, operator-only).
	OpsEvents [][]byte
	// applied tracks event_ids already applied (idempotency guard).
	applied map[string]bool

	// Error overrides — nil means success.
	ReadErr error
	IncrErr error
	PubErr  error

	breakerState  gobreaker.State
	stateChangeFn func(from, to gobreaker.State)
}

// NewFakeNATS returns an empty FakeNATS with initialised maps.
func NewFakeNATS() *FakeNATS {
	return &FakeNATS{
		Totals:  make(map[string]int64),
		applied: make(map[string]bool),
	}
}

// AlertEventRecord is one captured PublishSoftAlertEvent call.
type AlertEventRecord struct {
	ProjectID string
	Event     []byte
}

// PublishSoftAlertEvent records the budget.soft_alert event (satisfies
// llmproxy.AlertEventPublisher for the BFF.9e soft-alert observability
// assertion).
func (n *FakeNATS) PublishSoftAlertEvent(_ context.Context, projectID string, event []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.AlertEvents = append(n.AlertEvents, AlertEventRecord{ProjectID: projectID, Event: append([]byte(nil), event...)})
	return nil
}

// PublishOpsEvent records an operator-only event (budget.unbilled_stream,
// issue #9). Kept in a SEPARATE slice from AlertEvents so a test can assert
// that the loss record never reaches the tenant-facing project channel.
func (n *FakeNATS) PublishOpsEvent(_ context.Context, event []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.OpsEvents = append(n.OpsEvents, append([]byte(nil), event...))
	return nil
}

// OpsEventCount safely returns the number of captured operator-only events.
func (n *FakeNATS) OpsEventCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.OpsEvents)
}

// AlertEventCount safely returns the number of captured soft-alert events.
func (n *FakeNATS) AlertEventCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.AlertEvents)
}

// GetTotal safely reads the counter total for subject.
func (n *FakeNATS) GetTotal(subject string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Totals[subject]
}

// DeltaCount safely returns the number of write-behind deltas published so far.
func (n *FakeNATS) DeltaCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.Deltas)
}

// SetTotal seeds the counter for subject to v (used by NewSeededGovernance to
// initialise the spend level before the test runs).
func (n *FakeNATS) SetTotal(subject string, v int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Totals[subject] = v
}

// ReadBudget satisfies governance.natsClient.
func (n *FakeNATS) ReadBudget(_ context.Context, subject string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ReadErr != nil {
		return 0, n.ReadErr
	}
	return n.Totals[subject], nil
}

// IncrBudget satisfies governance.natsClient.
func (n *FakeNATS) IncrBudget(_ context.Context, subject string, delta int64) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.IncrErr != nil {
		return 0, n.IncrErr
	}
	n.Totals[subject] += delta
	return n.Totals[subject], nil
}

// IncrBudgetIdempotent satisfies governance.natsClient.
func (n *FakeNATS) IncrBudgetIdempotent(_ context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.IncrErr != nil {
		return 0, false, n.IncrErr
	}
	if n.applied[eventID] {
		return n.Totals[subject], false, nil
	}
	n.applied[eventID] = true
	n.Totals[subject] += delta
	return n.Totals[subject], true, nil
}

// PublishDelta satisfies governance.natsClient.
func (n *FakeNATS) PublishDelta(_ context.Context, _ string, payload []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.PubErr != nil {
		return n.PubErr
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	n.Deltas = append(n.Deltas, cp)
	return nil
}

// TryAlertCooldown satisfies governance.natsClient (always returns fire=true,
// no error in the harness — tests that need to assert cooldown suppression can
// override the fake after construction).
func (n *FakeNATS) TryAlertCooldown(_ context.Context, _ string) (bool, error) {
	return true, nil
}

// OnBreakerStateChange satisfies governance.natsClient.
func (n *FakeNATS) OnBreakerStateChange(fn func(from, to gobreaker.State)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stateChangeFn = fn
}

// BreakerState satisfies governance.natsClient.
func (n *FakeNATS) BreakerState() gobreaker.State {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.breakerState
}

// ── FakeDB ────────────────────────────────────────────────────────────────────

// FakeDB satisfies failmode.DB. QueryRow returns a scripted single-row result
// that failmode.Store.ReadSnapshot can scan. The Snapshot field is set by
// NewSeededGovernance from the hardLimitNano / spentNano arguments so tests see
// the correct spend level from the Postgres tier.
type FakeDB struct {
	Snapshot failmode.Snapshot
	SnapErr  error
}

// fakeRow is a scripted failmode.Row.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("fakeRow: arity mismatch")
	}
	for i, v := range r.vals {
		if err := fakeAssign(dest[i], v); err != nil {
			return err
		}
	}
	return nil
}

// fakeAssign copies v into dest, handling the types ReadSnapshot scans.
func fakeAssign(dest, v any) error {
	switch p := dest.(type) {
	case *bool:
		*p = v.(bool)
	case *int64:
		*p = v.(int64)
	case *int:
		*p = v.(int)
	case *float64:
		*p = v.(float64)
	case **string:
		if v == nil {
			*p = nil
		} else {
			s := v.(string)
			*p = &s
		}
	default:
		return errors.New("fakeRow: unsupported dest type")
	}
	return nil
}

// QueryRow satisfies failmode.DB. The SQL text and arguments are ignored; the
// scripted Snapshot is always returned (each test creates its own FakeDB with
// the desired state).
//
// Column order MUST match failmode.Store.ReadSnapshot's Scan call:
//
//	is_unlimited, hard_limit_nano, accumulated_nano, soft_alert_pct,
//	nats_fail_mode (*string/NULL), acc_found, age_seconds,
//	soft_alerts_disabled
func (d *FakeDB) QueryRow(_ context.Context, _ string, _ ...any) failmode.Row {
	if d.SnapErr != nil {
		return fakeRow{err: d.SnapErr}
	}
	snap := d.Snapshot
	var natsFM any // nil untyped → SQL NULL
	return fakeRow{vals: []any{
		snap.IsUnlimited,
		snap.HardLimitNano,
		snap.AccumulatedNano,
		snap.SoftAlertPct,
		natsFM,
		snap.Found,
		snap.Age.Seconds(),
		snap.SoftAlertsDisabled,
	}}
}

// Begin satisfies failmode.DB for the Reconciler's recovery path. Returns a
// no-op transaction that yields no outage rows so recovery passes complete
// trivially without touching the NATS fake.
func (d *FakeDB) Begin(_ context.Context) (failmode.Tx, error) {
	return &fakeNopTx{}, nil
}

// fakeNopTx is a no-op failmode.Tx for the reconciler in test harness.
type fakeNopTx struct{}

func (t *fakeNopTx) QueryRow(_ context.Context, sql string, args ...any) failmode.Row {
	// Issue #515: the outage-window write claims its event id first.
	if strings.Contains(sql, "processed_event_ids") {
		id, _ := args[0].(string)
		return fakeRow{vals: []any{id}}
	}
	return fakeRow{err: errors.New("fakeNopTx: not used")}
}
func (t *fakeNopTx) Query(_ context.Context, _ string, _ ...any) (failmode.Rows, error) {
	return &fakeEmptyRows{}, nil // no outage rows
}
func (t *fakeNopTx) ExecAffected(_ context.Context, _ string, _ ...any) (int64, error) {
	return 1, nil
}
func (t *fakeNopTx) Commit(_ context.Context) error   { return nil }
func (t *fakeNopTx) Rollback(_ context.Context) error { return nil }

// fakeEmptyRows is a failmode.Rows that yields no rows.
type fakeEmptyRows struct{}

func (r *fakeEmptyRows) Next() bool          { return false }
func (r *fakeEmptyRows) Scan(_ ...any) error { return errors.New("fakeEmptyRows: no rows") }
func (r *fakeEmptyRows) Err() error          { return nil }
func (r *fakeEmptyRows) Close()              {}

// compile-time assertions that FakeDB satisfies the failmode.DB interface.
var _ failmode.DB = (*FakeDB)(nil)

// ── nc2counter adapts FakeNATS to failmode.Counter for the Reconciler ─────────

type nc2counter struct{ nc *FakeNATS }

func (a *nc2counter) ReadBudget(ctx context.Context, subject string) (int64, error) {
	return a.nc.ReadBudget(ctx, subject)
}

func (a *nc2counter) IncrBudgetIdempotent(ctx context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	return a.nc.IncrBudgetIdempotent(ctx, subject, eventID, delta)
}

func (a *nc2counter) BudgetSubject(scope, scopeID string, periodStartUnix int64) string {
	return natspkg.BudgetSubject(scope, scopeID, periodStartUnix)
}

// ── SeededGovernanceOption ────────────────────────────────────────────────────

// SeededGovernanceOption adjusts NewSeededGovernance behaviour.
type SeededGovernanceOption func(*seededGovernanceConfig)

type seededGovernanceConfig struct {
	softAlertPct int
	params       *failmode.Params
}

// WithSoftAlertPct overrides the default 80% soft-alert threshold in the
// seeded snapshot. Values outside 1..100 are silently clamped by the FSM.
func WithSoftAlertPct(pct int) SeededGovernanceOption {
	return func(c *seededGovernanceConfig) { c.softAlertPct = pct }
}

// WithParams overrides the failmode.Params used by the GovernanceStore. When
// not set a safe default (ModeTieredHybrid, PGFreshness=5m, ExpectedReplicas=1)
// is used.
func WithParams(p failmode.Params) SeededGovernanceOption {
	return func(c *seededGovernanceConfig) { c.params = &p }
}

// ── NewSeededGovernance ───────────────────────────────────────────────────────

// NewSeededGovernance builds a real GovernanceStore over in-memory fakes, with
// both the NATS counter and the Postgres snapshot pre-seeded so the project
// identified by projectID appears to have spent spentNano nano-USD against a
// hard limit of hardLimitNano nano-USD.
//
// The returned *FakeNATS and *FakeDB are exposed so callers can assert counter
// increments, delta publishes, and alert-cooldown calls after the request
// completes.
//
// The GovernanceStore is started (its internal reconciler is bound to a
// background context) before being returned. The testing.T cleanup function
// cancels the background context when the test ends.
func NewSeededGovernance(
	t *testing.T,
	projectID int,
	hardLimitNano, spentNano int64,
	opts ...SeededGovernanceOption,
) (*governance.GovernanceStore, *FakeNATS, *FakeDB) {
	t.Helper()

	cfg := &seededGovernanceConfig{softAlertPct: 80}
	for _, o := range opts {
		o(cfg)
	}

	params := failmode.Params{
		Mode:             failmode.ModeTieredHybrid,
		PGFreshness:      5 * time.Minute,
		ExpectedReplicas: 1,
		DegradedCapNano:  0,
	}
	if cfg.params != nil {
		params = *cfg.params
	}

	// Derive the NATS budget subject for the current billing period so the
	// counter is seeded on the exact key the handler will read.
	now := time.Now().UTC()
	y, m, _ := now.Date()
	periodStart := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Unix()
	scopeID := itoa(projectID)
	subject := natspkg.BudgetSubject("project", scopeID, periodStart)

	nc := NewFakeNATS()
	nc.SetTotal(subject, spentNano)

	db := &FakeDB{
		Snapshot: failmode.Snapshot{
			IsUnlimited:     false,
			HardLimitNano:   hardLimitNano,
			AccumulatedNano: spentNano,
			SoftAlertPct:    cfg.softAlertPct,
			Found:           true,
			Age:             0,
		},
	}

	fmStore := failmode.NewStore(db)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, &nc2counter{nc: nc}, degraded, nil)

	gs := governance.NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gs.Start(ctx)

	return gs, nc, db
}

// itoa converts an int to a string without importing strconv in the exported
// surface (keeps the import list minimal; used only internally in this file).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ── MountedHandler ────────────────────────────────────────────────────────────

// MountedHandler assembles a fully-wired chi handler (via api.NewRouter) with:
//   - the given router as the LLMRouter,
//   - the given GovernanceStore as the BudgetChecker,
//   - a real cost.Calculator (no DB → default price table),
//   - the given identitySecret for HMAC verification (nil/empty disables it).
//
// The returned http.Handler is ready for httptest.NewServer / httptest.NewRecorder.
// Additional llmproxy.HandlerOption values (e.g. llmproxy.WithLoopBreaker()
// for the circular-routing guard test, llmproxy.WithAlertEventPublisher for
// the soft-alert event assertion) may be appended via extraOpts.
func MountedHandler(
	_ *testing.T,
	router llmproxy.LLMRouter,
	gov *governance.GovernanceStore,
	secret []byte,
	extraOpts ...llmproxy.HandlerOption,
) http.Handler {
	calc := cost.New(cost.Config{}) // no catalog DB → default price table
	opts := append([]llmproxy.HandlerOption{
		llmproxy.WithBudgetGate(gov, calc),
	}, extraOpts...)
	h := llmproxy.NewHandler(router, nil, secret, opts...)
	return api.NewRouter(h)
}

// ── StaticLegacyModels ────────────────────────────────────────────────────────

// ── MountedHandlerWithModels ──────────────────────────────────────────────────

// MountedHandlerWithModels is MountedHandler extended with a ModelResolver so
// the synthetic /llm/v1/models surface is populated.  Pass the result of
// llmproxy.NewStaticModelResolver(ids) to inject a fixed per-project model set
// without a live database.  When resolver is nil the behaviour degrades to
// MountedHandler (empty model set).
func MountedHandlerWithModels(
	t *testing.T,
	router llmproxy.LLMRouter,
	gov *governance.GovernanceStore,
	secret []byte,
	resolver *llmproxy.ModelResolver,
) http.Handler {
	calc := cost.New(cost.Config{})
	opts := []llmproxy.HandlerOption{
		llmproxy.WithBudgetGate(gov, calc),
	}
	if resolver != nil {
		opts = append(opts, llmproxy.WithModelResolver(resolver))
	}
	h := llmproxy.NewHandler(router, nil, secret, opts...)
	return api.NewRouter(h)
}

// StaticLegacyModels returns a fixed set of legacy /v1/models id strings used
// by models-parity fixtures (BFF.3). The list mirrors the subset the legacy
// LiteLLM gateway exposed and that the BFF parity gate asserts must remain
// present in the synthesised /llm/v1/models response.
func StaticLegacyModels() []string {
	return []string{
		"openai/gpt-4o",
		"openai/gpt-4o-mini",
		"openai/gpt-4-turbo",
		"openai/gpt-3.5-turbo",
		"anthropic/claude-3-5-sonnet-20241022",
		"anthropic/claude-3-5-haiku-20241022",
		"anthropic/claude-3-opus-20240229",
		"anthropic/claude-3-haiku-20240307",
		"openai/text-embedding-3-small",
		"openai/text-embedding-3-large",
	}
}
