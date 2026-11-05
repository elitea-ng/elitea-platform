package llmproxy

import (
	"encoding/json"

	"github.com/maximhq/bifrost/core/schemas"
)

// chat_template_kwargs.go — the thinking switch of a `toggle` model
// (ADR-0038 decision 2, #1452).
//
// A Qwen-family model on vLLM or SGLang has no reasoning_effort. The caller
// switches its thinking on or off with chat_template_kwargs.enable_thinking,
// and the llm-wire translation table sends exactly that form. The bifrost
// decode keeps only the fields it knows, so the key did not reach the
// provider: the model then thought on every step, whatever the setting.
//
// This file carries one value through, and only one. The vLLM and SGLang
// providers merge ExtraParams into the provider body; every other provider
// ignores ExtraParams, so a toggle form sent to a provider that has no
// template arguments changes nothing. Any other template argument is NOT
// forwarded: a template argument changes how the provider renders the
// prompt, and the gateway forwards only the one the platform sends.

const (
	chatTemplateKwargsField = "chat_template_kwargs"
	enableThinkingField     = "enable_thinking"
)

// forwardEnableThinking copies chat_template_kwargs.enable_thinking from the
// raw request body into the bifrost request's ExtraParams. A body without the
// key, or with a value that is not a boolean, changes nothing.
func forwardEnableThinking(raw []byte, req *schemas.BifrostChatRequest) {
	if req == nil || len(raw) == 0 {
		return
	}
	var body struct {
		Kwargs map[string]json.RawMessage `json:"chat_template_kwargs"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Kwargs == nil {
		return
	}
	value, present := body.Kwargs[enableThinkingField]
	if !present {
		return
	}
	var enabled bool
	if err := json.Unmarshal(value, &enabled); err != nil {
		return
	}
	if req.Params == nil {
		req.Params = &schemas.ChatParameters{}
	}
	if req.Params.ExtraParams == nil {
		req.Params.ExtraParams = map[string]interface{}{}
	}
	req.Params.ExtraParams[chatTemplateKwargsField] = map[string]interface{}{
		enableThinkingField: enabled,
	}
}
