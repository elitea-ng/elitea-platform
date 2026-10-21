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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// sse-flush-check (spec §2.3 / §7.3, gate BFF.1 / validator BFF.9a):
//
// The gateway MUST stream SSE responses incrementally — each chunk flushed as
// it is produced, not accumulated and delivered as one buffered blob at
// end-of-response. This must hold end-to-end through both hops (edge reverse
// proxy → gateway) and Traefik, for BOTH dialects:
//
//   - OpenAI    POST /llm/v1/chat/completions  — data-only frames, "data: [DONE]" terminator
//   - Anthropic POST /llm/v1/messages          — named-event frames, "event: message_stop" terminator
//
// Both are Content-Type text/event-stream; they differ only in event names and
// terminator, not transport. A buffering hop (e.g. a reverse proxy without
// FlushInterval<0, or missing X-Accel-Buffering: no) collapses the whole
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

// probeStream POSTs the dialect's streaming request to the gateway and classifies
// the response body. It returns an error if the request fails, the status is not
// 200, or the Content-Type is not text/event-stream — all of which mean the
// stream was not served correctly regardless of timing.
// sign, when non-nil, stamps auth/identity headers onto the request (the
// gateway enforces signed X-Elitea-* identity headers whenever
// GATEWAY_IDENTITY_SECRET is set — which FIX #9 makes mandatory with NATS on).
func probeStream(client *http.Client, gatewayURL string, p dialectProbe, minGap time.Duration, sign func(*http.Request)) (streamVerdict, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		strings.TrimRight(gatewayURL, "/")+p.path, strings.NewReader(p.body))
	if err != nil {
		return streamVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if sign != nil {
		sign(req)
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
// both dialect streaming endpoints on the gateway and exits 0 only when BOTH
// deliver their SSE frames incrementally.
func cmdSSEFlushCheck(args []string) {
	fs := flag.NewFlagSet("sse-flush-check", flag.ExitOnError)
	gatewayURL := fs.String("gateway-url", "http://localhost:8083", "base URL of the gateway (edge reverse proxy or elitea-llm-gateway-svc)")
	minGapMS := fs.Int("min-gap-ms", 5, "minimum max-inter-frame gap (ms) to accept a stream as incremental")
	timeoutS := fs.Int("timeout-s", 30, "per-request timeout in seconds")
	openaiModel := fs.String("openai-model", "gpt-4o", "model for the OpenAI-dialect probe")
	anthropicModel := fs.String("anthropic-model", "claude-sonnet-5", "model for the Anthropic-dialect probe")
	identitySecret := fs.String("identity-secret", os.Getenv("GATEWAY_IDENTITY_SECRET"), "edge identity HMAC secret (empty = unsigned)")
	projectID := fs.String("project-id", "", "project ID for the signed identity headers")
	userID := fs.String("user-id", "cutover-ctl", "user ID for the signed identity headers")
	tenantID := fs.String("tenant-id", "", "tenant ID for the signed identity headers")
	_ = fs.Parse(args)

	minGap := time.Duration(*minGapMS) * time.Millisecond
	client := &http.Client{Timeout: time.Duration(*timeoutS) * time.Second}

	var sign func(*http.Request)
	if *projectID != "" {
		sign = func(req *http.Request) {
			signIdentity(req, []byte(*identitySecret), *projectID, *userID, *tenantID)
		}
	}

	probes := buildProbes(*openaiModel, *anthropicModel)
	failed := false
	for _, p := range probes {
		verdict, err := probeStream(client, *gatewayURL, p, minGap, sign)
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
	fmt.Printf("✓ sse-flush-check: both dialects stream incrementally through %s\n", *gatewayURL)
}

// Test-compat probe singletons (default models).
var (
	openAIProbe    = buildProbes("gpt-4o", "claude-sonnet-5")[0]
	anthropicProbe = buildProbes("gpt-4o", "claude-sonnet-5")[1]
)

// signIdentity sets the signed X-Elitea-* identity on req, by the scheme
// elitea-main signs it (llmproxy.SignIdentityHeaders).
//
// TODO(#1359): elitea-llm-gateway serves /llm over gRPC only (GatewayService),
// so its HTTP listener answers none of these probes. sse-flush-check and
// overhead-check reach /llm only through the elitea-main edge now, which
// strips these headers and authenticates the caller instead. Port both probes
// to an edge bearer token, or to the GatewayService client budget-check uses.
func signIdentity(req *http.Request, secret []byte, projectID, userID, tenantID string) {
	llmproxy.SignIdentityHeaders(req.Header, secret, projectID, userID, tenantID, "")
}
