package llmproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/ssewriter"
)

// Config configures the browser-facing /llm proxy.
type Config struct {
	// Gateway is the elitea-llm-gateway client (NewGatewayClient). Required.
	Gateway Gateway

	// Logger receives proxy error events. Defaults to slog.Default().
	Logger *slog.Logger

	// ExecutionVerifier checks an inbound X-Elitea-Execution-Id against a
	// live execution of the caller before the edge signs it. Nil keeps the
	// shape check only. Production composition always sets it: the analytics
	// reads decide active users and run spend from this id.
	ExecutionVerifier ExecutionVerifier
}

// Proxy is elitea-main's /llm surface: it resolves who the request is for and
// hands it to the Gateway. The browser sees the gateway's answer unchanged —
// status, headers and body bytes, SSE framing included — and the realtime
// WebSocket ends here and continues as a Realtime session (proxy_realtime.go).
type Proxy struct {
	gateway  Gateway
	logger   *slog.Logger
	verifier ExecutionVerifier
	// audio caps the /llm/v1/audio requests one principal has in flight
	// (audio_limits.go).
	audio *audioInFlight
	// streamKeepalive overrides ssewriter.KeepaliveInterval in tests. Zero
	// means the default.
	streamKeepalive time.Duration
}

// New builds the proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Gateway == nil {
		return nil, errors.New("llmproxy: a Gateway is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		gateway:  cfg.Gateway,
		logger:   logger,
		verifier: cfg.ExecutionVerifier,
		audio:    newAudioInFlight(maxAudioInFlightPerPrincipal),
	}, nil
}

// ServeHTTP serves one /llm request. It clears the per-connection write
// deadline before streaming so the http.Server's global WriteTimeout does not
// hard-kill a long-lived SSE response on the /llm path (design §9.5).
//
// The /llm/v1/audio routes are not streams. They get a body ceiling and a
// deadline here (audio_limits.go) before the request leaves the edge.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rpc := routeOf(r.Method, r.URL.Path)
	if rpc == rpcNone {
		w.Header().Set("X-Accel-Buffering", "no")
		writeGatewayRouteMiss(w, r)
		return
	}
	r, release, ok := limitAudioRequest(w, r, p.audio)
	if !ok {
		return
	}
	defer release()
	hop.ClearWriteDeadline(w, p.logger)

	req := &Request{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Header:   r.Header,
		Body:     r.Body,
		Identity: p.identity(r),
	}
	if rpc == rpcRealtime {
		p.serveRealtime(w, r, req)
		return
	}
	resp, err := rpc.call(p.gateway)(r.Context(), req)
	if err != nil {
		p.writeHopError(w, r, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.writeResponse(w, r, resp)
}

// identity resolves who r is for: the edge-resolved project, user and tenant,
// and the inbound execution id when the verifier admits it
// (executionIDFromHeader, verifiedExecutionID).
func (p *Proxy) identity(r *http.Request) Identity {
	id := resolveIdentity(r.Context(), r.Header, p.verifier)
	return Identity{ProjectID: id.projectID, UserID: id.userID, TenantID: id.tenantID, ExecutionID: id.executionID}
}

// writeResponse copies the gateway's answer to the browser. Every body write
// is flushed at once, so an SSE stream is not re-buffered on this hop
// (design §6.3).
func (p *Proxy) writeResponse(w http.ResponseWriter, r *http.Request, resp *Response) {
	copyResponseHeader(w.Header(), resp.Header)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	// No downstream proxy (nginx) may buffer the stream; the gateway sets
	// this too, and the edge re-asserts it (design §6.3).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	// An event stream can go quiet for minutes while a model reasons before
	// its first token, and a proxy in front of elitea-main (a Cloudflare-style
	// proxy idles out at 100 seconds) then drops a call that is still alive.
	// ssewriter.Keepalive writes an SSE comment into an idle stream, only
	// between events, which every OpenAI-compatible client ignores. Any other
	// body is copied as it was, and so is an event stream whose length the
	// gateway declared: a comment would overrun that Content-Length.
	var out io.Writer = w
	flush := http.NewResponseController(w).Flush
	if isEventStream(resp.Header.Get("Content-Type")) && resp.ContentLength < 0 {
		keepalive := ssewriter.StartKeepalive(w, p.streamKeepalive)
		defer keepalive.Stop()
		out = keepalive
		flush = func() error { return nil } // Keepalive.Write flushes
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return
			}
			_ = flush()
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			p.logger.Error("llmproxy: the gateway stream broke", "err", err, "path", r.URL.Path)
			// As httputil.ReverseProxy does: the head is sent, so the only
			// honest signal left is to abort the response, not end it cleanly.
			panic(http.ErrAbortHandler)
		}
	}
}

// isEventStream reports whether a Content-Type names an SSE body.
func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// writeHopError answers a request whose gateway call failed.
func (p *Proxy) writeHopError(w http.ResponseWriter, r *http.Request, err error) {
	// An audio request the edge stopped (body ceiling, deadline) gets its own
	// named refusal, not "gateway unavailable".
	if writeAudioRefusal(w, r, err) {
		p.logger.Warn("llmproxy: audio request stopped at the edge", "err", err, "path", r.URL.Path)
		return
	}
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	p.logger.Error("llmproxy: upstream error", "err", err, "path", r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"error":{"message":"llm gateway unavailable","type":"service_unavailable","code":"upstream_unavailable"}}`))
}

// copyResponseHeader copies the end-to-end headers of src to dst.
func copyResponseHeader(dst, src http.Header) {
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	for _, name := range hopByHopHeaders {
		dst.Del(name)
	}
	dst.Del("Content-Length")
}
