package llmproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// A streamed completion must log the tokens it used.
//
// The defect this file pins: the settle path bills from billingContext(), which
// is context.Background() by design, so requestlog.FromContext() found nothing
// there and the row said 0 prompt and 0 completion tokens. The provider, the
// model and the call count were all correct on the same row, so an analytics
// TOKENS tile read 0 beside a correct CALLS tile and a correct per-model table.
//
// The tests below drive the REAL middleware over the REAL handler, because the
// defect lives in the join between them. A test that called SetTokens directly
// passes against the broken code.

// logCaptureSink keeps every batch the recorder writes.
type logCaptureSink struct {
	mu      sync.Mutex
	records []requestlog.Record
}

func (s *logCaptureSink) WriteBatch(_ context.Context, records []requestlog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
	return nil
}

func (s *logCaptureSink) Prune(_ context.Context, _ time.Time) (int64, error) { return 0, nil }

func (s *logCaptureSink) all() []requestlog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]requestlog.Record, len(s.records))
	copy(out, s.records)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// loggedRequest serves one request through the request-log middleware and
// returns the row the log received.
func loggedRequest(t *testing.T, h *Handler, path, body string) requestlog.Record {
	t.Helper()
	sink := &logCaptureSink{}
	recorder := requestlog.New(sink, discardLogger())
	if recorder == nil {
		t.Fatal("requestlog.New returned nil for a non-nil sink")
	}
	handler := requestlog.Middleware(recorder)(h.route())

	postJSON(t, handler, path, body)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(ctx)

	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("logged rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func usageChunk(prompt, completion int) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID:     "trailer",
		Object: "chat.completion.chunk",
		Usage: &schemas.BifrostLLMUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
		},
	}}
}

func TestStreamedChatLogsItsUsage(t *testing.T) {
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
		usageChunk(2351, 42),
	)
	h := NewHandler(&fakeRouter{streamChan: chunks}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[],"stream":true}`)

	if row.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", row.Status)
	}
	if row.PromptToks != 2351 || row.OutputToks != 42 {
		t.Errorf("logged tokens = (%d, %d), want (2351, 42)", row.PromptToks, row.OutputToks)
	}
}

// The streaming flag must say the request streamed.
//
// SetStreaming had no call site at all, so every row read `streaming = false`.
// The health view groups by that column precisely because a streamed duration
// is the whole stream and a buffered duration is one round trip; one mean over
// both describes neither.
func TestStreamedChatLogsTheStreamingFlag(t *testing.T) {
	chunks := newChunkChan(usageChunk(10, 5))
	h := NewHandler(&fakeRouter{streamChan: chunks}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[],"stream":true}`)

	if !row.Streaming {
		t.Error("streaming = false for a streamed request")
	}
}

// A buffered request keeps the flag false and still logs its usage. Without
// this the test above passes on a handler that hardcodes `true`.
func TestBufferedChatLogsUsageAndIsNotMarkedStreaming(t *testing.T) {
	h := NewHandler(&fakeRouter{chatResp: &schemas.BifrostChatResponse{
		ID:    "cmpl-1",
		Model: "gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if row.Streaming {
		t.Error("streaming = true for a buffered request")
	}
	if row.PromptToks != 11 || row.OutputToks != 7 {
		t.Errorf("logged tokens = (%d, %d), want (11, 7)", row.PromptToks, row.OutputToks)
	}
}

// A write after the row is emitted must not change it and must not race.
//
// The detached drain settles a cut stream after the handler returned. It calls
// updateUsage, which reaches the same recording chokepoint. The seal makes that
// late write a no-op instead of a data race; `-race` is what proves it.
func TestLateEnrichmentWriteIsDroppedNotRaced(t *testing.T) {
	sink := &logCaptureSink{}
	recorder := requestlog.New(sink, discardLogger())
	var late *requestlog.Enrichment
	captured := make(chan struct{})

	handler := requestlog.Middleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		late = requestlog.FromContext(r.Context())
		late.SetTokens(3, 4)
		w.WriteHeader(http.StatusOK)
		close(captured)
	}))

	postJSON(t, handler, "/llm/v1/chat/completions", `{}`)
	<-captured
	// The handler has returned, so the row is already queued.
	late.SetTokens(999, 999)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(ctx)

	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("logged rows = %d, want 1", len(rows))
	}
	if rows[0].PromptToks != 3 || rows[0].OutputToks != 4 {
		t.Errorf("logged tokens = (%d, %d), want (3, 4) — the late write must be dropped",
			rows[0].PromptToks, rows[0].OutputToks)
	}
}
