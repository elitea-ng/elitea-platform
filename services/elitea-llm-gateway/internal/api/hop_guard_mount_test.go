package api

// hop_guard_mount_test.go — the mount gate for hop-marker detection (#164).
//
// llmproxy's own tests drive HopGuard directly, so they stay green even if the
// router stops mounting it. That gap is the whole failure mode this file
// closes: a guard that is written, unit-tested and never installed.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// TestRouterMountsHopGuardOnEveryRoute walks the router with a request that
// carries this deployment's own hop marker and requires the loop refusal on
// every path — the inference routes, and the paths that reach NO handler.
//
// The NotFound case is the one that pins ROOT mounting. A guard installed per
// route, or inside each handler, answers 404 there; a routing loop aimed at a
// path the gateway does not serve is then unbounded, and the amplification
// backstop cannot see it because a 404 never reaches admission.
func TestRouterMountsHopGuardOnEveryRoute(t *testing.T) {
	marker := hopmarker.New([]byte("hop-secret"))
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil, llmproxy.WithHopMarker(marker))
	r := NewRouter(h)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"chat completions", http.MethodPost, "/llm/v1/chat/completions", `{"model":"openai/gpt-4o","messages":[]}`},
		{"anthropic messages", http.MethodPost, "/llm/v1/messages", `{"model":"anthropic/claude-3-5-sonnet","max_tokens":1,"messages":[]}`},
		{"embeddings", http.MethodPost, "/llm/v1/embeddings", `{"model":"openai/text-embedding-3-small","input":"hi"}`},
		{"models list", http.MethodGet, "/llm/v1/models", ""},
		{"a route the gateway does not serve", http.MethodPost, "/llm/v1/nothing-here", `{}`},
		{"a method the route does not serve", http.MethodDelete, "/llm/v1/chat/completions", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(hopmarker.Header, marker.Value())

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. The router does not mount the hop guard on this path, "+
					"so a circular route through it is never detected.", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "circular_routing_detected") {
				t.Errorf("body = %s, want the circular_routing_detected refusal", rec.Body.String())
			}
		})
	}
}

// TestRouterLeavesUnmarkedTrafficAlone is the false-positive floor. Every
// route above must still serve an ordinary request — the guard is mounted on
// the root, so a mistake here would break the whole surface at once.
func TestRouterLeavesUnmarkedTrafficAlone(t *testing.T) {
	marker := hopmarker.New([]byte("hop-secret"))
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil, llmproxy.WithHopMarker(marker))
	r := NewRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-4o","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a request with no hop marker: %s", rec.Code, rec.Body.String())
	}
}
