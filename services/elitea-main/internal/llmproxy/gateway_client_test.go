package llmproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
)

// TestGatewayClient_CancelReachesTheGateway: a caller that goes away ends
// the gateway's request context, so the gateway stops the provider call.
func TestGatewayClient_CancelReachesTheGateway(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan struct{})
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		close(started)
		<-r.Context().Done()
		close(ended)
	}))
	client := gatewayTo(t, backend, "")

	ctx, cancel := context.WithCancel(t.Context())
	resp, err := client.ChatCompletions(ctx, &Request{Body: strings.NewReader(`{"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	buf := make([]byte, 64)
	if n, err := resp.Body.Read(buf); err != nil || !strings.Contains(string(buf[:n]), "first") {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway handler did not see the cancellation")
	}
	_ = resp.Body.Close()
}

// TestGatewayClient_BodyCloseEndsTheStream: closing the body early is a
// cancellation too.
func TestGatewayClient_BodyCloseEndsTheStream(t *testing.T) {
	ended := make(chan struct{})
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: first\n\n")
		<-r.Context().Done()
		close(ended)
	}))
	resp, err := gatewayTo(t, backend, "").Messages(t.Context(), &Request{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body did not end the gateway call")
	}
}

// TestGatewayClient_RefusalsAreResponses: a budget 402 with its headers is a
// Response, never an error, and arrives byte for byte.
func TestGatewayClient_RefusalsAreResponses(t *testing.T) {
	const body = `{"error":{"message":"budget exceeded","type":"budget_exceeded","code":"insufficient_quota"}}` + "\n"
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, body)
	}))
	p := proxyTo(t, backend, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req.WithContext(middleware.ContextWithProject(req.Context(), middleware.ProjectContext{ProjectID: 7})))
	if rec.Code != http.StatusPaymentRequired || rec.Body.String() != body {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Ratelimit-Remaining-Requests") != "0" || len(rec.Header().Values("X-Multi")) != 2 {
		t.Fatalf("headers = %v", rec.Header())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatalf("a unary answer lost its Content-Length: %v", rec.Header())
	}
}

// TestGatewayClient_UploadStreams: a multipart upload larger than one frame
// arrives intact through the client stream, query included.
func TestGatewayClient_UploadStreams(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 200_000) // 3.2 MB
	var got []byte
	var gotPath, gotQuery, gotType string
	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		gotPath, gotQuery, gotType = r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `{"text":"ok"}`)
	}))
	resp, err := gatewayTo(t, backend, "").Transcribe(t.Context(), &Request{
		Path:     "/llm/v1/audio/translations",
		RawQuery: "a=1",
		Header:   http.Header{"Content-Type": {"multipart/form-data; boundary=x"}},
		Body:     bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("status %d, %d of %d bytes arrived", resp.StatusCode, len(got), len(payload))
	}
	if gotPath != "/llm/v1/audio/translations" || gotQuery != "a=1" || gotType != "multipart/form-data; boundary=x" {
		t.Fatalf("path %q query %q type %q", gotPath, gotQuery, gotType)
	}
}

// TestGatewayClient_RefusesAGatewayOutsideTheCA: the client verifies the
// gateway certificate; a server from another CA is a failed hop.
func TestGatewayClient_RefusesAGatewayOutsideTheCA(t *testing.T) {
	backend := llmproxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	other := grpcauthtest.NewPKI(t)
	client, err := NewGatewayClient(GatewayClientConfig{Target: backend.Target, TLS: other.ClientTLS(t, "elitea-main")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.ListModels(t.Context(), &Request{}); err == nil {
		t.Fatal("a gateway with a certificate from another CA was trusted")
	}
	// And the gateway requires a client certificate.
	anon, err := NewGatewayClient(GatewayClientConfig{Target: backend.Target, TLS: backend.PKI.AnonymousTLS()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = anon.Close() })
	if _, err := anon.ListModels(t.Context(), &Request{}); err == nil {
		t.Fatal("a call without a client certificate succeeded")
	}
}

// TestProxy_RouteMissesMatchTheGatewayRouter: a request no RPC serves is
// answered at the edge with the gateway router's own 404 and 405 bodies.
func TestProxy_RouteMissesMatchTheGatewayRouter(t *testing.T) {
	backend := llmproxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a route miss reached the gateway")
	}))
	p := proxyTo(t, backend, "")
	cases := []struct {
		method, path string
		status       int
		message      string
	}{
		{http.MethodPost, "/llm/v1/nope", http.StatusNotFound, "unknown route"},
		{http.MethodGet, "/llm/other", http.StatusNotFound, "unknown route"},
		{http.MethodGet, "/llm/v1/chat/completions", http.StatusMethodNotAllowed, "method not allowed"},
		{http.MethodPost, "/llm/v1/models", http.StatusMethodNotAllowed, "method not allowed"},
		// A WebSocket handshake is a GET; the legacy POST alias is gone.
		{http.MethodPost, "/llm/v1/realtime", http.StatusMethodNotAllowed, "method not allowed"},
		{http.MethodPost, "/llm/v1/messages/batches", http.StatusNotFound, "unknown messages sub-path"},
		{http.MethodGet, "/llm/v1/messages/batches", http.StatusMethodNotAllowed, "method not allowed"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		want := `{"error":{"message":"` + tc.message + `","type":"invalid_request_error"}}` + "\n"
		if rec.Code != tc.status || rec.Body.String() != want || rec.Header().Get("Server") != "Centry" {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.path, rec.Code, rec.Body.String(), tc.status, want)
		}
	}
}

// TestGatewayClient_DoRoutes: Do reaches the method the path names.
func TestGatewayClient_DoRoutes(t *testing.T) {
	var gotPath string
	backend := llmproxytest.Start(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	client := gatewayTo(t, backend, "")
	for _, path := range []string{"/llm/v1/models/openai/gpt-4o", "/llm/v1/models"} {
		if _, err := client.Do(t.Context(), &Request{Method: http.MethodGet, Path: path}); err != nil || gotPath != path {
			t.Fatalf("Do(GET %s) reached %q, %v", path, gotPath, err)
		}
	}
	if _, err := client.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/llm/v1/realtime"}); err != ErrNoGatewayRoute {
		t.Fatalf("Do(realtime) = %v, want ErrNoGatewayRoute", err)
	}
}
