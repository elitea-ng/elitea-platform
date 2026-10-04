package predict

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// captureIdentity runs one completion and returns the identity headers the
// gateway received.
func captureIdentity(t *testing.T, attributionID string) http.Header {
	t.Helper()
	got := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	completer := NewGatewayCompleter(server.URL, nil, "shared-secret")
	if _, err := completer.Complete(context.Background(), CompletionRequest{
		ProjectID:     "7",
		UserID:        "42",
		Model:         "gpt-4o",
		Messages:      []Message{{Role: "user", Content: "hello"}},
		AttributionID: attributionID,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return <-got
}

// The attribution id travels as the SIGNED execution id (legacy issue 6677).
// An unsigned header would be dropped by the gateway's v2 check, and the
// evaluation spend would land in the log with no attribution at all.
func TestCompleteSignsTheAttributionIDAsTheExecutionID(t *testing.T) {
	t.Parallel()

	headers := captureIdentity(t, "eval:12:case:34")
	if got := headers.Get(llmproxy.HeaderExecutionID); got != "eval:12:case:34" {
		t.Fatalf("execution id header = %q, want eval:12:case:34", got)
	}

	want := http.Header{}
	llmproxy.SignIdentityHeaders(want, []byte("shared-secret"), "7", "42", "", "eval:12:case:34")
	if headers.Get(llmproxy.HeaderSignature) != want.Get(llmproxy.HeaderSignature) {
		t.Fatalf("signature does not cover the execution id: got %q, want %q",
			headers.Get(llmproxy.HeaderSignature), want.Get(llmproxy.HeaderSignature))
	}
}

// A browser turn names no run, so the request keeps the v1 signature an
// older gateway still accepts.
func TestCompleteWithoutAttributionSignsV1(t *testing.T) {
	t.Parallel()

	headers := captureIdentity(t, "")
	if got := headers.Get(llmproxy.HeaderExecutionID); got != "" {
		t.Fatalf("execution id header = %q, want none", got)
	}
	want := http.Header{}
	llmproxy.SignIdentityHeaders(want, []byte("shared-secret"), "7", "42", "", "")
	if headers.Get(llmproxy.HeaderSignature) != want.Get(llmproxy.HeaderSignature) {
		t.Fatal("a request with no attribution must sign exactly as before")
	}
}

// An id outside the edge's charset is dropped, never sent: it becomes a
// header, and a CR or LF in it is header injection.
func TestCompleteDropsAnInvalidAttributionID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"eval 1", "eval\r\nX-Injected: 1", strings.Repeat("a", 129), "eval/1"} {
		headers := captureIdentity(t, id)
		if got := headers.Get(llmproxy.HeaderExecutionID); got != "" {
			t.Errorf("id %q: execution id header = %q, want none", id, got)
		}
	}
}

func TestValidAttributionID(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"":                       false,
		"eval:1:case:2":          true,
		"eval:1:judge:2":         true,
		"0c1d-ab_9.x":            true,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
		"has space":              false,
		"semi;colon":             false,
		"slash/in/it":            false,
	}
	for id, want := range cases {
		if got := ValidAttributionID(id); got != want {
			t.Errorf("ValidAttributionID(%q) = %v, want %v", id, got, want)
		}
	}
}
