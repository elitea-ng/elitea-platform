package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// sse-flush-check (spec §2.3 / §7.3, gate BFF.1 / validator BFF.9a):
//
// The gateway MUST stream SSE responses incrementally — each chunk flushed as
// it is produced, not accumulated and delivered as one buffered blob at
// end-of-response. This must hold end-to-end, as a caller of the documented
// /llm endpoint sees it, for BOTH dialects:
//
//   - OpenAI    POST /llm/v1/chat/completions  — data-only frames, "data: [DONE]" terminator
//   - Anthropic POST /llm/v1/messages          — named-event frames, "event: message_stop" terminator
//
// Both are Content-Type text/event-stream; they differ only in event names and
// terminator, not transport.
//
// WHERE IT PROBES, AND WHY THERE. The probe calls elitea-main's public /llm
// with a personal access token, exactly as an external caller does, so the
// stream it times has crossed every hop that could buffer it: the ingress,
// elitea-main's /llm edge (which must flush each body read), the
// GatewayService gRPC stream (which must carry each frame as its own message)
// and the gateway's own SSE writer. Calling GatewayService directly would skip
// the edge and the ingress, which is exactly where a missing flush or a
// missing X-Accel-Buffering: no would hide. Any buffering hop collapses the
// response into a single read, which this check detects and fails.
//
// Detection is timing-based and split into pure, unit-testable pieces:
//   - splitSSEFrames  — a bufio.SplitFunc that yields one frame per "\n\n"
//   - collectArrivals — stamps each frame's arrival time as the body is read
//   - classifyStream  — decides incremental vs buffered from the arrivals
//
// classifyStream needs no live gateway: it is exercised against synthetic
// chunked readers in sseflush_test.go.

// sseFrame is one SSE frame observed on the wire, with the elapsed time from the
// start of the read at which it became available.
type sseFrame struct {
	// Raw is the frame text without its trailing blank-line terminator.
	Raw string
	// Elapsed is the time from the first Read call to when this frame arrived.
	Elapsed time.Duration
}

// splitSSEFrames is a bufio.SplitFunc that splits an SSE stream on the "\n\n"
// frame terminator emitted by the gateway (ssewriter writes "data: …\n\n" for
// the OpenAI dialect and "event: …\ndata: …\n\n" for Anthropic). Returning one
// token per completed frame is what lets collectArrivals observe frames as they
// arrive rather than only at EOF.
func splitSSEFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		// Include the frame body but consume the "\n\n" terminator.
		return i + 2, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		// Trailing bytes with no terminator (e.g. a final unterminated frame).
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

// clock returns the current time; injected so tests are deterministic.
type clock func() time.Duration

// collectArrivals reads the SSE stream to completion, returning one sseFrame per
// frame with the elapsed time (via the supplied clock) at which the frame's
// terminator was observed. Because the scanner blocks on the underlying reader
// until a full frame is buffered, a reader that delivers chunks with real gaps
// produces spread-out Elapsed values, whereas a single-Read blob produces
// frames whose Elapsed values are all ~identical.
func collectArrivals(r io.Reader, now clock) ([]sseFrame, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	scanner.Split(splitSSEFrames)

	var frames []sseFrame
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\n")
		if raw == "" {
			continue
		}
		frames = append(frames, sseFrame{Raw: raw, Elapsed: now()})
	}
	if err := scanner.Err(); err != nil {
		return frames, err
	}
	return frames, nil
}

// streamVerdict is the outcome of classifying one dialect's stream.
type streamVerdict struct {
	Incremental bool
	FrameCount  int
	MaxGap      time.Duration
	Reason      string
}

// classifyStream decides whether the observed frames arrived incrementally. A
// stream is incremental when at least two content frames were seen AND the
// largest gap between consecutive frame arrivals is at least minGap — i.e. the
// stream was delivered across time, not as one buffered blob. A single frame,
// or a burst of frames that all arrive within minGap of each other, is treated
// as buffered.
func classifyStream(frames []sseFrame, minGap time.Duration) streamVerdict {
	v := streamVerdict{FrameCount: len(frames)}

	if len(frames) < 2 {
		v.Reason = fmt.Sprintf("only %d frame(s) observed; need >=2 to prove incremental delivery", len(frames))
		return v
	}

	for i := 1; i < len(frames); i++ {
		if gap := frames[i].Elapsed - frames[i-1].Elapsed; gap > v.MaxGap {
			v.MaxGap = gap
		}
	}

	if v.MaxGap < minGap {
		v.Reason = fmt.Sprintf("all %d frames arrived within %s (max inter-frame gap %s < %s); response looks buffered",
			len(frames), minGap, v.MaxGap, minGap)
		return v
	}

	v.Incremental = true
	v.Reason = fmt.Sprintf("%d frames, max inter-frame gap %s >= %s", len(frames), v.MaxGap, minGap)
	return v
}

// dialectProbe describes one dialect's streaming endpoint and request body.
type dialectProbe struct {
	name string
	path string
	body string
}

// openAIProbe and anthropicProbe are the two streaming requests the check
// issues. Both set stream:true; the request content is intentionally trivial
// because the check asserts transport behaviour (incremental flush), not model
// output.
// buildProbes constructs the two streaming probes with operator-chosen models
// (defaults preserve the original staging models; local runs point both at
// whatever the seeded credential's backend serves).
func buildProbes(openaiModel, anthropicModel string) []dialectProbe {
	return []dialectProbe{
		{
			name: "openai",
			path: "/llm/v1/chat/completions",
			body: fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"count to five"}]}`, openaiModel),
		},
		{
			name: "anthropic",
			path: "/llm/v1/messages",
			body: fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":64,"messages":[{"role":"user","content":"count to five"}]}`, anthropicModel),
		},
	}
}

// probeStream POSTs the dialect's streaming request to /llm at baseURL and
// classifies the response body. It returns an error if the request fails, the
// status is not 200, or the Content-Type is not text/event-stream — all of
// which mean the stream was not served correctly regardless of timing.
// auth, when non-nil, stamps the caller's credentials onto the request
// (edgeAuth).
func probeStream(client *http.Client, baseURL string, p dialectProbe, minGap time.Duration, auth func(*http.Request)) (streamVerdict, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		strings.TrimRight(baseURL, "/")+p.path, strings.NewReader(p.body))
	if err != nil {
		return streamVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if auth != nil {
		auth(req)
	}

	resp, err := client.Do(req)
	if err != nil {
		return streamVerdict{}, fmt.Errorf("request to %s failed: %w", p.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return streamVerdict{}, fmt.Errorf("%s returned status %d (want 200)", p.path, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return streamVerdict{}, fmt.Errorf("%s returned Content-Type %q (want text/event-stream)", p.path, ct)
	}

	start := time.Now()
	frames, err := collectArrivals(resp.Body, func() time.Duration { return time.Since(start) })
	if err != nil {
		return streamVerdict{}, fmt.Errorf("reading %s stream: %w", p.path, err)
	}
	return classifyStream(frames, minGap), nil
}

// cmdSSEFlushCheck is the `cutover-ctl sse-flush-check` entrypoint. It probes
// both dialect streaming endpoints on elitea-main's public /llm and exits 0
// only when BOTH deliver their SSE frames incrementally; 1 when either does
// not; 2 on a configuration error (no API key).
func cmdSSEFlushCheck(args []string) {
	fs := flag.NewFlagSet("sse-flush-check", flag.ExitOnError)
	baseURL := fs.String("base-url", envOr("DEPLOYMENT_URL", defaultBaseURL), "elitea-main public origin serving /llm (default $DEPLOYMENT_URL, else "+defaultBaseURL+")")
	apiKey := fs.String("api-key", os.Getenv("API_KEY"), "personal access token for /llm (default $API_KEY)")
	projectID := fs.String("project-id", "", "project to bill, sent as "+edgeProjectHeader+" (empty = the token owner's personal project)")
	minGapMS := fs.Int("min-gap-ms", 5, "minimum max-inter-frame gap (ms) to accept a stream as incremental")
	timeoutS := fs.Int("timeout-s", 30, "per-request timeout in seconds")
	openaiModel := fs.String("openai-model", "gpt-4o", "model for the OpenAI-dialect probe")
	anthropicModel := fs.String("anthropic-model", "claude-sonnet-5", "model for the Anthropic-dialect probe")
	_ = fs.Parse(args)

	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "sse-flush-check: --api-key (or $API_KEY) is required: /llm authenticates every caller")
		os.Exit(2)
	}
	minGap := time.Duration(*minGapMS) * time.Millisecond
	client := &http.Client{Timeout: time.Duration(*timeoutS) * time.Second}
	auth := func(req *http.Request) { edgeAuth(req, *apiKey, *projectID) }

	probes := buildProbes(*openaiModel, *anthropicModel)
	failed := false
	for _, p := range probes {
		verdict, err := probeStream(client, *baseURL, p, minGap, auth)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s (%s): %v\n", p.name, p.path, err)
			failed = true
			continue
		}
		if !verdict.Incremental {
			fmt.Fprintf(os.Stderr, "✗ %s (%s): %s\n", p.name, p.path, verdict.Reason)
			failed = true
			continue
		}
		fmt.Printf("✓ %s (%s): %s\n", p.name, p.path, verdict.Reason)
	}

	if failed {
		fmt.Fprintln(os.Stderr, "\nSSE incremental flush not proven for all dialects (spec §2.3, gate BFF.1).")
		os.Exit(1)
	}
	fmt.Printf("✓ sse-flush-check: both dialects stream incrementally through %s\n", *baseURL)
}

// Test-compat probe singletons (default models).
var (
	openAIProbe    = buildProbes("gpt-4o", "claude-sonnet-5")[0]
	anthropicProbe = buildProbes("gpt-4o", "claude-sonnet-5")[1]
)

// defaultBaseURL is elitea-main's HTTP listener on a local stack.
const defaultBaseURL = "http://localhost:8080"

// edgeProjectHeader is the /llm project selector (middleware.HeaderProjectSelector).
const edgeProjectHeader = "X-Project-Id"

// edgeAuth authenticates req as an external /llm caller: the personal access
// token as a bearer and, when projectID is set, the project to bill (the token
// owner must be a member). The edge resolves and signs the identity the
// gateway sees; a caller cannot set it, and any X-Elitea-* header it sends is
// stripped.
func edgeAuth(req *http.Request, apiKey, projectID string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if projectID != "" {
		req.Header.Set(edgeProjectHeader, projectID)
	}
}
