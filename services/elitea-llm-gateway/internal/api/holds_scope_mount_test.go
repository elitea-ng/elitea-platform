package api

// holds_scope_mount_test.go — the mount gate for the degraded-mode request
// scope (human decision, DECISIONS.md 2026-10-22). llmproxy's tests wrap the
// handler in the scope themselves, so they stay green if the router stops
// mounting it. Without the scope a request carries no holds, and a NATS-down
// admission then reserves nothing: concurrent admission is unbounded again.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// holdsSeeingRouter records whether the context a provider call receives
// carries the request's holds.
type holdsSeeingRouter struct {
	recordingRouter
	calls, withHolds atomic.Int64
}

func (r *holdsSeeingRouter) see(c *schemas.BifrostContext) {
	r.calls.Add(1)
	if failmode.HoldsFrom(c) != nil {
		r.withHolds.Add(1)
	}
}

func (r *holdsSeeingRouter) ChatCompletionRequest(c *schemas.BifrostContext, q *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.see(c)
	return r.recordingRouter.ChatCompletionRequest(c, q)
}

func (r *holdsSeeingRouter) EmbeddingRequest(c *schemas.BifrostContext, q *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	r.see(c)
	return r.recordingRouter.EmbeddingRequest(c, q)
}

func TestRouterMountsTheDegradedHoldsScope(t *testing.T) {
	rr := &holdsSeeingRouter{}
	r := NewRouter(llmproxy.NewHandler(rr, nil, nil))
	for _, tc := range []struct{ path, body string }{
		{"/llm/v1/chat/completions", `{"model":"openai/gpt-4o","messages":[]}`},
		{"/llm/v1/embeddings", `{"model":"openai/text-embedding-3-small","input":"hi"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	if rr.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", rr.calls.Load())
	}
	if rr.withHolds.Load() != rr.calls.Load() {
		t.Fatalf("%d of %d provider calls carried the request's holds; the router does not mount "+
			"DegradedHoldsScope, so a NATS-down admission reserves nothing", rr.withHolds.Load(), rr.calls.Load())
	}
}
