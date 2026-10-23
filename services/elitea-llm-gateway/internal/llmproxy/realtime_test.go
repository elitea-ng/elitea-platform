package llmproxy

// realtime_test.go — the /llm/v1/realtime route.
//
// Every test here drives a REAL client WebSocket against the mounted router. A
// unit test of the pump alone cannot see the two things that actually break on
// this route: the admission steps that must run while an http.ResponseWriter
// still exists, and the read limit, whose failure appears minutes into a live
// call rather than at setup.
//
// The codec under test is bifrost's OWN OpenAI provider, not a stand-in. The
// usage facts this route is built around (a transcription turn from which
// ExtractRealtimeTurnUsage returns nil; a duration-shaped envelope from which it
// returns a non-nil, all-zero struct) are facts about that code, so a fake codec
// would test the fake.

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// ── the fake provider socket ────────────────────────────────────────────────

// fakeProviderSocket stands in for the provider's WebSocket. It is the only
// half of a session a test can fake: the caller's half is a real socket over a
// real listener, because that is where the handshake and the read limit live.
type fakeProviderSocket struct {
	toGateway   chan []byte
	fromGateway chan []byte
	closed      chan struct{}
	closeOnce   sync.Once
	pings       atomic.Int64

	mu        sync.Mutex
	closeCode RealtimeCloseCode
	// closeDelay makes the session's own teardown take measurable time, so a
	// test can tell "CloseRealtimeSessions waited for the session" from
	// "CloseRealtimeSessions returned because it was tracking nothing".
	closeDelay time.Duration
}

func newFakeProviderSocket() *fakeProviderSocket {
	return &fakeProviderSocket{
		toGateway:   make(chan []byte, 16),
		fromGateway: make(chan []byte, 64),
		closed:      make(chan struct{}),
	}
}

func (f *fakeProviderSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-f.toGateway:
		return b, nil
	case <-f.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeProviderSocket) Write(ctx context.Context, frame []byte) error {
	cp := append([]byte(nil), frame...)
	select {
	case f.fromGateway <- cp:
		return nil
	case <-f.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeProviderSocket) Ping(ctx context.Context) error {
	f.pings.Add(1)
	select {
	case <-f.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (f *fakeProviderSocket) Close(code RealtimeCloseCode, _ string) {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		delay := f.closeDelay
		f.closeCode = code
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		close(f.closed)
	})
}

// send queues a provider event for the downlink to forward.
func (f *fakeProviderSocket) send(t *testing.T, frame string) {
	t.Helper()
	select {
	case f.toGateway <- []byte(frame):
	case <-time.After(2 * time.Second):
		t.Fatal("the provider socket could not queue a frame")
	}
}

// received waits for one frame the gateway forwarded upstream.
func (f *fakeProviderSocket) received(t *testing.T, within time.Duration) ([]byte, bool) {
	t.Helper()
	select {
	case b := <-f.fromGateway:
		return b, true
	case <-time.After(within):
		return nil, false
	}
}

// ── the codec and the dialer ────────────────────────────────────────────────

// openAICodec is bifrost's own OpenAI provider behind the RealtimeCodec port.
// The four methods it supplies read no provider field, so a zero value is the
// real implementation and not an approximation of it.
type openAICodec struct{ p openai.OpenAIProvider }

func (c *openAICodec) ToBifrostRealtimeEvent(raw json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	return c.p.ToBifrostRealtimeEvent(raw)
}

func (c *openAICodec) ToProviderRealtimeEvent(ev *schemas.BifrostRealtimeEvent) (json.RawMessage, error) {
	return c.p.ToProviderRealtimeEvent(ev)
}

func (c *openAICodec) ShouldStartRealtimeTurn(ev *schemas.BifrostRealtimeEvent) bool {
	return c.p.ShouldStartRealtimeTurn(ev)
}

func (c *openAICodec) ExtractRealtimeTurnUsage(raw []byte) *schemas.BifrostLLMUsage {
	return c.p.ExtractRealtimeTurnUsage(raw)
}

// fakeRealtimeDialer hands out one prepared provider socket and records what it
// was asked to dial, so a test can assert the MAPPED provider and model reached
// it — and that a refused upgrade never dialled at all.
type fakeRealtimeDialer struct {
	sock *fakeProviderSocket
	err  error
	// codec overrides bifrost's own OpenAI provider. Only the tests about a
	// frame the codec CANNOT decode set it; everything else uses the real one,
	// because the usage facts this route is built on are facts about that code.
	codec RealtimeCodec

	mu           sync.Mutex
	calls        int
	lastProvider string
	lastModel    string
	lastParams   url.Values
}

func (d *fakeRealtimeDialer) DialRealtime(_ *schemas.BifrostContext, provider schemas.ModelProvider, model string, params url.Values) (*RealtimeUpstream, error) {
	d.mu.Lock()
	d.calls++
	d.lastProvider, d.lastModel = string(provider), model
	d.lastParams = params
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	codec := d.codec
	if codec == nil {
		codec = &openAICodec{}
	}
	return &RealtimeUpstream{Socket: d.sock, Codec: codec, Subprotocol: "realtime"}, nil
}

func (d *fakeRealtimeDialer) dialed() (int, string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, d.lastProvider, d.lastModel
}

// waitForDial blocks until the provider dial has happened.
//
// The dial now runs AFTER websocket.Accept, so websocket.Dial returns to the
// test as soon as the 101 is written — before the handler has dialled anything.
// Every assertion about what the dialer received has to wait for it, or it
// reads a zero value and passes for the wrong reason.
func (d *fakeRealtimeDialer) waitForDial(t *testing.T) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if calls, _, _ := d.dialed(); calls > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the provider was never dialled")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// dialedParams returns the query parameters the handler forwarded to the dial.
func (d *fakeRealtimeDialer) dialedParams() url.Values {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastParams
}

// ── a gate that records every billed event id ───────────────────────────────

// recordingBudgetChecker records each UpdateUsage call. The billing assertions
// on this route are about the SET of increments, not the last one, so
// fakeBudgetChecker's single-slot recording cannot express them.
type recordingBudgetChecker struct {
	mu       sync.Mutex
	verdict  failmode.Decision
	updates  []recordedUpdate
	notified chan struct{}
	// checks counts CheckBudget calls. A test that has to wait for N re-check
	// TICKS cannot sleep for them: the interval is a fixture number and a sleep
	// makes the test flaky on a loaded box. This is the tick counter.
	checks atomic.Int64
}

type recordedUpdate struct {
	scope   string
	eventID string
	cost    int64
}

func newRecordingGate(v failmode.Verdict) *recordingBudgetChecker {
	return &recordingBudgetChecker{
		verdict:  failmode.Decision{Verdict: v, State: failmode.StateNATSHealthy},
		notified: make(chan struct{}, 64),
	}
}

func (g *recordingBudgetChecker) setVerdict(v failmode.Verdict) {
	g.mu.Lock()
	g.verdict = failmode.Decision{Verdict: v, State: failmode.StateNATSHealthy}
	g.mu.Unlock()
}

func (g *recordingBudgetChecker) CheckBudget(_ context.Context, _ int, _, _ string, _, _ int64) (failmode.Decision, error) {
	g.checks.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.verdict, nil
}

// waitForChecks blocks until the gate has answered n admission questions.
func (g *recordingBudgetChecker) waitForChecks(t *testing.T, n int64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for g.checks.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d budget-gate calls; got %d", n, g.checks.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (g *recordingBudgetChecker) UpdateUsage(_ context.Context, _ int, scope, _, eventID string, costNano, _, _ int64, _ *failmode.UsageDimensions) error {
	g.mu.Lock()
	g.updates = append(g.updates, recordedUpdate{scope: scope, eventID: eventID, cost: costNano})
	g.mu.Unlock()
	select {
	case g.notified <- struct{}{}:
	default:
	}
	return nil
}

func (g *recordingBudgetChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

// projectUpdates returns the project-scope increments seen so far.
func (g *recordingBudgetChecker) projectUpdates() []recordedUpdate {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]recordedUpdate, 0, len(g.updates))
	for _, u := range g.updates {
		if u.scope == budgetScopeProject {
			out = append(out, u)
		}
	}
	return out
}

// waitForProjectUpdates blocks until n project increments have landed.
func (g *recordingBudgetChecker) waitForProjectUpdates(t *testing.T, n int) []recordedUpdate {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if got := g.projectUpdates(); len(got) >= n {
			return got
		}
		select {
		case <-g.notified:
		case <-deadline:
			t.Fatalf("timed out waiting for %d project billing increments; got %d", n, len(g.projectUpdates()))
		}
	}
}

// ── the fixture ─────────────────────────────────────────────────────────────

// realtimeProjectID is the project every session in this file carries.
const realtimeProjectID = "42"

type realtimeFixture struct {
	handler *Handler
	server  *httptest.Server
	dialer  *fakeRealtimeDialer
	gate    *recordingBudgetChecker
	prov    *fakeProviderSocket
}

// newRealtimeFixture mounts the route on a real listener with a fake provider
// socket, a recording budget gate and a catalog-priced cost estimator.
func newRealtimeFixture(t *testing.T, opts ...HandlerOption) *realtimeFixture {
	t.Helper()
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	gate := newRecordingGate(failmode.Allow)
	// Non-zero per-unit rates so a billed turn produces a positive amount:
	// updateUsageUnits bills nothing at all for a zero cost, which would make a
	// billing assertion pass for the wrong reason.
	costs := &fakeCostEstimator{inputRateNano: 1000, outputRateNano: 2000}

	base := []HandlerOption{
		WithRealtimeDialer(dialer),
		WithBudgetGate(gate, costs),
		// Fast enough that a re-check test does not sleep for 15 s, slow enough
		// that it does not fire during a test that is not about it.
		WithRealtimeBudgetRecheck(25 * time.Millisecond),
	}
	h := NewHandler(newDispatchSpy(), nil, nil, append(base, opts...)...)
	srv := httptest.NewServer(h.route())
	t.Cleanup(srv.Close)
	return &realtimeFixture{handler: h, server: srv, dialer: dialer, gate: gate, prov: prov}
}

// realtimeURL builds the ws:// URL for the fixture's route.
func (f *realtimeFixture) realtimeURL(model string) string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/llm/v1/realtime?model=" + model + "&intent=transcription"
}

// dial opens a client session, exactly as the legacy relay does: a plain GET with
// the identity header, no Origin and no subprotocol.
func (f *realtimeFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(t.Context(), f.realtimeURL("gpt-4o-realtime-preview"), &websocket.DialOptions{
		HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}},
	})
	if err != nil {
		body := ""
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
			_ = resp.Body.Close()
		}
		t.Fatalf("dial the realtime route: %v (body=%s)", err, body)
	}
	conn.SetReadLimit(realtimeReadLimit)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// refusedDial performs the handshake and returns the HTTP response the gateway
// wrote instead of upgrading.
func refusedDial(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	conn, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("the handshake was accepted; it had to be refused before the upgrade")
	}
	if resp == nil {
		t.Fatalf("the handshake failed with no HTTP response: %v", err)
	}
	return resp
}

// decodeError reads an OpenAI-shaped error body.
func decodeError(t *testing.T, resp *http.Response) openAIError {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body openAIError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the refusal body is not an OpenAI-shaped error: %v", err)
	}
	return body
}

// readEvent reads one client frame and decodes its `type`.
func readEvent(t *testing.T, conn *websocket.Conn, within time.Duration) (string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	_, frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read a client frame: %v", err)
	}
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(frame, &env)
	return env.Type, frame
}

// ── admission: the three steps that must precede the upgrade ────────────────

// TestRealtime_UnpricedModelIsRefusedBeforeTheUpgrade is decision H2.
//
// cost.Calculator's default table prefix-matches, so "gpt-4o-realtime-preview"
// resolves onto the plain "gpt-4o" text row and bills a confident, roughly
// 10x-too-small figure that no unpriced counter can fire on. For a single audio
// request "bill zero and count it" bounds the loss at one call. A socket the
// tenant holds open has no such bound, so the session is refused instead — and
// it must be refused while an http.ResponseWriter still exists, because the
// hijack destroys the only way to write an OpenAI-shaped body.
func TestRealtime_UnpricedModelIsRefusedBeforeTheUpgrade(t *testing.T) {
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	// SourceFallback is what an un-catalogued model resolves to. It is a real
	// price with a real number; only its provenance says it was invented.
	costs := &fakeCostEstimator{inputRateNano: 1000, source: cost.SourceFallback}
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithBudgetGate(newRecordingGate(failmode.Allow), costs),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/llm/v1/realtime?model=gpt-4o-realtime-preview"
	resp := refusedDial(t, url, http.Header{headerProjectID: []string{realtimeProjectID}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body := decodeError(t, resp)
	if body.Error.Code != "model_not_priced" {
		t.Fatalf("error.code = %q, want %q", body.Error.Code, "model_not_priced")
	}
	if calls, _, _ := dialer.dialed(); calls != 0 {
		t.Fatalf("the provider was dialled %d times for an unpriced model; want 0", calls)
	}
}

// TestRealtime_CatalogPricedModelIsAdmitted holds the other half of H2: the gate
// admits a model the catalog prices, so the refusal above is not simply "the
// route never opens".
func TestRealtime_CatalogPricedModelIsAdmitted(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	_ = conn
	f.dialer.waitForDial(t)
	calls, _, model := f.dialer.dialed()
	if calls != 1 {
		t.Fatalf("the provider was dialled %d times; want 1", calls)
	}
	if model != "gpt-4o-realtime-preview" {
		t.Fatalf("the provider was dialled for model %q", model)
	}
}

// TestRealtime_SecondsOnlyCatalogRateAdmits covers the transcription billing
// shape. whisper-style realtime models are sold by the second and carry NO token
// price, so a token-only probe would refuse every one of them.
func TestRealtime_SecondsOnlyCatalogRateAdmits(t *testing.T) {
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(&fakeRealtimeDialer{sock: newFakeProviderSocket()}),
		WithBudgetGate(newRecordingGate(failmode.Allow), &secondsOnlyEstimator{}),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=gpt-4o-transcribe-realtime",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("a model with only a per-second catalog rate was refused: %v", err)
	}
	_ = conn.CloseNow()
}

// secondsOnlyEstimator prices SECONDS from the catalog and nothing else, which
// is exactly the shape of a whisper-style row: real per-second columns and NULL
// token columns.
type secondsOnlyEstimator struct{}

func (secondsOnlyEstimator) Cost(_ context.Context, _, _ string, _, _ int64) cost.Cost {
	return cost.Cost{TotalNanoUSD: 1, Basis: cost.BasisTokens, Source: cost.SourceFallback}
}

func (secondsOnlyEstimator) CostUnits(_ context.Context, _, _ string, u cost.Units) cost.Cost {
	if u.Basis() != cost.BasisSeconds {
		return cost.Cost{TotalNanoUSD: 1, Basis: cost.BasisTokens, Source: cost.SourceFallback}
	}
	return cost.Cost{TotalNanoUSD: u.InputMillis, Basis: cost.BasisSeconds, Source: cost.SourceCatalog}
}

// TestRealtime_BudgetRefusalPrecedesTheUpgrade proves the budget gate runs on
// this route like every other, and that its refusal reaches the caller as the
// SDK's budget contract rather than as a socket close nobody can read.
func TestRealtime_BudgetRefusalPrecedesTheUpgrade(t *testing.T) {
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithBudgetGate(newRecordingGate(failmode.Block402), &fakeCostEstimator{inputRateNano: 1000}),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	resp := refusedDial(t,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=gpt-4o-realtime-preview",
		http.Header{headerProjectID: []string{realtimeProjectID}})
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	body := decodeError(t, resp)
	if body.Error.Type != budgetErrorType || body.Error.Code != budgetCodeProject {
		t.Fatalf("refusal = (%q, %q), want (%q, %q): elitea-sdk matches on the TYPE alone",
			body.Error.Type, body.Error.Code, budgetErrorType, budgetCodeProject)
	}
	if calls, _, _ := dialer.dialed(); calls != 0 {
		t.Fatalf("the provider was dialled %d times for a refused budget; want 0", calls)
	}
}

// TestRealtime_MissingModelIs400 covers the one decode this route has. The model
// rides in the query string because a WebSocket handshake is a GET.
func TestRealtime_MissingModelIs400(t *testing.T) {
	f := newRealtimeFixture(t)
	resp := refusedDial(t,
		"ws"+strings.TrimPrefix(f.server.URL, "http")+"/llm/v1/realtime",
		http.Header{headerProjectID: []string{realtimeProjectID}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestRealtime_NoDialerAnswers501 covers the composition fault. A route mounted
// with no provider side must refuse rather than upgrade a socket it can connect
// to nothing.
func TestRealtime_NoDialerAnswers501(t *testing.T) {
	h := NewHandler(newDispatchSpy(), nil, nil)
	srv := httptest.NewServer(h.route())
	defer srv.Close()
	resp := refusedDial(t,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=m", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// ── the model map ───────────────────────────────────────────────────────────

// TestRealtime_DispatchesTheAsrSectionWireName proves the route resolves through
// the same model set every other dialect uses.
//
// There is no `realtime` configuration section: elitea-main writes five model
// types and none of them is one. A realtime ASR model is an `asr` row, which is
// already in addressableModelSections — so this asserts that the pair COVERS the
// new route, which is the property a missing pair would break (a 404 for a model
// the project configured correctly, the way /llm/v1/embeddings broke).
func TestRealtime_DispatchesTheAsrSectionWireName(t *testing.T) {
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	resolver := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{rows: []fakeModelRow{
		{title: "Voice in", data: []byte(`{"name":"openai/gpt-4o-transcribe"}`), section: "asr", typ: "asr_model"},
	}}})
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithModelResolver(resolver),
		WithBudgetGate(newRecordingGate(failmode.Allow), &fakeCostEstimator{inputRateNano: 1000}),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		// The caller sends the ADVERTISED title, which is what a caller that
		// read GET /llm/v1/models has.
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=Voice+in",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{mapProjectID}}})
	if err != nil {
		t.Fatalf("an asr row could not open a realtime session: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	dialer.waitForDial(t)
	_, gotProvider, gotModel := dialer.dialed()
	if gotProvider != "openai" || gotModel != "gpt-4o-transcribe" {
		t.Fatalf("the provider was dialled as (%q, %q), want (openai, gpt-4o-transcribe): "+
			"mapModel must run before the dial so the provider never sees a caller-authored title",
			gotProvider, gotModel)
	}
}

// ── the read limit ──────────────────────────────────────────────────────────

// TestRealtime_OversizedFrameEndsTheSessionWith1009 pins the read limit.
//
// The library default is 32 KiB, and realtime frames carry base64 audio well
// past it. The failure that default produces is not an error at setup: the
// session opens, works, and dies mid-call with close status 1009. This test
// sends one frame past OUR limit and asserts the close code, so the limit is a
// number a test observes rather than a number a comment claims.
func TestRealtime_OversizedFrameEndsTheSessionWith1009(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)

	oversized := make([]byte, realtimeReadLimit+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// The write itself succeeds: the peer only rejects the message after it has
	// read past the limit.
	_ = conn.Write(ctx, websocket.MessageText, oversized)

	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %v (err=%v), want 1009 StatusMessageTooBig", got, err)
	}
}

// TestRealtime_FrameUnderTheLimitIsForwarded is the discriminating half: a frame
// far past the library's 32 KiB default, and under ours, must reach the
// provider. Without it the test above would also pass with the limit set to 1.
func TestRealtime_FrameUnderTheLimitIsForwarded(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)

	// 128 KiB of base64 audio: four times the library default.
	audio := strings.Repeat("QUJDRA==", 16*1024)
	frame := `{"type":"input_audio_buffer.append","audio":"` + audio + `"}`
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write a 128 KiB audio frame: %v", err)
	}
	got, ok := f.prov.received(t, 3*time.Second)
	if !ok {
		t.Fatal("a 128 KiB audio frame never reached the provider; the read limit is too small")
	}
	if !strings.Contains(string(got), "input_audio_buffer.append") {
		t.Fatalf("the provider received an unexpected frame: %.80s", got)
	}
}

// TestRealtime_UnknownClientEventIsForwardedVerbatim covers the relay's opening
// frame. `transcription_session.update` is not an event bifrost has a case for,
// and dropping it would leave the session with no transcription configuration at
// all — a silent, total failure of the only caller this route has.
func TestRealtime_UnknownClientEventIsForwardedVerbatim(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)

	frame := `{"type":"transcription_session.update","session":{"input_audio_format":"pcm16",` +
		`"input_audio_transcription":{"model":"gpt-4o-transcribe","language":"en"}}}`
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write the session update: %v", err)
	}
	got, ok := f.prov.received(t, 3*time.Second)
	if !ok {
		t.Fatal("the relay's transcription_session.update never reached the provider")
	}
	if !strings.Contains(string(got), "input_audio_transcription") {
		t.Fatalf("the transcription configuration was lost on the way to the provider: %s", got)
	}
}

// ── billing ─────────────────────────────────────────────────────────────────

// responseDoneFrame is a `response.done` event with a token usage envelope.
func responseDoneFrame(in, out int) string {
	return fmt.Sprintf(`{"type":"response.done","response":{"usage":{"input_tokens":%d,`+
		`"output_tokens":%d,"total_tokens":%d}}}`, in, out, in+out)
}

// TestRealtime_EachTurnIsBilledWithItsOwnEventID is the billing rule.
//
// Two things are asserted together because they fail together. Billing per TURN:
// two turns must produce two increments, not one session-level increment. And a
// FRESH event id per turn: gateway.processed_event_ids has event_id as its
// primary key and NATS de-duplicates on it, so a session that reused one id
// would have turns 2..N accepted, applied nowhere, and reported as billed.
//
// spawnBillingGoroutine already mints a uuid per call, so calling it once per
// turn needs NO change to it. This test is what keeps that true.
func TestRealtime_EachTurnIsBilledWithItsOwnEventID(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)

	f.prov.send(t, responseDoneFrame(100, 50))
	f.prov.send(t, responseDoneFrame(200, 25))

	// The caller must receive both frames, and receive them BEFORE billing runs.
	for i := 0; i < 2; i++ {
		if got, _ := readEvent(t, conn, 3*time.Second); got != "response.done" {
			t.Fatalf("frame %d has type %q, want response.done", i, got)
		}
	}

	updates := f.gate.waitForProjectUpdates(t, 2)
	if len(updates) != 2 {
		t.Fatalf("got %d project increments for 2 turns, want exactly 2", len(updates))
	}
	if updates[0].eventID == updates[1].eventID {
		t.Fatalf("both turns billed under event id %q; turns 2..N would be swallowed as redeliveries",
			updates[0].eventID)
	}
	// 100*1000 + 50*2000, then 200*1000 + 25*2000: the per-turn amounts, summed
	// nowhere. An assertion on the total alone would pass for a single
	// session-level increment of the same size.
	want := map[int64]bool{200_000: true, 250_000: true}
	for _, u := range updates {
		if !want[u.cost] {
			t.Fatalf("a turn billed %d nano-USD; want one of %v", u.cost, want)
		}
		delete(want, u.cost)
	}
}

// TestRealtime_TranscriptionTurnIsBilledFromItsOwnUsage is FACT F2.
//
// A transcription-intent session never emits `response.done`, and bifrost's
// ExtractRealtimeTurnUsage reads `response.usage` off that event ONLY — so it
// returns nil for the event that DOES carry a well-formed top-level usage
// object. The subtest below asserts that directly against bifrost's own code, so
// the reason this route parses the envelope itself is recorded in a test rather
// than in a comment.
func TestRealtime_TranscriptionTurnIsBilledFromItsOwnUsage(t *testing.T) {
	const frame = `{"type":"conversation.item.input_audio_transcription.completed",` +
		`"transcript":"hello there","usage":{"type":"tokens","input_tokens":30,"output_tokens":7}}`

	t.Run("bifrost's own extractor reports nothing for it", func(t *testing.T) {
		if u := (&openAICodec{}).ExtractRealtimeTurnUsage([]byte(frame)); u != nil {
			t.Fatalf("ExtractRealtimeTurnUsage returned %+v; F2 says it returns nil here, and "+
				"this route's own parser exists only because it does", u)
		}
	})

	t.Run("the route bills it anyway", func(t *testing.T) {
		f := newRealtimeFixture(t)
		conn := f.dial(t)
		f.prov.send(t, frame)
		if got, _ := readEvent(t, conn, 3*time.Second); got != "conversation.item.input_audio_transcription.completed" {
			t.Fatalf("the transcript event was not forwarded; got %q", got)
		}
		updates := f.gate.waitForProjectUpdates(t, 1)
		if want := int64(30*1000 + 7*2000); updates[0].cost != want {
			t.Fatalf("billed %d nano-USD, want %d", updates[0].cost, want)
		}
	})
}

// TestRealtime_DurationShapedUsageIsBilledBySeconds covers the other
// transcription envelope. A duration is the only quantity on this path that
// arrives as a float, and secondsToMillis is the single crossing to the int64
// the money path requires.
func TestRealtime_DurationShapedUsageIsBilledBySeconds(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	f.prov.send(t, `{"type":"conversation.item.input_audio_transcription.completed",`+
		`"transcript":"hi","usage":{"type":"duration","seconds":3.5}}`)
	readEvent(t, conn, 3*time.Second)

	updates := f.gate.waitForProjectUpdates(t, 1)
	// 3.5 s → 3500 ms, at the fixture's 1000 nano-USD per input unit.
	if want := int64(3500 * 1000); updates[0].cost != want {
		t.Fatalf("billed %d nano-USD for 3.5 s, want %d", updates[0].cost, want)
	}
}

// TestRealtime_AllZeroUsageIsUnpricedAndNotBilledAsZero is the other half of
// FACT F2, and the one that would have shipped silently.
//
// bifrost's extractor returns a NON-NIL, ALL-ZERO BifrostLLMUsage for a
// duration-shaped `response.done` envelope. `if u != nil { bill(u) }` then bills
// zero and reports success — an under-bill with no counter, no log and no
// symptom. The turn must be counted as unpriced instead.
func TestRealtime_AllZeroUsageIsUnpricedAndNotBilledAsZero(t *testing.T) {
	const frame = `{"type":"response.done","response":{"usage":{"type":"duration","seconds":4.0}}}`

	t.Run("bifrost's extractor returns a non-nil all-zero struct", func(t *testing.T) {
		u := (&openAICodec{}).ExtractRealtimeTurnUsage([]byte(frame))
		if u == nil {
			t.Fatal("ExtractRealtimeTurnUsage returned nil; F2 says it returns an all-zero struct, " +
				"which is why this code discriminates on the QUANTITY and never on the pointer")
		}
		if u.PromptTokens != 0 || u.CompletionTokens != 0 {
			t.Fatalf("the struct is not all-zero: %+v", u)
		}
	})

	t.Run("the route bills nothing and counts the turn", func(t *testing.T) {
		before := realtimeTurnsUnpriced.Value()
		f := newRealtimeFixture(t)
		conn := f.dial(t)
		f.prov.send(t, frame)
		readEvent(t, conn, 3*time.Second)

		// Give the billing path the same window a real increment would take.
		deadline := time.After(time.Second)
		for realtimeTurnsUnpriced.Value() == before {
			select {
			case <-deadline:
				t.Fatal("the all-zero turn was neither billed nor counted as unpriced")
			case <-time.After(5 * time.Millisecond):
			}
		}
		if got := f.gate.projectUpdates(); len(got) != 0 {
			t.Fatalf("an all-zero usage envelope produced %d billing increments; want 0", len(got))
		}
	})
}

// ── re-gating (decision H1) ─────────────────────────────────────────────────

// TestRealtime_MidSessionOutageRefusesTurnsAndKeepsTheSocket is decision H1.
//
// A Block503 mid-session means the budget store could not answer. The turn is
// NOT forwarded — no un-gated turn may reach the provider — and the socket stays
// open, so a transient blip does not drop a live call. The client is told once,
// in the same contract the HTTP path uses.
func TestRealtime_MidSessionOutageRefusesTurnsAndKeepsTheSocket(t *testing.T) {
	// A slower re-check than the fixture default on purpose: this test is about
	// the state BETWEEN outages, and the fixture's 25 ms would reach
	// maxBudgetOutagesBeforeClose and close the session while the assertions
	// were still running. TestRealtime_RepeatedOutagesCloseTheSession covers the
	// close.
	f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(500*time.Millisecond))
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block503)

	typ, frame := readEvent(t, conn, 3*time.Second)
	if typ != "error" {
		t.Fatalf("the refusal event has type %q, want \"error\"", typ)
	}
	var ev realtimeErrorEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		t.Fatalf("decode the refusal event: %v", err)
	}
	if ev.Error.Type != "service_unavailable" || ev.Error.Code != "nats_unavailable" {
		t.Fatalf("refusal = (%q, %q), want (service_unavailable, nats_unavailable): "+
			"a mid-session refusal carries the same contract the HTTP path writes",
			ev.Error.Type, ev.Error.Code)
	}

	// Drain the provider's inbox, then prove nothing new reaches it.
	for {
		if _, ok := f.prov.received(t, 50*time.Millisecond); !ok {
			break
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"input_audio_buffer.append","audio":"QQ=="}`)); err != nil {
		t.Fatalf("the socket was closed; H1 requires it to stay open: %v", err)
	}
	if got, ok := f.prov.received(t, 300*time.Millisecond); ok {
		t.Fatalf("a turn reached the provider while the budget gate was refusing: %s", got)
	}

	// The socket is still live: a provider event still reaches the caller.
	f.prov.send(t, `{"type":"input_audio_buffer.speech_started"}`)
	if typ, _ := readEvent(t, conn, 3*time.Second); typ != "input_audio_buffer.speech_started" {
		t.Fatalf("the socket did not survive the refusal; got %q", typ)
	}
}

// TestRealtime_RecoveredGateResumesTurns is the discriminating other half: a
// refusal that never lifts is indistinguishable from a session that was simply
// broken.
func TestRealtime_RecoveredGateResumesTurns(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block503)
	if typ, _ := readEvent(t, conn, 3*time.Second); typ != "error" {
		t.Fatalf("expected the refusal event first, got %q", typ)
	}
	f.gate.setVerdict(failmode.Allow)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"input_audio_buffer.append","audio":"QQ=="}`)); err != nil {
			t.Fatalf("write after recovery: %v", err)
		}
		if _, ok := f.prov.received(t, 100*time.Millisecond); ok {
			return
		}
	}
	t.Fatal("turns never resumed after the budget gate recovered")
}

// TestRealtime_RepeatedOutagesCloseTheSession is the N of decision H1. A blip
// must not drop a live call, and a real outage must not hold an un-gated socket
// open without end.
func TestRealtime_RepeatedOutagesCloseTheSession(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block503)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the refusal event; keep reading until the close
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008: after %d consecutive gate outages "+
				"the session must close", got, err, maxBudgetOutagesBeforeClose)
		}
		return
	}
}

// TestRealtime_ExhaustedBudgetClosesTheSession covers the 402 half of H1: an
// exhausted budget is not a blip, so the session ends rather than idling.
func TestRealtime_ExhaustedBudgetClosesTheSession(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block402)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sawRefusal := false
	for {
		_, frame, err := conn.Read(ctx)
		if err == nil {
			var ev realtimeErrorEvent
			if json.Unmarshal(frame, &ev) == nil && ev.Error.Type == budgetErrorType {
				if ev.Error.Code != budgetCodeProject {
					t.Fatalf("the refusal code is %q, want %q", ev.Error.Code, budgetCodeProject)
				}
				sawRefusal = true
			}
			continue
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008", got, err)
		}
		// The CODE alone does not discriminate: the outage path closes with 1008
		// too, and its refusal event carries the same budget type. The REASON is
		// what says WHICH rule closed the session, and a 402 must close on the
		// FIRST refusal rather than after maxBudgetOutagesBeforeClose of them.
		var closeErr websocket.CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("the close was not a clean close frame: %v", err)
		}
		if !strings.Contains(closeErr.Reason, "budget exhausted") {
			t.Fatalf("close reason = %q, want the exhausted-budget message: an exhausted budget "+
				"closes immediately, not after the consecutive-outage count", closeErr.Reason)
		}
		if !sawRefusal {
			t.Fatal("the session closed without telling the caller which budget was exhausted")
		}
		return
	}
}

// ── security ────────────────────────────────────────────────────────────────

// TestRealtime_CrossOriginHandshakeIsRefused is the accept-side Origin policy.
//
// CORS does not apply to a WebSocket handshake, so without this check any page
// could open this route with the browser's ambient credentials. The default
// admits only a same-host Origin, or none at all — which is every non-browser
// client, including the relay this route exists for.
func TestRealtime_CrossOriginHandshakeIsRefused(t *testing.T) {
	f := newRealtimeFixture(t)
	resp := refusedDial(t, f.realtimeURL("gpt-4o-realtime-preview"), http.Header{
		headerProjectID: []string{realtimeProjectID},
		"Origin":        []string{"https://attacker.example"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-site Origin", resp.StatusCode)
	}
	// TestRealtime_RefusedOriginNeverDialsTheProvider holds the other half: the
	// refused handshake must not have opened a provider socket at all.
}

// TestRealtime_ConfiguredOriginIsAdmitted is the operator's opt-in. Without it
// the test above would also pass with the route refusing every Origin, which is
// a different (and undiagnosable) behaviour.
func TestRealtime_ConfiguredOriginIsAdmitted(t *testing.T) {
	f := newRealtimeFixture(t, WithRealtimeOrigins([]string{"allowed.example"}))
	conn, _, err := websocket.Dial(t.Context(), f.realtimeURL("gpt-4o-realtime-preview"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			headerProjectID: []string{realtimeProjectID},
			"Origin":        []string{"https://allowed.example"},
		},
	})
	if err != nil {
		t.Fatalf("a configured Origin was refused: %v", err)
	}
	_ = conn.CloseNow()
}

// ── capacity and lifecycle ──────────────────────────────────────────────────

// TestRealtime_SessionPoolIsBounded proves the global bound refuses rather than
// queues. Nothing else bounds a session: a hijacked connection has its deadlines
// cleared, so no server timeout can reap one.
func TestRealtime_SessionPoolIsBounded(t *testing.T) {
	f := newRealtimeFixture(t, WithRealtimeSessionLimit(1))
	first := f.dial(t)
	defer func() { _ = first.CloseNow() }()

	resp := refusedDial(t, f.realtimeURL("gpt-4o-realtime-preview"),
		http.Header{headerProjectID: []string{realtimeProjectID}})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the session pool is full", resp.StatusCode)
	}
}

// TestRealtime_CloseRealtimeSessionsEndsALiveSession is the shutdown phase.
//
// http.Server.Shutdown neither closes nor waits for a hijacked connection, so
// without this call a rolling deploy would leave every live session running
// until the pod is killed — and its last turn unbilled. The test asserts the
// caller is told (close status 1001 Going Away) and that the wait returns.
func TestRealtime_CloseRealtimeSessionsEndsALiveSession(t *testing.T) {
	f := newRealtimeFixture(t)
	// The session's own teardown takes measurable time, so "the wait returned
	// after the session finished" and "the wait returned because it was
	// tracking nothing" are different observations. Without this the second
	// looks exactly like the first.
	f.prov.mu.Lock()
	f.prov.closeDelay = 300 * time.Millisecond
	f.prov.mu.Unlock()
	conn := f.dial(t)

	done := make(chan struct{})
	go func() {
		f.handler.CloseRealtimeSessions(context.Background())
		close(done)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (err=%v), want 1001 StatusGoingAway", got, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseRealtimeSessions never returned; the session was not tracked")
	}
	select {
	case <-f.prov.closed:
	default:
		t.Fatal("CloseRealtimeSessions returned while the session was still tearing down: " +
			"the session was never added to the shutdown group, so the wait had nothing to wait for")
	}
}

// TestRealtime_UpgradeAfterShutdownIsRefused is the Add-after-Wait guard. A
// session that opens while shutdown is running would never be waited for, so it
// is refused instead.
func TestRealtime_UpgradeAfterShutdownIsRefused(t *testing.T) {
	f := newRealtimeFixture(t)
	f.handler.CloseRealtimeSessions(context.Background())

	resp := refusedDial(t, f.realtimeURL("gpt-4o-realtime-preview"),
		http.Header{headerProjectID: []string{realtimeProjectID}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once shutdown has begun", resp.StatusCode)
	}
}

// ── the unit-level readers ──────────────────────────────────────────────────

// TestRealtimeTranscriptionUnits covers the envelope reader's edges directly.
// The important row is total_tokens-only: input and output are priced at
// different rates, so splitting a total between them would be a number the
// gateway invented, and no invented number may reach the budget counter.
func TestRealtimeTranscriptionUnits(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  cost.Units
		wantK bool
	}{
		{"tokens", `{"usage":{"type":"tokens","input_tokens":10,"output_tokens":4}}`,
			cost.Units{InputTokens: 10, OutputTokens: 4}, true},
		{"duration", `{"usage":{"type":"duration","seconds":2.25}}`,
			cost.Units{InputMillis: 2250}, true},
		{"no usage", `{"type":"conversation.item.input_audio_transcription.completed"}`,
			cost.Units{}, false},
		{"total only", `{"usage":{"type":"tokens","total_tokens":14}}`, cost.Units{}, false},
		{"negative duration", `{"usage":{"type":"duration","seconds":-1}}`, cost.Units{}, false},
		{"sub-millisecond duration", `{"usage":{"type":"duration","seconds":0.0001}}`, cost.Units{}, false},
		{"not json", `{`, cost.Units{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := realtimeTranscriptionUnits([]byte(tc.raw))
			if ok != tc.wantK || got != tc.want {
				t.Fatalf("= (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.wantK)
			}
		})
	}
}

// TestRealtimeResponseUnits pins the F2 rule at the unit level: the test is on
// the QUANTITY, never on the pointer.
func TestRealtimeResponseUnits(t *testing.T) {
	if _, ok := realtimeResponseUnits(nil); ok {
		t.Error("a nil usage must not be billable")
	}
	if _, ok := realtimeResponseUnits(&schemas.BifrostLLMUsage{}); ok {
		t.Error("an all-zero usage must not be billable: bifrost returns exactly that for a " +
			"duration-shaped envelope, and billing it reports success while billing nothing")
	}
	got, ok := realtimeResponseUnits(&schemas.BifrostLLMUsage{PromptTokens: 3, CompletionTokens: 1})
	if !ok || got != (cost.Units{InputTokens: 3, OutputTokens: 1}) {
		t.Fatalf("= (%+v, %v), want ({3 1}, true)", got, ok)
	}
}

// TestRealtimeDialer_NilCoreYieldsNoDialer keeps a gateway with no core client
// on the 501 path instead of the panic path: WithRealtimeDialer(nil) leaves the
// route refusing, which is a diagnosable answer.
func TestRealtimeDialer_NilCoreYieldsNoDialer(t *testing.T) {
	if NewBifrostRealtimeDialer(nil, nil) != nil {
		t.Fatal("a nil core must yield no dialer; the route then answers 501 instead of panicking")
	}
}

// TestRealtimeDialErrorsAreDistinct pins the three dial failures apart. They are
// values rather than formatted strings so a caller can tell a configuration
// fault (an unknown provider, one with no realtime surface) from a credential
// gap without matching on text — and so a wrapped error keeps that meaning.
func TestRealtimeDialErrorsAreDistinct(t *testing.T) {
	all := []error{ErrRealtimeProviderUnknown, ErrRealtimeUnsupported, ErrRealtimeNoCredential}
	for i, a := range all {
		wrapped := fmt.Errorf("realtime: dial: %w", a)
		if !errors.Is(wrapped, a) {
			t.Errorf("a wrapped %v no longer matches itself", a)
		}
		for j, b := range all {
			if i != j && errors.Is(wrapped, b) {
				t.Errorf("%v and %v are the same error value; a credential gap would read as a "+
					"configuration fault", a, b)
			}
		}
	}
}

// TestRealtimeConstantsInSync keeps the env-facing defaults in config and the
// handler-facing defaults here from drifting apart. They describe one policy,
// and a mismatch would make the chart's comment say one number while the
// process enforced another — which for the re-check interval is the difference
// between a bounded session and an unbounded one.
func TestRealtimeConstantsInSync(t *testing.T) {
	if config.DefaultRealtimeBudgetRecheck != DefaultRealtimeBudgetRecheck {
		t.Errorf("config.DefaultRealtimeBudgetRecheck = %v, llmproxy.DefaultRealtimeBudgetRecheck = %v",
			config.DefaultRealtimeBudgetRecheck, DefaultRealtimeBudgetRecheck)
	}
	if config.DefaultRealtimeMaxSessions != DefaultRealtimeMaxSessions {
		t.Errorf("config.DefaultRealtimeMaxSessions = %d, llmproxy.DefaultRealtimeMaxSessions = %d",
			config.DefaultRealtimeMaxSessions, DefaultRealtimeMaxSessions)
	}
}

// TestRealtimeDefaultsAreArmedWithoutOptions covers the Handler a test or an
// embedder builds by hand. The re-check and the session bound are ON by
// default, for the reason the stream grace is: a Handler built without the
// options must still bound a session rather than gate it once and hold an
// unbounded socket.
func TestRealtimeDefaultsAreArmedWithoutOptions(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil)
	if h.realtimeRecheck != DefaultRealtimeBudgetRecheck {
		t.Errorf("realtimeRecheck = %v, want %v", h.realtimeRecheck, DefaultRealtimeBudgetRecheck)
	}
	_, total, per := h.realtimeLimit.snapshot()
	if total != DefaultRealtimeMaxSessions {
		t.Errorf("session pool = %d, want %d", total, DefaultRealtimeMaxSessions)
	}
	if per != DefaultRealtimeMaxSessions/realtimeSessionPerProjectDivisor {
		t.Errorf("per-project cap = %d, want %d", per,
			DefaultRealtimeMaxSessions/realtimeSessionPerProjectDivisor)
	}
	if h.realtimeKeepalive != realtimeKeepalivePeriod {
		t.Errorf("realtimeKeepalive = %v, want %v: the keepalive is the ONLY liveness check a "+
			"hijacked connection has", h.realtimeKeepalive, realtimeKeepalivePeriod)
	}
	if h.realtimePingBound != realtimeWriteTimeout {
		t.Errorf("realtimePingBound = %v, want %v: each keepalive ping needs a FULL deadline of its "+
			"own, or a caller that is slow to pong makes a healthy provider read as a dead peer",
			h.realtimePingBound, realtimeWriteTimeout)
	}
	if h.realtimeGateBound != realtimeGateTimeout {
		t.Errorf("realtimeGateBound = %v, want %v: without it a budget store that STALLS parks "+
			"the re-check goroutine for ever and the session runs un-gated",
			h.realtimeGateBound, realtimeGateTimeout)
	}
}

// TestRealtimeMetricNames_ListsEveryPublishedCounter closes the loop the
// scrape test cannot close on its own.
//
// The composition root builds its /metrics allowlist by RANGING OVER
// RealtimeMetricNames, and the scrape test ranges over the same function. A
// name deleted from the list therefore disappears from the route AND from the
// check that reads the route — the checker checks itself, and the control goes
// silent with every test green. This test names each constant individually, so
// dropping one from the list is a compile-time-visible, test-visible change.
func TestRealtimeMetricNames_ListsEveryPublishedCounter(t *testing.T) {
	// Every counter this file publishes, spelled out. Add a line here when you
	// publish one; that is the whole point of the file.
	want := []string{
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
	got := RealtimeMetricNames()
	if len(got) != len(want) {
		t.Fatalf("RealtimeMetricNames returned %d names, want %d: %v", len(got), len(want), got)
	}
	listed := make(map[string]bool, len(got))
	for _, n := range got {
		listed[n] = true
	}
	for _, n := range want {
		if !listed[n] {
			t.Errorf("%q is published but absent from RealtimeMetricNames, so it has NO route on "+
				"the gateway mux: expvar serves /debug/vars on http.DefaultServeMux, which this "+
				"process never serves", n)
		}
		if expvar.Get(n) == nil {
			t.Errorf("%q is named but never published; the scrape would carry an UNPUBLISHED line", n)
		}
	}
}

// TestRealtime_KeepalivePingsBothPeers is FACT F6.
//
// A hijacked connection has its deadlines CLEARED, so no server
// ReadHeaderTimeout and no IdleTimeout applies to a realtime session once the
// upgrade completes. Nothing but this pinger would ever notice a peer that went
// away without a FIN, and the session would hold its goroutines and its two
// sockets for as long as the process lived.
//
// Both directions are asserted. A pinger that only checks one of them leaves
// the other half of the session undetectably dead.
func TestRealtime_KeepalivePingsBothPeers(t *testing.T) {
	f := newRealtimeFixture(t)
	// Not an operator knob: the production value is 20 s and a test cannot wait
	// for it. TestRealtimeDefaultsAreArmedWithoutOptions pins the default.
	f.handler.realtimeKeepalive = 20 * time.Millisecond

	clientPings := make(chan struct{}, 8)
	conn, _, err := websocket.Dial(t.Context(), f.realtimeURL("gpt-4o-realtime-preview"), &websocket.DialOptions{
		HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}},
		OnPingReceived: func(context.Context, []byte) bool {
			select {
			case clientPings <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// The client only processes control frames while it is reading.
	go func() {
		for {
			if _, _, rerr := conn.Read(context.Background()); rerr != nil {
				return
			}
		}
	}()

	select {
	case <-clientPings:
	case <-time.After(3 * time.Second):
		t.Fatal("the gateway never pinged the CALLER; an idle session has no other liveness check")
	}
	deadline := time.After(3 * time.Second)
	for f.prov.pings.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the gateway never pinged the PROVIDER; a provider that went away silently " +
				"would hold the session open")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// slowCostEstimator prices from the catalog but takes its time doing it. It
// exists to make the ORDER of "forward, then bill" observable: updateUsageUnits
// computes the cost on the CALLING goroutine, so a billing step placed before
// the forward would add its whole latency to the caller's audio.
type slowCostEstimator struct{ delay time.Duration }

func (s slowCostEstimator) Cost(_ context.Context, _, _ string, in, out int64) cost.Cost {
	time.Sleep(s.delay)
	return cost.Cost{TotalNanoUSD: in + out, Basis: cost.BasisTokens, Source: cost.SourceCatalog}
}

func (s slowCostEstimator) CostUnits(ctx context.Context, p, m string, u cost.Units) cost.Cost {
	return s.Cost(ctx, p, m, u.InputTokens, u.OutputTokens)
}

// TestRealtime_TheFrameReachesTheCallerBeforeBillingRuns pins the downlink
// order.
//
// Billing must never delay the caller's audio and a billing bug must never be
// able to swallow a frame, so the forward happens FIRST. The price lookup runs
// on the pump's own goroutine, so a billing step moved ahead of the forward
// adds its whole latency to every turn boundary — audible on a live call, and
// invisible to a test that only checks that the frame eventually arrives.
func TestRealtime_TheFrameReachesTheCallerBeforeBillingRuns(t *testing.T) {
	const priceDelay = 400 * time.Millisecond
	prov := newFakeProviderSocket()
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(&fakeRealtimeDialer{sock: prov}),
		WithBudgetGate(newRecordingGate(failmode.Allow), slowCostEstimator{delay: priceDelay}),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=gpt-4o-realtime-preview",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	prov.send(t, responseDoneFrame(10, 1))
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, rerr := conn.Read(ctx); rerr != nil {
		t.Fatalf("read the turn event: %v", rerr)
	}
	if elapsed := time.Since(start); elapsed >= priceDelay {
		t.Fatalf("the caller waited %v for a turn event; the price lookup ran before the forward, "+
			"so every turn boundary carries the billing latency", elapsed)
	}
}

// TestRealtime_TurnStartTriggerAlsoGates covers the ADDITIONAL trigger.
//
// FACT F3: bifrost reports a turn start for exactly `response.create` and the
// SERVER-side `input_audio_buffer.committed`, and the legacy relay sends neither
// — which is why the periodic re-check is the mandatory mechanism. A client that
// DOES send one must still be gated at that moment, and this test is what keeps
// the second trigger wired: the re-check interval here is longer than the whole
// test, so nothing but the turn-start path can produce the close.
func TestRealtime_TurnStartTriggerAlsoGates(t *testing.T) {
	f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(time.Hour))
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block402)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write response.create: %v", err)
	}
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the refusal event
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008: a turn start must re-ask the budget "+
				"gate, and an exhausted budget must close the session there too", got, err)
		}
		// The turn must not have reached the provider.
		if frame, ok := f.prov.received(t, 200*time.Millisecond); ok {
			t.Fatalf("the turn reached the provider after the budget refused it: %s", frame)
		}
		return
	}
}

// ════════════════════════════════════════════════════════════════════════════
// The session defects found by running this route (2026-08-20). Each test below
// names the defect it pins and the failure that returns without it.
// ════════════════════════════════════════════════════════════════════════════

// ── shared fakes for the session-level tests ────────────────────────────────

// scriptedClientSocket is the CALLER's side of a session, under a test's
// control. The pump's ports are interfaces exactly so the two things a real
// handshake cannot produce on demand — a client write that FAILS, and the ORDER
// in which a session ends — can be driven directly.
type scriptedClientSocket struct {
	reads  chan []byte
	writes chan []byte
	// writeErr, when set, fails every write to the caller.
	writeErr error

	closed    chan struct{}
	closeOnce sync.Once
	// onClose observes the session's state at the instant Close is entered.
	onClose func()

	mu        sync.Mutex
	closeCode RealtimeCloseCode
}

func newScriptedClientSocket() *scriptedClientSocket {
	return &scriptedClientSocket{
		reads:  make(chan []byte, 16),
		writes: make(chan []byte, 32),
		closed: make(chan struct{}),
	}
}

func (c *scriptedClientSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.reads:
		return b, nil
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *scriptedClientSocket) Write(_ context.Context, frame []byte) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	select {
	case c.writes <- append([]byte(nil), frame...):
	default:
	}
	return nil
}

func (c *scriptedClientSocket) Ping(context.Context) error { return nil }

func (c *scriptedClientSocket) Close(code RealtimeCloseCode, _ string) {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
		c.mu.Lock()
		c.closeCode = code
		c.mu.Unlock()
		close(c.closed)
	})
}

func (c *scriptedClientSocket) code() RealtimeCloseCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCode
}

// newTestSession builds a live session over two fakes, with no handshake.
func newTestSession(t *testing.T, h *Handler, client RealtimeSocket, prov RealtimeSocket) *realtimeSession {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	t.Cleanup(cancel)
	// The budget gate reads the project from the CONTEXT, not from the session
	// struct. Without this line admissionVerdict finds no project, answers
	// "unlimited", and every gate assertion here would pass for that reason.
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, realtimeProjectID)
	m := realtimeModel{
		provider: "openai",
		model:    "gpt-4o-realtime-preview",
		pricing:  realtimePricing{tokens: true, seconds: true},
	}
	return &realtimeSession{
		h:         h,
		ctx:       ctx,
		cancel:    cancel,
		client:    client,
		up:        &RealtimeUpstream{Socket: prov, Codec: &openAICodec{}},
		resp:      m,
		asr:       m,
		projectID: realtimeProjectID,
	}
}

// perModelCatalogEstimator prices ONLY the models it was told about, from the
// catalog, and records the model each price lookup asked for. It is what makes
// "the session bills the model the provider now serves" observable.
type perModelCatalogEstimator struct {
	priced map[string]bool

	mu        sync.Mutex
	lastModel string
}

func (e *perModelCatalogEstimator) record(model string) {
	e.mu.Lock()
	e.lastModel = model
	e.mu.Unlock()
}

func (e *perModelCatalogEstimator) billedModel() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastModel
}

func (e *perModelCatalogEstimator) Cost(_ context.Context, _, model string, in, out int64) cost.Cost {
	e.record(model)
	if !e.priced[model] {
		return cost.Cost{TotalNanoUSD: in + out, Basis: cost.BasisTokens, Source: cost.SourceFallback}
	}
	return cost.Cost{TotalNanoUSD: in*1000 + out*2000, Basis: cost.BasisTokens, Source: cost.SourceCatalog}
}

func (e *perModelCatalogEstimator) CostUnits(ctx context.Context, p, model string, u cost.Units) cost.Cost {
	if u.Basis() != cost.BasisTokens {
		e.record(model)
		return cost.Cost{}
	}
	return e.Cost(ctx, p, model, u.InputTokens, u.OutputTokens)
}

// blindCodec is bifrost's OpenAI provider with the DECODER removed. A provider
// whose frame the codec cannot translate is not hypothetical: a new event type,
// or a provider dialect bifrost has no case for, produces exactly this.
type blindCodec struct{ openAICodec }

func (c *blindCodec) ToBifrostRealtimeEvent(json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	return nil, errors.New("this codec cannot decode any provider event")
}

// stallingChecker answers the first `answer` calls and then stops answering for
// ever, without an error and without looking at its context. That is the shape
// a wedged connection pool has: not an error, not a cancellation — nothing at
// all. The first answers exist so the UPGRADE still completes and only the LIVE
// session meets the stall.
type stallingChecker struct {
	answer  int64
	release chan struct{}
	calls   atomic.Int64
}

// newStallingChecker answers `answer` calls and stalls on every call after
// them. release is closed by the test's cleanup so the stalled goroutines are
// not left behind for the rest of the package's run.
func newStallingChecker(t *testing.T, answer int64) *stallingChecker {
	t.Helper()
	c := &stallingChecker{answer: answer, release: make(chan struct{})}
	t.Cleanup(func() { close(c.release) })
	return c
}

func (c *stallingChecker) CheckBudget(context.Context, int, string, string, int64, int64) (failmode.Decision, error) {
	if c.calls.Add(1) <= c.answer {
		return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
	}
	<-c.release
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
}

func (c *stallingChecker) UpdateUsage(context.Context, int, string, string, string, int64, int64, int64, *failmode.UsageDimensions) error {
	return nil
}

func (c *stallingChecker) TryAlertCooldown(context.Context, string, string, int64) (bool, error) {
	return false, nil
}

// ── DEFECT A: the caller's realtime query parameters ────────────────────────

// TestRealtime_LegacyClientIntentReachesTheProvider is the whole reason this
// route exists, and it did not work.
//
// indexer_asr_realtime.py dials
// `/v1/realtime?model=<m>&intent=transcription`. `intent` is what selects the
// provider's TRANSCRIPTION session mode. bifrost's RealtimeWebSocketURL builds
// `<base>/v1/realtime?model=<model>` and core holds no occurrence of "intent"
// at all, so the parameter was dropped and the provider opened a
// CONVERSATIONAL session for the only caller this route has.
//
// The URL below is the one that client builds, character for character.
func TestRealtime_LegacyClientIntentReachesTheProvider(t *testing.T) {
	f := newRealtimeFixture(t)
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") +
		"/llm/v1/realtime?model=gpt-4o-realtime-preview&intent=transcription"
	conn, _, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}},
	})
	if err != nil {
		t.Fatalf("dial the legacy client's URL: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	f.dialer.waitForDial(t)
	got := f.dialer.dialedParams()
	if got.Get("intent") != "transcription" {
		t.Fatalf("the provider dial carried intent=%q, want %q: without it the provider opens a "+
			"conversational session and the only caller this route has gets the wrong mode",
			got.Get("intent"), "transcription")
	}
	if got.Get("model") != "" {
		t.Fatalf("the caller's `model` was copied onto the provider URL as %q; mapModel and the "+
			"price gate own that parameter", got.Get("model"))
	}
}

// TestRealtimeForwardedQuery_IsAnAllowlistNotAPassthrough pins the security
// half. The caller's query string is attacker-controlled: a copied `model`
// undoes mapModel and the price gate, and a copied credential or deployment
// parameter picks which upstream the gateway dials.
func TestRealtimeForwardedQuery_IsAnAllowlistNotAPassthrough(t *testing.T) {
	in := url.Values{
		"intent":        []string{"transcription"},
		"model":         []string{"an-unpriced-model"},
		"api-key":       []string{"sk-attacker"},
		"api-version":   []string{"2099-01-01"},
		"deployment":    []string{"someone-elses"},
		"Authorization": []string{"Bearer nope"},
	}
	got := realtimeForwardedQuery(in)
	if got.Get("intent") != "transcription" {
		t.Fatalf("intent was not forwarded: %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("the forwarded query is %v; ONLY the allowlist may travel", got)
	}
}

// TestRealtimeForwardedQuery_DropsEmptyValues keeps `?intent=` from reaching a
// provider as an empty mode selector, which is not the same request as one that
// names no mode at all.
func TestRealtimeForwardedQuery_DropsEmptyValues(t *testing.T) {
	if got := realtimeForwardedQuery(url.Values{"intent": []string{"  "}}); len(got) != 0 {
		t.Fatalf("an empty intent was forwarded as %v", got)
	}
}

// TestRealtimeProviderURL_MergesTheIntentAndKeepsTheProviderModel is the dial
// half. The provider URL already carries `?model=` — the name bifrost resolved
// — and nothing a caller sends may replace it.
func TestRealtimeProviderURL_MergesTheIntentAndKeepsTheProviderModel(t *testing.T) {
	const base = "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview"
	got, err := realtimeProviderURL(base, url.Values{
		"intent": []string{"transcription"},
		"model":  []string{"an-unpriced-model"},
	})
	if err != nil {
		t.Fatalf("build the provider URL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse the built URL: %v", err)
	}
	if u.Query().Get("intent") != "transcription" {
		t.Fatalf("intent missing from %q", got)
	}
	if m := u.Query()["model"]; len(m) != 1 || m[0] != "gpt-4o-realtime-preview" {
		t.Fatalf("model = %v in %q; the provider's own model must survive untouched", m, got)
	}
}

// TestRealtimeProviderURL_NeverOverwritesAProviderParameter pins the guard that
// makes the rule above hold for ANY name the allowlist may grow.
//
// The allowlist keeps `model` out today, so the assertion above passes even
// with the guard removed. This one drives the guard directly: a parameter
// bifrost already put on the URL is the gateway's value, and a caller may not
// replace it by naming it too.
func TestRealtimeProviderURL_NeverOverwritesAProviderParameter(t *testing.T) {
	const base = "wss://example.test/v1/realtime?model=m&intent=provider-choice"
	got, err := realtimeProviderURL(base, url.Values{"intent": []string{"caller-choice"}})
	if err != nil {
		t.Fatalf("build the provider URL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse the built URL: %v", err)
	}
	if v := u.Query()["intent"]; len(v) != 1 || v[0] != "provider-choice" {
		t.Fatalf("intent = %v in %q; a value the provider URL already carries is the gateway's, "+
			"and a caller must not be able to replace or duplicate it", v, got)
	}
}

// TestRealtimeProviderURL_UnchangedWithNoParams keeps the common path free of a
// re-encode that could reorder or re-escape a credential-derived URL.
func TestRealtimeProviderURL_UnchangedWithNoParams(t *testing.T) {
	const base = "wss://example.test/v1/realtime?model=m"
	got, err := realtimeProviderURL(base, nil)
	if err != nil || got != base {
		t.Fatalf("= (%q, %v), want (%q, nil)", got, err, base)
	}
}

// ── DEFECT H: the dial ran before the upgrade ───────────────────────────────

// TestRealtime_RefusedOriginNeverDialsTheProvider is the security consequence
// of the old order.
//
// The Origin allowlist lives INSIDE websocket.Accept, and the dial used to run
// before it. So every refused cross-site handshake — and every plain GET, and
// every scan — opened one outbound WebSocket to a paid provider first. That is
// an unauthenticated caller driving connections to an upstream.
func TestRealtime_RefusedOriginNeverDialsTheProvider(t *testing.T) {
	f := newRealtimeFixture(t)
	resp := refusedDial(t, f.realtimeURL("gpt-4o-realtime-preview"), http.Header{
		headerProjectID: []string{realtimeProjectID},
		"Origin":        []string{"https://attacker.example"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-site Origin", resp.StatusCode)
	}
	if calls, _, _ := f.dialer.dialed(); calls != 0 {
		t.Fatalf("a refused cross-site handshake opened %d provider socket(s); want 0: the "+
			"Origin check runs inside Accept, so the dial must not precede it", calls)
	}
}

// TestRealtime_NonUpgradeRequestGetsAnOpenAIShapedError.
//
// websocket.Accept answers a non-handshake with a PLAIN-TEXT 426, which breaks
// the rule that every /llm route refuses with an OpenAI-shaped body. A caller
// reading `websocket: ...` as text cannot tell the gateway's refusal from a
// proxy's.
func TestRealtime_NonUpgradeRequestGetsAnOpenAIShapedError(t *testing.T) {
	f := newRealtimeFixture(t)
	resp, err := http.Get(f.server.URL + "/llm/v1/realtime?model=gpt-4o-realtime-preview")
	if err != nil {
		t.Fatalf("GET the realtime route: %v", err)
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("Upgrade header = %q, want \"websocket\" (RFC 7231 §6.5.15)", got)
	}
	body := decodeError(t, resp)
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "upgrade_required" {
		t.Fatalf("refusal = (%q, %q), want (invalid_request_error, upgrade_required): the "+
			"library's plain-text 426 is not an OpenAI-shaped body",
			body.Error.Type, body.Error.Code)
	}
	if calls, _, _ := f.dialer.dialed(); calls != 0 {
		t.Fatalf("a plain GET opened %d provider socket(s); want 0", calls)
	}
}

// TestRealtime_DialFailureIsReportedOnTheSocket is the price of accepting
// first: there is no status line left, so the refusal rides the socket. It
// carries the same OpenAI-shaped error object every other refusal on this route
// carries, and the close status says the gateway's own side failed.
func TestRealtime_DialFailureIsReportedOnTheSocket(t *testing.T) {
	f := newRealtimeFixture(t)
	f.dialer.err = errors.New("the provider refused the handshake")

	conn, _, err := websocket.Dial(t.Context(), f.realtimeURL("gpt-4o-realtime-preview"),
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("the upgrade must succeed before the dial is attempted: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	typ, frame := readEvent(t, conn, 3*time.Second)
	if typ != "error" {
		t.Fatalf("the first frame has type %q, want \"error\"", typ)
	}
	var ev realtimeErrorEvent
	if uerr := json.Unmarshal(frame, &ev); uerr != nil {
		t.Fatalf("decode the refusal event: %v", uerr)
	}
	if ev.Error.Code != "upstream_unavailable" {
		t.Fatalf("error.code = %q, want upstream_unavailable", ev.Error.Code)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, _, rerr := conn.Read(ctx)
	if got := websocket.CloseStatus(rerr); got != websocket.StatusInternalError {
		t.Fatalf("close status = %v (err=%v), want 1011", got, rerr)
	}
}

// ── DEFECT B: the model a client frame asks for ─────────────────────────────

// newModelSwapFixture mounts the route with a catalogue that prices exactly the
// models named, so a session.update to an unpriced one is refusable.
func newModelSwapFixture(t *testing.T, priced ...string) (*realtimeFixture, *perModelCatalogEstimator) {
	t.Helper()
	set := make(map[string]bool, len(priced))
	for _, m := range priced {
		set[m] = true
	}
	costs := &perModelCatalogEstimator{priced: set}
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	gate := newRecordingGate(failmode.Allow)
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithBudgetGate(gate, costs),
		WithRealtimeBudgetRecheck(time.Hour), // only the frame path may act here
	)
	srv := httptest.NewServer(h.route())
	t.Cleanup(srv.Close)
	return &realtimeFixture{handler: h, server: srv, dialer: dialer, gate: gate, prov: prov}, costs
}

// TestRealtime_SessionUpdateToAnUnpricedModelClosesTheSession is decision H2
// applied AFTER the upgrade.
//
// mapModel, the price gate and checkBudget all ran against the `model` QUERY
// parameter — but the model the provider actually serves is changed by a client
// frame, and the uplink forwarded that frame verbatim. So an admitted, priced
// model became an unpriced one one frame later, and billTurn kept pricing the
// original. Everything H2 refuses at the upgrade was reachable through this
// hole.
func TestRealtime_SessionUpdateToAnUnpricedModelClosesTheSession(t *testing.T) {
	f, _ := newModelSwapFixture(t, "gpt-4o-realtime-preview")
	conn := f.dial(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"session.update","session":{"model":"an-unpriced-model"}}`)); err != nil {
		t.Fatalf("write the session update: %v", err)
	}

	sawRefusal := false
	for {
		_, frame, err := conn.Read(ctx)
		if err == nil {
			var ev realtimeErrorEvent
			if json.Unmarshal(frame, &ev) == nil && ev.Error.Code == "model_not_priced" {
				sawRefusal = true
			}
			continue
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008: a client frame must not be able to "+
				"put an UNPRICED model on an admitted session", got, err)
		}
		if !sawRefusal {
			t.Fatal("the session closed without telling the caller which model was refused")
		}
		if got, ok := f.prov.received(t, 200*time.Millisecond); ok {
			t.Fatalf("the model swap reached the provider before it was refused: %s", got)
		}
		return
	}
}

// TestRealtime_SessionUpdateToAPricedModelIsAdoptedAndBilled is the
// discriminating half. "Close on any session.update" would pass the test above
// and break every caller. The new model must be ADMITTED, ADOPTED and then
// BILLED — pricing the original model after a swap is a wrong number, not a
// missing one.
func TestRealtime_SessionUpdateToAPricedModelIsAdoptedAndBilled(t *testing.T) {
	f, costs := newModelSwapFixture(t, "gpt-4o-realtime-preview", "gpt-4o-realtime-mini")
	conn := f.dial(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"session.update","session":{"model":"gpt-4o-realtime-mini"}}`)); err != nil {
		t.Fatalf("write the session update: %v", err)
	}
	if _, ok := f.prov.received(t, 3*time.Second); !ok {
		t.Fatal("an admitted model swap never reached the provider")
	}

	f.prov.send(t, responseDoneFrame(10, 5))
	if got, _ := readEvent(t, conn, 3*time.Second); got != "response.done" {
		t.Fatalf("the turn event was not forwarded; got %q", got)
	}
	f.gate.waitForProjectUpdates(t, 1)
	if got := costs.billedModel(); got != "gpt-4o-realtime-mini" {
		t.Fatalf("the turn was priced as %q; the session serves gpt-4o-realtime-mini since the "+
			"session.update, and billing must follow the model the provider serves", got)
	}
}

// TestRealtime_LegacyOpeningFrameStillWorks is the compatibility guard on the
// fix above. indexer_asr_realtime.py sends transcription_session.update on
// EVERY open, and that frame carries the transcription model, the audio format,
// the language and the VAD settings. "Refuse the frame" would leave the session
// with no transcription configuration at all — a silent, total failure of the
// only caller this route has. The payload below is that client's, field for
// field.
func TestRealtime_LegacyOpeningFrameStillWorks(t *testing.T) {
	f, _ := newModelSwapFixture(t, "gpt-4o-realtime-preview", "gpt-4o-transcribe")
	conn := f.dial(t)

	const opening = `{"type":"transcription_session.update","session":{` +
		`"input_audio_format":"pcm16",` +
		`"input_audio_transcription":{"model":"gpt-4o-transcribe","language":"en"},` +
		`"turn_detection":{"type":"server_vad","silence_duration_ms":300,"threshold":0.7}}}`
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(opening)); err != nil {
		t.Fatalf("write the legacy opening frame: %v", err)
	}
	got, ok := f.prov.received(t, 3*time.Second)
	if !ok {
		t.Fatal("the legacy client's opening frame never reached the provider")
	}
	for _, want := range []string{`"model":"gpt-4o-transcribe"`, `"language":"en"`,
		`"silence_duration_ms":300`, `"threshold":0.7`, `"input_audio_format":"pcm16"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("the provider frame lost %s: %s", want, got)
		}
	}
}

// TestRealtime_MappedModelIsWrittenBackIntoTheFrame covers the rewrite.
//
// A caller names the ADVERTISED title, and the provider must never see it —
// that is what mapModel is for. A frame that names a model therefore cannot be
// forwarded unchanged once the title maps to something else.
func TestRealtime_MappedModelIsWrittenBackIntoTheFrame(t *testing.T) {
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	resolver := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{rows: []fakeModelRow{
		{title: "Voice in", data: []byte(`{"name":"openai/gpt-4o-transcribe"}`), section: "asr", typ: "asr_model"},
	}}})
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithModelResolver(resolver),
		WithBudgetGate(newRecordingGate(failmode.Allow), &fakeCostEstimator{inputRateNano: 1000}),
		WithRealtimeBudgetRecheck(time.Hour),
	)
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=Voice+in",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{mapProjectID}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if werr := conn.Write(ctx, websocket.MessageText, []byte(
		`{"type":"transcription_session.update","session":{"input_audio_transcription":`+
			`{"model":"Voice in","language":"en"}}}`)); werr != nil {
		t.Fatalf("write the session update: %v", werr)
	}
	got, ok := prov.received(t, 3*time.Second)
	if !ok {
		t.Fatal("the session update never reached the provider")
	}
	if strings.Contains(string(got), "Voice in") {
		t.Fatalf("the provider received the caller-authored title: %s", got)
	}
	if !strings.Contains(string(got), `"model":"gpt-4o-transcribe"`) {
		t.Fatalf("the mapped model was not written back into the frame: %s", got)
	}
	if !strings.Contains(string(got), `"language":"en"`) {
		t.Fatalf("the rewrite lost a field the gateway does not own: %s", got)
	}
}

// TestRealtimeFrameModels covers the reader directly, including the frames that
// name NO model — those must cost one decode and change nothing.
func TestRealtimeFrameModels(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []realtimeAskedModel
	}{
		{"session update names the response model",
			`{"type":"session.update","session":{"model":"m"}}`,
			[]realtimeAskedModel{{realtimeSlotResponse, "m"}}},
		{"transcription session update names the asr model",
			`{"type":"transcription_session.update","session":{"input_audio_transcription":{"model":"a"}}}`,
			[]realtimeAskedModel{{realtimeSlotTranscription, "a"}}},
		{"both slots at once",
			`{"type":"session.update","session":{"model":"m","input_audio_transcription":{"model":"a"}}}`,
			[]realtimeAskedModel{{realtimeSlotResponse, "m"}, {realtimeSlotTranscription, "a"}}},
		{"an audio append names nothing",
			`{"type":"input_audio_buffer.append","audio":"QQ=="}`, nil},
		{"a session update with no model names nothing",
			`{"type":"session.update","session":{"voice":"alloy"}}`, nil},
		{"another event carrying a session object names nothing",
			`{"type":"session.created","session":{"model":"m"}}`, nil},
		{"not json", `{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := realtimeFrameModels([]byte(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("= %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── DEFECT C: a terminal frame the codec cannot decode ──────────────────────

// TestRealtime_UndecodableTerminalFrameIsNeverSilent.
//
// billTurn opened with `if ev == nil { return }`. A terminal frame the codec
// could not decode was therefore forwarded to the caller, billed nowhere, and
// counted on NO counter — not turns_billed, not turns_unpriced. Every other
// path in this file is explicit that a turn it cannot price must be counted.
//
// The three subtests are one property in three parts: an undecodable turn that
// CAN still be priced is billed, one that cannot is counted, and an undecodable
// frame that is not a turn at all moves nothing. The third is what stops the
// fix from over-counting every audio delta as a lost turn.
func TestRealtime_UndecodableTerminalFrameIsNeverSilent(t *testing.T) {
	newBlindFixture := func(t *testing.T) *realtimeFixture {
		t.Helper()
		f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(time.Hour))
		f.dialer.codec = &blindCodec{}
		return f
	}

	t.Run("the codec really cannot decode it", func(t *testing.T) {
		if _, err := (&blindCodec{}).ToBifrostRealtimeEvent([]byte(responseDoneFrame(1, 1))); err == nil {
			t.Fatal("the fake codec decoded the frame; the whole test would then prove nothing")
		}
	})

	t.Run("an undecodable turn with usage is still billed", func(t *testing.T) {
		f := newBlindFixture(t)
		conn := f.dial(t)
		f.prov.send(t, responseDoneFrame(100, 50))
		if got, _ := readEvent(t, conn, 3*time.Second); got != "response.done" {
			t.Fatalf("frame type = %q", got)
		}
		updates := f.gate.waitForProjectUpdates(t, 1)
		if updates[0].cost != 200_000 {
			t.Fatalf("billed %d nano-USD, want 200000", updates[0].cost)
		}
	})

	t.Run("an undecodable turn with no usage is counted as unpriced", func(t *testing.T) {
		before := realtimeTurnsUnpriced.Value()
		f := newBlindFixture(t)
		conn := f.dial(t)
		f.prov.send(t, `{"type":"response.done","response":{}}`)
		readEvent(t, conn, 3*time.Second)
		deadline := time.After(3 * time.Second)
		for realtimeTurnsUnpriced.Value() == before {
			select {
			case <-deadline:
				t.Fatal("a terminal frame the codec could not decode was billed nowhere and " +
					"counted nowhere; it left no trace at all")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})

	t.Run("an undecodable NON-terminal frame moves nothing", func(t *testing.T) {
		beforeUnpriced := realtimeTurnsUnpriced.Value()
		beforeBilled := realtimeTurnsBilled.Value()
		f := newBlindFixture(t)
		conn := f.dial(t)
		f.prov.send(t, `{"type":"response.audio.delta","delta":"QQ=="}`)
		readEvent(t, conn, 3*time.Second)
		time.Sleep(100 * time.Millisecond)
		if realtimeTurnsUnpriced.Value() != beforeUnpriced || realtimeTurnsBilled.Value() != beforeBilled {
			t.Fatal("an audio delta was counted as a turn; a session streams thousands of them " +
				"and the unpriced counter would become unreadable")
		}
	})
}

// ── DEFECT D: a completed turn whose forward to the caller fails ────────────

// TestRealtime_TurnIsBilledWhenTheClientWriteFails.
//
// The downlink returned on a client write error BEFORE billTurn ran. The
// provider had already done the work by the time it reported the completed
// turn, so a caller that went away between the turn and its terminal frame got
// that work free and no counter moved. That is the free-inference-on-disconnect
// class stream_drain.go exists to prevent.
func TestRealtime_TurnIsBilledWhenTheClientWriteFails(t *testing.T) {
	gate := newRecordingGate(failmode.Allow)
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{inputRateNano: 1000, outputRateNano: 2000}))

	client := newScriptedClientSocket()
	client.writeErr = io.ErrClosedPipe // the caller is gone
	prov := newFakeProviderSocket()
	s := newTestSession(t, h, client, prov)

	done := make(chan struct{})
	go func() { defer close(done); s.downlink() }()
	prov.send(t, responseDoneFrame(100, 50))

	updates := gate.waitForProjectUpdates(t, 1)
	if updates[0].cost != 200_000 {
		t.Fatalf("billed %d nano-USD, want 200000", updates[0].cost)
	}
	// The session still ENDS: an undeliverable caller is not a session to keep.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the downlink did not exit after the client write failed")
	}
}

// ── DEFECT E: a budget store that stalls rather than fails ──────────────────

// TestRealtime_StalledBudgetStoreIsAnOutage.
//
// regate and gateTurn asked admissionVerdict with the SESSION context, which
// has no deadline by design. admissionVerdict bounds each store read with a
// context deadline — and a store that STALLS while it ignores its context
// returns nothing at all. The re-check goroutine then parked for ever: the
// ticker never fired again and the session ran un-gated for the life of the
// process.
//
// The stalling checker below never returns and never looks at its context,
// which is the shape a wedged connection pool has.
func TestRealtime_StalledBudgetStoreIsAnOutage(t *testing.T) {
	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		// One answer for the UPGRADE's own budget check, then silence: only the
		// live session meets the stall.
		WithBudgetGate(newStallingChecker(t, 1), &fakeCostEstimator{inputRateNano: 1000}),
		WithRealtimeBudgetRecheck(20*time.Millisecond))
	// Not an operator knob: the production bound is realtimeGateTimeout and a
	// test cannot wait for it. TestRealtimeDefaultsAreArmedWithoutOptions pins
	// the default.
	h.realtimeGateBound = 30 * time.Millisecond
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	// The UPGRADE runs its own budget check on the request goroutine, which the
	// stalling store also blocks — so the handshake is dialled with a gate that
	// answers, and only the live session meets the stall.
	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=gpt-4o-realtime-preview",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sawOutage := false
	for {
		_, frame, rerr := conn.Read(ctx)
		if rerr == nil {
			var ev realtimeErrorEvent
			if json.Unmarshal(frame, &ev) == nil && ev.Error.Code == "nats_unavailable" {
				sawOutage = true
			}
			continue
		}
		if got := websocket.CloseStatus(rerr); got != websocket.StatusPolicyViolation {
			t.Fatalf("close status = %v (err=%v), want 1008: a gate that never answers is an "+
				"OUTAGE, and %d consecutive ones close the session (decision H1)",
				got, rerr, maxBudgetOutagesBeforeClose)
		}
		if !sawOutage {
			t.Fatal("the session closed without ever telling the caller the gate was unreachable")
		}
		return
	}
}

// TestRealtimeGateTimeout_ExceedsTheGatesOwnBudget keeps the bound from firing
// on a store that is merely SLOW. admissionVerdict makes up to TWO sequential
// reads, each bounded by budgetGateTimeout, so a bound at or below that would
// call a healthy-but-slow gate an outage and refuse turns for nothing.
func TestRealtimeGateTimeout_ExceedsTheGatesOwnBudget(t *testing.T) {
	if realtimeGateTimeout <= 2*budgetGateTimeout {
		t.Fatalf("realtimeGateTimeout = %v, but admissionVerdict can legitimately take %v "+
			"(the project read then the member read)", realtimeGateTimeout, 2*budgetGateTimeout)
	}
}

// ── DEFECT F and G: the order in which a session ends ───────────────────────

// TestRealtime_EndRefusesBeforeItCloses is decisions F and G together.
//
// The 402 path closed the client socket BEFORE cancelling and never set the
// refusing flag. So the uplink kept forwarding client events to the provider
// for the whole close handshake — which coder/websocket runs for up to a
// hardcoded 5 s. An exhausted budget must stop spend at once, not five seconds
// later.
//
// The CANCEL is last, and that is not the reviewers' prescription. Cancelling
// first destroys the close frame: the session context is the context the uplink
// reads the client socket with, and coder/websocket arms a context.AfterFunc
// that closes the connection ABRUPTLY when it fires (conn.go, setupReadTimeout).
// Measured on this code, cancel-then-close delivered the close status on 8 runs
// out of 30 and an unexplained EOF on the other 22.
// TestRealtime_ALateAllowCannotResumeAnEndingSession closes the window between
// the refusal and the cancel.
//
// end() runs refuse, close, cancel — in that order, and for the reasons the
// `ending` field documents. The close waits for the peer's close reply, so for
// that whole window the session context is NOT yet cancelled. A budget gate call
// that was already in flight when the session was refused can return Allow
// inside it. Guarded only on the context, resumeTurns would then clear the
// refusal and the uplink would forward to the provider again — on the very
// budget that just refused the session.
//
// The test drives exactly that interleaving: it calls resumeTurns FROM INSIDE
// the close handshake, which is the one moment the context check cannot catch.
func TestRealtime_ALateAllowCannotResumeAnEndingSession(t *testing.T) {
	h := NewHandler(newDispatchSpy(), nil, nil)
	client := newScriptedClientSocket()
	prov := newFakeProviderSocket()
	s := newTestSession(t, h, client, prov)

	var ctxDoneAtResume, refusingAfterResume bool
	client.onClose = func() {
		// This runs INSIDE end(), after the refusal and before the cancel.
		select {
		case <-s.ctx.Done():
			ctxDoneAtResume = true
		default:
		}
		// The late Allow.
		s.resumeTurns()
		refusingAfterResume = s.refusing.Load()
	}

	s.end(RealtimeClosePolicy, "project budget exhausted")

	if ctxDoneAtResume {
		t.Fatal("the context was already cancelled during the close handshake, so this test no " +
			"longer exercises the window it exists for; re-check end()'s ordering")
	}
	if !refusingAfterResume {
		t.Error("a late gate Allow cleared the refusal while the session was closing: the uplink " +
			"would resume forwarding to the provider on an exhausted budget for the rest of the " +
			"close handshake")
	}
}

func TestRealtime_EndRefusesBeforeItCloses(t *testing.T) {
	h := NewHandler(newDispatchSpy(), nil, nil)
	client := newScriptedClientSocket()
	prov := newFakeProviderSocket()
	s := newTestSession(t, h, client, prov)

	var refusingAtClose, ctxDoneAtClose bool
	client.onClose = func() {
		refusingAtClose = s.refusing.Load()
		select {
		case <-s.ctx.Done():
			ctxDoneAtClose = true
		default:
		}
	}

	s.end(RealtimeClosePolicy, "project budget exhausted")

	if !refusingAtClose {
		t.Error("the session was still forwarding client events to the provider when the close " +
			"handshake started; an exhausted budget must stop spend IMMEDIATELY, and the " +
			"handshake runs for up to 5 s")
	}
	if ctxDoneAtClose {
		t.Error("the session context was already cancelled when the close began; the library " +
			"then closes the connection abruptly and the caller never learns WHY the session ended")
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(time.Second):
		t.Error("the session context was never cancelled; the pump goroutines would never exit")
	}
	if got := client.code(); got != RealtimeClosePolicy {
		t.Errorf("close code = %d, want %d", got, RealtimeClosePolicy)
	}
}

// TestRealtime_ExhaustedBudgetStopsForwardingAtTheRegate wires the property
// above to the path that needs it. It is the regate 402 branch, driven
// directly, and it fails if that branch stops going through end().
func TestRealtime_ExhaustedBudgetStopsForwardingAtTheRegate(t *testing.T) {
	gate := newRecordingGate(failmode.Block402)
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{inputRateNano: 1000}),
		WithRealtimeBudgetRecheck(10*time.Millisecond))
	client := newScriptedClientSocket()
	prov := newFakeProviderSocket()
	s := newTestSession(t, h, client, prov)

	refusingAtClose := make(chan bool, 1)
	client.onClose = func() { refusingAtClose <- s.refusing.Load() }

	go s.regate()
	select {
	case got := <-refusingAtClose:
		if !got {
			t.Fatal("the exhausted-budget close began while the uplink was still forwarding; " +
				"the refusal flag has to be set FIRST")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the exhausted budget never closed the session")
	}
}

// TestRealtime_CloseRealtimeSessionsDoesNotBurnItsBudgetOnASilentPeer is the
// consequence reviewers reproduced.
//
// coder/websocket's Close waits, for a hardcoded 5 s, to read the peer's close
// reply — and it needs the connection readMu, which the uplink holds while it
// is parked in Read. One caller that stops reading therefore consumed the WHOLE
// shutdown budget. Measured before the bound: 5.001 s of a 5 s budget, and the
// wait returned with the session STILL LIVE, so the billing drain that runs
// next closed billing while that session's last turn was in flight.
func TestRealtime_CloseRealtimeSessionsDoesNotBurnItsBudgetOnASilentPeer(t *testing.T) {
	f := newRealtimeFixture(t)
	conn := f.dial(t)
	_ = conn // deliberately never read: the peer never answers the close frame

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), RealtimeCloseTimeout)
	defer cancel()
	f.handler.CloseRealtimeSessions(ctx)
	elapsed := time.Since(start)

	select {
	case <-f.prov.closed:
	default:
		t.Fatalf("CloseRealtimeSessions returned after %v with the session still live; the "+
			"billing drain runs next and would refuse that session's last turn", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("one silent peer cost %v of the %v shutdown budget; every other session on the "+
			"replica shares that budget", elapsed, RealtimeCloseTimeout)
	}
}

// ── DEFECT I: the admission basis and the billing basis can disagree ────────

// tokensOnlyCatalogEstimator prices TOKENS from the catalogue and holds no
// per-second rate at all. It is the shape of a conversational realtime row.
type tokensOnlyCatalogEstimator struct{}

func (tokensOnlyCatalogEstimator) Cost(_ context.Context, _, _ string, in, out int64) cost.Cost {
	return cost.Cost{TotalNanoUSD: in*1000 + out*2000, Basis: cost.BasisTokens, Source: cost.SourceCatalog}
}

func (e tokensOnlyCatalogEstimator) CostUnits(ctx context.Context, p, m string, u cost.Units) cost.Cost {
	if u.Basis() != cost.BasisTokens {
		// No rate for this basis. cost.Cost{} is what an uncatalogued unit
		// resolves to, and it is what makes the turn bill nothing.
		return cost.Cost{}
	}
	return e.Cost(ctx, p, m, u.InputTokens, u.OutputTokens)
}

// TestRealtime_TurnOnAnUnpricedBasisIsCounted.
//
// H2's probe accepts EITHER basis, and a turn bills on whatever the provider
// reports — so the two can disagree. A model the catalogue prices by TOKENS is
// admitted, the provider reports a DURATION for every turn, updateUsageUnits
// finds no per-second rate and answers billNotBillable, and NOTHING on this
// route moved: not turns_billed, not turns_unpriced. The session was H2
// admitted and billed zero for its whole life, in silence.
func TestRealtime_TurnOnAnUnpricedBasisIsCounted(t *testing.T) {
	beforeMismatch := realtimeTurnBasisMismatch.Value()
	beforeBilled := realtimeTurnsBilled.Value()

	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	gate := newRecordingGate(failmode.Allow)
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithBudgetGate(gate, tokensOnlyCatalogEstimator{}),
		WithRealtimeBudgetRecheck(time.Hour))
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=gpt-4o-realtime-preview",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("a token-priced model must still be admitted (decision H2): %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	prov.send(t, `{"type":"conversation.item.input_audio_transcription.completed",`+
		`"transcript":"hi","usage":{"type":"duration","seconds":3.5}}`)
	readEvent(t, conn, 3*time.Second)

	deadline := time.After(3 * time.Second)
	for realtimeTurnBasisMismatch.Value() == beforeMismatch {
		select {
		case <-deadline:
			t.Fatal("a turn reported a basis the catalogue does not price for this model; it " +
				"billed zero and no counter moved, so an H2-admitted session bills nothing in silence")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := gate.projectUpdates(); len(got) != 0 {
		t.Fatalf("the unpriced-basis turn produced %d billing increments; want 0", len(got))
	}
	if realtimeTurnsBilled.Value() != beforeBilled {
		t.Fatal("the turn was reported as billed while nothing reached the counter")
	}
}

// secondsOnlyCatalogEstimator prices SECONDS from the catalogue and holds NO
// token rate — the shape of a whisper-style transcription row.
//
// Its token arm is the part that matters: it returns a NON-ZERO cost whose
// Source is the FALLBACK, which is exactly what the real calculator does. The
// token basis never fails, because lookupCatalog fills NULL token columns from
// defaultPrice, so an un-catalogued token price is an INVENTED 1.0/3.0
// USD-per-1M figure rather than nothing.
type secondsOnlyCatalogEstimator struct{}

func (secondsOnlyCatalogEstimator) Cost(_ context.Context, _, _ string, in, out int64) cost.Cost {
	// The fabricated fallback. Non-zero, plausible, and NOT from the catalogue.
	return cost.Cost{TotalNanoUSD: in*1000 + out*3000, Basis: cost.BasisTokens, Source: cost.SourceFallback}
}

func (e secondsOnlyCatalogEstimator) CostUnits(ctx context.Context, p, m string, u cost.Units) cost.Cost {
	if u.Basis() == cost.BasisSeconds {
		return cost.Cost{TotalNanoUSD: u.InputMillis, Basis: cost.BasisSeconds, Source: cost.SourceCatalog}
	}
	return e.Cost(ctx, p, m, u.InputTokens, u.OutputTokens)
}

// TestRealtime_TurnOnAnUnpricedBasisIsNotBilledFromAFabricatedPrice is the
// OTHER direction of the basis disagreement, and the one that costs money.
//
// A model the catalogue prices by SECONDS alone is admitted by H2. The provider
// then reports TOKENS for a turn. Units.Basis() is tokens, so CostUnits takes
// the token arm — and that arm never fails: NULL token columns are filled from
// the legacy default table, so the turn prices at an invented rate. Counting the
// mismatch and billing anyway put a made-up figure on the authoritative counter
// of a socket the tenant holds open, which is the precise outcome decision H2
// was taken to prevent at the upgrade.
//
// The turn must therefore bill NOTHING. The catalogue prices this model, but
// not in the units this turn reported, so the gateway does not know what the
// turn cost — and a number it does not know is never a number it invents.
func TestRealtime_TurnOnAnUnpricedBasisIsNotBilledFromAFabricatedPrice(t *testing.T) {
	beforeMismatch := realtimeTurnBasisMismatch.Value()
	beforeBilled := realtimeTurnsBilled.Value()

	prov := newFakeProviderSocket()
	dialer := &fakeRealtimeDialer{sock: prov}
	gate := newRecordingGate(failmode.Allow)
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithRealtimeDialer(dialer),
		WithBudgetGate(gate, secondsOnlyCatalogEstimator{}),
		WithRealtimeBudgetRecheck(time.Hour))
	srv := httptest.NewServer(h.route())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/llm/v1/realtime?model=whisper-realtime",
		&websocket.DialOptions{HTTPHeader: http.Header{headerProjectID: []string{realtimeProjectID}}})
	if err != nil {
		t.Fatalf("a seconds-priced model must still be admitted (decision H2): %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// response.done reports TOKENS, which this model has no catalogue rate for.
	prov.send(t, responseDoneFrame(120, 40))
	readEvent(t, conn, 3*time.Second)

	deadline := time.After(3 * time.Second)
	for realtimeTurnBasisMismatch.Value() == beforeMismatch {
		select {
		case <-deadline:
			t.Fatal("a turn reported a basis the catalogue does not price for this model and the " +
				"mismatch was never counted")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The counter having moved is not the point. THIS is:
	if got := gate.projectUpdates(); len(got) != 0 {
		t.Fatalf("the turn billed %d increment(s) from a price the catalogue never supplied; "+
			"want 0 — decision H2 exists so a tenant-held socket cannot bill an invented rate", len(got))
	}
	if realtimeTurnsBilled.Value() != beforeBilled {
		t.Fatal("the turn was reported as billed while its price came from the default table")
	}
}

// TestRealtime_TurnOnAPricedBasisIsNotCounted is the discriminating half: the
// mismatch counter must stay still on the ordinary path, or an operator cannot
// alarm on it.
func TestRealtime_TurnOnAPricedBasisIsNotCounted(t *testing.T) {
	before := realtimeTurnBasisMismatch.Value()
	f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(time.Hour))
	conn := f.dial(t)
	f.prov.send(t, responseDoneFrame(10, 5))
	readEvent(t, conn, 3*time.Second)
	f.gate.waitForProjectUpdates(t, 1)
	if realtimeTurnBasisMismatch.Value() != before {
		t.Fatal("a normally-priced turn raised the basis-mismatch counter; the signal is unusable")
	}
}

// ── DEFECT J: a turn whose billing increment is refused ─────────────────────

// TestRealtime_RefusedTurnBillingIsAlarmed.
//
// billTurn discarded billRefused. That outcome means real, PROVIDER-REPORTED
// spend was dropped because billing is already draining — the tenant used the
// model and nothing charged for it. streamSettler treats the identical
// condition as alarmable and publishes budget.unbilled_stream; a session is not
// different, so it publishes the same event, with the outcome naming this
// surface.
func TestRealtime_RefusedTurnBillingIsAlarmed(t *testing.T) {
	events := newRecordingEvents()
	f := newRealtimeFixture(t, WithOpsEventPublisher(events), WithRealtimeBudgetRecheck(time.Hour))
	conn := f.dial(t)

	// Close billing under the live session, exactly as a rolling deploy does
	// when a session outlives the drain phase.
	f.handler.DrainBilling()
	f.prov.send(t, responseDoneFrame(100, 50))
	readEvent(t, conn, 3*time.Second)

	events.waitForEvent(t)
	got := events.decodeUnbilled(t)
	if got.Reason != lossReasonBillingRefused {
		t.Fatalf("reason = %q, want %q", got.Reason, lossReasonBillingRefused)
	}
	if got.DrainOutcome != realtimeDrainOutcome {
		t.Fatalf("drain_outcome = %q, want %q: the alarm has to say WHICH surface lost the spend",
			got.DrainOutcome, realtimeDrainOutcome)
	}
	if got.ProjectID != realtimeProjectID {
		t.Fatalf("project_id = %q, want %q", got.ProjectID, realtimeProjectID)
	}
}

// TestRealtime_BilledTurnPublishesNothing is the discriminating half. The
// unbilled event is the one signal that detects real loss; a route that
// published it on every clean turn would drown it.
func TestRealtime_BilledTurnPublishesNothing(t *testing.T) {
	events := newRecordingEvents()
	f := newRealtimeFixture(t, WithOpsEventPublisher(events), WithRealtimeBudgetRecheck(time.Hour))
	conn := f.dial(t)
	f.prov.send(t, responseDoneFrame(100, 50))
	readEvent(t, conn, 3*time.Second)
	f.gate.waitForProjectUpdates(t, 1)

	select {
	case <-events.fired:
		t.Fatal("a clean, billed turn published budget.unbilled_stream")
	case <-time.After(200 * time.Millisecond):
	}
}

// ── DEFECT K: the refused-turns counter counted ticks ───────────────────────

// TestRealtime_DroppedFramesAreCountedPerFrameNotPerTick.
//
// realtimeTurnsRefused.Add(1) sat in refuseTurns, which regate calls once per
// re-check TICK — while the frames actually dropped are dropped by the uplink's
// refusing check, which counted nothing. At the shipped 15 s interval an
// operator read "1" while a caller streaming 20 audio frames a second had 300
// of them thrown away. The two are different quantities and both are published
// now.
func TestRealtime_DroppedFramesAreCountedPerFrameNotPerTick(t *testing.T) {
	beforeDropped := realtimeFramesDropped.Value()
	beforeTurns := realtimeTurnsRefused.Value()

	// One tick refuses; the next three would close the session, so the interval
	// is long enough for the assertions to finish inside the first one.
	f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(750*time.Millisecond))
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block503)
	if typ, _ := readEvent(t, conn, 3*time.Second); typ != "error" {
		t.Fatalf("expected the refusal event first, got %q", typ)
	}

	const frames = 8
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for i := 0; i < frames; i++ {
		if err := conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"input_audio_buffer.append","audio":"QQ=="}`)); err != nil {
			t.Fatalf("write audio frame %d: %v", i, err)
		}
	}

	deadline := time.After(3 * time.Second)
	for realtimeFramesDropped.Value()-beforeDropped < frames {
		select {
		case <-deadline:
			t.Fatalf("the gateway dropped %d client frames and counted %d; the counter reports "+
				"re-check ticks, not the traffic a refusal really stopped",
				frames, realtimeFramesDropped.Value()-beforeDropped)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := realtimeTurnsRefused.Value() - beforeTurns; got != 0 {
		t.Fatalf("turns_refused moved by %d while the caller sent no turn-start event at all; "+
			"that counter must count TURNS", got)
	}
}

// TestRealtime_ARefusedTurnStartCountsAsATurn is the other half. The only
// caller this route has sends no turn-start event, so turns_refused would read
// zero for ever if nothing ever raised it — a counter that is always zero is
// indistinguishable from a broken one.
func TestRealtime_ARefusedTurnStartCountsAsATurn(t *testing.T) {
	before := realtimeTurnsRefused.Value()
	f := newRealtimeFixture(t, WithRealtimeBudgetRecheck(time.Hour))
	conn := f.dial(t)
	f.gate.setVerdict(failmode.Block503)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write response.create: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for realtimeTurnsRefused.Value() == before {
		select {
		case <-deadline:
			t.Fatal("a turn start the budget gate refused was not counted as a refused turn")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if frame, ok := f.prov.received(t, 200*time.Millisecond); ok {
		t.Fatalf("the refused turn reached the provider: %s", frame)
	}
}

// TestRealtime_SubprotocolNegotiation covers the accept-side change the
// Accept-before-dial order forced.
//
// The provider's subprotocol is no longer known when the caller's socket is
// accepted, so the gateway offers this surface's ONE name. Both callers must
// still work: the legacy relay asks for none, and a browser client asks for
// "realtime". RFC 6455 always allows the empty one.
func TestRealtime_SubprotocolNegotiation(t *testing.T) {
	f := newRealtimeFixture(t)
	for _, tc := range []struct {
		name  string
		offer []string
		want  string
	}{
		{"the relay offers none", nil, ""},
		{"a client offers realtime", []string{realtimeSubprotocol}, realtimeSubprotocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, err := websocket.Dial(t.Context(), f.realtimeURL("gpt-4o-realtime-preview"),
				&websocket.DialOptions{
					HTTPHeader:   http.Header{headerProjectID: []string{realtimeProjectID}},
					Subprotocols: tc.offer,
				})
			if err != nil {
				t.Fatalf("dial with subprotocols %v: %v", tc.offer, err)
			}
			defer func() { _ = conn.CloseNow() }()
			if got := conn.Subprotocol(); got != tc.want {
				t.Fatalf("negotiated subprotocol = %q, want %q", got, tc.want)
			}
		})
	}
}

// ── the mid-session gate is a RE-CHECK, not a request ───────────────────────

// loopBreakerHits counts every timestamp the backstop's sliding window holds.
// It is the quantity the re-check used to pollute.
func loopBreakerHits(b *loopBreaker) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, ts := range b.hits {
		n += len(ts)
	}
	return n
}

// TestRealtime_TheBudgetRecheckIsNotCountedAsARequest is the session-level half
// of the loop-breaker separation.
//
// THE DEFECT. gateVerdict called admissionVerdict, and admissionVerdict records
// a hit in the per-(project, model) amplification backstop. The re-check runs on
// a ticker for the whole life of every session, so the gateway's own gating work
// arrived in that window as if it were tenant traffic. Enough long sessions on
// one project and model then opened the circuit for that project's REAL /llm
// requests — an availability defect the gateway caused itself.
//
// The threshold here is deliberately far above what the test produces. The
// assertion is on the WINDOW, not on the circuit: a circuit that trips deletes
// its own hit list, so a test that waited for the trip would read zero and could
// not tell the two worlds apart.
func TestRealtime_TheBudgetRecheckIsNotCountedAsARequest(t *testing.T) {
	f := newRealtimeFixture(t, WithLoopBreakerParams(LoopBreakerParams{
		Threshold: 1000,
		Window:    time.Minute,
		OpenFor:   time.Minute,
	}))
	conn := f.dial(t)
	defer func() { _ = conn.CloseNow() }()
	f.dialer.waitForDial(t)

	// One gate call for the UPGRADE, then ten re-check ticks.
	const ticks = 10
	f.gate.waitForChecks(t, 1+ticks)

	if got := loopBreakerHits(f.handler.loopGuard); got != 1 {
		t.Fatalf("the backstop window holds %d hits after %d budget re-checks, want exactly 1 "+
			"(the upgrade). A re-check is not an arrival: counting it lets long sessions open the "+
			"circuit for the project's real /llm traffic", got, ticks)
	}
	// The tuple must also still admit an arriving request.
	_, _, model := f.dialer.dialed()
	if ok, _ := f.handler.loopGuard.observe(realtimeProjectID, model); !ok {
		t.Fatal("the backstop circuit is open for the session's own tuple; the re-checks opened it")
	}
}

// TestRealtime_ATrippedBackstopRefusesTurnsButKeepsTheSession separates the two
// non-Allow verdicts decision H1 does NOT cover.
//
// THE DEFECT. regate's default branch counted EVERY non-Allow, non-402 verdict
// toward maxBudgetOutagesBeforeClose. H1 was authored for one condition: the
// budget store did not answer. A tripped amplification backstop is a different
// condition with a different lifetime — the circuit opens for 5 s by default —
// so applying H1 to it tore down live calls under a policy nobody wrote for
// them, and for a condition that had already cleared.
//
// The session must refuse turns, tell the caller, and STAY OPEN.
func TestRealtime_ATrippedBackstopRefusesTurnsButKeepsTheSession(t *testing.T) {
	const threshold = 3
	f := newRealtimeFixture(t,
		WithLoopBreakerParams(LoopBreakerParams{
			Threshold: threshold,
			Window:    time.Minute,
			// Longer than the whole observation below, so the circuit cannot
			// close again and make the session survive for the wrong reason.
			OpenFor: time.Minute,
		}),
		// Fast enough that many more than maxBudgetOutagesBeforeClose ticks run
		// inside the observation window.
		WithRealtimeBudgetRecheck(5*time.Millisecond))
	conn := f.dial(t)
	defer func() { _ = conn.CloseNow() }()
	f.dialer.waitForDial(t)
	_, _, model := f.dialer.dialed()

	// Ordinary /llm traffic on the SAME tuple opens the circuit. allow() is what
	// an arriving request calls, so this is that traffic and not a stub.
	opened := false
	for i := 0; i < threshold+1 && !opened; i++ {
		ok, _ := f.handler.loopGuard.allow(realtimeProjectID, model)
		opened = !ok
	}
	if !opened {
		t.Fatalf("the backstop circuit never opened after %d requests at threshold %d", threshold+1, threshold)
	}

	// The caller is told, once, in the shape every /llm refusal uses.
	typ, frame := readEvent(t, conn, 3*time.Second)
	if typ != string(schemas.RTEventError) {
		t.Fatalf("first client frame is %q, want an error event", typ)
	}
	var ev realtimeErrorEvent
	if err := json.Unmarshal(frame, &ev); err != nil {
		t.Fatalf("the refusal frame is not an error event: %v", err)
	}
	if ev.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("refusal code = %q, want rate_limit_exceeded: the caller must be able to tell a "+
			"backstop refusal from a budget one", ev.Error.Code)
	}

	// Far more re-check ticks than maxBudgetOutagesBeforeClose now run against
	// the open circuit. The session must survive every one of them.
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	for {
		_, _, rerr := conn.Read(ctx)
		if rerr == nil {
			continue // a duplicate refusal event is a separate defect, not this one
		}
		if errors.Is(rerr, context.DeadlineExceeded) {
			return // the socket is still open, which is the whole assertion
		}
		t.Fatalf("the session closed (%v, close status %v) while the backstop circuit was open. "+
			"Decision H1's consecutive-outage counter is for a budget store that did not answer; "+
			"a tripped circuit breaker must refuse turns and KEEP the socket", rerr, websocket.CloseStatus(rerr))
	}
}

// ── one deadline per keepalive ping ─────────────────────────────────────────

// blockingPingSocket never answers a ping. It is the slow peer.
type blockingPingSocket struct {
	fakeProviderSocket
}

func (b *blockingPingSocket) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// deadlinePingSocket answers at once and records how much budget its ping had.
type deadlinePingSocket struct {
	fakeProviderSocket

	mu          sync.Mutex
	remaining   time.Duration
	hadDeadline bool
}

func (d *deadlinePingSocket) Ping(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	d.mu.Lock()
	d.hadDeadline = ok
	if ok {
		d.remaining = time.Until(dl)
	}
	d.mu.Unlock()
	// A REAL socket fails a ping on a context that has already run out. Answering
	// nil here would hide the whole defect: the shared-deadline version started
	// this call on an expired context, and a fake that ignores that reports a
	// healthy peer in both worlds.
	return ctx.Err()
}

func (d *deadlinePingSocket) budget() (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remaining, d.hadDeadline
}

// TestRealtime_EachKeepalivePingGetsItsOwnDeadline is the false-liveness defect.
//
// THE DEFECT. The two pings shared ONE context and ran one after the other.
// coder/websocket's Ping waits for the pong, so a caller that was slow to answer
// spent the whole shared budget. The provider ping then started on a context
// that had already expired, answered "context deadline exceeded", and keepalive
// tore the session down — reporting a healthy provider as a dead peer.
//
// The assertion is on the PROVIDER's result and on the PROVIDER's budget. Both
// come from the same defect and both are needed: the error alone could be fixed
// by swallowing it, and the budget alone could be satisfied by a deadline that
// is never enforced.
func TestRealtime_EachKeepalivePingGetsItsOwnDeadline(t *testing.T) {
	const bound = 150 * time.Millisecond
	h := NewHandler(newDispatchSpy(), nil, nil)
	// Not an operator knob: the production bound is realtimeWriteTimeout and a
	// test cannot wait 10 s for one ping.
	// TestRealtimeDefaultsAreArmedWithoutOptions pins the default.
	h.realtimePingBound = bound

	slow := &blockingPingSocket{}
	fast := &deadlinePingSocket{}
	s := newTestSession(t, h, slow, fast)

	start := time.Now()
	clientErr, providerErr := s.pingPeers()
	elapsed := time.Since(start)

	if clientErr == nil {
		t.Fatal("the slow CLIENT ping reported success; the test peer never answers")
	}
	if providerErr != nil {
		t.Fatalf("the PROVIDER ping failed with %v although the provider answered at once. "+
			"A slow caller must not spend the provider's ping budget: a shared deadline turns a "+
			"healthy provider into a failed peer and tears the session down", providerErr)
	}
	got, hadDeadline := fast.budget()
	if !hadDeadline {
		t.Fatal("the provider ping ran with no deadline at all; a peer that stops reading would park the pinger")
	}
	if got <= bound/2 {
		t.Fatalf("the provider ping had %v of its %v budget left. It must get a FULL deadline of its "+
			"own, not what the client ping left over", got, bound)
	}
	if elapsed >= 2*bound {
		t.Fatalf("one keepalive round took %v, which is more than two ping budgets (%v). The two "+
			"pings must not be serialised behind one another", elapsed, 2*bound)
	}
}
