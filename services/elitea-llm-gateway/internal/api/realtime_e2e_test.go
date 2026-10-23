package api

// realtime_e2e_test.go — the end-to-end proof that /llm/v1/realtime is MOUNTED
// and that a turn on it is billed.
//
// It is deliberately in this package and not in llmproxy. The handler tests
// there build their own chi router, so they cannot see a route that is missing
// from internal/api/router.go — which is the mistake this file exists to catch.
// Everything below runs against NewRouter, over a real listener, with a real
// client WebSocket performing a real RFC 6455 handshake.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// headerProjectID is the edge's signed project header. The gateway's own
// constant is unexported; the literal is the wire contract either way.
const headerProjectID = "X-Elitea-Project-Id"

// e2eProviderSocket is the provider half of the session. The caller's half is a
// real socket, so only this one is faked.
type e2eProviderSocket struct {
	toGateway   chan []byte
	fromGateway chan []byte
	closed      chan struct{}
	once        sync.Once
}

func newE2EProviderSocket() *e2eProviderSocket {
	return &e2eProviderSocket{
		toGateway:   make(chan []byte, 8),
		fromGateway: make(chan []byte, 8),
		closed:      make(chan struct{}),
	}
}

func (s *e2eProviderSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.toGateway:
		return b, nil
	case <-s.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *e2eProviderSocket) Write(ctx context.Context, frame []byte) error {
	select {
	case s.fromGateway <- append([]byte(nil), frame...):
		return nil
	case <-s.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *e2eProviderSocket) Ping(context.Context) error { return nil }

func (s *e2eProviderSocket) Close(llmproxy.RealtimeCloseCode, string) {
	s.once.Do(func() { close(s.closed) })
}

// e2eCodec is bifrost's own OpenAI realtime codec. Its four methods read no
// provider field, so the zero value is the real implementation.
type e2eCodec struct{ p openai.OpenAIProvider }

func (c *e2eCodec) ToBifrostRealtimeEvent(raw json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	return c.p.ToBifrostRealtimeEvent(raw)
}

func (c *e2eCodec) ToProviderRealtimeEvent(ev *schemas.BifrostRealtimeEvent) (json.RawMessage, error) {
	return c.p.ToProviderRealtimeEvent(ev)
}

func (c *e2eCodec) ShouldStartRealtimeTurn(ev *schemas.BifrostRealtimeEvent) bool {
	return c.p.ShouldStartRealtimeTurn(ev)
}

func (c *e2eCodec) ExtractRealtimeTurnUsage(raw []byte) *schemas.BifrostLLMUsage {
	return c.p.ExtractRealtimeTurnUsage(raw)
}

type e2eDialer struct {
	sock *e2eProviderSocket

	mu     sync.Mutex
	params url.Values
}

func (d *e2eDialer) DialRealtime(_ *schemas.BifrostContext, _ schemas.ModelProvider, _ string, params url.Values) (*llmproxy.RealtimeUpstream, error) {
	d.mu.Lock()
	d.params = params
	d.mu.Unlock()
	return &llmproxy.RealtimeUpstream{Socket: d.sock, Codec: &e2eCodec{}, Subprotocol: "realtime"}, nil
}

// dialedParams returns the query the route forwarded onto the provider dial.
func (d *e2eDialer) dialedParams() url.Values {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.params
}

// e2eGate admits everything and records what it was asked to bill.
type e2eGate struct {
	mu      sync.Mutex
	billed  []int64
	ids     []string
	changed chan struct{}
}

func (g *e2eGate) CheckBudget(context.Context, int, string, string, int64, int64) (failmode.Decision, error) {
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
}

func (g *e2eGate) UpdateUsage(_ context.Context, _ int, scope, _, eventID string, costNano, _, _ int64, _ *failmode.UsageDimensions) error {
	if scope != failmode.ScopeProject {
		return nil
	}
	g.mu.Lock()
	g.billed = append(g.billed, costNano)
	g.ids = append(g.ids, eventID)
	g.mu.Unlock()
	select {
	case g.changed <- struct{}{}:
	default:
	}
	return nil
}

func (g *e2eGate) TryAlertCooldown(context.Context, string, string, int64) (bool, error) {
	return false, nil
}

func (g *e2eGate) snapshot() ([]int64, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int64(nil), g.billed...), append([]string(nil), g.ids...)
}

// e2eCosts prices one token at one nano-USD from the CATALOG, which is what the
// realtime admission gate demands (an un-catalogued model is refused).
type e2eCosts struct{}

func (e2eCosts) Cost(_ context.Context, _, _ string, in, out int64) cost.Cost {
	return cost.Cost{TotalNanoUSD: in + out, Basis: cost.BasisTokens, Source: cost.SourceCatalog}
}

func (c e2eCosts) CostUnits(ctx context.Context, provider, model string, u cost.Units) cost.Cost {
	return c.Cost(ctx, provider, model, u.InputTokens, u.OutputTokens)
}

// TestRealtimeRouteIsMountedAndBillsATurn is the end-to-end acceptance test.
//
// It drives a real WebSocket handshake against the router NewRouter builds,
// sends a client event, watches it reach the provider, has the provider answer
// with a completed turn, and asserts BOTH that the caller received the frame and
// that the turn was billed. A unit test of the pump proves none of that: a pump
// with no route is a pump nobody can reach, and the /llm/v1/embeddings defect is
// exactly the shape of a correct handler behind a missing route.
func TestRealtimeRouteIsMountedAndBillsATurn(t *testing.T) {
	prov := newE2EProviderSocket()
	gate := &e2eGate{changed: make(chan struct{}, 8)}
	dialer := &e2eDialer{sock: prov}
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil,
		llmproxy.WithRealtimeDialer(dialer),
		llmproxy.WithBudgetGate(gate, e2eCosts{}),
	)
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") +
		"/llm/v1/realtime?model=gpt-4o-realtime-preview&intent=transcription"
	conn, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{headerProjectID: []string{"42"}},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("the realtime handshake failed against the mounted router: %v (status=%d)", err, status)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// The relay's `intent` must reach the provider dial. It selects the
	// TRANSCRIPTION session mode, bifrost's URL builder does not carry it, and
	// without it the only caller this route has gets a conversational session.
	// The dial runs AFTER the upgrade, so it is asserted once the session is
	// proven live below rather than here.

	// The relay's opening frame must reach the provider.
	if err := conn.Write(ctx, websocket.MessageText, []byte(
		`{"type":"transcription_session.update","session":{"input_audio_format":"pcm16"}}`)); err != nil {
		t.Fatalf("write the session update: %v", err)
	}
	select {
	case got := <-prov.fromGateway:
		if !strings.Contains(string(got), "transcription_session.update") {
			t.Fatalf("the provider received %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client event never reached the provider")
	}
	if got := dialer.dialedParams().Get("intent"); got != "transcription" {
		t.Fatalf("the provider dial carried intent=%q, want \"transcription\": the legacy relay "+
			"selects the transcription session mode with it", got)
	}

	// One completed turn, with usage.
	prov.toGateway <- []byte(
		`{"type":"response.done","response":{"usage":{"input_tokens":40,"output_tokens":2,"total_tokens":42}}}`)

	_, frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read the provider event: %v", err)
	}
	if !strings.Contains(string(frame), "response.done") {
		t.Fatalf("the caller received %s", frame)
	}

	deadline := time.After(5 * time.Second)
	for {
		if billed, ids := gate.snapshot(); len(billed) > 0 {
			if billed[0] != 42 {
				t.Fatalf("the turn billed %d nano-USD, want 42 (40 input + 2 output)", billed[0])
			}
			if ids[0] == "" {
				t.Fatal("the turn was billed under an empty event id; NATS de-duplicates on it")
			}
			return
		}
		select {
		case <-gate.changed:
		case <-deadline:
			t.Fatal("the completed turn was never billed")
		}
	}
}

// TestRealtimeRouteAnswersGET is the route-shape guard. A WebSocket handshake is
// a GET, and a route registered only for POST answers 405 to every real client.
func TestRealtimeRouteAnswersGET(t *testing.T) {
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/realtime?model=m", nil)
	rec := httptest.NewRecorder()
	NewRouter(h).ServeHTTP(rec, req)
	if rec.Code == http.StatusMethodNotAllowed || rec.Code == http.StatusNotFound {
		t.Fatalf("GET /llm/v1/realtime = %d; the handshake method must reach the handler", rec.Code)
	}
}
