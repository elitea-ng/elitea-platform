package grpcapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/grpc/metadata"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// echoProvider is a provider socket that answers every frame with itself.
type echoProvider struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newEchoProvider() *echoProvider {
	return &echoProvider{frames: make(chan []byte, 8), closed: make(chan struct{})}
}

func (p *echoProvider) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-p.frames:
		return b, nil
	case <-p.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *echoProvider) Write(_ context.Context, frame []byte) error {
	select {
	case p.frames <- append([]byte(nil), frame...):
		return nil
	case <-p.closed:
		return io.ErrClosedPipe
	}
}

func (p *echoProvider) Ping(context.Context) error { return nil }

func (p *echoProvider) Close(llmproxy.RealtimeCloseCode, string) {
	p.once.Do(func() { close(p.closed) })
}

type echoCodec struct{ p openai.OpenAIProvider }

func (c *echoCodec) ToBifrostRealtimeEvent(raw json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	return c.p.ToBifrostRealtimeEvent(raw)
}

func (c *echoCodec) ToProviderRealtimeEvent(ev *schemas.BifrostRealtimeEvent) (json.RawMessage, error) {
	return c.p.ToProviderRealtimeEvent(ev)
}

func (c *echoCodec) ShouldStartRealtimeTurn(ev *schemas.BifrostRealtimeEvent) bool {
	return c.p.ShouldStartRealtimeTurn(ev)
}

func (c *echoCodec) ExtractRealtimeTurnUsage(raw []byte) *schemas.BifrostLLMUsage {
	return c.p.ExtractRealtimeTurnUsage(raw)
}

type echoDialer struct{ provider *echoProvider }

func (d echoDialer) DialRealtime(*schemas.BifrostContext, schemas.ModelProvider, string, url.Values) (*llmproxy.RealtimeUpstream, error) {
	return &llmproxy.RealtimeUpstream{Socket: d.provider, Codec: &echoCodec{}, Subprotocol: "realtime"}, nil
}

func handshake(query string, extra ...string) *gatewayv1.HttpRequestHead {
	h := head(http.MethodGet, "/llm/v1/realtime", append([]string{
		"Upgrade", "websocket",
		"Connection", "Upgrade",
		"Sec-Websocket-Version", "13",
		"Sec-Websocket-Key", "dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-Websocket-Protocol", "realtime",
	}, extra...)...)
	h.RawQuery = query
	return h
}

func openRealtime(t *testing.T, client gatewayv1.GatewayServiceClient, ctx context.Context, h *gatewayv1.HttpRequestHead) (gatewayv1.GatewayService_RealtimeClient, *gatewayv1.HttpResponseHead) {
	t.Helper()
	stream, err := client.Realtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Open{Open: h}}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv head: %v", err)
	}
	if first.GetHead() == nil {
		t.Fatalf("first frame = %v, want the head", first)
	}
	return stream, first.GetHead()
}

// TestRealtimeEchoThroughTheRealHandler runs the gateway's realtime route over
// the bidirectional stream: the handshake is accepted as 101 with the
// negotiated subprotocol, a caller message reaches the provider, and the
// provider's answer comes back as a message.
func TestRealtimeEchoThroughTheRealHandler(t *testing.T) {
	pki := newTestPKI(t)
	provider := newEchoProvider()
	h := llmproxy.NewHandler(nil, nil, nil, llmproxy.WithRealtimeDialer(echoDialer{provider}))
	srv := startTestServer(t, pki, api.NewRouter(h), nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))

	ctx := metadata.AppendToOutgoingContext(testContext(t), "x-elitea-project-id", "42")
	stream, gotHead := openRealtime(t, client, ctx, handshake("model=gpt-4o-realtime-preview"))
	if gotHead.GetStatusCode() != http.StatusSwitchingProtocols || headerOf(gotHead, "Sec-WebSocket-Protocol") != "realtime" {
		t.Fatalf("head = %v, want 101 with the realtime subprotocol", gotHead)
	}

	event := `{"type":"transcription_session.update","session":{"input_audio_format":"pcm16"}}`
	if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Message{
		Message: &gatewayv1.RealtimeMessage{Data: []byte(event)},
	}}); err != nil {
		t.Fatal(err)
	}
	for {
		in, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if m := in.GetMessage(); m != nil {
			if !strings.Contains(string(m.GetData()), "transcription_session.update") {
				t.Fatalf("echo = %s", m.GetData())
			}
			break
		}
	}

	// The caller closes: the session ends and the stream completes.
	if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Close{
		Close: &gatewayv1.RealtimeClose{Code: 1000, Reason: "bye"},
	}}); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("the session did not end cleanly: %v", err)
		}
	}
}

// TestRealtimeRefusalIsAnHTTPResponse: a refusal before the upgrade is the
// handler's status and body, as over HTTP.
func TestRealtimeRefusalIsAnHTTPResponse(t *testing.T) {
	pki := newTestPKI(t)
	h := llmproxy.NewHandler(nil, nil, nil, llmproxy.WithRealtimeDialer(echoDialer{newEchoProvider()}))
	srv := startTestServer(t, pki, api.NewRouter(h), nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))

	stream, gotHead := openRealtime(t, client, testContext(t), handshake(""))
	if gotHead.GetStatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing model", gotHead.GetStatusCode())
	}
	var body strings.Builder
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body.Write(in.GetData())
	}
	if !strings.Contains(body.String(), "`model` query parameter is required") {
		t.Fatalf("body = %s", body.String())
	}

	// A cross-site Origin is refused by the same rule websocket.Accept applies.
	stream, gotHead = openRealtime(t, client, testContext(t),
		handshake("model=m", "Origin", "https://evil.example"))
	if gotHead.GetStatusCode() != http.StatusForbidden {
		t.Fatalf("a foreign Origin answered %d, want 403", gotHead.GetStatusCode())
	}
	_ = stream.CloseSend()
}

// TestRealtimeSocketPingWriteAndClose drives the socket the handler gets
// directly: a ping is answered by the caller's pong, a write is a message,
// and a close carries its status.
func TestRealtimeSocketPingWriteAndClose(t *testing.T) {
	pki := newTestPKI(t)
	result := make(chan error, 1)
	llm := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sock, sub, err := llmproxy.RealtimeAcceptorFrom(r.Context())(w, r, llmproxy.RealtimeAcceptOptions{
			Subprotocols: []string{"realtime"}, ReadLimit: 8,
		})
		if err != nil {
			result <- err
			return
		}
		if sub != "realtime" {
			t.Errorf("subprotocol = %q", sub)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := sock.Ping(ctx); err != nil {
			result <- err
			return
		}
		if err := sock.Write(ctx, []byte("hello")); err != nil {
			result <- err
			return
		}
		data, err := sock.Read(ctx)
		if err != nil || string(data) != "short" {
			t.Errorf("read = %q, %v", data, err)
		}
		// A message over the read limit ends the read side.
		if _, err := sock.Read(ctx); err == nil {
			t.Error("a message over the read limit was accepted")
		}
		sock.Close(llmproxy.RealtimeClosePolicy, "budget")
		result <- nil
	})
	srv := startTestServer(t, pki, llm, nil)
	client := dialTestClient(t, srv, pki.clientTLS(t, "elitea-main"))
	stream, gotHead := openRealtime(t, client, testContext(t), handshake(""))
	if gotHead.GetStatusCode() != http.StatusSwitchingProtocols {
		t.Fatalf("head = %v", gotHead)
	}
	in, err := stream.Recv()
	if err != nil || in.GetPing() == nil {
		t.Fatalf("want a ping, got %v, %v", in, err)
	}
	if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Pong{
		Pong: &gatewayv1.RealtimePong{Id: in.GetPing().GetId()},
	}}); err != nil {
		t.Fatal(err)
	}
	in, err = stream.Recv()
	if err != nil || string(in.GetMessage().GetData()) != "hello" || in.GetMessage().GetBinary() {
		t.Fatalf("want the text message, got %v, %v", in, err)
	}
	for _, data := range []string{"short", "far too long"} {
		if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Message{
			Message: &gatewayv1.RealtimeMessage{Data: []byte(data)},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	in, err = stream.Recv()
	if err != nil || in.GetClose().GetCode() != int32(llmproxy.RealtimeClosePolicy) || in.GetClose().GetReason() != "budget" {
		t.Fatalf("want the close, got %v, %v", in, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
