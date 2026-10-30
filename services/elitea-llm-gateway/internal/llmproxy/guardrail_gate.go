package llmproxy

// guardrail_gate.go — the content guardrail pipeline on the /llm path
// (ADR-0043 decision 1, issue #1500). internal/guardrail holds the detectors
// and the stream filter; this file selects the rows of a request and applies
// them to each dialect's request and answer.
//
// # Where each stage runs
//
//	input   after mapModel and the MCP allowlist, BEFORE the budget gate: a
//	        refused request moves no counter. It reads every text part of the
//	        request messages (and the Responses instructions) and masks them
//	        in place, so the provider never receives a masked match.
//	output  non-streamed: on the decoded answer, before the gateway writes it.
//	        Streamed: per chunk, through guardrail.StreamFilter (stream.go
//	        there documents the look-behind buffer and its latency).
//
// Embeddings skip the output stage. Their input stage runs only rows with
// apply_to_embeddings. Images, audio, count_tokens and realtime carry no
// guardrail in this batch.
//
// # Refusals
//
// A block is 400 `guardrail_error` / `guardrail_blocked`. A detector failure
// on a closed row is 503 `guardrail_error` / `guardrail_unavailable`. The
// message names the row, the detector and the labels, never the content. A
// block on a streamed answer ends the stream with the same body as an error
// event. A blocked answer is still billed: the provider produced it.
//
// # Verdicts
//
// Every detector run is a guardrail.Verdict, merged per request in one
// collector on the request context, and handed to the request log, which
// writes gateway.guardrail_verdicts (internal/requestlog/verdicts.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// Refusal codes.
const (
	guardrailErrorType       = "guardrail_error"
	guardrailCodeBlocked     = "guardrail_blocked"
	guardrailCodeUnavailable = "guardrail_unavailable"
	lossReasonGuardrail      = "guardrail_blocked"
)

// GuardrailSecretResolver resolves the {{secret.NAME}} reference of an http
// adapter's auth header. main wires it to the Fernet vault of the platform's
// public project, where platform administrators keep shared secrets.
type GuardrailSecretResolver func(ctx context.Context, ref string) (string, error)

// WithGuardrailSecrets wires the adapter secret resolver. Without it an
// adapter that names auth_secret fails, and its row's fail mode applies.
func WithGuardrailSecrets(resolve GuardrailSecretResolver) HandlerOption {
	return func(h *Handler) { h.guardrailSecrets = resolve }
}

type guardrailCollectorKey struct{}

// guardrailCollector returns the request's verdict collector, creating it on
// first use.
func guardrailCollector(ctx *schemas.BifrostContext) *guardrail.Collector {
	if col, ok := ctx.Value(guardrailCollectorKey{}).(*guardrail.Collector); ok {
		return col
	}
	col := guardrail.NewCollector()
	ctx.SetValue(guardrailCollectorKey{}, col)
	return col
}

// recordGuardrailVerdicts hands the request's verdicts to the request log.
// Handlers defer it, so it runs after the last stage.
func recordGuardrailVerdicts(ctx *schemas.BifrostContext) {
	col, ok := ctx.Value(guardrailCollectorKey{}).(*guardrail.Collector)
	if !ok {
		return
	}
	requestlog.FromContext(ctx).SetGuardrailVerdicts(col.Verdicts())
}

// guardrailPipeline returns the rows of stage that select the request, or nil.
func (h *Handler) guardrailPipeline(ctx *schemas.BifrostContext, provider, model, stage, traffic string) *guardrail.Pipeline {
	if h.policy == nil {
		return nil
	}
	snap := h.policy.Current()
	if !snap.HasGuardrails() {
		return nil
	}
	sub := h.policySubject(ctx, schemas.ModelProvider(provider), model)
	if snap.HasAgentGuardrails() {
		sub.Agent = h.rateLimiter.Agent(ctx, identityExecutionFromCtx(ctx))
	}
	rows := snap.Guardrails(sub, stage, traffic)
	if len(rows) == 0 {
		return nil
	}
	return &guardrail.Pipeline{Stage: stage, Rows: rows, Env: h.guardrailEnv(), Collector: guardrailCollector(ctx)}
}

// guardrailEnv builds the adapter environment. The client is the probe
// client: no redirects, no keep-alive, and a private address only when the
// egress allowlist names a private destination. A handler without an egress
// policy refuses every adapter call.
func (h *Handler) guardrailEnv() guardrail.Env {
	env := guardrail.Env{}
	if h.egressPolicy != nil {
		env.EgressAllows = h.egressPolicy.EgressAllows
		env.HTTPClient = newCheckConnectionProbeClientWithTimeout(
			h.egressPolicy.EgressPrivateNetworkAllowed(), guardrail.MaxTimeout)
	}
	if h.guardrailSecrets != nil {
		env.ResolveSecret = h.guardrailSecrets
	}
	return env
}

// guardrailRefusal is the status, code and message of a verdict that refuses.
func guardrailRefusal(v *guardrail.Verdict) (int, string, string) {
	status, code := http.StatusBadRequest, guardrailCodeBlocked
	what := "blocked the content"
	if v.Outcome == guardrail.OutcomeErrorClosed {
		status, code = http.StatusServiceUnavailable, guardrailCodeUnavailable
		what = "failed and its fail mode is closed"
	}
	msg := "content guardrail " + strconv.Quote(v.Row) + " detector " + strconv.Quote(v.Detector) + " " + what +
		" (stage " + v.Stage
	if len(v.Labels) > 0 {
		msg += ", labels " + strings.Join(v.Labels, ",")
	}
	return status, code, msg + ")"
}

func writeGuardrailRefusal(w http.ResponseWriter, v *guardrail.Verdict) {
	status, code, msg := guardrailRefusal(v)
	writeError(w, status, guardrailErrorType, msg, code)
}

// guardrailErrorJSON is the OpenAI-shaped refusal body for a stream event.
func guardrailErrorJSON(v *guardrail.Verdict) string {
	_, code, msg := guardrailRefusal(v)
	data, _ := json.Marshal(openAIError{Error: openAIErrorFields{Message: msg, Type: guardrailErrorType, Code: code}})
	return string(data)
}

// anthropicGuardrailErrorJSON is the refusal in the Anthropic stream error
// frame, the frame the Messages stream already uses for a provider error.
func anthropicGuardrailErrorJSON(v *guardrail.Verdict) string {
	_, code, msg := guardrailRefusal(v)
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": guardrailErrorType, "code": code, "message": msg},
	})
	return string(data)
}

// ---------------------------------------------------------------------------
// Input stage

// applyInputGuardrails runs the input stage on texts and writes the masked
// values back. It returns false after it has written a refusal.
func (h *Handler) applyInputGuardrails(w http.ResponseWriter, ctx *schemas.BifrostContext, provider, model, traffic string, texts []*string) bool {
	pipe := h.guardrailPipeline(ctx, provider, model, guardrail.StageInput, traffic)
	if pipe == nil || len(texts) == 0 {
		return true
	}
	parts := make([]string, len(texts))
	for i, t := range texts {
		parts[i] = *t
	}
	masked, blocked := pipe.CheckAll(ctx, parts)
	if blocked != nil {
		writeGuardrailRefusal(w, blocked)
		return false
	}
	for i, t := range texts {
		*t = masked[i]
	}
	return true
}

func chatMessageContentTexts(c *schemas.ChatMessageContent, out []*string) []*string {
	if c == nil {
		return out
	}
	if c.ContentStr != nil {
		out = append(out, c.ContentStr)
	}
	for i := range c.ContentBlocks {
		if c.ContentBlocks[i].Text != nil {
			out = append(out, c.ContentBlocks[i].Text)
		}
	}
	return out
}

func chatRequestTexts(req *schemas.BifrostChatRequest) []*string {
	var out []*string
	for i := range req.Input {
		out = chatMessageContentTexts(req.Input[i].Content, out)
	}
	return out
}

func responsesMessageTexts(msgs []schemas.ResponsesMessage, out []*string) []*string {
	for i := range msgs {
		c := msgs[i].Content
		if c == nil {
			continue
		}
		if c.ContentStr != nil {
			out = append(out, c.ContentStr)
		}
		for j := range c.ContentBlocks {
			if c.ContentBlocks[j].Text != nil {
				out = append(out, c.ContentBlocks[j].Text)
			}
		}
	}
	return out
}

func responsesRequestTexts(req *schemas.BifrostResponsesRequest) []*string {
	var out []*string
	if req.Params != nil && req.Params.Instructions != nil {
		out = append(out, req.Params.Instructions)
	}
	return responsesMessageTexts(req.Input, out)
}

func textRequestTexts(req *schemas.BifrostTextCompletionRequest) []*string {
	if req.Input == nil {
		return nil
	}
	var out []*string
	if req.Input.PromptStr != nil {
		out = append(out, req.Input.PromptStr)
	}
	for i := range req.Input.PromptArray {
		out = append(out, &req.Input.PromptArray[i])
	}
	return out
}

func embeddingRequestTexts(req *schemas.BifrostEmbeddingRequest) []*string {
	var out []*string
	for i := range req.Input {
		for j := range req.Input[i].Content {
			if req.Input[i].Content[j].Text != nil {
				out = append(out, req.Input[i].Content[j].Text)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Output stage, non-streamed

// applyOutputGuardrails runs the output stage on the answer's texts in place.
// It returns the refusing verdict, or nil.
func (h *Handler) applyOutputGuardrails(ctx *schemas.BifrostContext, provider, model string, texts []*string) *guardrail.Verdict {
	pipe := h.guardrailPipeline(ctx, provider, model, guardrail.StageOutput, policy.TrafficGeneration)
	if pipe == nil || len(texts) == 0 {
		return nil
	}
	parts := make([]string, len(texts))
	for i, t := range texts {
		parts[i] = *t
	}
	masked, blocked := pipe.CheckAll(ctx, parts)
	if blocked != nil {
		return blocked
	}
	for i, t := range texts {
		*t = masked[i]
	}
	return nil
}

func chatResponseTexts(resp *schemas.BifrostChatResponse) []*string {
	if resp == nil {
		return nil
	}
	var out []*string
	for i := range resp.Choices {
		c := &resp.Choices[i]
		if c.ChatNonStreamResponseChoice != nil && c.Message != nil {
			out = chatMessageContentTexts(c.Message.Content, out)
		}
		if c.TextCompletionResponseChoice != nil && c.Text != nil {
			out = append(out, c.Text)
		}
	}
	return out
}

func textResponseTexts(resp *schemas.BifrostTextCompletionResponse) []*string {
	if resp == nil {
		return nil
	}
	var out []*string
	for i := range resp.Choices {
		c := &resp.Choices[i]
		if c.TextCompletionResponseChoice != nil && c.Text != nil {
			out = append(out, c.Text)
		}
	}
	return out
}

func responsesResponseTexts(resp *schemas.BifrostResponsesResponse) []*string {
	if resp == nil {
		return nil
	}
	return responsesMessageTexts(resp.Output, nil)
}

// ---------------------------------------------------------------------------
// Output stage, streamed

// guardedStream filters the text of one streamed answer. Each text channel
// (a chat choice, a Responses content part) has its own filter.
type guardedStream struct {
	pipe    *guardrail.Pipeline
	filters map[string]*guardrail.StreamFilter
	order   []string
	// chat: the last chunk's id, model and object, for the synthetic chunk
	// that carries the held text when the stream closes without a finish.
	lastChat *schemas.BifrostChatResponse
	// responses: the coordinates of each filter's channel.
	coords map[string]responsesCoord
}

type responsesCoord struct {
	outputIndex  *int
	contentIndex *int
	itemID       *string
}

// newGuardedStream returns nil when no output row selects the request.
func (h *Handler) newGuardedStream(ctx *schemas.BifrostContext, provider, model string) *guardedStream {
	pipe := h.guardrailPipeline(ctx, provider, model, guardrail.StageOutput, policy.TrafficGeneration)
	if pipe == nil {
		return nil
	}
	return &guardedStream{pipe: pipe, filters: map[string]*guardrail.StreamFilter{}, coords: map[string]responsesCoord{}}
}

func (g *guardedStream) filter(key string) *guardrail.StreamFilter {
	f, ok := g.filters[key]
	if !ok {
		f = g.pipe.NewStream()
		g.filters[key] = f
		g.order = append(g.order, key)
	}
	return f
}

// chatChunk filters the content deltas of a chat chunk in place.
// A nil stream (no output row) changes nothing.
func (g *guardedStream) chatChunk(ctx context.Context, resp *schemas.BifrostChatResponse) *guardrail.Verdict {
	if g == nil || resp == nil {
		return nil
	}
	g.lastChat = resp
	for i := range resp.Choices {
		c := &resp.Choices[i]
		if c.ChatStreamResponseChoice == nil {
			continue
		}
		f := g.filter("chat:" + strconv.Itoa(c.Index))
		text := ""
		if c.Delta != nil && c.Delta.Content != nil {
			text = *c.Delta.Content
		}
		out, blocked := f.Push(ctx, text)
		if blocked != nil {
			return blocked
		}
		if c.FinishReason != nil {
			tail, b := f.Finish(ctx)
			if b != nil {
				return b
			}
			out += tail
		}
		if out == "" && (c.Delta == nil || c.Delta.Content == nil) {
			continue
		}
		if c.Delta == nil {
			c.Delta = &schemas.ChatStreamResponseChoiceDelta{}
		}
		c.Delta.Content = &out
	}
	return nil
}

// finishChat releases the text still held when the provider closed the
// stream without a finish reason. It returns a synthetic chunk carrying it
// (or nil), or the refusing verdict.
func (g *guardedStream) finishChat(ctx context.Context) (*schemas.BifrostChatResponse, *guardrail.Verdict) {
	if g == nil {
		return nil, nil
	}
	var choices []schemas.BifrostResponseChoice
	for _, key := range g.order {
		tail, blocked := g.filters[key].Finish(ctx)
		if blocked != nil {
			return nil, blocked
		}
		if tail == "" {
			continue
		}
		index, _ := strconv.Atoi(strings.TrimPrefix(key, "chat:"))
		content := tail
		choices = append(choices, schemas.BifrostResponseChoice{
			Index: index,
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
				Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &content},
			},
		})
	}
	if len(choices) == 0 {
		return nil, nil
	}
	out := &schemas.BifrostChatResponse{Choices: choices, Object: "chat.completion.chunk"}
	if g.lastChat != nil {
		out.ID, out.Model, out.Created = g.lastChat.ID, g.lastChat.Model, g.lastChat.Created
	}
	return out, nil
}

func responsesKey(outputIndex, contentIndex *int) string {
	o, c := 0, 0
	if outputIndex != nil {
		o = *outputIndex
	}
	if contentIndex != nil {
		c = *contentIndex
	}
	return "resp:" + strconv.Itoa(o) + ":" + strconv.Itoa(c)
}

func guardrailIndex(v int) *int { return &v }

// responsesEvent filters one Responses stream event in place. It returns the
// events to send before it (a delta that releases held text ahead of a done
// event), or the refusing verdict.
func (g *guardedStream) responsesEvent(ctx context.Context, sr *schemas.BifrostResponsesStreamResponse) ([]*schemas.BifrostResponsesStreamResponse, *guardrail.Verdict) {
	if g == nil || sr == nil {
		return nil, nil
	}
	switch sr.Type {
	case schemas.ResponsesStreamResponseTypeOutputTextDelta:
		key := responsesKey(sr.OutputIndex, sr.ContentIndex)
		if _, seen := g.coords[key]; !seen {
			g.coords[key] = responsesCoord{outputIndex: sr.OutputIndex, contentIndex: sr.ContentIndex, itemID: sr.ItemID}
		}
		text := ""
		if sr.Delta != nil {
			text = *sr.Delta
		}
		out, blocked := g.filter(key).Push(ctx, text)
		if blocked != nil {
			return nil, blocked
		}
		sr.Delta = &out
		return nil, nil

	case schemas.ResponsesStreamResponseTypeOutputTextDone:
		key := responsesKey(sr.OutputIndex, sr.ContentIndex)
		before, full, blocked := g.finishKey(ctx, key, sr.Text)
		if blocked != nil {
			return nil, blocked
		}
		if full != nil {
			sr.Text = full
		}
		return before, nil

	case schemas.ResponsesStreamResponseTypeContentPartDone:
		if sr.Part == nil || sr.Part.Text == nil {
			return nil, nil
		}
		key := responsesKey(sr.OutputIndex, sr.ContentIndex)
		before, full, blocked := g.finishKey(ctx, key, sr.Part.Text)
		if blocked != nil {
			return nil, blocked
		}
		sr.Part.Text = full
		return before, nil

	case schemas.ResponsesStreamResponseTypeOutputItemDone:
		if sr.Item == nil || sr.Item.Content == nil {
			return nil, nil
		}
		var before []*schemas.BifrostResponsesStreamResponse
		for ci := range sr.Item.Content.ContentBlocks {
			block := &sr.Item.Content.ContentBlocks[ci]
			if block.Text == nil {
				continue
			}
			ev, full, blocked := g.finishKey(ctx, responsesKey(sr.OutputIndex, guardrailIndex(ci)), block.Text)
			if blocked != nil {
				return nil, blocked
			}
			block.Text = full
			before = append(before, ev...)
		}
		return before, nil

	case schemas.ResponsesStreamResponseTypeCompleted, schemas.ResponsesStreamResponseTypeIncomplete:
		if sr.Response == nil {
			return nil, nil
		}
		var before []*schemas.BifrostResponsesStreamResponse
		for oi := range sr.Response.Output {
			c := sr.Response.Output[oi].Content
			if c == nil {
				continue
			}
			for ci := range c.ContentBlocks {
				block := &c.ContentBlocks[ci]
				if block.Text == nil {
					continue
				}
				ev, full, blocked := g.finishKey(ctx, responsesKey(guardrailIndex(oi), guardrailIndex(ci)), block.Text)
				if blocked != nil {
					return nil, blocked
				}
				block.Text = full
				before = append(before, ev...)
			}
		}
		return before, nil
	}
	return nil, nil
}

// finishKey ends the channel key. It returns a delta event carrying the
// released tail (when the channel held text) and the masked full text to put
// in a done event. A done event for a channel that streamed no delta is
// checked whole.
func (g *guardedStream) finishKey(ctx context.Context, key string, full *string) ([]*schemas.BifrostResponsesStreamResponse, *string, *guardrail.Verdict) {
	f, ok := g.filters[key]
	if !ok {
		if full == nil {
			return nil, nil, nil
		}
		masked, blocked := g.pipe.Check(ctx, *full)
		if blocked != nil {
			return nil, nil, blocked
		}
		return nil, &masked, nil
	}
	tail, blocked := f.Finish(ctx)
	if blocked != nil {
		return nil, nil, blocked
	}
	released := f.Released()
	var before []*schemas.BifrostResponsesStreamResponse
	if tail != "" {
		before = append(before, g.deltaEvent(key, tail))
	}
	return before, &released, nil
}

func (g *guardedStream) deltaEvent(key, text string) *schemas.BifrostResponsesStreamResponse {
	coord := g.coords[key]
	return &schemas.BifrostResponsesStreamResponse{
		Type:         schemas.ResponsesStreamResponseTypeOutputTextDelta,
		OutputIndex:  coord.outputIndex,
		ContentIndex: coord.contentIndex,
		ItemID:       coord.itemID,
		Delta:        &text,
	}
}

// finishResponses releases the text of channels that never got a done event
// before the provider closed the stream.
func (g *guardedStream) finishResponses(ctx context.Context) ([]*schemas.BifrostResponsesStreamResponse, *guardrail.Verdict) {
	if g == nil {
		return nil, nil
	}
	var out []*schemas.BifrostResponsesStreamResponse
	for _, key := range g.order {
		tail, blocked := g.filters[key].Finish(ctx)
		if blocked != nil {
			return nil, blocked
		}
		if tail != "" {
			out = append(out, g.deltaEvent(key, tail))
		}
	}
	return out, nil
}
