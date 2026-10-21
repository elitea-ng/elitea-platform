package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chunkedReader delivers a sequence of byte chunks, advancing a virtual clock by
// a per-chunk gap on each Read. It models a streaming HTTP body where each chunk
// is flushed at a distinct time — exactly what an incremental SSE stream looks
// like on the wire — without needing a real network or real sleeps.
type chunkedReader struct {
	chunks  []string
	gap     time.Duration
	elapsed *time.Duration
	i       int
	rest    string
}

func newChunkedReader(chunks []string, gap time.Duration, elapsed *time.Duration) *chunkedReader {
	return &chunkedReader{chunks: chunks, gap: gap, elapsed: elapsed}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.rest == "" {
		if c.i >= len(c.chunks) {
			return 0, io.EOF
		}
		// Advance the virtual clock as this chunk "arrives".
		*c.elapsed += c.gap
		c.rest = c.chunks[c.i]
		c.i++
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func TestSSEFlush_SplitFrames(t *testing.T) {
	// Two complete frames, one trailing unterminated frame at EOF.
	data := "data: a\n\ndata: b\n\ndata: c"

	adv, tok, err := splitSSEFrames([]byte(data), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(tok) != "data: a" || adv != len("data: a\n\n") {
		t.Fatalf("first frame = %q (adv %d), want %q", tok, adv, "data: a")
	}

	// Incomplete final segment, not at EOF: request more data.
	adv, tok, err = splitSSEFrames([]byte("data: c"), false)
	if err != nil || adv != 0 || tok != nil {
		t.Fatalf("incomplete-not-eof = (adv %d, tok %q, err %v), want (0, nil, nil)", adv, tok, err)
	}

	// Incomplete final segment at EOF: emit the remaining bytes.
	adv, tok, err = splitSSEFrames([]byte("data: c"), true)
	if err != nil || string(tok) != "data: c" || adv != len("data: c") {
		t.Fatalf("incomplete-at-eof = (adv %d, tok %q, err %v)", adv, tok, err)
	}

	// Empty at EOF: nothing to emit.
	adv, tok, err = splitSSEFrames(nil, true)
	if err != nil || adv != 0 || tok != nil {
		t.Fatalf("empty-at-eof = (adv %d, tok %q, err %v), want (0, nil, nil)", adv, tok, err)
	}
}

func TestSSEFlush_CollectArrivalsIncremental(t *testing.T) {
	var elapsed time.Duration
	// OpenAI-dialect frames delivered one per chunk, 10ms apart.
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	r := newChunkedReader(chunks, 10*time.Millisecond, &elapsed)

	frames, err := collectArrivals(r, func() time.Duration { return elapsed })
	if err != nil {
		t.Fatalf("collectArrivals: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if frames[0].Elapsed >= frames[1].Elapsed || frames[1].Elapsed >= frames[2].Elapsed {
		t.Fatalf("frame arrivals not strictly increasing: %v %v %v",
			frames[0].Elapsed, frames[1].Elapsed, frames[2].Elapsed)
	}
	if !strings.Contains(frames[2].Raw, "[DONE]") {
		t.Fatalf("last frame = %q, want [DONE] terminator", frames[2].Raw)
	}
}

func TestSSEFlush_CollectArrivalsAnthropic(t *testing.T) {
	var elapsed time.Duration
	// Anthropic-dialect: named events, terminated by message_stop.
	chunks := []string{
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}
	r := newChunkedReader(chunks, 8*time.Millisecond, &elapsed)

	frames, err := collectArrivals(r, func() time.Duration { return elapsed })
	if err != nil {
		t.Fatalf("collectArrivals: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if !strings.HasPrefix(frames[0].Raw, "event: content_block_delta") {
		t.Fatalf("first frame = %q", frames[0].Raw)
	}
	if !strings.HasPrefix(frames[1].Raw, "event: message_stop") {
		t.Fatalf("last frame = %q, want message_stop", frames[1].Raw)
	}
}

func TestSSEFlush_CollectArrivalsBuffered(t *testing.T) {
	var elapsed time.Duration
	// A single buffered read: all frames arrive in one chunk at the same instant.
	blob := "data: a\n\ndata: b\n\ndata: [DONE]\n\n"
	r := newChunkedReader([]string{blob}, 10*time.Millisecond, &elapsed)

	frames, err := collectArrivals(r, func() time.Duration { return elapsed })
	if err != nil {
		t.Fatalf("collectArrivals: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	// All three frames came from the same Read, so identical Elapsed.
	if frames[0].Elapsed != frames[1].Elapsed || frames[1].Elapsed != frames[2].Elapsed {
		t.Fatalf("buffered frames should share arrival time: %v %v %v",
			frames[0].Elapsed, frames[1].Elapsed, frames[2].Elapsed)
	}
}

func TestSSEFlush_ClassifyStream(t *testing.T) {
	minGap := 5 * time.Millisecond

	tests := []struct {
		name       string
		frames     []sseFrame
		wantOK     bool
		wantFrames int
	}{
		{
			name: "incremental spread across time",
			frames: []sseFrame{
				{Raw: "data: a", Elapsed: 0},
				{Raw: "data: b", Elapsed: 10 * time.Millisecond},
				{Raw: "data: [DONE]", Elapsed: 20 * time.Millisecond},
			},
			wantOK:     true,
			wantFrames: 3,
		},
		{
			name: "buffered blob all same instant",
			frames: []sseFrame{
				{Raw: "data: a", Elapsed: 3 * time.Millisecond},
				{Raw: "data: b", Elapsed: 3 * time.Millisecond},
				{Raw: "data: [DONE]", Elapsed: 3 * time.Millisecond},
			},
			wantOK:     false,
			wantFrames: 3,
		},
		{
			name: "gap just below threshold",
			frames: []sseFrame{
				{Raw: "data: a", Elapsed: 0},
				{Raw: "data: b", Elapsed: 4 * time.Millisecond},
			},
			wantOK:     false,
			wantFrames: 2,
		},
		{
			name: "gap exactly at threshold is incremental",
			frames: []sseFrame{
				{Raw: "data: a", Elapsed: 0},
				{Raw: "data: b", Elapsed: 5 * time.Millisecond},
			},
			wantOK:     true,
			wantFrames: 2,
		},
		{
			name:       "single frame cannot prove incremental",
			frames:     []sseFrame{{Raw: "data: [DONE]", Elapsed: 12 * time.Millisecond}},
			wantOK:     false,
			wantFrames: 1,
		},
		{
			name:       "no frames",
			frames:     nil,
			wantOK:     false,
			wantFrames: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyStream(tc.frames, minGap)
			if v.Incremental != tc.wantOK {
				t.Fatalf("Incremental = %v, want %v (reason: %s)", v.Incremental, tc.wantOK, v.Reason)
			}
			if v.FrameCount != tc.wantFrames {
				t.Fatalf("FrameCount = %d, want %d", v.FrameCount, tc.wantFrames)
			}
			if v.Reason == "" {
				t.Fatalf("Reason should always be set")
			}
		})
	}
}

// flushingStreamHandler writes n data frames, flushing between each with a small
// real delay, so probeStream observes genuine inter-frame gaps.
func flushingStreamHandler(dialect string, n int, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < n; i++ {
			if dialect == "anthropic" {
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"text\":\"x\"}\n\n")
			} else {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			}
			fl.Flush()
			time.Sleep(delay)
		}
		if dialect == "anthropic" {
			_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		} else {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		fl.Flush()
	}
}

func TestSSEFlush_ProbeLiveIncremental(t *testing.T) {
	srv := httptest.NewServer(flushingStreamHandler("openai", 3, 10*time.Millisecond))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	v, err := probeStream(client, srv.URL, openAIProbe, 3*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("probeStream: %v", err)
	}
	if !v.Incremental {
		t.Fatalf("expected incremental, got buffered: %s", v.Reason)
	}
}

func TestSSEFlush_ProbeNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := probeStream(client, srv.URL, openAIProbe, time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("expected status error, got %v", err)
	}
}

func TestSSEFlush_ProbeWrongContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := probeStream(client, srv.URL, anthropicProbe, time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("expected content-type error, got %v", err)
	}
}

func TestSSEFlush_ProbeRequestError(t *testing.T) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	// Unroutable address → connection error.
	_, err := probeStream(client, "http://127.0.0.1:0", openAIProbe, time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected request failure, got %v", err)
	}
}

// The probe reaches /llm as an external caller: the token as a bearer and the
// billed project as the edge's selector header, on the dialect's /llm path.
// No X-Elitea-* identity is sent; the edge would strip it.
func TestSSEFlush_ProbeAuthenticatesAsAnEdgeCaller(t *testing.T) {
	var seen *http.Request
	inner := flushingStreamHandler("openai", 2, time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		inner(w, r)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	auth := func(req *http.Request) { edgeAuth(req, "pat-123", "4321") }
	if _, err := probeStream(client, srv.URL+"/", openAIProbe, time.Millisecond, auth); err != nil {
		t.Fatalf("probeStream: %v", err)
	}
	if seen == nil {
		t.Fatal("the probe never reached the server")
	}
	if seen.URL.Path != "/llm/v1/chat/completions" {
		t.Errorf("path = %q, want /llm/v1/chat/completions", seen.URL.Path)
	}
	if h := seen.Header.Get("Authorization"); h != "Bearer pat-123" {
		t.Errorf("Authorization = %q, want Bearer pat-123", h)
	}
	if h := seen.Header.Get("X-Project-Id"); h != "4321" {
		t.Errorf("X-Project-Id = %q, want 4321", h)
	}
	for name := range seen.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-elitea-") {
			t.Errorf("the probe sent identity header %s; only the edge may", name)
		}
	}

	// No project: the token owner's personal project, so no selector at all.
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	edgeAuth(req, "pat-123", "")
	if _, ok := req.Header["X-Project-Id"]; ok {
		t.Error("an empty project id still sent a selector header")
	}
}
