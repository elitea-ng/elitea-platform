// Package engine is the host's client for an engine sidecar (ADR-0023 H2).
//
// An application whose engine cannot be Go — a dependency closure that stays
// in Python — runs it as a sidecar next to this host and speaks NDJSON to it
// over a Unix socket. The host keeps the SPI, admission, the parameter
// merge, composition and upload; the sidecar runs one tool at a time:
//
//	POST /engine/invoke                 {invocation_id, tool, arguments}
//	  → NDJSON: {"thinking": "…"} and {"token": "…"} interleaved, then
//	    {"result": {…}} | {"error": {…}}
//	POST /engine/invocations/{id}/stop  requests a cooperative stop
//
// Progress lines become the invocation's thinking events as they arrive and
// token lines become its answer events, IN THE ORDER THEY ARRIVE: they share
// one event list, so a poll shows the answer growing between the steps that
// produced it. Two keys rather than one because the two are different things
// and a reader must not have to guess which (issue #701; the envelope is
// conformance/provider/fixtures/deepwiki/stream/token_events.json).
//
// The host's own stop checkpoint is watched while the stream is open, and a
// stop is forwarded to the sidecar — which terminates the engine's worker
// subprocess — before this side gives up on the stream.
//
// It was DeepWiki's (internal/apps/deepwiki/run/engine.go) until ADR-0023
// stage H4c and moved unchanged; the only application-specific thing in it
// was the word "DeepWiki" in the messages a caller reads, which is now the
// Label a client is built with. Which tools a sidecar serves, and how its
// result dict is composed, stay with the application.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// line is one NDJSON line of the sidecar's stream.
type line struct {
	Thinking *string        `json:"thinking,omitempty"`
	Token    *string        `json:"token,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Error    *lineError     `json:"error,omitempty"`
}

type lineError struct {
	Message       string `json:"message"`
	ErrorType     string `json:"error_type"`
	ErrorCategory string `json:"error_category"`
}

// Client speaks the sidecar protocol over a Unix socket.
type Client struct {
	Socket string
	// Label names the engine in the messages a caller reads ("The DeepWiki
	// engine is not reachable at …"). Empty is "The engine".
	Label      string
	HTTP       *http.Client
	StopPeriod time.Duration
}

// NewClient dials the socket for every request; the URL host is a
// placeholder the transport ignores.
func NewClient(socket, label string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
		// A generation can run for an hour; nothing here may time it out.
		ResponseHeaderTimeout: 0,
	}
	return &Client{Socket: socket, Label: label, HTTP: &http.Client{Transport: transport}, StopPeriod: 250 * time.Millisecond}
}

// subject is how the engine is named in a message.
func (c *Client) subject() string {
	if c.Label == "" {
		return "The engine"
	}
	return "The " + c.Label + " engine"
}

// Invoke runs one tool and streams its progress into the invocation. It
// returns the engine's result dict — success or not; the caller maps a
// failed result — or an error for a transport failure, a refusal the
// sidecar reported, or a stop.
func (c *Client) Invoke(ctx context.Context, tool string, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"invocation_id": tc.InvocationID(), "tool": tool, "arguments": arguments})
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "encode the engine request: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine/engine/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "%v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "%s is not reachable at %s: %v", c.subject(), c.Socket, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, spi.Failf(spi.KindRuntime, "%s refused the invocation: HTTP %d %s", c.subject(), response.StatusCode, strings.TrimSpace(string(text)))
	}

	// The stop watcher: the host's checkpoint is the only place a stop is
	// visible; the sidecar must hear about it to kill the worker.
	stopped := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(c.StopPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tc.Checkpoint() != nil {
					c.stop(tc.InvocationID())
					close(stopped)
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-watcherDone }()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var next line
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, spi.Failf(spi.KindRuntime, "%s sent a line this host cannot read: %.120s", c.subject(), raw)
		}
		switch {
		case next.Thinking != nil:
			if err := tc.Thinking(ctx, *next.Thinking); err != nil {
				return nil, err
			}
		case next.Token != nil:
			// Before the error and result cases for the same reason
			// Thinking is: a line is exactly one of these, and the order
			// only records which key is read first.
			if err := tc.Token(ctx, *next.Token); err != nil {
				return nil, err
			}
		case next.Error != nil:
			return nil, spi.Failf(KindOf(next.Error.ErrorType), "%s", next.Error.Message)
		case next.Result != nil:
			return next.Result, nil
		}
	}
	select {
	case <-stopped:
		return nil, spi.ErrCancelled
	default:
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, spi.Failf(spi.KindRuntime, "%s's stream ended in error: %v", c.subject(), err)
	}
	if tc.Checkpoint() != nil {
		return nil, spi.ErrCancelled
	}
	return nil, spi.Failf(spi.KindRuntime, "%s closed the stream without a result", c.subject())
}

// stop asks the sidecar to stop one invocation. Best effort: the stream
// is cancelled either way, and the sidecar terminates its worker on its
// own checkpoint.
func (c *Client) stop(invocationID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine/engine/invocations/"+invocationID+"/stop", nil)
	if err != nil {
		return
	}
	if response, err := c.HTTP.Do(request); err == nil {
		_ = response.Body.Close()
	}
}

// KindOf maps the sidecar's error_type — the Python exception class name —
// onto the host's kinds; the classifier does the rest.
func KindOf(errorType string) spi.Kind {
	switch errorType {
	case "FileNotFoundError":
		return spi.KindNotFound
	case "ValueError":
		return spi.KindValue
	case "MemoryError":
		return spi.KindMemory
	case "KeyError":
		return spi.KindKey
	case "RuntimeError", "":
		return spi.KindRuntime
	default:
		return spi.KindGeneric
	}
}
