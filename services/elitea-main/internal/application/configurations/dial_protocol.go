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
	// ErrDialProtocolAnthropicIsClaudeOnly refuses the anthropic protocol on
	// an OpenAI-family model. The gateway then sends every request to
	// /anthropic/v1/messages with x-api-key, which DIAL cannot serve for a gpt
	// deployment. It is the same failure as the openai protocol on Claude.
	ErrDialProtocolAnthropicIsClaudeOnly = errors.New(
		"data.dial_protocol anthropic serves Claude models only; use azure or openai for a gpt model")
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
	case DialProtocolAzure:
	case DialProtocolAnthropic:
		if name, _ := data["name"].(string); looksLikeOpenAIModel(name) {
			return "", false, ErrDialProtocolAnthropicIsClaudeOnly
		}
	case DialProtocolOpenAI:
		if name, _ := data["name"].(string); looksLikeClaudeModel(name) {
			return "", false, ErrDialProtocolOpenAIIsGPTOnly
		}
	default:
		return "", false, ErrInvalidDialProtocol
	}
	return value, true, nil
}

// looksLikeOpenAIModel reports whether a provider model name names an OpenAI
// model. It is the gateway's own test for the family (bifrost
// schemas.IsOpenAIModel), case-folded: "gpt-", "text-embedding-", or an
// o-series id such as "o3" or "o4-mini" after any "provider/" prefix.
//
// The two family tests are deliberately narrow. A name that matches neither
// (a Gemini deployment, a custom alias) is not refused for either protocol:
// the write path cannot prove what the deployment serves, and the field
// description says which family each protocol carries.
func looksLikeOpenAIModel(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(lower, "gpt-") || strings.Contains(lower, "text-embedding-") {
		return true
	}
	if i := strings.LastIndexAny(lower, "/:"); i >= 0 {
		lower = lower[i+1:]
	}
	if len(lower) < 2 || lower[0] != 'o' || lower[1] < '0' || lower[1] > '9' {
		return false
	}
	return len(lower) == 2 || lower[2] == '-'
}

// looksLikeClaudeModel reports whether a provider model name names an
// Anthropic model. It is the gateway's own test for the family
// (bifrost schemas.IsAnthropicModel), case-folded.
func looksLikeClaudeModel(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic.")
}
