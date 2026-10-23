// Package llmproxy is the gateway's /llm transport layer: it mounts the chi
// routes under /llm, decodes the OpenAI/Anthropic wire dialects into
// bifrost/core request structs itself, calls the embedded core request methods
// directly, and writes a net/http SSE loop over the returned
// chan *schemas.BifrostStreamChunk (design §6.3).
//
// It deliberately does NOT call the integrations Create*RouteConfigs factories
// (they require a fasthttp lib.HandlerStore) nor the RequestConverter /
// ChatStreamResponseConverter helpers with an *http.Request — those are
// fasthttp-internal and cannot be mounted here. The gateway owns its decode +
// SSE framing.
package llmproxy

import (
	"context"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// BudgetChecker is the pre-LLM admission gate. *governance.GovernanceStore
// satisfies it; tests inject a stub so no live NATS is needed.
type BudgetChecker interface {
	// CheckBudget performs the pre-flight budget check. projectID is the
	// numeric Elitea project ID; scope + scopeID identify the budget tier
	// (typically "project" + numeric project ID string). periodStartUnix is
	// the current billing-period start in Unix seconds. reqCostNano is the
	// pre-estimated request cost (0 is valid for a pure admission check).
	CheckBudget(ctx context.Context, projectID int, scope, scopeID string, periodStartUnix, reqCostNano int64) (failmode.Decision, error)

	// UpdateUsage records a billed completion onto the authoritative counter
	// and publishes a write-behind delta. eventID must be unique per billed
	// completion (UUID or similar) to guarantee idempotent increments.
	//
	// dims carries the request's reporting dimensions for the usage ledger
	// (issue #320) and may be nil. It never influences the counter. A request
	// billed against two scopes passes dims on ONE of them, so the ledger holds
	// one row per request.
	UpdateUsage(ctx context.Context, projectID int, scope, scopeID, eventID string, costNano int64, periodStartUnix, periodEndUnix int64, dims *failmode.UsageDimensions) error

	// TryAlertCooldown checks and claims the soft-alert cooldown for the
	// given scope/scopeID/period. Returns true when the soft-alert should
	// fire (first crossing within the cooldown window), false when it is
	// already on cooldown. Errors are treated as "do not fire" to keep the
	// soft-alert path non-fatal.
	TryAlertCooldown(ctx context.Context, scope, scopeID string, periodStartUnix int64) (bool, error)
}

// AlertEventPublisher publishes the budget.soft_alert event onto the
// platform event space (NATS subject gateway.events.project.<id>.events —
// the same subject scheme elitea-main's natsbus EventBus subscribes to).
// *nats.Client satisfies it; the preflight FakeNATS records calls.
type AlertEventPublisher interface {
	// PublishSoftAlertEvent publishes the pre-marshalled event envelope for
	// projectID. Implementations must bound the operation with the ctx
	// deadline (every NATS op is bounded — CLAUDE.md).
	PublishSoftAlertEvent(ctx context.Context, projectID string, event []byte) error
}

// OpsEventPublisher publishes operator-only events onto gateway.events.ops.*.
// *nats.Client satisfies it.
//
// It is deliberately a SEPARATE port from AlertEventPublisher, not another
// method on it: the two have different audiences. budget.soft_alert is
// tenant-facing by design and rides the per-project subject elitea-main relays
// to project members; budget.unbilled_stream is a record of billing the gateway
// could not do, and telling a tenant in real time which of their streams went
// unbilled is an oracle for the conditions that produce it (gateway-review,
// issue #9). Operators see it; tenants do not.
type OpsEventPublisher interface {
	// PublishOpsEvent publishes the pre-marshalled envelope. Implementations
	// must bound the operation with the ctx deadline (every NATS op is
	// bounded — CLAUDE.md).
	PublishOpsEvent(ctx context.Context, event []byte) error
}

// CostEstimator resolves per-request LLM cost in int64 nano-USD.
// *cost.Calculator satisfies it; tests may inject a zero-cost stub.
type CostEstimator interface {
	// Cost prices a token-billed request. It is the token-only form of
	// CostUnits and every non-audio route uses it.
	Cost(ctx context.Context, provider, model string, inputTokens, outputTokens int64) cost.Cost

	// CostUnits prices a request in whichever denomination the provider
	// reported: tokens, seconds (as milliseconds) or characters (issue #323).
	// A returned Cost with an empty Basis is UNPRICED — the catalog carries no
	// rate for those units — and must NOT be read as a request that cost
	// nothing.
	CostUnits(ctx context.Context, provider, model string, u cost.Units) cost.Cost
}

// LLMRouter is the seam over the embedded bifrost/core client. The handler
// depends on this interface (not the concrete *bifrost.Bifrost) so the SSE
// loop, dialect decode, and error mapping are unit-testable with a fake that
// returns canned chunks and errors without a live provider.
//
// The methods mirror the core request methods the /llm surface needs
// (design §6.2). The second return is the concrete *schemas.BifrostError
// (which implements error) — handlers type-check it for status code, provider,
// and fallback fields rather than treating it as an opaque error.
type LLMRouter interface {
	// ChatCompletionRequest performs a unary OpenAI-dialect chat completion.
	ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError)
	// ChatCompletionStreamRequest performs a streaming OpenAI-dialect chat completion.
	ChatCompletionStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError)
	// TextCompletionRequest performs a unary text (legacy completions) request.
	TextCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError)
	// EmbeddingRequest performs an embeddings request.
	EmbeddingRequest(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError)
	// ResponsesRequest performs a unary Responses-API request. Anthropic
	// /v1/messages routes through this in v1.7.3 (RouteConfigTypeAnthropic).
	ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError)
	// ResponsesStreamRequest performs a streaming Responses-API request. This is
	// the Anthropic /v1/messages streaming path in v1.7.3.
	ResponsesStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError)
	// CountTokensRequest performs a synchronous (non-SSE) token count. Backs
	// /v1/messages/count_tokens (a Responses-API request under the hood).
	CountTokensRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError)
	// ImageGenerationRequest performs an image-generation request.
	ImageGenerationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError)
	// ImageEditRequest performs an image-edit request (multipart body).
	ImageEditRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError)
	// ImageVariationRequest performs an image-variation request (multipart body).
	ImageVariationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError)
	// SpeechRequest performs a text-to-speech request. Backs
	// /llm/v1/audio/speech (issue #323).
	SpeechRequest(ctx *schemas.BifrostContext, req *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError)
	// TranscriptionRequest performs a speech-to-text request (multipart body).
	// Backs /llm/v1/audio/transcriptions and /llm/v1/audio/translations.
	TranscriptionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError)
}

// bifrostLLMRouter is the default LLMRouter backed by the embedded
// *bifrost.Bifrost client. Its methods forward 1:1 to the core client; the
// *bifrost.Bifrost method set already matches LLMRouter, so this is a thin
// named wrapper that lets the concrete type satisfy the interface without
// leaking *bifrost.Bifrost into handler code.
type bifrostLLMRouter struct {
	core *bifrost.Bifrost
}

// NewBifrostRouter wraps an embedded bifrost/core client as an LLMRouter.
func NewBifrostRouter(core *bifrost.Bifrost) LLMRouter {
	return &bifrostLLMRouter{core: core}
}

func (r *bifrostLLMRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return r.core.ChatCompletionRequest(ctx, req)
}

func (r *bifrostLLMRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.core.ChatCompletionStreamRequest(ctx, req)
}

func (r *bifrostLLMRouter) TextCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return r.core.TextCompletionRequest(ctx, req)
}

func (r *bifrostLLMRouter) EmbeddingRequest(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	return r.core.EmbeddingRequest(ctx, req)
}

func (r *bifrostLLMRouter) ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	return r.core.ResponsesRequest(ctx, req)
}

func (r *bifrostLLMRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.core.ResponsesStreamRequest(ctx, req)
}

func (r *bifrostLLMRouter) CountTokensRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return r.core.CountTokensRequest(ctx, req)
}

func (r *bifrostLLMRouter) ImageGenerationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return r.core.ImageGenerationRequest(ctx, req)
}

func (r *bifrostLLMRouter) ImageEditRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return r.core.ImageEditRequest(ctx, req)
}

func (r *bifrostLLMRouter) ImageVariationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return r.core.ImageVariationRequest(ctx, req)
}

func (r *bifrostLLMRouter) SpeechRequest(ctx *schemas.BifrostContext, req *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return r.core.SpeechRequest(ctx, req)
}

func (r *bifrostLLMRouter) TranscriptionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return r.core.TranscriptionRequest(ctx, req)
}

// Compile-time assertions that both the concrete *bifrost.Bifrost method set
// and the default wrapper satisfy LLMRouter. The first guards against upstream
// signature drift at the pinned tag; the second guards the wrapper.
var (
	_ LLMRouter     = (*bifrost.Bifrost)(nil)
	_ LLMRouter     = (*bifrostLLMRouter)(nil)
	_ CostEstimator = (*cost.Calculator)(nil)
)
