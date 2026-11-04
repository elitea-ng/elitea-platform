package llmproxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
)

func chatRequestFrom(t *testing.T, body string) *schemas.BifrostChatRequest {
	t.Helper()
	var req openai.OpenAIChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	out := req.ToBifrostChatRequest(ctx)
	forwardEnableThinking([]byte(body), out)
	return out
}

func TestForwardEnableThinkingCarriesTheSwitch(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		body := `{"model":"vllm/qwen","messages":[{"role":"user","content":"hi"}],` +
			`"chat_template_kwargs":{"enable_thinking":` + map[bool]string{true: "true", false: "false"}[enabled] + `}}`
		req := chatRequestFrom(t, body)
		kwargs, ok := req.Params.ExtraParams[chatTemplateKwargsField].(map[string]interface{})
		if !ok {
			t.Fatalf("enable_thinking=%v: chat_template_kwargs not forwarded: %#v", enabled, req.Params.ExtraParams)
		}
		if kwargs[enableThinkingField] != enabled {
			t.Fatalf("enable_thinking=%v: got %#v", enabled, kwargs)
		}
	}
}

func TestForwardEnableThinkingForwardsNoOtherTemplateArgument(t *testing.T) {
	req := chatRequestFrom(t, `{"model":"vllm/qwen","messages":[{"role":"user","content":"hi"}],`+
		`"chat_template_kwargs":{"enable_thinking":true,"add_generation_prompt":false}}`)
	kwargs := req.Params.ExtraParams[chatTemplateKwargsField].(map[string]interface{})
	if len(kwargs) != 1 {
		t.Fatalf("only enable_thinking may be forwarded, got %#v", kwargs)
	}
}

func TestForwardEnableThinkingIgnoresAbsentOrMalformedValues(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"chat_template_kwargs":{}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"chat_template_kwargs":{"enable_thinking":"yes"}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"chat_template_kwargs":"x"}`,
	} {
		req := chatRequestFrom(t, body)
		if req.Params != nil {
			if _, ok := req.Params.ExtraParams[chatTemplateKwargsField]; ok {
				t.Fatalf("%s: forwarded a value it must not: %#v", body, req.Params.ExtraParams)
			}
		}
	}
}
