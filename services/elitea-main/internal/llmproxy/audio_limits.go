package llmproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// audio_limits.go — the edge bounds for the /llm/v1/audio routes.
//
// WHY THE EDGE BOUNDS THESE ROUTES. The browser voice client (dictation,
// read-aloud and speaking mode in elitea-web) calls these routes directly with
// the session cookie. Before, voice used a socket.io server that elitea-main
// does not run (removed in #126). The /llm proxy is a byte-streaming proxy
// built for SSE: it clears the write deadline and sets no request deadline,
// because a chat stream may legitimately run for many minutes. An audio
// request is not a stream. It is one upload and one answer, so it gets:
//
//   - A BODY CEILING, applied before the first byte travels to the gateway.
//     The gateway applies its own ceiling too (llmproxy/audio.go,
//     maxTranscriptionUpload), but only after the body crossed the mTLS hop.
//     A Content-Length above the ceiling is refused at once. A chunked body
//     is cut off at the ceiling and refused with the same 413.
//   - A DEADLINE. A provider that never answers no longer holds an edge
//     goroutine and a gateway connection for ever. The caller gets a 504 with
//     a named code, not a hang and not a generic 502.
//
// Authentication, the project selector, project membership, the model
// resolution, the budget gate and the authored rate limits all stay where they
// are: the /llm middleware chain (internal/api/router.go, mountLLM) and the
// gateway's admission sequence. This file adds no second policy.

// audioRouteLimit is the bound for one audio route.
type audioRouteLimit struct {
	maxBody int64
	timeout time.Duration
}

const (
	// maxSpeechBody bounds a text-to-speech request. The body is JSON with
	// the text to read: OpenAI's own limit for `input` is 4096 characters,
	// and a UTF-8 character is at most 4 bytes. 64 KiB holds that text plus
	// the voice, speed and instructions fields with a wide margin.
	maxSpeechBody int64 = 64 << 10
	// maxTranscriptionBody bounds a speech-to-text upload. The gateway
	// accepts a 25 MiB audio file (OpenAI's documented limit); 1 MiB on top
	// covers the multipart framing and the text fields, so the edge never
	// refuses a body the gateway would accept.
	maxTranscriptionBody int64 = 26 << 20

	// speechTimeout bounds a text-to-speech request. One request reads at most
	// 4096 characters, which providers synthesise in well under a minute.
	speechTimeout = 2 * time.Minute
	// transcriptionTimeout bounds a speech-to-text request. A 25 MiB upload
	// is about thirteen minutes of 16 kHz audio, and a provider transcribes
	// that in a fraction of its length.
	transcriptionTimeout = 5 * time.Minute
)

// audioRouteLimits maps the exact gateway audio paths to their bounds. The
// realtime WebSocket route is NOT here: it is a long-lived session, and the
// gateway bounds it with its own keepalive and budget re-check
// (elitea-llm-gateway internal/llmproxy/realtime.go).
var audioRouteLimits = map[string]audioRouteLimit{
	"/llm/v1/audio/speech":         {maxBody: maxSpeechBody, timeout: speechTimeout},
	"/llm/v1/audio/transcriptions": {maxBody: maxTranscriptionBody, timeout: transcriptionTimeout},
	"/llm/v1/audio/translations":   {maxBody: maxTranscriptionBody, timeout: transcriptionTimeout},
}

// audioRefusal is the request-scoped record of why the edge stopped an audio
// request. The reverse proxy reports a body that hit the ceiling and a
// deadline that expired as plain transport errors, and the two are not
// reliably distinguishable from a dead gateway by the error value alone. The
// limiter therefore records the reason where it happens, and the proxy's
// ErrorHandler reads it back.
type audioRefusal struct {
	bodyTooLarge atomic.Bool
}

type audioRefusalKey struct{}

func audioRefusalFrom(ctx context.Context) *audioRefusal {
	refusal, _ := ctx.Value(audioRefusalKey{}).(*audioRefusal)
	return refusal
}

// countingBody fails the read that crosses the ceiling and records it.
type countingBody struct {
	body    io.ReadCloser
	left    int64
	refusal *audioRefusal
}

var errAudioBodyTooLarge = errors.New("llmproxy: audio request body exceeds the edge ceiling")

func (b *countingBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One more byte would cross the ceiling. Read one to tell "exactly at
		// the ceiling" from "past it".
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			b.refusal.bodyTooLarge.Store(true)
			return 0, errAudioBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *countingBody) Close() error { return b.body.Close() }

// limitAudioRequest applies the audio bounds to r. It returns the request to
// forward and a release function, or ok=false when it already wrote the
// refusal. A request on any other path is returned unchanged.
func limitAudioRequest(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	limit, isAudio := audioRouteLimits[r.URL.Path]
	if !isAudio {
		return r, func() {}, true
	}
	if r.ContentLength > limit.maxBody {
		writeAudioBodyTooLarge(w)
		return nil, func() {}, false
	}
	refusal := &audioRefusal{}
	ctx, cancel := context.WithTimeout(r.Context(), limit.timeout)
	ctx = context.WithValue(ctx, audioRefusalKey{}, refusal)
	limited := r.WithContext(ctx)
	if r.Body != nil && r.Body != http.NoBody {
		limited.Body = &countingBody{body: r.Body, left: limit.maxBody, refusal: refusal}
	}
	return limited, cancel, true
}

// writeAudioRefusal writes the refusal for an audio request the edge stopped,
// and reports whether it did. It is called from the proxy's ErrorHandler.
func writeAudioRefusal(w http.ResponseWriter, r *http.Request, err error) bool {
	refusal := audioRefusalFrom(r.Context())
	if refusal == nil {
		return false
	}
	if refusal.bodyTooLarge.Load() || errors.Is(err, errAudioBodyTooLarge) {
		writeAudioBodyTooLarge(w)
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"the audio request did not complete in time","type":"api_error","code":"upstream_timeout"}}`))
		return true
	}
	return false
}

func writeAudioBodyTooLarge(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = w.Write([]byte(`{"error":{"message":"the audio request body is too large","type":"invalid_request_error","code":"request_too_large"}}`))
}
