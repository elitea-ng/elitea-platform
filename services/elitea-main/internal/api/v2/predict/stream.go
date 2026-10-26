package predict

// stream.go — the streaming variant of the gateway completion (ADR-0037
// decision 4). It is the same hop as Complete: llmproxy.Gateway, i.e.
// GatewayService.ChatCompletions over gRPC with mutual TLS, the identity
// signed by the client. There is no internal HTTP call here. ChatCompletions is
// a server-streaming RPC already; the only difference from the blocking mode
// is `"stream": true` in the body and reading the OpenAI SSE dialect the
// gateway relays (internal/llmproxy/handler.go streamOpenAI in
// services/elitea-llm-gateway): data-only frames, a terminal `data: [DONE]`,
// and a mid-stream failure as a data frame carrying `{"error": ...}` with no
// [DONE] after it.
//
// Metering is unchanged: the gateway settles a stream from the provider's own
// usage trailer (and meters a loss when the stream dies early), against the
// project signed into the identity. One CompleteStream call is one metered
// completion, exactly like one Complete call.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// StreamingCompleter performs one completion and hands each text delta to
// onDelta as it arrives. It returns the full text. onDelta returning an error
// stops the stream and that error is returned.
//
// The contract mirrors Completer: no fabricated content, and never ("", nil).
type StreamingCompleter interface {
	CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(string) error) (string, error)
}

// ErrStreamCutOff reports a stream that ended without the gateway's [DONE]
// marker and without an error frame: the hop broke mid-answer.
var ErrStreamCutOff = errors.New("predict: gateway stream ended before completion")

// maxStreamLineBytes bounds one SSE line from the gateway. A chat chunk is
// small; a line past this bound is not a chunk this reader can trust.
const maxStreamLineBytes = 1 << 20

// streamChunk is the part of an OpenAI chat-completion chunk this reader uses.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error json.RawMessage `json:"error"`
}

// CompleteStream performs one streamed chat completion through the gateway.
func (c *GatewayCompleter) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(string) error) (string, error) {
	raw, err := json.Marshal(chatCompletionsRequest{
		Model:           req.Model,
		Messages:        req.Messages,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
		Stream:          true,
	})
	if err != nil {
		return "", fmt.Errorf("predict: encode request: %w", err)
	}
	attributionID := ""
	if ValidAttributionID(req.AttributionID) {
		attributionID = req.AttributionID
	}
	resp, err := c.gateway.ChatCompletions(ctx, &llmproxy.Request{
		Header:   http.Header{"Content-Type": {"application/json"}, "Accept": {"text/event-stream"}},
		Body:     bytes.NewReader(raw),
		Identity: llmproxy.Identity{ProjectID: req.ProjectID, UserID: req.UserID, ExecutionID: attributionID},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("predict: call gateway: %w", ctxErr)
		}
		if _, hasDeadline := ctx.Deadline(); hasDeadline && status.Code(err) == codes.DeadlineExceeded {
			return "", fmt.Errorf("predict: call gateway: %w: %w", context.DeadlineExceeded, err)
		}
		return "", fmt.Errorf("predict: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The upstream body is not echoed, as in Complete.
		return "", fmt.Errorf("predict: gateway responded with status %d", resp.StatusCode)
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			// Blank separators, comments and event names carry no text.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			if full.Len() == 0 {
				return "", ErrNoContent
			}
			return full.String(), nil
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return "", fmt.Errorf("predict: decode gateway stream chunk: %w", err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			// The provider error text is not echoed (typed/safe boundary).
			return "", errors.New("predict: gateway stream reported an upstream error")
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			full.WriteString(choice.Delta.Content)
			if err := onDelta(choice.Delta.Content); err != nil {
				return "", err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("predict: read gateway stream: %w", ctxErr)
		}
		return "", fmt.Errorf("predict: read gateway stream: %w", err)
	}
	return "", ErrStreamCutOff
}

// CompleteStream resolves an omitted model like Complete does, then streams
// through the next completer. A next completer that cannot stream answers in
// one piece, handed to onDelta as one delta.
func (c *defaultModelCompleter) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(string) error) (string, error) {
	resolved, err := c.resolve(ctx, req)
	if err != nil {
		return "", err
	}
	return Stream(ctx, c.next, resolved, onDelta)
}

// Stream runs req through completer, streaming when completer can and
// answering in one delta when it cannot. Callers use this instead of a type
// assertion so a test stub or a decorator without streaming still serves.
func Stream(ctx context.Context, completer Completer, req CompletionRequest, onDelta func(string) error) (string, error) {
	if streaming, ok := completer.(StreamingCompleter); ok {
		return streaming.CompleteStream(ctx, req, onDelta)
	}
	content, err := completer.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	if err := onDelta(content); err != nil {
		return "", err
	}
	return content, nil
}
