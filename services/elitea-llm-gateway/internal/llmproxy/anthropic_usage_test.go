package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// The Rust native facade sums uncached input, cache reads, and cache writes.
// The gateway must expose Anthropic counters, not cache-inclusive Bifrost input.
func TestMessagesCacheUsageDoesNotDoubleCountInput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "unary"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			response := &schemas.BifrostResponsesResponse{
				ID: strPtr("cache-usage"), Model: "claude-haiku",
				Usage: &schemas.ResponsesResponseUsage{
					InputTokens: 10000, OutputTokens: 23, TotalTokens: 10023,
					InputTokensDetails: &schemas.ResponsesResponseInputTokens{
						CachedReadTokens: 8000, CachedWriteTokens: 1500,
					},
				},
			}
			fake := &fakeRouter{respResp: response}
			if streaming {
				fake.streamChan = newChunkChan(
					&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
						Type:     schemas.ResponsesStreamResponseTypeCreated,
						Response: &schemas.BifrostResponsesResponse{ID: strPtr("cache-usage"), Model: "claude-haiku"},
					}},
					&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
						Type: schemas.ResponsesStreamResponseTypeCompleted, Response: response,
					}},
				)
			}
			body := `{"model":"claude-haiku","max_tokens":128,"messages":[{"role":"user","content":"hello"}]`
			if streaming {
				body += `,"stream":true`
			}
			rec := postJSON(t, NewHandler(fake, nil, nil).route(), "/llm/v1/messages", body+"}")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
			}
			payload := rec.Body.String()
			if streaming {
				payload = ""
				for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
					if strings.HasPrefix(frame, "event: message_delta\n") {
						if payload != "" {
							t.Fatal("duplicate terminal usage event")
						}
						payload = strings.TrimPrefix(frame, "event: message_delta\ndata: ")
					}
				}
			}
			var decoded struct {
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
					Read   int `json:"cache_read_input_tokens"`
					Write  int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
				t.Fatal(err)
			}
			u := decoded.Usage
			if u.Input != 500 || u.Read != 8000 || u.Write != 1500 || u.Output != 23 {
				t.Fatalf("incorrect Anthropic usage: %+v", u)
			}
			if u.Input+u.Read+u.Write+u.Output != 10023 {
				t.Fatal("native facade reconstruction would change combined occupancy")
			}
		})
	}
}
