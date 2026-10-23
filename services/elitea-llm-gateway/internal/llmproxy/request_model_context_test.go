// request_model_context_test.go — proves the /llm handler publishes the model
// it dispatches on the request context (issue #455).
//
// The account builds the Azure api-version alias from that value, and bifrost
// resolves the alias by the model name. If the handler never writes it, the
// account's alias is never built and the api-version silently reverts to
// bifrost's own default — a defect the account's own tests cannot see, because
// they set the context value themselves.
package llmproxy

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// modelCtxSpy records the value of account.ContextKeyRequestModel on the
// context each dispatch carried.
type modelCtxSpy struct {
	dispatchSpy
	seen []string
}

func (s *modelCtxSpy) capture(ctx *schemas.BifrostContext) {
	v, _ := ctx.Value(account.ContextKeyRequestModel).(string)
	s.seen = append(s.seen, v)
}

func (s *modelCtxSpy) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	s.capture(ctx)
	return s.dispatchSpy.ChatCompletionRequest(ctx, req)
}

func (s *modelCtxSpy) ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	s.capture(ctx)
	return s.dispatchSpy.ResponsesRequest(ctx, req)
}

var _ LLMRouter = (*modelCtxSpy)(nil)

// TestHandlerPublishesTheDispatchedModel asserts the context the router receives
// carries the PROVIDER's model name, not the caller's advertised title. The
// alias the account builds is keyed by the name bifrost later resolves, so the
// mapped name is the only correct one.
func TestHandlerPublishesTheDispatchedModel(t *testing.T) {
	spy := &modelCtxSpy{dispatchSpy: *newDispatchSpy()}
	resolver := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{rows: modelMapRows()}})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	for _, tc := range []struct {
		name, path, id, want string
	}{
		{"chat", "/llm/v1/chat/completions", "Prod GPT", "gpt-5.1"},
		{"responses", "/llm/v1/responses", "openai/gpt-4o", "gpt-4o-2024-11-20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy.seen = nil
			var body string
			if tc.path == "/llm/v1/responses" {
				body = fmt.Sprintf(`{"model":%q,"input":"hi"}`, tc.id)
			} else {
				body = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tc.id)
			}
			rec := postAs(t, h, tc.path, mapProjectID, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if len(spy.seen) != 1 {
				t.Fatalf("dispatches = %d, want 1", len(spy.seen))
			}
			if spy.seen[0] != tc.want {
				t.Fatalf("context model = %q, want the provider's own name %q "+
					"(the Azure api-version alias is keyed by it)", spy.seen[0], tc.want)
			}
		})
	}
}

// TestHandlerPublishesTheModelWhenTheModelSetIsUnknown covers the degraded path.
// When the project's model set cannot be read the caller's model is forwarded
// unchanged, and the context must still name it — otherwise a database blip
// would also silently drop every tenant's api-version.
func TestHandlerPublishesTheModelWhenTheModelSetIsUnknown(t *testing.T) {
	spy := &modelCtxSpy{dispatchSpy: *newDispatchSpy()}
	h := NewHandler(spy, nil, nil).route() // no model resolver at all

	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(spy.seen) != 1 || spy.seen[0] != "gpt-4o" {
		t.Fatalf("context model = %v, want [gpt-4o]", spy.seen)
	}
}
