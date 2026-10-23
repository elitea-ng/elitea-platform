package llmproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// realtimeGateway answers the realtime handshake as the gateway does: it
// records what arrived, refuses a request with no model, and accepts the
// rest with the "realtime" subprotocol.
type realtimeGateway struct {
	mu   sync.Mutex
	seen http.Header
	host string
}

func (g *realtimeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.seen = r.Header.Clone()
	g.host = r.Host
	g.mu.Unlock()
	if r.URL.Query().Get("model") == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"`+"`model`"+` query parameter is required","type":"invalid_request_error"}}`)
		return
	}
	w.Header().Set("Sec-WebSocket-Protocol", "realtime")
	w.Header().Set("X-Request-Id", "rt-1")
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func (g *realtimeGateway) header() http.Header {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen
}

func realtimeEdge(t *testing.T) (*realtimeGateway, string) {
	t.Helper()
	g := &realtimeGateway{}
	backend := llmproxytest.Start(t, g)
	p := proxyTo(t, backend, "s3cr3t")
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r.WithContext(fullCtx()))
	}))
	t.Cleanup(edge.Close)
	return g, "ws" + strings.TrimPrefix(edge.URL, "http")
}

// TestProxyCarriesTheRealtimeSession proves the browser's WebSocket ends at
// the edge and continues as a Realtime session (issue #323, #1359):
//
//   - the handshake headers reach the gateway, and the client's credentials
//     do not; the identity arrives signed;
//   - the gateway's subprotocol and headers reach the browser in the 101;
//   - the gateway's keepalive ping becomes a browser ping, and its pong
//     returns to the gateway;
//   - messages cross in both directions, and a gateway close reaches the
//     browser with its status and reason.
func TestProxyCarriesTheRealtimeSession(t *testing.T) {
	g, url := realtimeEdge(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, url+"/llm/v1/realtime?model=m&intent=transcription", &websocket.DialOptions{
		Subprotocols: []string{"realtime"},
		HTTPHeader:   http.Header{"Authorization": {"Bearer client-token"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if conn.Subprotocol() != "realtime" || resp.Header.Get("X-Request-Id") != "rt-1" {
		t.Fatalf("subprotocol %q, X-Request-Id %q: the gateway's 101 head did not reach the browser",
			conn.Subprotocol(), resp.Header.Get("X-Request-Id"))
	}

	seen := g.header()
	if !strings.EqualFold(seen.Get("Upgrade"), "websocket") || seen.Get("Sec-WebSocket-Key") == "" {
		t.Fatalf("the gateway saw Upgrade=%q Sec-WebSocket-Key=%q; the handshake headers were dropped",
			seen.Get("Upgrade"), seen.Get("Sec-WebSocket-Key"))
	}
	if seen.Get("Authorization") != "" {
		t.Fatalf("the gateway saw Authorization=%q; the client credential crossed the trust boundary", seen.Get("Authorization"))
	}
	if !verifyIdentitySignature(seen, []byte("s3cr3t")) {
		t.Fatal("the realtime identity is not signed")
	}

	// The gateway pinged first: the browser answers while it reads, and the
	// pong comes back to the gateway, which reports it.
	read := func() string {
		t.Helper()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(data)
	}
	if got := read(); got != "pong 7" {
		t.Fatalf("first message = %q, want the gateway's pong report", got)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "hello" {
		t.Fatalf("echo = %q", got)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(llmproxytest.CloseRequest)); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != "budget" {
		t.Fatalf("read after the gateway close = %v, want close 1008 \"budget\"", err)
	}
}

// TestProxyRealtimeBrowserClose: a browser close ends the gateway session.
func TestProxyRealtimeBrowserClose(t *testing.T) {
	_, url := realtimeEdge(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url+"/llm/v1/realtime?model=m", &websocket.DialOptions{Subprotocols: []string{"realtime"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	if err := conn.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestProxyRealtimeRefusalIsHTTP: a gateway refusal before the upgrade is
// the browser's HTTP answer, status and body.
func TestProxyRealtimeRefusalIsHTTP(t *testing.T) {
	_, url := realtimeEdge(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url+"/llm/v1/realtime", nil)
	if err == nil {
		t.Fatal("a refused handshake was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %+v, want 400", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "query parameter is required") {
		t.Fatalf("body = %q", body)
	}
}

func TestTruncateCloseReason(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes
	got := truncateCloseReason(long)
	if len(got) > maxCloseReason || !strings.HasPrefix(long, got) || len(got)%2 != 0 {
		t.Fatalf("truncated to %d bytes, cut a character or kept too much", len(got))
	}
	if truncateCloseReason("ok") != "ok" {
		t.Fatal("a short reason changed")
	}
}
