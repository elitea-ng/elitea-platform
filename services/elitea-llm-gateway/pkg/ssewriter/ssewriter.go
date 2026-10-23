// Package ssewriter provides a helper for writing Server-Sent Events (SSE)
// over net/http with per-chunk flushing.
//
// It is a gateway-local copy of elitea-main's pkg/ssewriter (they live in
// separate Go modules): the /llm SSE loop (design §6.3) MUST reuse a helper
// that sets X-Accel-Buffering: no and Connection: keep-alive, otherwise
// a buffering proxy holds the stream and the incremental-flush spike (BF0.3a) is a
// false positive. New wires an http.ResponseController so the streaming loop
// can clear the per-connection write deadline (§9.5) without hard-killing an
// active SSE response.
package ssewriter

import (
	"fmt"
	"net/http"
	"time"
)

// Writer wraps an http.ResponseWriter and provides SSE-specific write methods.
// Every write flushes immediately so chunks reach the client (and the reverse
// proxy in front of it) as they are produced, not at end-of-response.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// New returns a Writer for the given ResponseWriter. It sets the required SSE
// headers and clears the per-connection write deadline (§9.5) so a long LLM
// stream is not truncated by the server's WriteTimeout profile. It returns an
// error if the writer does not support flushing (a hard prerequisite for
// incremental SSE).
func New(w http.ResponseWriter) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("ssewriter: streaming not supported")
	}

	// §6.3/§9.5: clear the per-connection write deadline before the first
	// flush so an in-flight SSE response is not hard-killed mid-stream. A
	// missing controller (or an unsupported deadline) is non-fatal — the
	// server profile already disables WriteTimeout — so the error is ignored.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	return &Writer{w: w, flusher: flusher}, nil
}

// Event writes a named SSE event with a data payload and flushes immediately.
// An empty event name writes a data-only frame (the OpenAI dialect shape).
func (s *Writer) Event(event, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Data writes a data-only SSE frame ("data: <data>\n\n") and flushes. This is
// the OpenAI dialect framing.
func (s *Writer) Data(data string) error {
	return s.Event("", data)
}

// Raw writes a pre-formatted SSE frame verbatim (including its own event:/data:
// lines and trailing blank line) and flushes. It is used when a converter
// already returns a complete frame string (the Anthropic multi-event and error
// paths).
func (s *Writer) Raw(frame string) error {
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
