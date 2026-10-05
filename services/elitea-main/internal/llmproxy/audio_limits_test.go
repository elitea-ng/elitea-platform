package llmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// audioRequest builds an edge-authenticated request for path, as the /llm
// middleware chain leaves it: a resolved project on the context.
func audioRequest(path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, body)
	ctx := middleware.ContextWithProject(req.Context(), middleware.ProjectContext{ProjectID: 7})
	return req.WithContext(ctx)
}

func errorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, raw)
	}
	return body.Error.Code
}

// chunkedReader hides its length, so the request carries no Content-Length
// and only the streaming ceiling can stop it.
type chunkedReader struct{ r io.Reader }

func (c chunkedReader) Read(p []byte) (int, error) { return c.r.Read(p) }

// withAudioLimit swaps one route's bound for the test's duration.
func withAudioLimit(t *testing.T, path string, limit audioRouteLimit) {
	t.Helper()
	prev, had := audioRouteLimits[path]
	audioRouteLimits[path] = limit
	t.Cleanup(func() {
		if had {
			audioRouteLimits[path] = prev
		} else {
			delete(audioRouteLimits, path)
		}
	})
}

// TestAudio_SpeechForwardsTheAudioBytesUnchanged is the browser read-aloud
// contract: the edge passes the provider's raw audio body through, with its
// Content-Type, and carries the edge-resolved project to the gateway.
func TestAudio_SpeechForwardsTheAudioBytesUnchanged(t *testing.T) {
	audio := []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0xff, 0x10}
	var gotProject, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProject = r.Header.Get(HeaderProjectID)
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer backend.Close()

	p := proxyTo(t, backend.URL, "sekret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, audioRequest("/llm/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"hi"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), audio) {
		t.Fatalf("body = %v, want the gateway's audio bytes %v", rec.Body.Bytes(), audio)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q, want audio/mpeg", ct)
	}
	if gotProject != "7" {
		t.Fatalf("gateway saw project %q, want 7", gotProject)
	}
	if gotBody != `{"model":"tts","input":"hi"}` {
		t.Fatalf("gateway saw body %q", gotBody)
	}
}

// TestAudio_OversizeDeclaredBodyIsRefusedBeforeTheGateway: a Content-Length
// past the ceiling never crosses the mTLS hop.
func TestAudio_OversizeDeclaredBodyIsRefusedBeforeTheGateway(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer backend.Close()

	for path, limit := range map[string]int64{
		"/llm/v1/audio/speech":         maxSpeechBody,
		"/llm/v1/audio/transcriptions": maxTranscriptionBody,
		"/llm/v1/audio/translations":   maxTranscriptionBody,
	} {
		t.Run(path, func(t *testing.T) {
			p := proxyTo(t, backend.URL, "")
			req := audioRequest(path, strings.NewReader("x"))
			req.ContentLength = limit + 1
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			if code := errorCode(t, rec.Body.Bytes()); code != "request_too_large" {
				t.Fatalf("code = %q, want request_too_large", code)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("gateway was called %d times for refused bodies, want 0", n)
	}
}

// TestAudio_OversizeChunkedBodyIsCutOffAtTheCeiling: a body with no declared
// length is stopped by the streaming ceiling with the same refusal.
func TestAudio_OversizeChunkedBodyIsCutOffAtTheCeiling(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	withAudioLimit(t, "/llm/v1/audio/speech", audioRouteLimit{maxBody: 1024, timeout: time.Minute})

	p := proxyTo(t, backend.URL, "")
	req := audioRequest("/llm/v1/audio/speech", chunkedReader{bytes.NewReader(bytes.Repeat([]byte("a"), 4096))})
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "request_too_large" {
		t.Fatalf("code = %q, want request_too_large", code)
	}
}

// TestAudio_BodyAtTheCeilingIsForwarded: the ceiling is inclusive, so a body
// of exactly the limit is not refused.
func TestAudio_BodyAtTheCeilingIsForwarded(t *testing.T) {
	var got int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = len(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	withAudioLimit(t, "/llm/v1/audio/transcriptions", audioRouteLimit{maxBody: 1024, timeout: time.Minute})

	p := proxyTo(t, backend.URL, "")
	req := audioRequest("/llm/v1/audio/transcriptions", chunkedReader{bytes.NewReader(bytes.Repeat([]byte("a"), 1024))})
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got != 1024 {
		t.Fatalf("gateway read %d bytes, want 1024", got)
	}
}

// TestAudio_AProviderThatNeverAnswersTimesOutWith504: the deadline turns a
// hung upstream into a named refusal instead of an open connection.
func TestAudio_AProviderThatNeverAnswersTimesOutWith504(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)
	withAudioLimit(t, "/llm/v1/audio/speech", audioRouteLimit{maxBody: maxSpeechBody, timeout: 50 * time.Millisecond})

	p := proxyTo(t, backend.URL, "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, audioRequest("/llm/v1/audio/speech", strings.NewReader(`{"model":"tts","input":"hi"}`)))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "upstream_timeout" {
		t.Fatalf("code = %q, want upstream_timeout", code)
	}
}

// TestAudio_OtherRoutesKeepTheStreamingContract: the chat routes carry no
// ceiling and no deadline from this file. A long SSE stream must not be cut.
func TestAudio_OtherRoutesKeepTheStreamingContract(t *testing.T) {
	var got int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Errorf("chat request carries a deadline; the audio bound leaked onto it")
		}
		raw, _ := io.ReadAll(r.Body)
		got = len(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p := proxyTo(t, backend.URL, "")
	body := bytes.Repeat([]byte("a"), int(maxSpeechBody)+1)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, audioRequest("/llm/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got != len(body) {
		t.Fatalf("gateway read %d bytes, want %d", got, len(body))
	}
}

// TestAudio_ADeadGatewayIsStillA502: the audio refusals do not swallow the
// ordinary "gateway unavailable" answer.
func TestAudio_ADeadGatewayIsStillA502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := backend.URL
	backend.Close()

	p := proxyTo(t, url, "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, audioRequest("/llm/v1/audio/speech", strings.NewReader(`{}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "upstream_unavailable" {
		t.Fatalf("code = %q, want upstream_unavailable", code)
	}
}
