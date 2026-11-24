package llmproxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestStream_NilChannelNilError_502 covers the router-contract-violation branch
// shared by all three SSE writers (streamOpenAI, streamResponses,
// streamAnthropic): the router returns a nil chunk channel AND a nil error.
// Ranging over the nil channel would hang the request forever, so each writer
// must abort with a 502 carrying the spec §2.5 OpenAI-shaped error body — and
// must do so BEFORE beginStream, so no SSE headers or frames reach the client.
func TestStream_NilChannelNilError_502(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"openai", "/llm/v1/chat/completions",
			`{"model":"gpt-4o","messages":[],"stream":true}`},
		{"responses", "/llm/v1/responses",
			`{"model":"gpt-4o","input":"hi","stream":true}`},
		{"anthropic", "/llm/v1/messages",
			`{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[],"stream":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Zero-value fakeRouter: streamChan and streamErr are both nil, so the
			// stream call returns exactly (nil, nil).
			h := newTestHandler(&fakeRouter{}, nil, nil)
			rec := postJSON(t, h.route(), tc.path, tc.body)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
			}
			var out openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("unmarshal 502 body: %v; raw=%s", err, rec.Body.String())
			}
			if out.Error.Type != "api_error" {
				t.Errorf("error.type = %q, want api_error (spec §2.5)", out.Error.Type)
			}
			if out.Error.Code != "bad_gateway" {
				t.Errorf("error.code = %q, want bad_gateway (spec §2.5)", out.Error.Code)
			}
			if out.Error.Message == "" {
				t.Error("error.message is empty; the 502 must explain the failure")
			}

			// No SSE headers and no SSE framing: the stream was never begun.
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json (no SSE headers)", ct)
			}
			for _, h := range []string{"Cache-Control", "Connection"} {
				if v := rec.Header().Get(h); v != "" {
					t.Errorf("%s = %q, want unset (stream must not have been begun)", h, v)
				}
			}
			for _, frame := range []string{"data: ", "event: "} {
				if strings.Contains(rec.Body.String(), frame) {
					t.Errorf("body contains SSE frame %q; got:\n%s", frame, rec.Body.String())
				}
			}
		})
	}
}

// disconnectWriter is an http.ResponseWriter + http.Flusher whose Write fails
// after the response headers are committed, simulating a client that hangs up
// mid-stream. The SSE loops must observe the write error and return without
// panicking or looping forever (the "client disconnected" branch).
type disconnectWriter struct {
	header  http.Header
	status  int
	writes  int
	flushes int
}

func (d *disconnectWriter) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}

func (d *disconnectWriter) Write(p []byte) (int, error) {
	d.writes++
	return 0, errors.New("connection reset by peer")
}

func (d *disconnectWriter) WriteHeader(status int) { d.status = status }
func (d *disconnectWriter) Flush()                 { d.flushes++ }

// TestStreamOpenAI_ClientDisconnect drives streamOpenAI against a writer that
// errors on the first chunk write; the loop must return on the write error
// rather than continuing to marshal/write subsequent chunks.
//
// The channel is pre-filled and closed (no separate producer goroutine): c2 is
// buffered and never consumed because the loop exits after the first write error.
// This exercises the "client disconnected" early-return branch without needing a
// live goroutine, since the SSE loop (`for chunk := range ch`) ranges over the
// pre-filled closed channel and returns on the first write failure.
func TestStreamOpenAI_ClientDisconnect(t *testing.T) {
	// Two chunks in a buffered, pre-closed channel. The first write fails and the
	// loop returns; c2 is never consumed (remains buffered) — that is the expected
	// behaviour. `writes == 1` is the authoritative assertion.
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1"}},
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c2"}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	w := &disconnectWriter{}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.Chat(w, req)

	if w.writes != 1 {
		t.Fatalf("writes = %d, want 1 (loop must stop on the first write error)", w.writes)
	}
}

// TestStreamOpenAI_SkipsNilChunk feeds a nil chunk between two real chunks; the
// nil must be skipped and both real chunks + [DONE] delivered.
func TestStreamOpenAI_SkipsNilChunk(t *testing.T) {
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1"}},
		nil,
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c2"}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions",
		`{"model":"gpt-4o","messages":[],"stream":true}`)
	body := rec.Body.String()
	for _, want := range []string{`"id":"c1"`, `"id":"c2"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q; got:\n%s", want, body)
		}
	}
	// Exactly two data chunks + DONE — the nil contributed no frame.
	if n := strings.Count(body, "data: "); n != 3 {
		t.Errorf("data frame count = %d, want 3 (nil chunk must be skipped)", n)
	}
}

// TestStreamResponses_ClientDisconnect drives the OpenAI Responses SSE loop
// against a failing writer.
func TestStreamResponses_ClientDisconnect(t *testing.T) {
	chunks := newChunkChan(&schemas.BifrostStreamChunk{
		BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type:     schemas.ResponsesStreamResponseTypeCreated,
			Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")},
		},
	})
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	w := &disconnectWriter{}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.Responses(w, req)

	if w.writes != 1 {
		t.Fatalf("writes = %d, want 1 (loop must stop on write error)", w.writes)
	}
}

// TestStreamResponses_SkipsNilAndEmptyChunks covers the two continue branches:
// a nil chunk and a chunk whose BifrostResponsesStreamResponse is nil (neither
// an error nor a payload) must both be skipped, leaving only the real event.
func TestStreamResponses_SkipsNilAndEmptyChunks(t *testing.T) {
	chunks := newChunkChan(
		nil,
		&schemas.BifrostStreamChunk{}, // non-nil chunk, nil sub-response
		&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type:     schemas.ResponsesStreamResponseTypeCreated,
			Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")},
		}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses",
		`{"model":"gpt-4o","input":"hi","stream":true}`)
	body := rec.Body.String()
	if n := strings.Count(body, "event: "); n != 1 {
		t.Errorf("event frame count = %d, want 1 (nil + empty chunks must be skipped)", n)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("responses stream must not emit [DONE]; got:\n%s", body)
	}
}

// TestStreamAnthropic_ClientDisconnect drives the Anthropic SSE loop against a
// failing writer; the per-event write error must end the loop.
func TestStreamAnthropic_ClientDisconnect(t *testing.T) {
	chunks := newChunkChan(&schemas.BifrostStreamChunk{
		BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type:     schemas.ResponsesStreamResponseTypeCreated,
			Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1"), Model: "claude-3-5-sonnet"},
		},
	})
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	w := &disconnectWriter{}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages",
		strings.NewReader(`{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.Messages(w, req)

	if w.writes == 0 {
		t.Fatalf("writes = 0, want >=1 (a message_start event must be attempted)")
	}
}

// TestStreamAnthropic_SkipsNilAndNonEventChunks covers the skip branches: a nil
// chunk, and a chunk whose Responses type converts to zero Anthropic events
// (ResponsesStreamResponseTypeInProgress → no message_* event), must both be
// skipped without emitting a frame; the trailing Created chunk yields one
// message_start.
func TestStreamAnthropic_SkipsNilAndNonEventChunks(t *testing.T) {
	chunks := newChunkChan(
		nil,
		&schemas.BifrostStreamChunk{}, // nil sub-response
		&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type: schemas.ResponsesStreamResponseTypeInProgress, // converts to zero events
		}},
		&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type:     schemas.ResponsesStreamResponseTypeCreated,
			Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")},
		}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := newTestHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[],"stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, "event: ") {
		t.Errorf("anthropic stream missing event frame; got:\n%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("anthropic stream must not emit [DONE]; got:\n%s", body)
	}
}
