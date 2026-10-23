package ssewriter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/ssewriter"
)

// wrappedWriter reproduces the shape every response on the router carries:
// apimw.OtelMiddleware's statusRecorder embeds the http.ResponseWriter
// INTERFACE and exposes Unwrap. Only ResponseWriter's three methods are
// promoted, so `w.(http.Flusher)` is false even though the writer underneath
// flushes perfectly well — the exact reason /api/v2/events/prompt_lib/{id}
// answered 500 "streaming not supported" the first time it was mounted (#152).
type wrappedWriter struct {
	http.ResponseWriter
}

func (w wrappedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// selfWrappingWriter unwraps to itself — a middleware bug that must terminate
// the walk rather than spin.
type selfWrappingWriter struct {
	http.ResponseWriter
}

func (w *selfWrappingWriter) Unwrap() http.ResponseWriter { return w }

// unflushableWriter neither flushes nor unwraps.
type unflushableWriter struct {
	header http.Header
}

func (w *unflushableWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *unflushableWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *unflushableWriter) WriteHeader(int)             {}

func TestNewAcceptsAWriterThatOnlyFlushesThroughUnwrap(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	sse, err := ssewriter.New(wrappedWriter{ResponseWriter: recorder})
	if err != nil {
		t.Fatalf("ssewriter.New() error = %v, want nil for a writer that unwraps to a flusher", err)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if err := sse.Event("application.created", `{"probe":true}`); err != nil {
		t.Fatalf("Event() error = %v", err)
	}
	if err := sse.Comment("heartbeat"); err != nil {
		t.Fatalf("Comment() error = %v", err)
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: application.created\n", "data: {\"probe\":true}\n\n", ": heartbeat\n\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, want it to contain %q", body, want)
		}
	}
}

func TestNewRejectsWritersThatCannotFlush(t *testing.T) {
	t.Parallel()

	for name, writer := range map[string]http.ResponseWriter{
		"no flush, no unwrap": &unflushableWriter{},
		"unwraps to itself":   &selfWrappingWriter{ResponseWriter: &unflushableWriter{}},
		"nil":                 nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ssewriter.New(writer); err == nil {
				t.Fatal("ssewriter.New() error = nil, want it to refuse an unflushable writer " +
					"instead of accepting one and stalling the client on a silent stream")
			}
		})
	}
}
