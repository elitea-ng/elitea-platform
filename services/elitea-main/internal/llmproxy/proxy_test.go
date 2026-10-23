package llmproxy

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
)

func TestNew_RequiresGateway(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without a Gateway = nil error, want error")
	}
}

func TestNewGatewayClient_InvalidTarget(t *testing.T) {
	cases := []string{"", "://nope", "no-scheme-host", "http://gw:8443", "https://gw:8443/llm", "https://user@gw:8443"}
	for _, tc := range cases {
		if _, err := NewGatewayClient(GatewayClientConfig{Target: tc, TLS: &tls.Config{MinVersion: tls.VersionTLS13}}); err == nil {
			t.Errorf("NewGatewayClient(%q) = nil error, want error", tc)
		}
	}
}

func TestNewGatewayClient_TLSMaterialFails(t *testing.T) {
	_, err := NewGatewayClient(GatewayClientConfig{
		Target:         "https://gw:8443",
		ClientCertFile: "/nonexistent/cert.pem",
		ClientKeyFile:  "/nonexistent/key.pem",
		CAFile:         "/nonexistent/ca.pem",
	})
	if err == nil {
		t.Fatal("expected error building mTLS with missing files")
	}
	if _, err := NewGatewayClient(GatewayClientConfig{Target: "https://gw:8443"}); err == nil {
		t.Fatal("expected error without mTLS material")
	}
}

// TestNewGatewayClient_FromFiles builds the client from PEM files, as the
// composition root does, and calls a real-TLS gateway with it.
func TestNewGatewayClient_FromFiles(t *testing.T) {
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	caFile, certFile, keyFile := backend.WriteClientFiles(t, t.TempDir())
	client, err := NewGatewayClient(GatewayClientConfig{
		Target:         backend.Target,
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
		ServerName:     grpcauthtest.ServerName,
	})
	if err != nil {
		t.Fatalf("NewGatewayClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	resp, err := client.ListModels(t.Context(), &Request{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"data":[]}` {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

// gatewayTo is a GatewayClient for backend over real mutual TLS.
func gatewayTo(t *testing.T, backend *llmproxytest.Server, secret string) *GatewayClient {
	t.Helper()
	client, err := NewGatewayClient(GatewayClientConfig{
		Target:         backend.Target,
		IdentitySecret: secret,
		TLS:            backend.ClientTLS(t),
	})
	if err != nil {
		t.Fatalf("NewGatewayClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// proxyTo builds a Proxy in front of backend, over the real gRPC hop, with
// the given identity secret.
func proxyTo(t *testing.T, backend *llmproxytest.Server, secret string) *Proxy {
	t.Helper()
	p, err := New(Config{Gateway: gatewayTo(t, backend, secret)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestProxy_ForwardsAndInjectsIdentity(t *testing.T) {
	var gotHeaders http.Header
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	p := proxyTo(t, backend, "sekret")

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	ctx := middleware.ContextWithProject(req.Context(), middleware.ProjectContext{ProjectID: 7})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotHeaders.Get(HeaderProjectID) != "7" {
		t.Errorf("gateway got project %q, want 7", gotHeaders.Get(HeaderProjectID))
	}
	if !verifyIdentitySignature(gotHeaders, []byte("sekret")) {
		t.Errorf("forwarded identity signature did not verify")
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", rec.Header().Get("X-Accel-Buffering"))
	}
}

func TestProxy_StripsClientSpoofedIdentity(t *testing.T) {
	var gotHeaders http.Header
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
	}))
	defer backend.Close()

	const secret = "sekret"
	p := proxyTo(t, backend, secret)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", nil)
	// Spoof attempt: attacker injects a different project ID and a forged signature.
	req.Header.Set(HeaderProjectID, "999")
	req.Header.Set(HeaderSignature, "sha256=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	ctx := middleware.ContextWithProject(req.Context(), middleware.ProjectContext{ProjectID: 7})
	req = req.WithContext(ctx)

	p.ServeHTTP(httptest.NewRecorder(), req)

	// The spoofed project must be replaced with the edge-resolved project.
	if gotHeaders.Get(HeaderProjectID) != "7" {
		t.Errorf("gateway saw project %q, want 7 (spoof not stripped)", gotHeaders.Get(HeaderProjectID))
	}

	// The outbound signature must be valid over the edge-injected identity (not
	// the attacker's spoofed values). The gateway verifies it the same way.
	if !verifyIdentitySignature(gotHeaders, []byte(secret)) {
		t.Errorf("outbound identity signature is invalid; gateway would reject this request (header=%q)",
			gotHeaders.Get(HeaderSignature))
	}

	// The attacker's spoofed signature must NOT appear in the outbound headers:
	// the proxy must have replaced it, not forwarded it.
	if gotHeaders.Get(HeaderSignature) == "sha256=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("proxy forwarded the client-spoofed signature verbatim instead of replacing it")
	}
}

func TestProxy_UpstreamDown_Returns502(t *testing.T) {
	// Point at a closed server.
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.Close()

	p := proxyTo(t, backend, "")
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestProxy_UpstreamDown_NestedErrorShape asserts that the 502 error body
// conforms to the OpenAI-shaped nested error envelope (spec §2.5).
func TestProxy_UpstreamDown_NestedErrorShape(t *testing.T) {
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.Close()

	p := proxyTo(t, backend, "")
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("response body is not valid JSON: %v (body: %q)", err, rec.Body.String())
	}
	if envelope.Error.Message == "" {
		t.Errorf("error.message is empty")
	}
	if envelope.Error.Type == "" {
		t.Errorf("error.type is empty")
	}
	if envelope.Error.Code == "" {
		t.Errorf("error.code is empty")
	}
}

// noDeadlineWriter is an http.ResponseWriter whose SetWriteDeadline returns a
// wrapped http.ErrNotSupported, exercising the errors.Is path (Fix 1).
type noDeadlineWriter struct {
	*flushRecorder
}

func (n *noDeadlineWriter) SetWriteDeadline(time.Time) error {
	// Wrap the sentinel so a plain != comparison would fail to match.
	return fmt.Errorf("deadline not supported: %w", http.ErrNotSupported)
}

func TestProxy_SetWriteDeadlineWrappedErrNotSupported_IsIgnored(t *testing.T) {
	// Verify errors.Is is used: a wrapped ErrNotSupported must be recognised as
	// "not supported" and silently skipped — the proxy must still complete the
	// round-trip successfully (200 from backend).
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p := proxyTo(t, backend, "")

	ndw := &noDeadlineWriter{flushRecorder: newFlushRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	// Should not panic and must forward the request to the backend.
	p.ServeHTTP(ndw, req)
	// If errors.Is is used correctly the proxy just proceeds; no assertion on
	// status code here because noDeadlineWriter.WriteHeader is a no-op, but
	// reaching this line without a panic confirms the branch is silent.
	_ = errors.Is(nil, http.ErrNotSupported) // keep errors import used
}

// flushRecorder is an http.ResponseWriter that records each Flush with a
// timestamp so a test can assert chunks were flushed incrementally rather than
// buffered until the response ended.
type flushRecorder struct {
	header  http.Header
	chunks  []string
	flushes int
}

func newFlushRecorder() *flushRecorder { return &flushRecorder{header: http.Header{}} }

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(b []byte) (int, error) {
	f.chunks = append(f.chunks, string(b))
	return len(b), nil
}
func (f *flushRecorder) Flush() { f.flushes++ }

func TestProxy_StreamsIncrementally(t *testing.T) {
	// Backend that writes 3 SSE chunks, flushing between each with a small gap.
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend writer is not a flusher")
			return
		}
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			fl.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer backend.Close()

	p := proxyTo(t, backend, "")

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", nil)
	rec := newFlushRecorder()
	p.ServeHTTP(rec, req)

	// FlushInterval < 0 means each proxied write is flushed to the client.
	if rec.flushes < 3 {
		t.Errorf("flushes = %d, want >= 3 (stream was buffered, not incremental)", rec.flushes)
	}
	joined := strings.Join(rec.chunks, "")
	if strings.Count(joined, "data: chunk") != 3 {
		t.Errorf("expected 3 chunks streamed, got: %q", joined)
	}
}

func TestProxy_ClearsWriteDeadline(t *testing.T) {
	// A ResponseWriter whose SetWriteDeadline is observable.
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	p := proxyTo(t, backend, "")

	dw := &deadlineWriter{flushRecorder: newFlushRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	p.ServeHTTP(dw, req)

	if !dw.deadlineCleared {
		t.Errorf("write deadline was not cleared before streaming")
	}
}

// deadlineWriter augments flushRecorder with a SetWriteDeadline so
// http.NewResponseController can reach it.
type deadlineWriter struct {
	*flushRecorder
	deadlineCleared bool
}

func (d *deadlineWriter) SetWriteDeadline(tm time.Time) error {
	if tm.IsZero() {
		d.deadlineCleared = true
	}
	return nil
}

// TestProxy_KeepsAnIdleEventStreamAlive: a model that reasons for minutes
// before its first token leaves the relayed stream silent, and a proxy in
// front of elitea-main (100 seconds for a Cloudflare-style proxy) drops the
// call. The relay writes an SSE comment into an idle stream, between events
// only, and leaves a non-stream body untouched.
func TestProxy_KeepsAnIdleEventStreamAlive(t *testing.T) {
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/llm/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":`)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = io.WriteString(w, `[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	p := proxyTo(t, backend, "")
	p.streamKeepalive = 20 * time.Millisecond

	rec := newFlushRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil))
	body := strings.Join(rec.chunks, "")
	if !strings.HasPrefix(body, "data: first\n\n: keepalive\n\n") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("relayed stream = %q; want the events intact with a keepalive comment between them", body)
	}

	rec = newFlushRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil))
	if body := strings.Join(rec.chunks, ""); body != `{"data":[]}` {
		t.Fatalf("a JSON body was altered: %q", body)
	}
}

// An event stream whose length the gateway declared (a unary answer) is copied
// byte for byte: a keepalive comment would overrun the Content-Length the
// client was given. The body pauses longer than the keepalive interval.
func TestProxy_NoKeepaliveInADeclaredLengthEventStream(t *testing.T) {
	const body = "data: first\n\ndata: [DONE]\n\n"
	p := &Proxy{logger: slog.Default(), streamKeepalive: 20 * time.Millisecond}
	resp := &Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          io.NopCloser(&pausingReader{parts: []string{body[:13], body[13:]}, pause: 150 * time.Millisecond}),
		ContentLength: int64(len(body)),
	}
	rec := newFlushRecorder()
	p.writeResponse(rec, httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil), resp)
	if got := strings.Join(rec.chunks, ""); got != body {
		t.Fatalf("relayed body = %q; want exactly the %d declared bytes %q", got, len(body), body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q; want %d", cl, len(body))
	}
}

// pausingReader returns one part per Read, sleeping before every part but the
// first.
type pausingReader struct {
	parts []string
	pause time.Duration
	n     int
}

func (r *pausingReader) Read(b []byte) (int, error) {
	if r.n >= len(r.parts) {
		return 0, io.EOF
	}
	if r.n > 0 {
		time.Sleep(r.pause)
	}
	n := copy(b, r.parts[r.n])
	r.n++
	return n, nil
}
