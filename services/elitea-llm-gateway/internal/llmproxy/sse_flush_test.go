package llmproxy

// BF0.3a — SSE incremental-flush spike (design §2.3; the biggest technical
// unknown that gates cutover).
//
// The claim under test is NARROW but load-bearing: the gateway's net/http SSE
// loop must write and FLUSH each stream chunk to the client BEFORE the next
// chunk is produced — i.e. incrementally — rather than buffering the whole
// response and flushing once at end-of-request. A false positive here would
// pass CI while every real LLM stream arrived as a single blob after the
// provider finished, defeating streaming end-to-end.
//
// httptest.ResponseRecorder cannot prove this on its own: it records the final
// body regardless of when Flush was called, so "the body contains all chunks"
// is satisfied equally by an incremental writer and a buffer-then-dump writer.
//
// The proof is therefore a LOCK-STEP PRODUCER driving an instrumented
// http.Flusher:
//
//   - flushRecorder is an http.ResponseWriter + http.Flusher whose Flush()
//     snapshots the bytes written so far and signals a channel.
//   - The producer goroutine sends chunk i into an UNBUFFERED channel, then
//     blocks until it observes chunk i's flush before sending chunk i+1.
//   - If the handler buffered instead of flushing per chunk, the flush signal
//     for chunk 0 never arrives, the producer blocks forever, and the test
//     watchdog fails. A passing run therefore PROVES each chunk was flushed
//     before the next was produced ⇒ before end-of-response.
//
// It runs the real handler path (h.Chat / h.Messages → streamOpenAI /
// streamAnthropic → beginStream's http.Flusher assertion → pkg/ssewriter →
// Flush) for BOTH dialects, which are both text/event-stream and differ only
// in event names / terminator (§2.3).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// flushSnapshot captures the state of the response at the moment Flush() was
// called: how many bytes had been written and the flush ordinal.
type flushSnapshot struct {
	ordinal    int
	bytesSoFar int
}

// flushRecorder is an http.ResponseWriter that implements http.Flusher and
// records every Flush call: the byte count written so far and a signal on
// flushed so a lock-step producer can advance only after observing the flush.
// It is intentionally NOT an httptest.ResponseRecorder — the whole point is to
// observe flush timing, which the recorder discards.
type flushRecorder struct {
	mu        sync.Mutex
	hdr       http.Header
	body      strings.Builder
	status    int
	snapshots []flushSnapshot
	// flushed receives one value per Flush() so the producer can wait for a
	// specific chunk to have been flushed before producing the next.
	flushed chan flushSnapshot
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{
		hdr:     http.Header{},
		status:  http.StatusOK,
		flushed: make(chan flushSnapshot, 64),
	}
}

func (f *flushRecorder) Header() http.Header { return f.hdr }

func (f *flushRecorder) WriteHeader(status int) { f.status = status }

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.Write(p)
}

// Flush snapshots the bytes written so far and signals the producer. This is
// the observability seam the whole spike depends on.
func (f *flushRecorder) Flush() {
	f.mu.Lock()
	snap := flushSnapshot{ordinal: len(f.snapshots), bytesSoFar: f.body.Len()}
	f.snapshots = append(f.snapshots, snap)
	f.mu.Unlock()
	// Buffered channel large enough for the test's chunk count; the send never
	// blocks in practice, so a slow consumer cannot deadlock the handler.
	f.flushed <- snap
}

func (f *flushRecorder) bodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.String()
}

func (f *flushRecorder) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.snapshots)
}

// compile-time proof the recorder is a flushable ResponseWriter — the same
// interface beginStream asserts on.
var (
	_ http.ResponseWriter = (*flushRecorder)(nil)
	_ http.Flusher        = (*flushRecorder)(nil)
)

// lockStepRouter is an LLMRouter whose streaming methods return a channel fed
// by a producer that advances only after each chunk's flush is observed on the
// recorder. This turns "did it flush incrementally?" into "did the producer
// make progress?" — provable without wall-clock heuristics.
type lockStepRouter struct {
	fakeRouter
	chunks   []*schemas.BifrostStreamChunk
	rec      *flushRecorder
	t        *testing.T
	produced chan int // ordinal of each chunk actually handed to the handler
}

// run starts the producer goroutine and returns the stream channel the handler
// will range over. The channel is UNBUFFERED so a send blocks until the
// handler receives — combined with waiting on rec.flushed after each send,
// this enforces strict produce→flush→produce lock-step.
func (l *lockStepRouter) run() chan *schemas.BifrostStreamChunk {
	ch := make(chan *schemas.BifrostStreamChunk) // unbuffered: hand off one at a time
	l.produced = make(chan int, len(l.chunks))
	go func() {
		defer close(ch)
		for i, c := range l.chunks {
			// Wait until the previous chunk has been flushed before producing
			// the next. For i==0 there is nothing to wait for.
			if i > 0 {
				select {
				case <-l.rec.flushed:
				case <-time.After(2 * time.Second):
					l.t.Errorf("chunk %d was not flushed before producing chunk %d "+
						"(handler is buffering, not flushing incrementally)", i-1, i)
					return
				}
			}
			ch <- c
			l.produced <- i
		}
	}()
	return ch
}

func (l *lockStepRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	l.captureVK(ctx)
	return l.run(), nil
}

func (l *lockStepRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	l.captureVK(ctx)
	return l.run(), nil
}

// serveLockStep runs the given handler func against the recorder in a
// goroutine and returns when the handler completes (or a watchdog fires). It
// asserts the handler finished — a hang means the producer blocked because a
// flush never happened.
func serveLockStep(t *testing.T, h *Handler, l *lockStepRouter, serve func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"stream":true}`))
	done := make(chan struct{})
	go func() {
		serve(l.rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler did not complete within watchdog: SSE loop is buffering, "+
			"not flushing per chunk (flushes so far: %d/%d chunks)",
			l.rec.flushCount(), len(l.chunks))
	}
}

// assertIncremental verifies the flush trace proves incremental delivery: the
// number of flushes matches the expected frame count, and the body grew
// monotonically across flushes (each flush carried NEW bytes, so no flush was
// a redundant end-of-response dump).
func assertIncremental(t *testing.T, rec *flushRecorder, wantFlushes int) {
	t.Helper()
	// Capture snapshots AND current body length under the same lock so the
	// body-length read is not a data race with concurrent Write() calls.
	rec.mu.Lock()
	snaps := append([]flushSnapshot(nil), rec.snapshots...)
	finalBodyLen := rec.body.Len()
	rec.mu.Unlock()

	if len(snaps) != wantFlushes {
		t.Fatalf("flush count = %d, want %d (each SSE frame must flush exactly once)", len(snaps), wantFlushes)
	}
	prev := 0
	for _, s := range snaps {
		if s.bytesSoFar <= prev {
			t.Errorf("flush %d wrote no new bytes (bytesSoFar=%d, prev=%d): "+
				"a flush that adds nothing means the writer buffered", s.ordinal, s.bytesSoFar, prev)
		}
		prev = s.bytesSoFar
	}
	// The first flush must occur strictly before the full body is present —
	// i.e. the first snapshot's byte count is less than the final body length
	// whenever there is more than one frame. This is the crux of "written
	// before end-of-response".
	if wantFlushes > 1 && snaps[0].bytesSoFar >= finalBodyLen {
		t.Errorf("first flush already held the entire body (%d >= %d): not incremental",
			snaps[0].bytesSoFar, finalBodyLen)
	}
}

// TestSSEIncrementalFlush is the BF0.3a spike. It proves per-chunk incremental
// flush for BOTH dialects through the real gateway SSE loop.
func TestSSEIncrementalFlush(t *testing.T) {
	t.Run("OpenAI", func(t *testing.T) {
		chunks := []*schemas.BifrostStreamChunk{
			{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c0", Object: "chat.completion.chunk"}},
			{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
			{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c2", Object: "chat.completion.chunk"}},
		}
		rec := newFlushRecorder()
		l := &lockStepRouter{chunks: chunks, rec: rec, t: t}
		h := newTestHandler(l, nil, nil)

		serveLockStep(t, h, l, h.Chat)

		body := rec.bodyString()
		for _, want := range []string{`"id":"c0"`, `"id":"c1"`, `"id":"c2"`, "data: [DONE]"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q; got:\n%s", want, body)
			}
		}
		// 3 data chunks + terminal [DONE] frame = 4 flushes.
		assertIncremental(t, rec, len(chunks)+1)
	})

	t.Run("Anthropic", func(t *testing.T) {
		// Each response.created chunk converts to exactly one message_start
		// Anthropic event (one flush). Anthropic streams have no [DONE]
		// terminator — completion is the stream close (§2.3).
		mk := func(id string) *schemas.BifrostStreamChunk {
			return &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
				Type:     schemas.ResponsesStreamResponseTypeCreated,
				Response: &schemas.BifrostResponsesResponse{ID: strPtr(id), Model: "claude-3-5-sonnet"},
			}}
		}
		chunks := []*schemas.BifrostStreamChunk{mk("r0"), mk("r1"), mk("r2")}
		rec := newFlushRecorder()
		l := &lockStepRouter{chunks: chunks, rec: rec, t: t}
		h := newTestHandler(l, nil, nil)

		serveLockStep(t, h, l, h.Messages)

		body := rec.bodyString()
		if got := strings.Count(body, "event: message_start"); got != len(chunks) {
			t.Errorf("message_start event count = %d, want %d; got:\n%s", got, len(chunks), body)
		}
		if strings.Contains(body, "[DONE]") {
			t.Errorf("anthropic stream must not emit [DONE]; got:\n%s", body)
		}
		// One event per chunk, no terminator = len(chunks) flushes.
		assertIncremental(t, rec, len(chunks))
	})
}
