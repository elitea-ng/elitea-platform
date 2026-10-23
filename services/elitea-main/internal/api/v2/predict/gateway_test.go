package predict

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy/gatewaytest"
)

func TestCompleteReturnsTheFirstNonEmptyChoiceContent(t *testing.T) {
	t.Parallel()

	var captured struct {
		path    string
		project string
		user    string
		body    map[string]any
	}
	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.project = r.Header.Get("X-Elitea-Project-Id")
		captured.user = r.Header.Get("X-Elitea-User-Id")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		// The first choice is deliberately empty: a caller that reads
		// choices[0] blindly would return "" and the handler would report a
		// successful empty answer.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}},{"message":{"content":"generated"}}]}`))
	}))

	completer := NewGatewayCompleter(gatewaytest.Client(t, server, "shared-secret"))
	temperature := 0.25
	maxTokens := 512
	content, err := completer.Complete(context.Background(), CompletionRequest{
		ProjectID:   "7",
		UserID:      "42",
		Model:       "gpt-4o",
		Temperature: &temperature,
		MaxTokens:   &maxTokens,
		Messages:    []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if content != "generated" {
		t.Errorf("content = %q, want %q", content, "generated")
	}
	if captured.path != "/llm/v1/chat/completions" {
		t.Errorf("path = %q, want /llm/v1/chat/completions", captured.path)
	}
	if captured.project != "7" || captured.user != "42" {
		t.Errorf("identity headers = project %q user %q, want 7 / 42", captured.project, captured.user)
	}
	if got := captured.body["model"]; got != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", got)
	}
	if got := captured.body["stream"]; got != false {
		t.Errorf("stream = %v, want false — this is the blocking mode", got)
	}
	if got := captured.body["temperature"]; got != 0.25 {
		t.Errorf("temperature = %v, want 0.25", got)
	}
}

// A zero temperature is a legitimate, meaningful setting. If the field were a
// plain float64 it would be indistinguishable from "absent" and silently
// dropped, so the request would be sent at the provider's own default.
func TestCompleteSendsAnExplicitZeroTemperature(t *testing.T) {
	t.Parallel()

	var body map[string]any
	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))

	zero := 0.0
	if _, err := NewGatewayCompleter(gatewaytest.Client(t, server, "s")).Complete(context.Background(), CompletionRequest{
		ProjectID:   "1",
		Temperature: &zero,
		Messages:    []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	temperature, present := body["temperature"]
	if !present {
		t.Fatalf("temperature was dropped from the request body: %v", body)
	}
	if temperature != 0.0 {
		t.Errorf("temperature = %v, want 0", temperature)
	}
}

func TestCompleteReportsANonOKGatewayStatusAsAnError(t *testing.T) {
	t.Parallel()

	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"provider quota exceeded for key sk-live-abcdef"}}`))
	}))

	content, err := NewGatewayCompleter(gatewaytest.Client(t, server, "s")).Complete(context.Background(), CompletionRequest{
		ProjectID: "1", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("Complete returned no error for a 429; content = %q", content)
	}
	if content != "" {
		t.Errorf("content = %q on a failed call, want empty", content)
	}
	// The upstream body can carry provider error text and secrets. It must not
	// reach the error the handler logs and never the browser.
	if got := err.Error(); strings.Contains(got, "sk-live-abcdef") || strings.Contains(got, "quota") {
		t.Errorf("error echoes the upstream body: %q", got)
	}
}

func TestCompleteReportsAMalformedGatewayBodyAsAnError(t *testing.T) {
	t.Parallel()

	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices": [ this is not json`))
	}))

	if _, err := NewGatewayCompleter(gatewaytest.Client(t, server, "s")).Complete(context.Background(), CompletionRequest{
		ProjectID: "1", Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Complete accepted a malformed gateway body")
	}
}

// A 200 carrying no assistant text is not a successful empty answer: the
// caller renders whatever comes back straight into a document.
func TestCompleteRefusesAnEmptyCompletion(t *testing.T) {
	t.Parallel()

	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))

	_, err := NewGatewayCompleter(gatewaytest.Client(t, server, "s")).Complete(context.Background(), CompletionRequest{
		ProjectID: "1", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}

func TestCompleteAbandonsTheCallWhenTheContextDeadlinePasses(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	server := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"too late"}}]}`))
	}))
	defer func() {
		close(release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	content, err := NewGatewayCompleter(gatewaytest.Client(t, server, "s")).Complete(ctx, CompletionRequest{
		ProjectID: "1", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("Complete waited past its deadline and returned %q", content)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a wrapped context.DeadlineExceeded", err)
	}
}

func TestNewGatewayCompleterYieldsNothingWithoutAGateway(t *testing.T) {
	t.Parallel()

	completer := NewGatewayCompleter(nil)
	if completer != nil {
		t.Error("an empty LLM_GATEWAY_URL produced a completer; callers must see nothing to compose")
	}
}

func TestResolveRequestTimeoutClampsTheCallerSuppliedDeadline(t *testing.T) {
	t.Parallel()

	seconds := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"absent takes the default", nil, defaultRequestTimeout},
		// 0 meant "go asynchronous" in legacy. There is no asynchronous half
		// here, so it must not mean "no deadline" or "give up immediately".
		{"zero takes the default rather than meaning async", seconds(0), defaultRequestTimeout},
		{"negative takes the default", seconds(-30), defaultRequestTimeout},
		{"below the floor is raised", seconds(1), minRequestTimeout},
		{"a realistic value is honoured", seconds(60), 60 * time.Second},
		{"above the ceiling is capped", seconds(86400), maxRequestTimeout},
	} {
		if got := resolveRequestTimeout(tc.in); got != tc.want {
			t.Errorf("%s: resolveRequestTimeout = %s, want %s", tc.name, got, tc.want)
		}
	}
}
