package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gatewayv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

func head(method, path string, headers ...string) *gatewayv1.HttpRequestHead {
	h := &gatewayv1.HttpRequestHead{Method: method, Path: path}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Headers = append(h.Headers, &gatewayv1.HttpHeader{Name: headers[i], Values: []string{headers[i+1]}})
	}
	return h
}

func headerOf(h *gatewayv1.HttpResponseHead, name string) string {
	for _, header := range h.GetHeaders() {
		if http.CanonicalHeaderKey(header.GetName()) == http.CanonicalHeaderKey(name) && len(header.GetValues()) > 0 {
			return header.GetValues()[0]
		}
	}
	return ""
}

// readStream collects a streamed response: the head and the body bytes.
func readStream(t *testing.T, recv func() (*gatewayv1.ResponseFrame, error)) (*gatewayv1.HttpResponseHead, string) {
	t.Helper()
	var (
		gotHead *gatewayv1.HttpResponseHead
		body    strings.Builder
	)
	for {
		frame, err := recv()
		if errors.Is(err, io.EOF) {
			return gotHead, body.String()
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch f := frame.GetFrame().(type) {
		case *gatewayv1.ResponseFrame_Head:
			if gotHead != nil {
				t.Fatal("a second head frame")
			}
			gotHead = f.Head
		case *gatewayv1.ResponseFrame_Data:
			if gotHead == nil {
				t.Fatal("body data before the head")
			}
			body.Write(f.Data)
		}
	}
}

// TestChatStreamsTheHandlerResponseWithIdentity is the main path: an admitted
// caller, the identity tuple in metadata, an SSE body that arrives in frames,
// and the response headers the browser must still see.
func TestChatStreamsTheHandlerResponseWithIdentity(t *testing.T) {
	pki := newTestPKI(t)
	var seen http.Header
	var seenBody string
	llm := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)
		if r.URL.Path != "/llm/v1/chat/completions" || r.Method != http.MethodPost || r.URL.RawQuery != "a=1" {
			t.Errorf("handler saw %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "41")
		w.WriteHeader(http.StatusOK)
		for i := range 3 {
			_, _ = fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))

	ctx := metadata.AppendToOutgoingContext(testContext(t),
		"x-elitea-project-id", "7", "x-elitea-user-id", "u1", "x-elitea-identity-signature", "sha256=abc")
	h := head(http.MethodPost, "/llm/v1/chat/completions",
		"Content-Type", "application/json",
		// A project id in the head is NOT identity: it must be dropped.
		"X-Elitea-Project-Id", "999",
		"Connection", "keep-alive",
		"X-Elitea-Llm-Hop", "marker")
	h.RawQuery = "a=1"
	stream, err := client.ChatCompletions(ctx, &gatewayv1.ChatCompletionsRequest{Request: &gatewayv1.HttpRequest{
		Head: h, Body: []byte(`{"model":"m","stream":true}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	gotHead, body := readStream(t, func() (*gatewayv1.ResponseFrame, error) {
		m, err := stream.Recv()
		return m.GetFrame(), err
	})
	if gotHead.GetStatusCode() != http.StatusOK || headerOf(gotHead, "Content-Type") != "text/event-stream" ||
		headerOf(gotHead, "X-Ratelimit-Remaining-Requests") != "41" {
		t.Fatalf("head = %v", gotHead)
	}
	if want := "data: {\"n\":0}\n\ndata: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: [DONE]\n\n"; body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if seenBody != `{"model":"m","stream":true}` {
		t.Fatalf("handler body = %q", seenBody)
	}
	if seen.Get("X-Elitea-Project-Id") != "7" || seen.Get("X-Elitea-User-Id") != "u1" ||
		seen.Get("X-Elitea-Identity-Signature") != "sha256=abc" {
		t.Fatalf("identity headers = %v; they must come from metadata, and only from it", seen)
	}
	if seen.Get("Connection") != "" {
		t.Fatal("a hop-by-hop header reached the handler")
	}
	if seen.Get("X-Elitea-Llm-Hop") != "marker" {
		t.Fatal("the hop marker must reach the handler: it is the loop detector (issue #164)")
	}
}

func TestCallersOutsideTheAllowlistAreRefused(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTestServer(t, pki, http.NotFoundHandler(), nil)
	cases := map[string]struct {
		tls  func() *tlsConfigT
		code codes.Code
	}{
		"another workload": {func() *tlsConfigT { return pki.clientTLS(t, "elitea-worker") }, codes.PermissionDenied},
		"no certificate":   {func() *tlsConfigT { return pki.anonymousTLS() }, codes.Unavailable},
		"another CA":       {func() *tlsConfigT { return withRoots(newTestPKI(t).clientTLS(t, "elitea-main"), pki) }, codes.Unavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := dialTestClient(t, srv, tc.tls())
			_, err := client.ListModels(testContext(t), &gatewayv1.ListModelsRequest{Request: &gatewayv1.HttpRequest{
				Head: head(http.MethodGet, "/llm/v1/models"),
			}})
			if status.Code(err) != tc.code {
				t.Fatalf("ListModels = %v, want %s", err, tc.code)
			}
		})
	}
}

// TestCancelReachesTheHandlerContext: cancelling the call cancels the
// request context, exactly as a closed HTTP connection did.
func TestCancelReachesTheHandlerContext(t *testing.T) {
	pki := newTestPKI(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	llm := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	ctx, cancel := context.WithCancel(testContext(t))
	stream, err := client.Messages(ctx, &gatewayv1.MessagesRequest{Request: &gatewayv1.HttpRequest{
		Head: head(http.MethodPost, "/llm/v1/messages"), Body: []byte(`{}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler context was not cancelled when the call was")
	}
}

// TestUnaryRelaysARefusalAsAResponse: a gateway refusal is a head and a body,
// not a gRPC error, so elitea-main writes the browser the same 402.
func TestUnaryRelaysARefusalAsAResponse(t *testing.T) {
	pki := newTestPKI(t)
	llm := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"budget_exceeded"}}`)
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	resp, err := client.Embeddings(testContext(t), &gatewayv1.EmbeddingsRequest{Request: &gatewayv1.HttpRequest{
		Head: head(http.MethodPost, "/llm/v1/embeddings"), Body: []byte(`{}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResponse().GetHead().GetStatusCode() != http.StatusPaymentRequired ||
		string(resp.GetResponse().GetBody()) != `{"error":{"type":"budget_exceeded"}}` {
		t.Fatalf("response = %v", resp.GetResponse())
	}
}

func TestClientStreamBodyReachesTheHandler(t *testing.T) {
	pki := newTestPKI(t)
	llm := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		_, _ = fmt.Fprintf(w, "%s %d %s", r.URL.Path, len(raw), r.Header.Get("Content-Type"))
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	stream, err := client.Transcribe(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&gatewayv1.TranscribeRequest{Frame: &gatewayv1.RequestFrame{Frame: &gatewayv1.RequestFrame_Head{
		Head: head(http.MethodPost, "/llm/v1/audio/translations", "Content-Type", "multipart/form-data; boundary=x"),
	}}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := stream.Send(&gatewayv1.TranscribeRequest{Frame: &gatewayv1.RequestFrame{Frame: &gatewayv1.RequestFrame_Data{
			Data: make([]byte, 1000),
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.GetResponse().GetBody()); got != "/llm/v1/audio/translations 3000 multipart/form-data; boundary=x" {
		t.Fatalf("body = %q", got)
	}
}

func TestAnRPCServesOnlyItsOwnRoutes(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTestServer(t, pki, http.NotFoundHandler(), nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	for _, h := range []*gatewayv1.HttpRequestHead{
		head(http.MethodPost, "/llm/v1/chat/completions"),
		head(http.MethodGet, "/llm/v1/embeddings"),
		head(http.MethodPost, "/llm/v1/../v1/embeddings"),
		nil,
	} {
		_, err := client.Embeddings(testContext(t), &gatewayv1.EmbeddingsRequest{Request: &gatewayv1.HttpRequest{Head: h}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Embeddings(%v) = %v, want InvalidArgument", h, err)
		}
	}
}

func TestAHandlerPanicIsAnInternalErrorNotACrash(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTestServer(t, pki, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	_, err := client.CountTokens(testContext(t), &gatewayv1.CountTokensRequest{Request: &gatewayv1.HttpRequest{
		Head: head(http.MethodPost, "/llm/v1/messages/count_tokens"),
	}})
	if status.Code(err) != codes.Internal {
		t.Fatalf("CountTokens = %v, want Internal", err)
	}
}

// TestSignatureIsVerifiedByTheGatewayHandler runs the REAL router: a wrong
// signature is the handler's own 403, a right one is served.
func TestSignatureIsVerifiedByTheGatewayHandler(t *testing.T) {
	pki := newTestPKI(t)
	secret := []byte("identity-secret")
	h := llmproxy.NewHandler(nil, nil, secret)
	srv := startTestServer(t, pki, api.NewRouter(h), nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))

	call := func(sig string) int32 {
		signed := http.Header{}
		llmproxy.SignIdentityHeaders(signed, secret, "5", "u", "", "")
		if sig == "" {
			sig = signed.Get("X-Elitea-Identity-Signature")
		}
		ctx := metadata.AppendToOutgoingContext(testContext(t),
			"x-elitea-project-id", "5", "x-elitea-user-id", "u", "x-elitea-identity-signature", sig)
		resp, err := client.ListModels(ctx, &gatewayv1.ListModelsRequest{Request: &gatewayv1.HttpRequest{
			Head: head(http.MethodGet, "/llm/v1/models"),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetResponse().GetHead().GetStatusCode()
	}
	if got := call("sha256=forged"); got != http.StatusForbidden {
		t.Fatalf("a forged signature answered %d, want 403", got)
	}
	if got := call(""); got != http.StatusOK {
		t.Fatalf("a valid signature answered %d, want 200", got)
	}
}

func TestGovernanceStatusServesTheStatusHandler(t *testing.T) {
	pki := newTestPKI(t)
	status := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/governance/status" || r.Method != http.MethodGet {
			t.Errorf("status handler saw %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"loaded":true}`)
	})
	srv := startTestServer(t, pki, http.NotFoundHandler(), status)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	resp, err := client.GovernanceStatus(testContext(t), &gatewayv1.GovernanceStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.GetResponse().GetBody()) != `{"loaded":true}` {
		t.Fatalf("body = %q", resp.GetResponse().GetBody())
	}
}

// TestDrainWaitsForCallsAndRefusesNewOnes pins the first shutdown phase.
func TestDrainWaitsForCallsAndRefusesNewOnes(t *testing.T) {
	pki := newTestPKI(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	llm := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, "done")
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	result := make(chan error, 1)
	go func() {
		_, err := client.Embeddings(testContext(t), &gatewayv1.EmbeddingsRequest{Request: &gatewayv1.HttpRequest{
			Head: head(http.MethodPost, "/llm/v1/embeddings"),
		}})
		result <- err
	}()
	<-started
	drained := make(chan error, 1)
	go func() { drained <- srv.service.Drain(testContext(t)) }()
	select {
	case <-drained:
		t.Fatal("Drain returned while a call was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := srv.service.begin(); status.Code(err) != codes.Unavailable {
		t.Fatalf("a call during the drain = %v, want Unavailable", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("the call in flight failed: %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain = %v", err)
	}
}
