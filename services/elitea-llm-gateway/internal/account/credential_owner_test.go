package account

import (
	"context"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// The request log classifies the credential that served a request (legacy
// issue 6709) by reading the key bifrost/core SELECTED off the request's
// context. These tests run a real bifrost/core over the real account, because
// the classification depends on bifrost writing that id onto the context the
// gateway passed in. A test that set the value by hand would pass even if
// bifrost stopped writing it.

func chatThroughCore(t *testing.T, rows map[string][][]any, upstreamURL string) (*schemas.BifrostContext, *schemas.BifrostError) {
	t.Helper()
	acct := newChatTestAccount(t, upstreamURL, rows)
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: acct, InitialPoolSize: 1})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(core.Shutdown)

	ctx := schemas.NewBifrostContext(ctxWithProject(callerProject), schemas.NoDeadline)
	_, bErr := core.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{
		Provider: schemas.VLLM,
		Model:    "platform-gpt",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")},
		}},
	})
	return ctx, bErr
}

func TestSelectedCredentialOwner_ASharedCredentialIsThePlatforms(t *testing.T) {
	upstream := newRecordingProvider(t)
	defer upstream.Close()

	ctx, bErr := chatThroughCore(t, map[string][][]any{
		callerProject: {},
		publicProject: {vllmRow("pub-1", "platform-vllm", "SHARED-KEY", upstream.URL, true)},
	}, upstream.URL)
	if bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if got := SelectedCredentialOwner(ctx); got != requestlog.CredentialOwnerPlatform {
		t.Fatalf("owner = %q, want %q", got, requestlog.CredentialOwnerPlatform)
	}
}

func TestSelectedCredentialOwner_TheCallersOwnCredentialIsTheProjects(t *testing.T) {
	upstream := newRecordingProvider(t)
	defer upstream.Close()

	ctx, bErr := chatThroughCore(t, map[string][][]any{
		callerProject: {vllmRow("own-1", "own-vllm", "OWN-KEY", upstream.URL, false)},
		publicProject: {},
	}, upstream.URL)
	if bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if got := upstream.lastAuth(); got != "Bearer OWN-KEY" {
		t.Fatalf("upstream Authorization = %q, want the project's own key", got)
	}
	if got := SelectedCredentialOwner(ctx); got != requestlog.CredentialOwnerProject {
		t.Fatalf("owner = %q, want %q", got, requestlog.CredentialOwnerProject)
	}
}

// A request no credential served classifies as unknown, not as either owner.
func TestSelectedCredentialOwner_NoSelectedKeyIsUnknown(t *testing.T) {
	if got := SelectedCredentialOwner(context.Background()); got != "" {
		t.Fatalf("owner = %q for a context with no selected key, want empty", got)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, "")
	if got := SelectedCredentialOwner(ctx); got != "" {
		t.Fatalf("owner = %q for a cleared selected key, want empty", got)
	}
}
