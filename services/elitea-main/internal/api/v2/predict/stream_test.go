package predict

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/gatewaytest"
)

// sseServer answers every chat completion with the given SSE body, the way the
// gateway's streamOpenAI frames it, and records the request body.
func sseServer(body string, captured *map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	})
}

func TestCompleteStreamHandsEachDeltaOverAndReturnsTheWholeText(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		": keepalive\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	completer := NewGatewayCompleter(gatewaytest.Client(t, sseServer(body, &captured), "shared-secret"))

	var deltas []string
	full, err := completer.CompleteStream(context.Background(), CompletionRequest{
		ProjectID: "7", UserID: "42", Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if full != "Hello" || strings.Join(deltas, "|") != "Hel|lo" {
		t.Fatalf("full = %q deltas = %v, want Hello and [Hel lo]", full, deltas)
	}
	if captured["stream"] != true {
		t.Errorf("request stream = %v, want true", captured["stream"])
	}
}

func TestCompleteStreamRefusesAStreamWithoutDone(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"name\\\"\"}}]}\n\n"
	completer := NewGatewayCompleter(gatewaytest.Client(t, sseServer(body, &captured), "shared-secret"))

	_, err := completer.CompleteStream(context.Background(), CompletionRequest{ProjectID: "7", Model: "m"},
		func(string) error { return nil })
	if !errors.Is(err, ErrStreamCutOff) {
		t.Fatalf("err = %v, want ErrStreamCutOff", err)
	}
}

func TestCompleteStreamReportsAnUpstreamErrorFrameWithoutItsText(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	body := "data: {\"error\":{\"message\":\"provider secret detail\"}}\n\n"
	completer := NewGatewayCompleter(gatewaytest.Client(t, sseServer(body, &captured), "shared-secret"))

	_, err := completer.CompleteStream(context.Background(), CompletionRequest{ProjectID: "7", Model: "m"},
		func(string) error { return nil })
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want an error that does not echo the provider text", err)
	}
}

func TestCompleteStreamTreatsAnEmptyAnswerAsNoContent(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	completer := NewGatewayCompleter(gatewaytest.Client(t, sseServer("data: [DONE]\n\n", &captured), "shared-secret"))

	_, err := completer.CompleteStream(context.Background(), CompletionRequest{ProjectID: "7", Model: "m"},
		func(string) error { return nil })
	if !errors.Is(err, ErrNoContent) {
		t.Fatalf("err = %v, want ErrNoContent", err)
	}
}

func TestCompleteStreamMapsANonOKStatusToAnError(t *testing.T) {
	t.Parallel()

	server := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	})
	completer := NewGatewayCompleter(gatewaytest.Client(t, server, "shared-secret"))
	_, err := completer.CompleteStream(context.Background(), CompletionRequest{ProjectID: "7", Model: "m"},
		func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "402") {
		t.Fatalf("err = %v, want the gateway status named", err)
	}
}

type blockingOnly struct{ content string }

func (b blockingOnly) Complete(context.Context, CompletionRequest) (string, error) { return b.content, nil }

func TestStreamFallsBackToOneDeltaForABlockingCompleter(t *testing.T) {
	t.Parallel()

	var deltas []string
	full, err := Stream(context.Background(), blockingOnly{content: "whole"}, CompletionRequest{},
		func(delta string) error {
			deltas = append(deltas, delta)
			return nil
		})
	if err != nil || full != "whole" || len(deltas) != 1 || deltas[0] != "whole" {
		t.Fatalf("full = %q deltas = %v err = %v", full, deltas, err)
	}
}
