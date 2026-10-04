package configurations

import (
	"errors"
	"strings"
)

// dial_protocol.go — the per-model AI DIAL protocol (legacy issue #6707).
//
// One AI DIAL key serves three upstream protocol families: the Azure-shaped
// route (the default), the OpenAI Responses API, and the native Anthropic
// Messages API. The choice belongs to the MODEL, because one key serves gpt,
// Claude and Gemini models together. So the field lives on the llm_model row as
// `data.dial_protocol`, and the LLM gateway reads it when the linked credential
// is an ai_dial credential (services/elitea-llm-gateway/internal/account/
// dial_protocol.go). Every other credential type ignores it.
//
// The default writes nothing. A row with no field and a row that says "azure"
// dispatch identically, so an existing model changes in no way.

// DialProtocolField is the llm_model `data` key.
const DialProtocolField = "dial_protocol"

// The three protocols. The values are the gateway's, byte for byte.
const (
	DialProtocolAzure     = "azure"
	DialProtocolOpenAI    = "openai"
	DialProtocolAnthropic = "anthropic"
)

// DialProtocols lists the accepted values in a stable order, for a refusal
// message and for the registry schema's enum.
func DialProtocols() []string {
	return []string{DialProtocolAzure, DialProtocolOpenAI, DialProtocolAnthropic}
}

var (
	// ErrInvalidDialProtocol refuses a value that is not one of DialProtocols.
	ErrInvalidDialProtocol = errors.New(
		"data.dial_protocol must be one of: " + strings.Join(DialProtocols(), ", "))
	// ErrDialProtocolOpenAIIsGPTOnly refuses the openai protocol on a Claude
	// model. DIAL answers 503 for a Claude deployment on /openai/v1/responses
	// and 404 on /openai/v1/chat/completions, so the row would be stored as a
	// working model that no request can reach.
	ErrDialProtocolOpenAIIsGPTOnly = errors.New(
		"data.dial_protocol openai serves gpt models only; use anthropic for a Claude model")
)

// ValidateLLMModelDialProtocol checks `data.dial_protocol` of an llm_model.
// present is false when the field is absent or null; the caller then writes
// nothing, which is the default protocol.
func ValidateLLMModelDialProtocol(data map[string]any) (protocol string, present bool, err error) {
	raw, exists := data[DialProtocolField]
	if !exists || raw == nil {
		return "", false, nil
	}
	value, isString := raw.(string)
	if !isString {
		return "", false, ErrInvalidDialProtocol
	}
	switch value {
	case DialProtocolAzure, DialProtocolAnthropic:
	case DialProtocolOpenAI:
		if name, _ := data["name"].(string); looksLikeClaudeModel(name) {
			return "", false, ErrDialProtocolOpenAIIsGPTOnly
		}
	default:
		return "", false, ErrInvalidDialProtocol
	}
	return value, true, nil
}

// looksLikeClaudeModel reports whether a provider model name names an
// Anthropic model. It is the gateway's own test for the family
// (bifrost schemas.IsAnthropicModel), case-folded.
func looksLikeClaudeModel(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic.")
}
