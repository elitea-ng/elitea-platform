package llmproxy

// gateway.go — the transport-agnostic surface of elitea-llm-gateway.
//
// Gateway is what every elitea-main caller of the LLM gateway uses: the
// browser-facing /llm proxy (proxy.go), predict_llm and the evaluation judge
// (api/v2/predict), the configuration checks and model/voice listings
// (api/v2/configurations), the admin governance status (api/gateway), and the
// worker-facing elitea.llm.v1.ModelService (#1359 U2), which calls the
// methods below directly.
//
// A Request is one /llm request in its provider-native wire format — the
// OpenAI or Anthropic JSON body, or the multipart form — and a Response is
// the status, headers and body the gateway answered, SSE framing included.
// Identity is who the request is FOR; the implementation signs it (the
// X-Elitea-* HMAC, GATEWAY_IDENTITY_SECRET) and the gateway verifies it, so a
// caller never handles the signature itself.
//
// GatewayClient (gateway_client.go) is the implementation: gRPC over mutual
// TLS to elitea.gateway.v1.GatewayService.

import (
	"context"
	"io"
	"net/http"
)

// Identity is the caller tuple a gateway request is made for. Empty fields
// are not part of the tuple. Nothing is signed without a ProjectID.
type Identity struct {
	ProjectID   string
	UserID      string
	TenantID    string
	ExecutionID string
}

// Request is one /llm request.
type Request struct {
	// Method and Path name the route ("POST", "/llm/v1/chat/completions").
	// The named Gateway methods fill them when they are empty.
	Method string
	Path   string
	// RawQuery is the query string without "?".
	RawQuery string
	// Header holds the end-to-end request headers (Content-Type,
	// anthropic-version, ...). X-Elitea identity headers here are ignored:
	// Identity is the only source of identity.
	Header http.Header
	// Body is the request body; nil is empty.
	Body io.Reader
	// Identity is signed by the Gateway implementation.
	Identity Identity
}

// Response is the gateway's answer. Body streams for the streaming routes
// (chat, completions, messages, responses, speech); the caller must Close it.
// A read error on Body after the head is a broken stream, not an EOF.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	// ContentLength is the body size when it is known (a unary answer), and
	// -1 for a stream.
	ContentLength int64
}

// Gateway is elitea-llm-gateway as elitea-main sees it. Every method returns
// the gateway's own HTTP status in Response — a budget 402, a rate-limit
// 429, a policy 403, an upstream provider error — and an error only when
// the hop itself failed (the gateway unreachable, the call refused).
type Gateway interface {
	ChatCompletions(ctx context.Context, req *Request) (*Response, error)
	Completions(ctx context.Context, req *Request) (*Response, error)
	Messages(ctx context.Context, req *Request) (*Response, error)
	CountTokens(ctx context.Context, req *Request) (*Response, error)
	Responses(ctx context.Context, req *Request) (*Response, error)
	Embeddings(ctx context.Context, req *Request) (*Response, error)
	ImageGenerate(ctx context.Context, req *Request) (*Response, error)
	// ImageEdit serves /llm/v1/images/edits and /llm/v1/images/variations
	// (req.Path picks; edits by default). The multipart body streams.
	ImageEdit(ctx context.Context, req *Request) (*Response, error)
	Speech(ctx context.Context, req *Request) (*Response, error)
	// Transcribe serves /llm/v1/audio/transcriptions and
	// /llm/v1/audio/translations (req.Path picks; transcriptions by
	// default). The multipart body streams.
	Transcribe(ctx context.Context, req *Request) (*Response, error)
	ListModels(ctx context.Context, req *Request) (*Response, error)
	// GetModel needs req.Path "/llm/v1/models/<model>".
	GetModel(ctx context.Context, req *Request) (*Response, error)
	CheckConnection(ctx context.Context, req *Request) (*Response, error)
	ListProviderModels(ctx context.Context, req *Request) (*Response, error)
	ListProviderVoices(ctx context.Context, req *Request) (*Response, error)
	// MeteredAdmit and MeteredSettle serve /llm/v1/metered/admit and
	// /llm/v1/metered/settle (#1238): a metered tool operation is admitted by
	// the gateway's budget gate before its provider is called, then billed.
	MeteredAdmit(ctx context.Context, req *Request) (*Response, error)
	MeteredSettle(ctx context.Context, req *Request) (*Response, error)
	// GovernanceStatus reads the gateway's enforcement state. It carries no
	// identity.
	GovernanceStatus(ctx context.Context) (*Response, error)
	// Realtime opens a realtime session (/llm/v1/realtime). req carries the
	// caller's WebSocket handshake headers. A refusal returns no session and
	// the refusal Response; an accepted session returns the 101 head (its
	// headers carry the negotiated Sec-WebSocket-Protocol) and the session.
	Realtime(ctx context.Context, req *Request) (RealtimeSession, *Response, error)
	// Do dispatches req to the method its Method and Path name. It returns
	// ErrNoGatewayRoute for a request no method serves.
	Do(ctx context.Context, req *Request) (*Response, error)
}

// RealtimeSession is the gateway side of a live realtime session.
type RealtimeSession interface {
	// Recv returns the next event from the gateway.
	Recv() (RealtimeEvent, error)
	// Send sends one caller message.
	Send(data []byte, binary bool) error
	// Pong answers the gateway's ping id once the caller's pong arrived.
	Pong(id uint64) error
	// Close tells the gateway the caller closed with code and reason.
	Close(code int, reason string) error
	// End releases the session.
	End()
}

// RealtimeEvent is one gateway event. Exactly one of Message, PingID,
// Close is set.
type RealtimeEvent struct {
	Message []byte
	Binary  bool
	// PingID asks the caller side to ping the browser and Pong this id.
	PingID uint64
	// CloseCode and CloseReason end the session.
	Closed      bool
	CloseCode   int
	CloseReason string
}
