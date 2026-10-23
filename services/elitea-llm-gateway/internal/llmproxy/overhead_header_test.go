package llmproxy

// overhead_header_test.go — issue #17. X-Elapsed-Ms feeds the BFF.9d overhead
// gate (design §10.2). It must count the credential resolution that runs INSIDE
// the router call, and it must still exclude the provider round-trip.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/overhead"
)

// resolveDelay stands for the Account's Postgres read plus the Fernet decrypt.
// providerDelay stands for the provider round-trip the metric must exclude.
const (
	resolveDelay  = 40 * time.Millisecond
	providerDelay = 300 * time.Millisecond
)

// credentialRouter simulates what bifrost/core does inside one router call: it
// routes, resolves the caller's credential (which marks the overhead Meter the
// way account.GetKeysForProvider does), then calls the provider.
type credentialRouter struct {
	fakeRouter
	// skipMark models a request that resolves no credential — a direct key or a
	// plugin short-circuit.
	skipMark bool
}

func (c *credentialRouter) work(ctx *schemas.BifrostContext) {
	time.Sleep(resolveDelay)
	if !c.skipMark {
		overhead.FromContext(ctx).MarkCredentialsResolved()
	}
	time.Sleep(providerDelay)
}

func (c *credentialRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	c.work(ctx)
	return c.fakeRouter.ChatCompletionRequest(ctx, req)
}

func (c *credentialRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	c.work(ctx)
	return c.fakeRouter.ChatCompletionStreamRequest(ctx, req)
}

// elapsedMs reads X-Elapsed-Ms off the RESULT header. The result header is the
// snapshot taken when the response head was written, so the read also proves
// the stamp landed before the first body byte.
func elapsedMs(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	v := rec.Result().Header.Get("X-Elapsed-Ms")
	if v == "" {
		t.Fatal("X-Elapsed-Ms missing from the written response header")
	}
	ms, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("X-Elapsed-Ms = %q, want a float: %v", v, err)
	}
	return ms
}

// TestChat_ElapsedHeaderCountsCredentialResolution is the issue #17 regression.
// The header must carry the in-router credential resolution. Before the fix the
// handler stamped the value BEFORE dispatch, so the header reported well under
// a millisecond and the gate read an overhead that excluded the Account's
// Postgres read and Fernet decrypt.
func TestChat_ElapsedHeaderCountsCredentialResolution(t *testing.T) {
	router := &credentialRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := NewHandler(router, nil, nil)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ms := elapsedMs(t, rec)
	const wantAtLeast = 30.0 // resolveDelay with slack for a loaded runner
	if ms < wantAtLeast {
		t.Fatalf("X-Elapsed-Ms = %.3f, want at least %.0f: the header excludes credential resolution (issue #17)", ms, wantAtLeast)
	}
	const wantBelow = 200.0 // well under resolveDelay + providerDelay
	if ms >= wantBelow {
		t.Fatalf("X-Elapsed-Ms = %.3f, want below %.0f: the header now carries the provider round-trip", ms, wantBelow)
	}
}

// TestChatStream_ElapsedHeaderCountsCredentialResolution asserts the streaming
// path stamps the same measurement, and stamps it before beginStream writes the
// SSE response head.
func TestChatStream_ElapsedHeaderCountsCredentialResolution(t *testing.T) {
	router := &credentialRouter{}
	router.streamChan = newChunkChan()
	h := NewHandler(router, nil, nil)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", true /* stream */))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ms := elapsedMs(t, rec)
	if ms < 30.0 {
		t.Fatalf("X-Elapsed-Ms = %.3f, want at least 30: the streaming header excludes credential resolution (issue #17)", ms)
	}
	if ms >= 200.0 {
		t.Fatalf("X-Elapsed-Ms = %.3f, want below 200: the streaming header now carries the provider round-trip", ms)
	}
}

// TestChat_ElapsedHeaderExcludesProviderWhenNoCredentialResolves asserts the
// fallback. With no mark the header reports the pre-dispatch time alone. It
// must NOT become a plain time.Since after the router call, which would report
// the whole provider round-trip as gateway overhead.
func TestChat_ElapsedHeaderExcludesProviderWhenNoCredentialResolves(t *testing.T) {
	router := &credentialRouter{skipMark: true}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := NewHandler(router, nil, nil)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ms := elapsedMs(t, rec)
	if ms >= 30.0 {
		t.Fatalf("X-Elapsed-Ms = %.3f, want the pre-dispatch time alone: an unmarked request reports router time", ms)
	}
}
