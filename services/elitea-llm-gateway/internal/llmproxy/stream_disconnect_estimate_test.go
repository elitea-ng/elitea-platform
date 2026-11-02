package llmproxy

// stream_disconnect_estimate_test.go — issue #1201, end to end through REAL
// bifrost core and a fake OpenAI upstream over HTTP.
//
// A streaming caller that disconnects mid-stream must make the upstream see
// its request cancelled promptly (the model slot is free), and the request log
// and the bill must carry an estimate that is not below what was relayed. A
// normal stream and a non-streaming disconnect must not change.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

const (
	slowUpstreamChunks   = 200 // 200 chunks x 50 ms = 10 s of generation
	slowUpstreamInterval = 50 * time.Millisecond
	slowUpstreamPrompt   = 9
)

// slowOpenAIUpstream is a fake OpenAI /v1/chat/completions. A streaming
// request gets one "tok" chunk every slowUpstreamInterval, then a usage
// chunk. A unary request waits for its own request context to end. It
// records the moment each request context ended, which is the moment the
// upstream saw the cancel.
type slowOpenAIUpstream struct {
	*httptest.Server
	hits      atomic.Int64
	mu        sync.Mutex
	cancelled []time.Time
	done      chan struct{} // receives one value per finished request
}

func newSlowOpenAIUpstream(t *testing.T, chunks int) *slowOpenAIUpstream {
	t.Helper()
	u := &slowOpenAIUpstream{done: make(chan struct{}, 16)}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		defer func() { u.done <- struct{}{} }()
		raw, _ := io.ReadAll(r.Body)
		ctx := r.Context()
		if !strings.Contains(string(raw), `"stream":true`) {
			// Unary: a long generation that only a cancel ends.
			<-ctx.Done()
			u.markCancelled()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(data string) bool {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		for i := range chunks {
			chunk := fmt.Sprintf(`{"id":"c%d","object":"chat.completion.chunk","created":1,"model":"gpt-4o",`+
				`"choices":[{"index":0,"delta":{"content":"tok"},"finish_reason":null}]}`, i)
			if !send(chunk) {
				u.markCancelled()
				return
			}
			select {
			case <-ctx.Done():
				u.markCancelled()
				return
			case <-time.After(slowUpstreamInterval):
			}
		}
		send(`{"id":"cz","object":"chat.completion.chunk","created":1,"model":"gpt-4o",` +
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		send(fmt.Sprintf(`{"id":"cu","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			slowUpstreamPrompt, chunks, slowUpstreamPrompt+chunks))
		send("[DONE]")
	}))
	// Hit counts matter here: one request must be one connection.
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	u.Server = srv
	t.Cleanup(srv.Close)
	return u
}

func (u *slowOpenAIUpstream) markCancelled() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cancelled = append(u.cancelled, time.Now())
}

func (u *slowOpenAIUpstream) cancelledAt() []time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]time.Time(nil), u.cancelled...)
}

// waitDone waits for one upstream request to finish.
func (u *slowOpenAIUpstream) waitDone(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-u.done:
	case <-time.After(within):
		t.Fatalf("the upstream request did not end within %v", within)
	}
}

// slowUpstreamAccount serves one OpenAI key on the fake upstream.
type slowUpstreamAccount struct{ baseURL string }

func (slowUpstreamAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (slowUpstreamAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{
		ID: "slow-key", Value: schemas.SecretVar{Val: "sk-test", SecretType: schemas.SecretTypePlainText},
		Models: []string{"*"}, Weight: 1,
	}}, nil
}

func (a slowUpstreamAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	cfg := &schemas.ProviderConfig{
		NetworkConfig:            schemas.NetworkConfig{BaseURL: a.baseURL},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 4, BufferSize: 10},
	}
	cfg.NetworkConfig.AllowPrivateNetwork = true
	return cfg, nil
}

// estimateHarness is the gateway (real bifrost core, request-log middleware,
// a recording budget gate) on a real HTTP server.
type estimateHarness struct {
	upstream *slowOpenAIUpstream
	h        *Handler
	gate     *fakeBudgetChecker
	calc     *fakeCostEstimator
	sink     *logCaptureSink
	recorder *requestlog.Recorder
	url      string
	served   chan struct{} // receives one value per request the gateway finished
}

func newEstimateHarness(t *testing.T, chunks int) *estimateHarness {
	t.Helper()
	up := newSlowOpenAIUpstream(t, chunks)
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: slowUpstreamAccount{baseURL: up.URL}, InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(core.Shutdown)

	gate := allowGate()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	// The full grace is armed: the old design would keep the upstream running
	// for up to 15 s after the hangup. The cancel must not wait for it.
	h := NewHandler(NewBifrostRouter(core), nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(newRecordingEvents()),
		WithStreamGrace(MaxStreamGrace))
	sink := &logCaptureSink{}
	recorder := requestlog.New(sink, discardLogger())
	served := make(chan struct{}, 16)
	logged := requestlog.Middleware(recorder)(h.route())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logged.ServeHTTP(w, r)
		served <- struct{}{}
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	t.Cleanup(srv.Close)
	return &estimateHarness{upstream: up, h: h, gate: gate, calc: calc, sink: sink, recorder: recorder, url: srv.URL, served: served}
}

// row stops the recorder and returns the one row it wrote.
func (e *estimateHarness) row(t *testing.T) requestlog.Record {
	t.Helper()
	// The row is emitted when the gateway handler returns.
	select {
	case <-e.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway handler did not return")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.recorder.Stop(ctx)
	rows := e.sink.all()
	if len(rows) != 1 {
		t.Fatalf("logged rows = %d, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// request starts a POST to the gateway. The caller owns the response.
func (e *estimateHarness) request(t *testing.T, ctx context.Context, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/llm/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "30")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	return client.Do(req)
}

const estimatePrompt = "please count to two hundred"

func estimateBody(stream bool) string {
	return fmt.Sprintf(`{"model":"openai/gpt-4o","stream":%t,"stream_options":{"include_usage":true},`+
		`"messages":[{"role":"user","content":%q}]}`, stream, estimatePrompt)
}

func TestStreamDisconnect_UpstreamCancelledPromptlyAndEstimateBilled(t *testing.T) {
	e := newEstimateHarness(t, slowUpstreamChunks)

	resp, err := e.request(t, context.Background(), estimateBody(true))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Read three frames, so the relayed output is known to be at least 3
	// chunks, then hang up.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for strings.Count(string(buf), "data: ") < 3 {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			t.Fatalf("read stream: %v", rerr)
		}
	}
	hungUp := time.Now()
	_ = resp.Body.Close()

	e.upstream.waitDone(t, 3*time.Second)
	cancelled := e.upstream.cancelledAt()
	if len(cancelled) != 1 {
		t.Fatalf("the upstream saw %d cancelled request(s), want 1 — it ran on after the hangup", len(cancelled))
	}
	if d := cancelled[0].Sub(hungUp); d > 2*time.Second {
		t.Errorf("the upstream saw the cancel %v after the hangup, want well under the %v grace", d, MaxStreamGrace)
	}
	if got := e.upstream.hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}

	e.gate.waitForUpdate(t)
	e.h.DrainBilling()
	row := e.row(t)

	relayed := int64(3) // three "tok" chunks reached the caller at least
	if row.ErrorCode != errorCodeUsageEstimated {
		t.Errorf("row error_code = %q, want %q", row.ErrorCode, errorCodeUsageEstimated)
	}
	if row.Status != http.StatusOK || !row.Streaming {
		t.Errorf("row status/streaming = %d/%v, want 200/true", row.Status, row.Streaming)
	}
	wantPrompt := tokensFromBytes(int64(len(estimatePrompt)))
	if row.PromptToks != wantPrompt {
		t.Errorf("row prompt tokens = %d, want the estimate %d", row.PromptToks, wantPrompt)
	}
	if row.OutputToks < relayed {
		t.Errorf("row completion tokens = %d, want >= %d (the relayed chunks)", row.OutputToks, relayed)
	}
	if got := e.gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want 1", got)
	}
	in, out := e.calc.getLastTokens()
	if in != row.PromptToks || out != row.OutputToks {
		t.Errorf("billed tokens = (%d, %d), want the logged (%d, %d) — the bill and the log must agree",
			in, out, row.PromptToks, row.OutputToks)
	}
}

func TestStreamComplete_RealUpstream_BillsProviderUsage(t *testing.T) {
	const chunks = 4
	e := newEstimateHarness(t, chunks)

	resp, err := e.request(t, context.Background(), estimateBody(true))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = resp.Body.Close()
	e.upstream.waitDone(t, 3*time.Second)

	e.gate.waitForUpdate(t)
	e.h.DrainBilling()
	row := e.row(t)

	if len(e.upstream.cancelledAt()) != 0 {
		t.Error("a stream read to the end was cancelled")
	}
	if row.ErrorCode != "" {
		t.Errorf("row error_code = %q, want empty on a complete stream", row.ErrorCode)
	}
	if row.PromptToks != slowUpstreamPrompt || row.OutputToks != chunks {
		t.Errorf("row tokens = (%d, %d), want the provider's (%d, %d)",
			row.PromptToks, row.OutputToks, slowUpstreamPrompt, chunks)
	}
	if in, out := e.calc.getLastTokens(); in != slowUpstreamPrompt || out != chunks {
		t.Errorf("billed tokens = (%d, %d), want (%d, %d)", in, out, slowUpstreamPrompt, chunks)
	}
}

func TestUnaryDisconnect_RealUpstream_CancelsAndBillsNothing(t *testing.T) {
	e := newEstimateHarness(t, slowUpstreamChunks)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		resp, err := e.request(t, ctx, estimateBody(false))
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	waitFor(t, func() bool { return e.upstream.hits.Load() == 1 })
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("the cancelled unary request returned no error")
	}

	e.upstream.waitDone(t, 3*time.Second)
	if len(e.upstream.cancelledAt()) != 1 {
		t.Fatal("the upstream did not see the unary request cancelled")
	}
	e.h.DrainBilling()
	row := e.row(t)
	if row.ErrorCode == errorCodeUsageEstimated {
		t.Errorf("a unary disconnect was marked %q; only a stream bills an estimate", errorCodeUsageEstimated)
	}
	if got := e.gate.updateCalls.Load(); got != 0 {
		t.Errorf("UpdateUsage calls = %d, want 0 — a unary request with no response has no usage", got)
	}
}

func TestPromptEstimate_CountsTextNotInlineMedia(t *testing.T) {
	text := "describe this picture"
	image := "data:image/png;base64," + strings.Repeat("QUJD", 50_000)
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{{
		Role: schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: &text},
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: image}},
		}},
	}}}
	if got, want := chatPromptEstimate(req), tokensFromBytes(int64(len(text))); got != want {
		t.Errorf("chatPromptEstimate = %d, want %d — inline media bytes are not prompt tokens", got, want)
	}
}

func TestRelayedOutputTokens_NeverBelowChunkCount(t *testing.T) {
	s := &streamSettler{}
	for range 10 {
		s.relayed(1) // ten one-byte chunks: bytes/4 says 3, chunks say 10
	}
	if got := s.relayedOutputTokens(); got != 10 {
		t.Errorf("relayedOutputTokens = %d, want 10", got)
	}
	s.relayed(400)
	if got := s.relayedOutputTokens(); got != tokensFromBytes(410) {
		t.Errorf("relayedOutputTokens = %d, want %d", got, tokensFromBytes(410))
	}
}
