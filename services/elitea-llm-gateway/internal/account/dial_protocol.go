// dial_protocol.go — the per-model AI DIAL protocol (legacy issue #6707).
//
// One AI DIAL key serves three upstream protocol families at the same time:
//
//	azure      /openai/...           Azure-shaped. The default, and the only
//	                                 shape the gateway used before this change.
//	openai     /openai/v1/responses  OpenAI Responses API. gpt models only:
//	                                 DIAL answers 503 for a Claude deployment.
//	anthropic  /anthropic/v1/messages Native Anthropic Messages. The only route
//	                                 that accepts `thinking` / `output_config`.
//
// The protocol is a property of the MODEL, not of the credential: one DIAL key
// legitimately serves gpt, Claude and Gemini models together. So the choice is
// stored on the llm_model row (`data.dial_protocol`), read by the model
// resolver, and carried to this package on the linked-credential pin.
//
// # How a protocol becomes a request
//
// bifrost's Azure provider already speaks all three routes. It picks the route
// from the model FAMILY: an Anthropic-family model goes to
// `{endpoint}/anthropic/v1/messages` with `x-api-key`, and every other model
// goes to `{endpoint}/openai/v1/...` with `api-key`. Without an override the
// family is guessed from a substring of the model name. A per-key alias
// (schemas.AliasConfig.ModelFamily) replaces the guess with a statement, and it
// is the same alias mechanism the api-version already uses (issue #455).
//
//   - azure: no alias family. The request is byte-identical to the request
//     before this change.
//   - anthropic: ModelFamily = anthropic, so every chat, responses and messages
//     request goes to the native Messages route, whatever the name says.
//   - openai: ModelFamily = openai, and the /llm handler converts a chat
//     completion into a Responses request (DIAL does not serve
//     /openai/v1/chat/completions). See llmproxy/modelmap.go.
//
// Neither alias carries the credential's api-version. Both routes are
// versionless: the Anthropic route rejects the parameter, and the verified
// OpenAI profile sends none.

package account

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// DialProtocol is the stored `data.dial_protocol` value of an llm_model row.
type DialProtocol string

const (
	// DialProtocolAzure is the default. It changes nothing.
	DialProtocolAzure DialProtocol = "azure"
	// DialProtocolOpenAI routes the model through DIAL's OpenAI Responses API.
	DialProtocolOpenAI DialProtocol = "openai"
	// DialProtocolAnthropic routes the model through DIAL's native Anthropic
	// Messages passthrough.
	DialProtocolAnthropic DialProtocol = "anthropic"
)

// DialCredentialType is the only credential type the protocol applies to. Any
// other credential type ignores the field.
const DialCredentialType = "ai_dial"

// ParseDialProtocol reads a stored protocol value. The empty value is the
// default. The second result is false for a value this gateway does not know;
// the first result is then the default, so an unknown value keeps the request
// exactly as it was before the field existed.
func ParseDialProtocol(raw string) (DialProtocol, bool) {
	switch DialProtocol(strings.ToLower(strings.TrimSpace(raw))) {
	case "", DialProtocolAzure:
		return DialProtocolAzure, true
	case DialProtocolOpenAI:
		return DialProtocolOpenAI, true
	case DialProtocolAnthropic:
		return DialProtocolAnthropic, true
	default:
		return DialProtocolAzure, false
	}
}

// modelFamily returns the bifrost model family a protocol forces. The second
// result is false for the default protocol, which forces nothing.
func (p DialProtocol) modelFamily() (schemas.ModelFamily, bool) {
	switch p {
	case DialProtocolOpenAI:
		return schemas.ModelFamilyOpenAI, true
	case DialProtocolAnthropic:
		return schemas.ModelFamilyAnthropic, true
	default:
		return "", false
	}
}

// ConvertsChatToResponses reports whether a chat completion for a model on
// this protocol must be sent as a Responses request. DIAL serves the OpenAI
// family on /openai/v1/responses only.
func (p DialProtocol) ConvertsChatToResponses() bool {
	return p == DialProtocolOpenAI
}
