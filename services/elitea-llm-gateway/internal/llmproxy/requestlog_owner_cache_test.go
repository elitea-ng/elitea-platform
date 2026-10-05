package llmproxy

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// Legacy issue 6709: the request log must say who owns the credential that
// served a request, and how many prompt tokens the provider read from and wrote
// to its cache. These tests drive the REAL middleware over the REAL handler, as
// stream_requestlog_test.go does, because both values are joined in from the
// billing path and not from the transport.

func cachedUsage(prompt, completion, cacheRead, cacheWrite int) *schemas.BifrostLLMUsage {
	return &schemas.BifrostLLMUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  cacheRead,
			CachedWriteTokens: cacheWrite,
		},
	}
}

func TestBufferedChatLogsCacheTokensAndAPlatformCredential(t *testing.T) {
	h := NewHandler(&fakeRouter{
		chatResp:      &schemas.BifrostChatResponse{ID: "cmpl-1", Model: "gpt-4o", Usage: cachedUsage(1200, 30, 1000, 150)},
		selectedKeyID: "shared:pub-1",
	}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if row.CacheReadToks != 1000 || row.CacheWriteToks != 150 {
		t.Errorf("logged cache tokens = (%d, %d), want (1000, 150)", row.CacheReadToks, row.CacheWriteToks)
	}
	if row.CredentialOwner != requestlog.CredentialOwnerPlatform {
		t.Errorf("credential owner = %q, want %q for a shared key", row.CredentialOwner, requestlog.CredentialOwnerPlatform)
	}
}

func TestBufferedChatLogsAProjectCredential(t *testing.T) {
	h := NewHandler(&fakeRouter{
		chatResp:      &schemas.BifrostChatResponse{ID: "cmpl-1", Model: "gpt-4o", Usage: cachedUsage(10, 5, 0, 0)},
		selectedKeyID: "17",
	}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if row.CredentialOwner != requestlog.CredentialOwnerProject {
		t.Errorf("credential owner = %q, want %q for the project's own key", row.CredentialOwner, requestlog.CredentialOwnerProject)
	}
}

// A response from a router that selected no key leaves the owner empty. An
// empty owner is "unknown", which is the only true statement here.
func TestBufferedChatWithoutASelectedKeyLeavesTheOwnerEmpty(t *testing.T) {
	h := NewHandler(&fakeRouter{
		chatResp: &schemas.BifrostChatResponse{ID: "cmpl-1", Model: "gpt-4o", Usage: cachedUsage(10, 5, 0, 0)},
	}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if row.CredentialOwner != "" {
		t.Errorf("credential owner = %q, want empty", row.CredentialOwner)
	}
}

func TestStreamedChatLogsCacheTokensAndTheCredentialOwner(t *testing.T) {
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
			ID: "trailer", Object: "chat.completion.chunk", Usage: cachedUsage(2351, 42, 2048, 0),
		}},
	)
	h := NewHandler(&fakeRouter{streamChan: chunks, selectedKeyID: "shared:pub-1"}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/chat/completions",
		`{"model":"openai/gpt-4o","messages":[],"stream":true}`)

	if row.PromptToks != 2351 || row.OutputToks != 42 {
		t.Errorf("logged tokens = (%d, %d), want (2351, 42)", row.PromptToks, row.OutputToks)
	}
	if row.CacheReadToks != 2048 || row.CacheWriteToks != 0 {
		t.Errorf("logged cache tokens = (%d, %d), want (2048, 0)", row.CacheReadToks, row.CacheWriteToks)
	}
	if row.CredentialOwner != requestlog.CredentialOwnerPlatform {
		t.Errorf("credential owner = %q, want %q", row.CredentialOwner, requestlog.CredentialOwnerPlatform)
	}
}

func TestBufferedResponsesLogsCacheTokens(t *testing.T) {
	h := NewHandler(&fakeRouter{respResp: &schemas.BifrostResponsesResponse{
		Usage: &schemas.ResponsesResponseUsage{
			InputTokens:  900,
			OutputTokens: 12,
			TotalTokens:  912,
			InputTokensDetails: &schemas.ResponsesResponseInputTokens{
				CachedReadTokens:  640,
				CachedWriteTokens: 64,
			},
		},
	}}, nil, nil)

	row := loggedRequest(t, h, "/llm/v1/responses", `{"model":"openai/gpt-4o","input":"hi"}`)

	if row.CacheReadToks != 640 || row.CacheWriteToks != 64 {
		t.Errorf("logged cache tokens = (%d, %d), want (640, 64)", row.CacheReadToks, row.CacheWriteToks)
	}
}

func TestCacheTokenExtractorsTolerateMissingDetails(t *testing.T) {
	if _, _, ok := cacheTokensFromLLMUsage(nil); ok {
		t.Error("nil chat usage reported cache tokens")
	}
	if _, _, ok := cacheTokensFromLLMUsage(&schemas.BifrostLLMUsage{PromptTokens: 3}); ok {
		t.Error("chat usage without prompt details reported cache tokens")
	}
	if _, _, ok := cacheTokensFromResponsesUsage(&schemas.ResponsesResponseUsage{InputTokens: 3}); ok {
		t.Error("responses usage without input details reported cache tokens")
	}
	if _, _, ok := cacheTokensFromChunk(nil); ok {
		t.Error("a nil chunk reported cache tokens")
	}
}
