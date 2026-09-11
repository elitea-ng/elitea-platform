// Package predict serves POST /api/v2/elitea_core/predict_llm/prompt_lib/{projectID}
// (#194): one stateless turn against an LLM, with no agent, no tools, no
// version id and no conversation.
//
// WHAT THIS REPLACES. router.go's NOTE(#126) recorded this path among the
// routes that stood behind a nil `RouterConfig.Predictor` gate nothing ever
// assigned. Their only implementation was the prototype Redis RPC client to
// pylon-indexer, which published raw JSON onto a channel served through
// arbiter's gzip(pickle(...)) codec — every call was dropped on decode — so
// #126 deleted the transport rather than repairing it. The capability stayed
// open as this issue.
//
// BLOCKING ONLY, ON PURPOSE. Legacy's predict_sio_llm has two modes: blocking
// (await_task_timeout > 0, the answer comes back in the HTTP response) and
// async (await_task_timeout == 0, a task id comes back and the tokens arrive
// over the socket.io `application_predict` event). The async half is NOT
// ported and nothing here approximates it: elitea-main has no socket.io
// transport, and inventing a task id this service could never resolve would be
// the same shape of contract fiction the deleted RPC client was. Every request
// this handler accepts is answered synchronously with the generated content in
// the response body, whatever `await_task_timeout` says — see
// resolveRequestTimeout for the bounded meaning it is given instead.
//
// THE LLM HOP. The completion is performed by services/elitea-llm-gateway over
// the same mTLS-internal transport, with the same signed identity headers, the
// /llm reverse proxy and the configurations check-connection client already
// use (internal/llmproxy.NewMTLSTransport + SignIdentityHeaders). The gateway
// is the only LLM data plane: it resolves the project's provider credentials
// and model definitions itself and owns the SSRF-safe egress allowlist. There
// is no second client here, no direct provider dial, and no LiteLLM facade to
// fall back to.
package predict

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// Message is one chat turn on the wire to the gateway.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is what the handler asks the LLM plane for. It is
// deliberately not the HTTP request body: the body is a legacy-shaped
// document with an `extra: allow` tail, and only these fields have a meaning
// this service can honour.
type CompletionRequest struct {
	// ProjectID is the {projectID} path parameter, already bound to the
	// caller's membership by the router's projectScoped middleware. It is
	// signed into the identity headers, so the gateway bills and authorizes
	// against it.
	ProjectID string
	// UserID is the authenticated caller, signed alongside the project.
	UserID string
	// Model is llm_settings.model_name or the catalog default resolved before dispatch.
	Model    string
	Messages []Message
	// Temperature and MaxTokens are pointers so "absent" and "explicitly 0"
	// stay distinguishable — 0 temperature is a legitimate, meaningful value.
	Temperature     *float64
	MaxTokens       *int
	ReasoningEffort string
}

// ErrNoContent reports a 200 from the gateway that carried no assistant text.
// It is separated from a transport failure because it is not one: the hop
// worked and the answer is unusable, which the handler must not report as
// success (the caller renders the content straight into a document).
var ErrNoContent = errors.New("predict: gateway returned no completion content")

// Completer performs one blocking completion. Implementations MUST NOT
// fabricate content: every non-empty string this returns is rendered by the
// caller as model output.
type Completer interface {
	Complete(ctx context.Context, req CompletionRequest) (string, error)
}

// GatewayCompleter is the only production implementation: an HTTP client for
// elitea-llm-gateway's OpenAI-dialect chat-completions endpoint.
type GatewayCompleter struct {
	httpClient     *http.Client
	baseURL        string
	identitySecret []byte
}

// NewGatewayCompleter builds a completer against gatewayBaseURL (e.g.
// "https://elitea-llm-gateway-svc:8443") over transport. The client carries NO
// client-level timeout: the per-request deadline comes from the handler's
// context (resolveRequestTimeout), so a caller's await_task_timeout is the one
// bound that applies rather than being silently capped by a second, hidden one.
func NewGatewayCompleter(gatewayBaseURL string, transport http.RoundTripper, identitySecret string) *GatewayCompleter {
	return &GatewayCompleter{
		httpClient:     &http.Client{Transport: transport},
		baseURL:        strings.TrimRight(gatewayBaseURL, "/"),
		identitySecret: []byte(identitySecret),
	}
}

// NewGatewayCompleterFromConfig builds the completer from the same
// LLM_GATEWAY_URL / mTLS / GATEWAY_IDENTITY_SECRET environment configuration
// the /llm reverse proxy and the check-connection client use, so an operator
// configures the gateway hop once.
//
// It returns (nil, nil) when gatewayURL is empty. Callers MUST treat that as
// "no LLM plane is composed" and answer 503 naming the variable — never 404,
// and never a stub that invents content. #126 is precisely what an invisible
// 404 costs.
func NewGatewayCompleterFromConfig(gatewayURL, clientCertFile, clientKeyFile, caFile, identitySecret string) (*GatewayCompleter, error) {
	if gatewayURL == "" {
		return nil, nil
	}
	transport, err := llmproxy.NewMTLSTransport(clientCertFile, clientKeyFile, caFile, "")
	if err != nil {
		return nil, fmt.Errorf("predict: build mTLS transport: %w", err)
	}
	return NewGatewayCompleter(gatewayURL, transport, identitySecret), nil
}

// chatCompletionsRequest is the OpenAI-dialect body the gateway serves as a
// catch-all under /llm/v1/* (services/elitea-llm-gateway/internal/llmproxy/handler.go).
type chatCompletionsRequest struct {
	Model           string    `json:"model,omitempty"`
	Messages        []Message `json:"messages"`
	Temperature     *float64  `json:"temperature,omitempty"`
	MaxTokens       *int      `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	// Streaming is never requested: this is the blocking mode, and the
	// handler has no transport to stream over.
	Stream bool `json:"stream"`
}

type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete performs one blocking chat completion.
//
// Every failure is returned as an error. It never returns ("", nil): an empty
// completion is ErrNoContent, so no caller can mistake a silent upstream for a
// successful empty answer.
func (c *GatewayCompleter) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	raw, err := json.Marshal(chatCompletionsRequest{
		Model:           req.Model,
		Messages:        req.Messages,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
	})
	if err != nil {
		return "", fmt.Errorf("predict: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/llm/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("predict: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// No execution id: this turn is made from a browser, not from a runtime
	// execution, so the identity tuple signs v1 exactly as the
	// check-connection client does.
	llmproxy.SignIdentityHeaders(httpReq.Header, c.identitySecret, req.ProjectID, req.UserID, "", "")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("predict: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The upstream body is not echoed: it can carry provider error text,
		// which is the repository's typed/safe API error boundary. The status
		// is enough for an operator reading the log line the handler writes.
		return "", fmt.Errorf("predict: gateway responded with status %d", resp.StatusCode)
	}

	var out chatCompletionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("predict: decode gateway response: %w", err)
	}
	for _, choice := range out.Choices {
		if choice.Message.Content != "" {
			return choice.Message.Content, nil
		}
	}
	return "", ErrNoContent
}

// requestTimeoutBounds is the meaning this port gives await_task_timeout.
//
// Legacy read it as "seconds to wait before the call gives up and goes async".
// There is no async half here (see the package doc), so it is reinterpreted as
// the deadline of THIS HTTP request and nothing else — clamped, because it is
// caller-supplied and the handler holds a worker for its whole duration. 0,
// absent, or negative takes the default rather than meaning "async": a request
// this service cannot answer asynchronously must not be answered with silence.
const (
	defaultRequestTimeout = 60 * time.Second
	minRequestTimeout     = 5 * time.Second
	maxRequestTimeout     = 180 * time.Second
)

func resolveRequestTimeout(awaitTaskTimeoutSeconds *int) time.Duration {
	if awaitTaskTimeoutSeconds == nil || *awaitTaskTimeoutSeconds <= 0 {
		return defaultRequestTimeout
	}
	requested := time.Duration(*awaitTaskTimeoutSeconds) * time.Second
	if requested < minRequestTimeout {
		return minRequestTimeout
	}
	if requested > maxRequestTimeout {
		return maxRequestTimeout
	}
	return requested
}
