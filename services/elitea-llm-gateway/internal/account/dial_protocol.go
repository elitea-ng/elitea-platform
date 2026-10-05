// dial_protocol.go — the per-model AI DIAL protocol (legacy issue #6707).
//
// One AI DIAL key serves three upstream protocol families at the same time.
// The routes below were measured on a real DIAL deployment (the issue body):
//
//	azure      /openai/deployments/{model}/chat/completions?api-version=...
//	           Azure-deployment-shaped. The default. DIAL serves it for every
//	           model family, but it refuses `thinking` for Claude models.
//	openai     /openai/v1/responses  OpenAI Responses API. gpt models only:
//	           DIAL answers 503 for a Claude or Gemini deployment.
//	anthropic  /anthropic/v1/messages  Native Anthropic Messages. Claude models
//	           only. The only route that accepts `thinking` / `output_config`.
//
// DIAL does NOT serve /openai/v1/chat/completions (404 "Route is not found"),
// although bifrost's Azure provider builds exactly that URL for every
// non-Anthropic chat. The default protocol therefore needs the deployment
// route below; without it every gpt or Gemini chat on an ai_dial credential
// fails.
//
// The protocol is a property of the MODEL, not of the credential: one DIAL key
// legitimately serves gpt, Claude and Gemini models together. So the choice is
// stored on the llm_model row (`data.dial_protocol`), read by the model
// resolver, and carried to this package on the linked-credential pin. It
// applies only when the credential ROW is an ai_dial row (credential.configType);
// what the model row says about the link type does not decide it.
//
// # How a protocol becomes a request
//
// bifrost's Azure provider picks the route from the model FAMILY: an
// Anthropic-family model goes to `{endpoint}/anthropic/v1/messages` with
// `x-api-key`, and every other model goes to `{endpoint}/openai/v1/...` with
// `api-key`. Without an override the family is guessed from a substring of
// the model name. A per-key alias (schemas.AliasConfig) replaces the guess with
// a statement, and it is the same alias mechanism the api-version already uses
// (issue #455).
//
//   - azure: a model whose name bifrost reads as Claude keeps the guessed
//     Messages route, exactly as before. Every other model gets the deployment
//     route for a chat completion and for an embedding (dialDeploymentAlias).
//   - anthropic: ModelFamily = anthropic, so every chat, responses and messages
//     request goes to the native Messages route, whatever the name says.
//   - openai: ModelFamily = openai, and the /llm handler converts a chat
//     completion into a Responses request. See llmproxy/modelmap.go.
//
// Neither forced alias carries the credential's api-version. Both routes are
// versionless: the Anthropic route rejects the parameter, and the verified
// OpenAI profile sends none.
//
// The model probe (llmproxy/checkconnection_model.go) asks DialRouteFor for
// the route, so Test connection reaches the route a real request reaches.

package account

import (
	"net/url"
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

// DialRoute names the DIAL route a chat completion reaches.
type DialRoute string

const (
	// DialRouteDeployment is /openai/deployments/{model}/chat/completions.
	DialRouteDeployment DialRoute = "deployment"
	// DialRouteResponses is /openai/v1/responses.
	DialRouteResponses DialRoute = "responses"
	// DialRouteMessages is /anthropic/v1/messages.
	DialRouteMessages DialRoute = "messages"
)

// DialRouteFor returns the route a chat completion for model takes on
// protocol p. The default protocol keeps bifrost's own family guess for a
// Claude name (schemas.IsAnthropicModel, the same test core applies), so a
// Claude model that worked before keeps its route.
func DialRouteFor(p DialProtocol, model string) DialRoute {
	switch p {
	case DialProtocolOpenAI:
		return DialRouteResponses
	case DialProtocolAnthropic:
		return DialRouteMessages
	}
	if schemas.IsAnthropicModel(model) {
		return DialRouteMessages
	}
	return DialRouteDeployment
}

// DialDefaultAPIVersion is the api-version the deployment route carries when
// the credential names none. It is a GA version that knows
// max_completion_tokens, which the reasoning deployments require.
const DialDefaultAPIVersion = "2024-10-21"

// DialDeploymentURL builds {apiBase}/openai/deployments/{model}/{operation}
// with the api-version query. operation is "chat/completions" or "embeddings".
// The runtime and the model probe both build the URL here.
func DialDeploymentURL(apiBase, model, operation, apiVersion string) string {
	apiVersion = strings.TrimSpace(apiVersion)
	if apiVersion == "" {
		apiVersion = DialDefaultAPIVersion
	}
	return strings.TrimRight(strings.TrimSpace(apiBase), "/") +
		"/openai/deployments/" + url.PathEscape(model) + "/" + operation +
		"?api-version=" + url.QueryEscape(apiVersion)
}

// DispatchKind is the operation the /llm handler dispatches. The account
// needs it because the DIAL deployment route differs per operation, and core
// resolves the key before it calls the provider.
type DispatchKind string

const (
	// DispatchChat is a chat completion, streamed or not.
	DispatchChat DispatchKind = "chat"
	// DispatchEmbedding is an embedding request.
	DispatchEmbedding DispatchKind = "embedding"
)

// ContextKeyDispatchKind carries the DispatchKind of the request. A request
// without it (the Responses API, text completion, audio) keeps the key the
// gateway built before the deployment route existed.
const ContextKeyDispatchKind schemas.BifrostContextKey = "elitea-dispatch-kind"

// dialDeploymentAlias points one model at DIAL's deployment route.
//
// bifrost v1.7.15 has no per-key path override for the Azure provider: it
// always appends /openai/v1/chat/completions (or /openai/v1/embeddings) to the
// endpoint. The alias endpoint is therefore the WHOLE deployment URL followed
// by "#". The suffix bifrost appends becomes the URL fragment, which an HTTP
// client never sends, so the wire request is exactly the deployment route.
// TestDialDefaultProtocolUsesTheDeploymentRoute pins the wire request against
// the real core, so a bifrost change to the URL build fails that test.
//
// The family is forced to openai so that the route cannot depend on a name
// guess, and the api-version travels in the URL, which is where the route
// reads it.
func dialDeploymentAlias(c credential, model string, kind DispatchKind) (schemas.AliasConfig, bool) {
	operation := ""
	switch kind {
	case DispatchChat:
		operation = "chat/completions"
	case DispatchEmbedding:
		operation = "embeddings"
	default:
		return schemas.AliasConfig{}, false
	}
	endpoint := DialDeploymentURL(c.apiBase, model, operation, c.apiVersion) + "#"
	return schemas.AliasConfig{
		ModelID:       model,
		ModelFamily:   schemas.Ptr(schemas.ModelFamilyOpenAI),
		AzureAliasCfg: &schemas.AzureAliasCfg{Endpoint: schemas.Ptr(plainSecret(endpoint))},
	}, true
}
