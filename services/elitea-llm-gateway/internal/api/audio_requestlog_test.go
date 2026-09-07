// audio_requestlog_test.go — issue #323, the log half of audio parity.
//
// The per-request log is applied as middleware on the router root, so every
// route is covered "without being named". That is a structural argument, and
// this file turns it into a measured one for the audio routes: the whole point
// of #323 is that a voice deployment must be as observable and as accountable
// as a chat one, and an audio request that leaves no row is a request an
// operator cannot find.
package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// auditSink keeps every record the recorder writes.
type auditSink struct {
	mu      sync.Mutex
	records []requestlog.Record
}

func (s *auditSink) WriteBatch(_ context.Context, records []requestlog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
	return nil
}

func (s *auditSink) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *auditSink) routes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r.Route)
	}
	return out
}

// TestAudioRequestsReachTheRequestLog posts to the audio surface and reads the
// rows back off the sink.
//
// It uses the ROUTE PATTERN the recorder stores, not the raw URL, because that
// is what the admin Logs view groups on. A route that was mounted outside the
// logged subtree would produce an empty pattern and be invisible there while
// still answering 200 to the caller.
func TestAudioRequestsReachTheRequestLog(t *testing.T) {
	sink := &auditSink{}
	recorder := requestlog.New(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if recorder == nil {
		t.Fatal("the recorder is nil, so this test would assert nothing")
	}
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil)
	r := NewRouterWithLog(h, recorder)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi","voice":"alloy"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Stop drains what is buffered, so the assertion does not race the flush
	// ticker.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(ctx)

	var found bool
	for _, route := range sink.routes() {
		if route == "/llm/v1/audio/speech" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no request-log row for the audio route; the recorder saw %v. "+
			"An audio request that leaves no row cannot be found in the admin Logs view, "+
			"and audio is the surface issue #323 added", sink.routes())
	}
}
