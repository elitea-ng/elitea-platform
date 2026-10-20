package llmproxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// The refusal log names the upstream status and the provider's type, code and
// parameter path, and never the message, which can echo prompt content.
func TestProviderRefusalLogKeepsFixedFieldsOnly(t *testing.T) {
	var out bytes.Buffer
	h := &Handler{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	e := bErr(403, "permission_error", "content_policy_violation",
		"prompt said: PROMPT-ECHO-DO-NOT-LOG")
	e.Error.Param = "messages[3].content"
	e.ExtraFields.RoutingInfo.Provider = "vllm"
	e.ExtraFields.RoutingInfo.Model = "gpt-5.4-mini"

	h.writeOpenAIError(httptest.NewRecorder(), e)

	line := out.String()
	if strings.Contains(line, "DO-NOT-LOG") || strings.Contains(line, "prompt said") {
		t.Fatalf("the provider message reached the log: %s", line)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("one JSON log line: %v: %s", err, line)
	}
	want := map[string]any{
		"event":       "llm_provider_refusal",
		"status":      float64(403),
		"error_type":  "permission_error",
		"error_code":  "content_policy_violation",
		"error_param": "messages[3].content",
		"provider":    "vllm",
		"model":       "gpt-5.4-mini",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v (%s)", key, got[key], value, line)
		}
	}
}

// A provider field that is not a short token is replaced, never passed through.
func TestRefusalTokenRefusesFreeText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"forbidden", "forbidden"},
		{"messages[0].content", "messages[0].content"},
		{"has space", "redacted"},
		{"line\nbreak", "redacted"},
		{"quote\"", "redacted"},
		{strings.Repeat("a", maxRefusalFieldBytes), strings.Repeat("a", maxRefusalFieldBytes)},
		{strings.Repeat("a", maxRefusalFieldBytes+1), "redacted"},
		{"café", "redacted"},
	} {
		if got := refusalToken(tc.in); got != tc.want {
			t.Errorf("refusalToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
