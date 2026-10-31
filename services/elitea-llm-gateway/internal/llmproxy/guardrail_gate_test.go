package llmproxy

// The ADR-0043 decision 1 verification tests on the /llm path: a streamed
// answer that holds a secret ends with the guardrail error under `block`, and
// reaches the client masked under `mask`, the secret split across chunks.
// Both write a verdict without the matched text to the request log. An
// adapter timeout refuses on a closed row and proceeds with a flag on an open
// one.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// A GitHub token shape (secret rule github-token). Not a real token.
const guardTestSecret = "ghp_" + "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210"

type verdictSink struct {
	mu      sync.Mutex
	records []requestlog.Record
}

func (s *verdictSink) WriteBatch(_ context.Context, records []requestlog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
	return nil
}

func (s *verdictSink) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *verdictSink) verdicts() []guardrail.Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []guardrail.Verdict
	for _, r := range s.records {
		out = append(out, r.Guardrails...)
	}
	return out
}

func guardRow(name string, guard map[string]any) policy.Row {
	return govRow(policy.TypeGuardrail, name, map[string]any{"guardrail": guard})
}

func secretsRow(stage, action string) policy.Row {
	return guardRow("secrets-"+action, map[string]any{
		"stage": stage, "stream_window_bytes": 64.0,
		"detectors": []any{map[string]any{"type": "secrets", "action": action}},
	})
}

// serveLogged runs one handler call behind the request log middleware and
// returns the response and the verdicts the log received.
func serveLogged(t *testing.T, serve http.HandlerFunc, req *http.Request) (*httptest.ResponseRecorder, []guardrail.Verdict) {
	t.Helper()
	sink := &verdictSink{}
	rec := requestlog.New(sink, nil)
	resp := httptest.NewRecorder()
	requestlog.Middleware(rec)(serve).ServeHTTP(resp, req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec.Stop(ctx)
	return resp, sink.verdicts()
}

func guardHandler(t *testing.T, router *trackingRouter, rows ...policy.Row) *Handler {
	t.Helper()
	return NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithEgressPolicy(fakeEgressPolicy{allow: true, privateNetwork: true}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, rows...)}, nil, nil))
}

func guardChatDelta(text string, finish bool) *schemas.BifrostStreamChunk {
	choice := schemas.BifrostResponseChoice{
		Index:                    0,
		ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: strPtr(text)}},
	}
	if finish {
		choice.FinishReason = strPtr("stop")
	}
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "gpt-4o",
		Choices: []schemas.BifrostResponseChoice{choice},
	}}
}

// splitAnswer is an answer with the secret split across two chunks, inside
// text long enough to exercise the look-behind window.
func splitAnswer() []string {
	lead := strings.Repeat("The deploy key is below. ", 4)
	return []string{lead + guardTestSecret[:12], guardTestSecret[12:] + " and nothing else.", strings.Repeat(" ok", 30)}
}

// sseContent concatenates the content deltas of an OpenAI chat SSE body.
func sseContent(t *testing.T, body string) (string, []string) {
	t.Helper()
	var content strings.Builder
	var errs []string
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *openAIErrorFields `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		if frame.Error != nil {
			errs = append(errs, frame.Error.Code)
		}
		for _, c := range frame.Choices {
			if c.Delta.Content != nil {
				content.WriteString(*c.Delta.Content)
			}
		}
	}
	return content.String(), errs
}

func assertVerdictWithoutContent(t *testing.T, verdicts []guardrail.Verdict, outcome string) {
	t.Helper()
	found := false
	for _, v := range verdicts {
		if v.Outcome == outcome && v.DetectorType == guardrail.TypeSecrets {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s verdict in the request log: %+v", outcome, verdicts)
	}
	raw, _ := json.Marshal(verdicts)
	if strings.Contains(string(raw), guardTestSecret[:10]) || strings.Contains(string(raw), guardTestSecret[12:]) {
		t.Fatalf("a verdict carries the matched text: %s", raw)
	}
}

func TestGuardrailStreamBlockEndsWithTheGuardrailError(t *testing.T) {
	router := &trackingRouter{}
	parts := splitAnswer()
	router.streamChan = newChunkChan(guardChatDelta(parts[0], false), guardChatDelta(parts[1], false), guardChatDelta(parts[2], true))
	h := guardHandler(t, router, secretsRow("output", "block"))

	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", true))
	content, errs := sseContent(t, resp.Body.String())
	if len(errs) != 1 || errs[0] != guardrailCodeBlocked {
		t.Fatalf("stream errors = %v, want one %s; body=%s", errs, guardrailCodeBlocked, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "[DONE]") {
		t.Fatal("a blocked stream ended with [DONE]")
	}
	if strings.Contains(content, "ghp_") {
		t.Fatalf("the client received the blocked secret: %q", content)
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeBlocked)
}

func TestGuardrailStreamMaskNeverSendsTheRawSecret(t *testing.T) {
	router := &trackingRouter{}
	parts := splitAnswer()
	router.streamChan = newChunkChan(guardChatDelta(parts[0], false), guardChatDelta(parts[1], false), guardChatDelta(parts[2], true))
	h := guardHandler(t, router, secretsRow("output", "mask"))

	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", true))
	content, errs := sseContent(t, resp.Body.String())
	if len(errs) != 0 {
		t.Fatalf("stream errors = %v", errs)
	}
	if strings.Contains(resp.Body.String(), guardTestSecret[:8]) || strings.Contains(resp.Body.String(), guardTestSecret[12:24]) {
		t.Fatalf("the client received raw secret text: %s", resp.Body.String())
	}
	want := strings.Join(parts, "")
	want = strings.Replace(want, guardTestSecret, guardrail.MaskMarker("github-token"), 1)
	if content != want {
		t.Fatalf("masked content = %q, want %q", content, want)
	}
	if !strings.Contains(resp.Body.String(), "[DONE]") {
		t.Fatal("a masked stream did not end with [DONE]")
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)
}

func TestGuardrailStreamMaskReleasesHeldTextWithoutAFinishChunk(t *testing.T) {
	router := &trackingRouter{}
	router.streamChan = newChunkChan(guardChatDelta("short answer "+guardTestSecret[:20], false), guardChatDelta(guardTestSecret[20:], false))
	h := guardHandler(t, router, secretsRow("output", "mask"))

	resp, _ := serveLogged(t, h.Chat, chatReqWithProject(t, "42", true))
	content, _ := sseContent(t, resp.Body.String())
	if content != "short answer "+guardrail.MaskMarker("github-token") {
		t.Fatalf("content = %q", content)
	}
}

func guardResponsesDelta(text string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeOutputTextDelta, OutputIndex: guardrailIndex(0),
		ContentIndex: guardrailIndex(0), ItemID: strPtr("msg_1"), Delta: strPtr(text),
	}}
}

func TestGuardrailResponsesStreamMasksDeltasAndDoneEvents(t *testing.T) {
	router := &trackingRouter{}
	parts := splitAnswer()
	full := strings.Join(parts, "")
	done := &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeOutputTextDone, OutputIndex: guardrailIndex(0),
		ContentIndex: guardrailIndex(0), ItemID: strPtr("msg_1"), Text: strPtr(full),
	}}
	completed := &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeCompleted,
		Response: &schemas.BifrostResponsesResponse{Output: []schemas.ResponsesMessage{{
			Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
				Type: schemas.ResponsesOutputMessageContentTypeText, Text: strPtr(full),
			}}},
		}}},
	}}
	router.streamChan = newChunkChan(guardResponsesDelta(parts[0]), guardResponsesDelta(parts[1]), guardResponsesDelta(parts[2]), done, completed)
	h := guardHandler(t, router, secretsRow("output", "mask"))

	body := `{"model":"openai/gpt-4o","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/responses", strings.NewReader(body))
	req.Header.Set(headerProjectID, "42")
	resp, verdicts := serveLogged(t, h.Responses, req)
	if strings.Contains(resp.Body.String(), guardTestSecret[:8]) || strings.Contains(resp.Body.String(), guardTestSecret[12:24]) {
		t.Fatalf("the client received raw secret text: %s", resp.Body.String())
	}
	if strings.Count(resp.Body.String(), guardrail.MaskMarker("github-token")) < 3 {
		t.Fatalf("the delta, done and completed events are not all masked: %s", resp.Body.String())
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)
}

func TestGuardrailAnthropicStreamMasks(t *testing.T) {
	router := &trackingRouter{}
	parts := splitAnswer()
	router.streamChan = newChunkChan(guardResponsesDelta(parts[0]), guardResponsesDelta(parts[1]), guardResponsesDelta(parts[2]))
	h := guardHandler(t, router, secretsRow("output", "mask"))

	resp, _ := serveLogged(t, h.Messages, messagesStreamReqWithProject(t, "42"))
	if strings.Contains(resp.Body.String(), guardTestSecret[:8]) || strings.Contains(resp.Body.String(), guardTestSecret[12:24]) {
		t.Fatalf("the client received raw secret text: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), guardrail.MaskMarker("github-token")) {
		t.Fatalf("the masked text did not reach the client: %s", resp.Body.String())
	}
}

func TestGuardrailAnthropicStreamBlock(t *testing.T) {
	router := &trackingRouter{}
	parts := splitAnswer()
	router.streamChan = newChunkChan(guardResponsesDelta(parts[0]), guardResponsesDelta(parts[1]), guardResponsesDelta(parts[2]))
	h := guardHandler(t, router, secretsRow("output", "block"))

	resp, _ := serveLogged(t, h.Messages, messagesStreamReqWithProject(t, "42"))
	if !strings.Contains(resp.Body.String(), "event: error") || !strings.Contains(resp.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("no guardrail error event: %s", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "ghp_") {
		t.Fatalf("the client received the blocked secret: %s", resp.Body.String())
	}
}

func TestGuardrailUnaryOutputMaskAndInputBlock(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "x", Choices: []schemas.BifrostResponseChoice{{
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{
			Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: strPtr("key " + guardTestSecret)},
		}},
	}}}
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", false))
	if resp.Code != http.StatusOK || strings.Contains(resp.Body.String(), "ghp_") ||
		!strings.Contains(resp.Body.String(), guardrail.MaskMarker("github-token")) {
		t.Fatalf("unary answer not masked: %d %s", resp.Code, resp.Body.String())
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)

	router2 := &trackingRouter{}
	h2 := guardHandler(t, router2, secretsRow("input", "block"))
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"use ` + guardTestSecret + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(headerProjectID, "42")
	resp2, verdicts2 := serveLogged(t, h2.Chat, req)
	if resp2.Code != http.StatusBadRequest || !strings.Contains(resp2.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("input block = %d %s", resp2.Code, resp2.Body.String())
	}
	if router2.called.Load() {
		t.Fatal("the provider was called for a blocked request")
	}
	assertVerdictWithoutContent(t, verdicts2, guardrail.OutcomeBlocked)
}

func TestGuardrailInputMaskReachesTheProviderMasked(t *testing.T) {
	router := &capturingRouter{}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, secretsRow("input", "mask"))}, nil, nil))
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"use ` + guardTestSecret + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(headerProjectID, "42")
	rec := httptest.NewRecorder()
	h.Chat(rec, req)
	router.mu.Lock()
	got := router.last
	router.mu.Unlock()
	if got == nil || len(got.Input) == 0 || got.Input[0].Content == nil || got.Input[0].Content.ContentStr == nil {
		t.Fatalf("no dispatched request: %d %s", rec.Code, rec.Body.String())
	}
	if *got.Input[0].Content.ContentStr != "use "+guardrail.MaskMarker("github-token") {
		t.Fatalf("the provider received %q", *got.Input[0].Content.ContentStr)
	}
}

func TestGuardrailEmbeddingsSkipOutputAndUnflaggedInputRows(t *testing.T) {
	router := &trackingRouter{}
	router.embResp = &schemas.BifrostEmbeddingResponse{}
	h := guardHandler(t, router, secretsRow("input", "block"), secretsRow("output", "block"))
	body := `{"model":"openai/text-embedding-3-small","input":"` + guardTestSecret + `"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings", strings.NewReader(body))
	req.Header.Set(headerProjectID, "42")
	resp, verdicts := serveLogged(t, h.Embeddings, req)
	if resp.Code != http.StatusOK || len(verdicts) != 0 {
		t.Fatalf("a row without apply_to_embeddings ran on embeddings: %d %+v", resp.Code, verdicts)
	}

	optIn := guardRow("embed-in", map[string]any{
		"stage": "input", "apply_to_embeddings": true,
		"detectors": []any{map[string]any{"type": "secrets", "action": "block"}},
	})
	h2 := guardHandler(t, &trackingRouter{}, optIn)
	req2 := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings", strings.NewReader(body))
	req2.Header.Set(headerProjectID, "42")
	resp2, _ := serveLogged(t, h2.Embeddings, req2)
	if resp2.Code != http.StatusBadRequest {
		t.Fatalf("an apply_to_embeddings row did not run: %d %s", resp2.Code, resp2.Body.String())
	}
}

func slowClassifier(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"findings":[]}`))
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func adapterGuardRow(url, failMode string) policy.Row {
	return guardRow("classifier-"+failMode, map[string]any{
		"stage": "input", "timeout_ms": 100.0, "fail_mode": failMode,
		"detectors": []any{map[string]any{"name": "classifier", "type": "http", "action": "block", "url": url}},
	})
}

func TestGuardrailAdapterTimeoutClosedRefuses(t *testing.T) {
	srv := slowClassifier(t)
	router := &trackingRouter{}
	h := guardHandler(t, router, adapterGuardRow(srv.URL, guardrail.FailClosed))
	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", false))
	if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), guardrailCodeUnavailable) {
		t.Fatalf("closed timeout = %d %s", resp.Code, resp.Body.String())
	}
	if router.called.Load() {
		t.Fatal("the provider was called after a closed adapter failure")
	}
	if len(verdicts) != 1 || verdicts[0].Outcome != guardrail.OutcomeErrorClosed {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}

func TestGuardrailAdapterTimeoutOpenProceedsAndFlags(t *testing.T) {
	srv := slowClassifier(t)
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := guardHandler(t, router, adapterGuardRow(srv.URL, guardrail.FailOpen))
	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", false))
	if resp.Code != http.StatusOK || !router.called.Load() {
		t.Fatalf("open timeout did not proceed: %d %s", resp.Code, resp.Body.String())
	}
	if len(verdicts) != 1 || verdicts[0].Outcome != guardrail.OutcomeErrorOpen ||
		verdicts[0].Labels[0] != guardrail.LabelDetectorError {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}
