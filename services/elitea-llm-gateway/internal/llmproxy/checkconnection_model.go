package llmproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// checkconnection_model.go is the MODEL half of POST /llm/v1/check_connection
// (issue 6793 of the legacy tracker: "Test connection in the LLM model form").
//
// The credential half (checkconnection.go) answers "does this key
// authenticate?" with a read-only listing. That answer is true for a wrong
// model name and for a wrong DIAL API protocol, so an admin learns about either
// mistake only at the first chat message. When the request names a `model`,
// this file answers the question the model form asks instead: "does this model
// answer?". It sends ONE real completion request with a one-token output
// budget. That is the smallest request that proves the model name, the route
// and the protocol together.
//
// Each probe uses the route and the dialect the RUNTIME uses for the same
// credential. A probe on another route reports failures the runtime never
// sees, and successes the runtime cannot repeat.
//
// Every rule of the credential probe still holds:
//
//   - every host the probe contacts is named by dialTargets first and goes
//     through the operator's egress allowlist before any dial (issue 13);
//   - the dial uses the same address-validating, redirect-refusing client;
//   - no raw provider body crosses the wire. Detail carries, at most, the
//     provider's own one-line error message after scrubProviderMessage has
//     removed URLs, host names, IP addresses, key material and stack traces.

// checkModelProbeTimeout bounds the whole model probe. A completion is slower
// than a listing, and a cold self-hosted model can take seconds to load, so
// the bound is the 30 seconds the form promises ("Timed out" within about 30
// seconds), not the 8 seconds of the credential probe. It is a var so a test
// can shorten it.
var checkModelProbeTimeout = 30 * time.Second

// checkModelMaxNameLength bounds the model name. It is interpolated into a
// URL path for Azure, DIAL, Bedrock and Vertex.
const checkModelMaxNameLength = 256

// checkModelMaxErrorBody and checkModelMaxSuccessBody bound how much of a
// provider answer this process reads.
const (
	checkModelMaxErrorBody   = 16 << 10
	checkModelMaxSuccessBody = 64 << 10
)

// checkModelProbeKind is the value of checkConnectionResponse.Probe for a
// model probe. elitea-main requires it before it reports "Connected": a
// gateway that predates this file ignores `model` and answers the credential
// probe, and that answer is exactly the false positive the model test exists
// to remove.
const checkModelProbeKind = "completion"

// Reason vocabulary that only the model probe emits. The credential probe's
// unauthorized / unreachable / upstream_error / egress_not_allowed values keep
// their meaning.
const (
	checkConnectionReasonModelNotFound = "model_not_found"
	checkConnectionReasonProtocol      = "protocol_error"
	checkConnectionReasonTimeout       = "timeout"
	checkConnectionReasonRateLimited   = "rate_limited"
	checkConnectionReasonInvalidModel  = "invalid_model"
)

// checkModelMaxConcurrent bounds how many model probes this process runs at
// the same time. A model probe is a real, billed completion that holds a
// goroutine and an upstream connection for up to checkModelProbeTimeout. It
// does not go through the budget gate, so a caller must not be able to start
// an unbounded number of them. elitea-main also limits the rate per project
// and user; this bound protects the gateway itself.
const checkModelMaxConcurrent = 8

// modelProbeSlots holds one token for each model probe that runs now. It is a
// var so a test can replace it with a smaller channel.
var modelProbeSlots = make(chan struct{}, checkModelMaxConcurrent)

// defaultAzureCompletionAPIVersion is the Azure OpenAI api-version a model
// probe uses when the credential names none. It is a GA version that accepts
// max_completion_tokens, which the reasoning deployments require.
const defaultAzureCompletionAPIVersion = "2024-10-21"

// anthropicAPIVersion is the anthropic-version header value the Messages API
// requires. The vLLM-class probe sends it when use_anthropic_endpoints is set.
const anthropicAPIVersion = "2023-06-01"

// checkModelPrompt is the whole user message of a probe.
const checkModelPrompt = "ping"

// modelCompletionProbe is one credential type's model probe.
type modelCompletionProbe struct {
	dialTargets func(req checkConnectionRequest) ([]string, error)
	run         func(ctx context.Context, client *http.Client, req checkConnectionRequest) error
}

// modelCompletionProbes holds a model probe for every type the credential
// probe covers. A type absent here answers unsupported_type.
var modelCompletionProbes = map[string]modelCompletionProbe{
	"open_ai":        {dialTargets: checkConnectionAPIBaseTargets, run: probeOpenAIChatCompletion},
	"vllm":           {dialTargets: checkConnectionAPIBaseTargets, run: probeOpenAIChatCompletion},
	"azure_open_ai":  {dialTargets: checkConnectionAPIBaseTargets, run: probeAzureChatCompletion},
	"open_ai_azure":  {dialTargets: checkConnectionAPIBaseTargets, run: probeAzureChatCompletion},
	"ai_dial":        {dialTargets: checkConnectionAPIBaseTargets, run: probeDialCompletion},
	"ollama":         {dialTargets: checkConnectionAPIBaseTargets, run: probeOllamaChat},
	"amazon_bedrock": {dialTargets: checkConnectionBedrockRuntimeTargets, run: probeBedrockConverse},
	"vertex_ai":      {dialTargets: checkConnectionVertexTargets, run: probeVertexGenerateContent},
}

// modelProbeError is a model probe that reached the provider and got an
// answer that is not a completion. message is the provider's own error text,
// NOT yet scrubbed: the handler scrubs it with the request's secrets in hand.
type modelProbeError struct {
	status  int
	message string
	// notCompletion marks a 2xx answer whose body is not a completion of the
	// expected protocol: a web page, a listing, another API's JSON.
	notCompletion bool
}

func (e *modelProbeError) Error() string {
	if e.notCompletion {
		return fmt.Sprintf("check_connection: status %d answer is not a completion", e.status)
	}
	return fmt.Sprintf("check_connection: provider returned status %d", e.status)
}

// checkModelConnection is CheckConnection for a request that names a model.
// The signature and the body bound are already checked by the caller.
func (h *Handler) checkModelConnection(w http.ResponseWriter, r *http.Request, req checkConnectionRequest) {
	req.Model = strings.TrimSpace(req.Model)
	if reason, detail := validateModelProbeRequest(req); reason != "" {
		writeJSON(w, http.StatusOK, checkConnectionResponse{Success: false, Reason: reason, Detail: detail, Probe: checkModelProbeKind})
		return
	}

	probe, ok := modelCompletionProbes[req.Type]
	if !ok {
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonUnsupported, Probe: checkModelProbeKind,
		})
		return
	}
	targets, err := probe.dialTargets(req)
	if err != nil {
		var fe *checkConnectionFieldError
		if errors.As(err, &fe) {
			writeJSON(w, http.StatusOK, checkConnectionResponse{
				Success: false, Reason: fe.reason, Detail: fe.detail, Probe: checkModelProbeKind,
			})
			return
		}
		h.logger.WarnContext(r.Context(), "check_connection: could not resolve a model probe target",
			"type", req.Type, "err", err)
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonUnreachable,
			Detail: "could not reach the provider", Probe: checkModelProbeKind,
		})
		return
	}
	if !h.egressAllowsAll(r.Context(), req.Type, targets) {
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonEgress, Probe: checkModelProbeKind,
		})
		return
	}

	// The slot is taken after every refusal that costs nothing, and before
	// the dial. A full set of slots is a refusal, not a queue: a queued probe
	// would still hold this request open.
	slots := modelProbeSlots
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		h.logger.WarnContext(r.Context(), "check_connection: model probe refused, too many running",
			"type", req.Type, "limit", cap(slots))
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: checkConnectionReasonRateLimited,
			Detail: "too many model tests are running. Try again in a moment.", Probe: checkModelProbeKind,
		})
		return
	}

	client := newCheckConnectionProbeClientWithTimeout(h.probeAllowsPrivateNetwork(req), checkModelProbeTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), checkModelProbeTimeout)
	defer cancel()

	// The probe is a real completion that the budget gate does not see, so
	// every probe is logged with the signed project and user. That record is
	// what an audit of the probe spend reads.
	identity := identityFromHeaders(r.Header)
	started := time.Now()
	err = probe.run(ctx, client, req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		reason, detail := classifyModelProbeError(ctx, err)
		detail = scrubProviderMessage(detail, modelProbeSecrets(req))
		h.logger.WarnContext(r.Context(), "check_connection: model probe failed",
			"type", req.Type, "model", req.Model, "project_id", identity.projectID, "user_id", identity.userID,
			"reason", reason, "latency_ms", latency, "err", err)
		writeJSON(w, http.StatusOK, checkConnectionResponse{
			Success: false, Reason: reason, Detail: detail, Probe: checkModelProbeKind, LatencyMS: latency,
		})
		return
	}
	h.logger.InfoContext(r.Context(), "check_connection: model probe answered",
		"type", req.Type, "model", req.Model, "project_id", identity.projectID, "user_id", identity.userID,
		"latency_ms", latency)
	writeJSON(w, http.StatusOK, checkConnectionResponse{
		Success: true, Reason: checkConnectionReasonOK, Probe: checkModelProbeKind, LatencyMS: latency,
	})
}

// egressAllowsAll is the allowlist gate both probes apply: fail closed with no
// policy or no target, and refuse when any one target is not allowed.
func (h *Handler) egressAllowsAll(ctx context.Context, credentialType string, targets []string) bool {
	if h.egressPolicy == nil || len(targets) == 0 {
		h.logger.WarnContext(ctx, "check_connection: no egress policy or no gated target", "type", credentialType)
		return false
	}
	for _, target := range targets {
		if !h.egressPolicy.EgressAllows(target) {
			h.logger.WarnContext(ctx, "check_connection: probe host is not on the egress allowlist", "type", credentialType)
			return false
		}
	}
	return true
}

// validateModelProbeRequest refuses a model name that cannot be sent safely.
// The model name goes into a URL path for four of the types, so a control
// character or an over-long value is refused before any dial.
//
// A name made of dots only is refused too. url.PathEscape keeps "." and "..",
// so such a name becomes a dot-segment in the Azure, DIAL and Bedrock routes,
// and an upstream normalises {api_base}/openai/deployments/../chat/completions
// to a route other than the deployment route. The redeemed key would then go
// to a path that the egress review did not assume.
func validateModelProbeRequest(req checkConnectionRequest) (reason, detail string) {
	if req.Model == "" || utf8.RuneCountInString(req.Model) > checkModelMaxNameLength {
		return checkConnectionReasonInvalidModel, "the model name is empty or too long"
	}
	for _, r := range req.Model {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return checkConnectionReasonInvalidModel, "the model name contains a space or a control character"
		}
	}
	if strings.Trim(req.Model, ".") == "" {
		return checkConnectionReasonInvalidModel, "the model name cannot be only dots"
	}
	return "", ""
}

// classifyModelProbeError maps a probe failure onto the model reason
// vocabulary. The returned detail is the provider's message, unscrubbed.
func classifyModelProbeError(ctx context.Context, err error) (reason, detail string) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return checkConnectionReasonTimeout, ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return checkConnectionReasonTimeout, ""
	}
	var me *modelProbeError
	if errors.As(err, &me) {
		if me.notCompletion {
			return checkConnectionReasonProtocol, "the endpoint answered, but not with a model completion"
		}
		switch {
		case me.status == http.StatusUnauthorized || me.status == http.StatusForbidden:
			return checkConnectionReasonUnauth, me.message
		case me.status == http.StatusNotFound:
			return checkConnectionReasonModelNotFound, me.message
		case me.status == http.StatusTooManyRequests:
			return checkConnectionReasonRateLimited, me.message
		case me.status == http.StatusBadRequest || me.status == http.StatusUnprocessableEntity:
			if messageNamesMissingModel(me.message) {
				return checkConnectionReasonModelNotFound, me.message
			}
			return checkConnectionReasonProtocol, me.message
		case me.status == http.StatusMethodNotAllowed || me.status == http.StatusUnsupportedMediaType ||
			(me.status >= 300 && me.status < 400):
			return checkConnectionReasonProtocol, me.message
		case me.status >= 500:
			return checkConnectionReasonUpstream, me.message
		default:
			return checkConnectionReasonUpstream, me.message
		}
	}
	// A credential-probe error (signing, token exchange) keeps its own
	// classification: it names a credential problem, not a model problem.
	var pe *checkConnectionProbeError
	if errors.As(err, &pe) && pe.status != 0 {
		return classifyCheckConnectionProbeError(err)
	}
	return checkConnectionReasonUnreachable, ""
}

// missingModelPattern recognises the provider messages that say a model or a
// deployment does not exist, which several providers send with status 400.
var missingModelPattern = regexp.MustCompile(
	`(?i)(model|deployment)[^.]{0,80}(not found|does not exist|doesn't exist|not exist|unknown|invalid|not supported|unsupported|no such)` +
		`|(unknown|invalid|unsupported) (model|deployment)`)

func messageNamesMissingModel(message string) bool {
	return missingModelPattern.MatchString(message)
}

// modelProbeSecrets lists the request values that must never appear in a
// Detail, whatever the provider echoes back.
func modelProbeSecrets(req checkConnectionRequest) []string {
	return []string{req.APIKey, req.AWSAccessKeyID, req.AWSSecretAccessKey, req.AWSSessionToken, string(req.VertexCredentials)}
}

// --- provider message scrubbing ---------------------------------------------

const scrubbedMessageMaxRunes = 300

var (
	scrubURLPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"'<>]+`)
	// IPv4 with an optional port.
	scrubIPv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`)
	// IPv6: a compressed form ("fd00::1") or at least four full groups, so a
	// clock time ("12:30:45") stays.
	scrubIPv6Pattern = regexp.MustCompile(
		`(?i)\[?(?:(?:[0-9a-f]{1,4}:)*[0-9a-f]{0,4}::(?:[0-9a-f]{1,4}:)*[0-9a-f]{0,4}|(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{1,4})\]?(?::\d{1,5})?`)
	// A DNS name: two or more labels, the last one alphabetic. A model name
	// such as "gpt-4.1" or "claude-3.5-sonnet" has a numeric last label and
	// stays.
	scrubHostPattern = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}\b(?::\d{1,5})?`)
	// Key-shaped values: a long run of key characters, with or without a
	// common prefix.
	scrubTokenPattern = regexp.MustCompile(`(?i)\b(?:bearer\s+)?(?:sk-|key-|pk-)?[a-z0-9_\-]{32,}\b`)
)

// scrubProviderMessage returns the part of a provider error message that may
// be shown to a project admin: the first line, without URLs, host names, IP
// addresses, key-shaped strings or any of the request's own secrets, at most
// scrubbedMessageMaxRunes long.
func scrubProviderMessage(message string, secrets []string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	// The first line only: a stack trace starts on the second.
	if index := strings.IndexAny(message, "\r\n"); index >= 0 {
		message = message[:index]
	}
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); len(secret) >= 4 {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = scrubURLPattern.ReplaceAllString(message, "[url]")
	message = scrubIPv4Pattern.ReplaceAllString(message, "[address]")
	message = scrubIPv6Pattern.ReplaceAllString(message, "[address]")
	message = scrubHostPattern.ReplaceAllString(message, "[host]")
	message = scrubTokenPattern.ReplaceAllString(message, "[redacted]")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > scrubbedMessageMaxRunes {
		runes := []rune(message)
		message = string(runes[:scrubbedMessageMaxRunes]) + "…"
	}
	return message
}

// providerErrorMessage reads the error text out of the common provider error
// shapes. A body that is not JSON yields "": an HTML error page is not a
// message, and it is the shape most likely to carry an internal host name.
func providerErrorMessage(body []byte) string {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return ""
	}
	if list, ok := document.([]any); ok && len(list) > 0 {
		document = list[0]
	}
	object, ok := document.(map[string]any)
	if !ok {
		return ""
	}
	if nested, ok := object["error"].(map[string]any); ok {
		if message, ok := nested["message"].(string); ok {
			return message
		}
	}
	for _, key := range []string{"error", "message", "Message", "detail", "error_message"} {
		if message, ok := object[key].(string); ok {
			return message
		}
	}
	return ""
}

// --- the probes ---------------------------------------------------------------

// probeOpenAIChatCompletion probes an open_ai or vllm credential on the route
// the runtime uses for it (account.ProviderForCredential):
//
//   - open_ai on OpenAI's own origin: Bifrost's OpenAI provider, which posts to
//     {origin}/v1/chat/completions. The probe sends max_completion_tokens,
//     which OpenAI's reasoning models require.
//   - vllm, and open_ai on any other origin: Bifrost's vLLM provider. The
//     runtime removes one trailing /v1 from api_base and Bifrost adds /v1 back
//     (account.bifrostVLLMBaseURL), so an api_base with or without /v1 works.
//     The probe builds its URL the same way. It sends max_tokens, which every
//     OpenAI-compatible server accepts.
//   - a vLLM-class credential with use_anthropic_endpoints: the runtime speaks
//     the Anthropic dialect to {root}/v1/messages with Bearer auth, so the
//     probe does too.
//
// A reasoning model behind a compatible endpoint can refuse max_tokens, and an
// older server can refuse max_completion_tokens. Either refusal is a 400 that
// names the field. The probe then sends the request again, once, with the
// other field (postCompletionWithTokenField).
func probeOpenAIChatCompletion(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	headers := map[string]string{}
	if req.APIKey != "" {
		headers["Authorization"] = "Bearer " + req.APIKey
	}
	messages := []map[string]string{{"role": "user", "content": checkModelPrompt}}
	root := openAICompatibleRootURL(req.APIBase)
	if req.Type != "vllm" && isOpenAIAPIOrigin(req.APIBase) {
		body := map[string]any{"model": req.Model, "messages": messages}
		return postCompletionWithTokenField(ctx, client, root+"/v1/chat/completions",
			headers, body, "max_completion_tokens", "max_tokens", "choices")
	}
	if req.UseAnthropicEndpoints {
		headers["anthropic-version"] = anthropicAPIVersion
		body := map[string]any{"model": req.Model, "messages": messages, "max_tokens": 1}
		return modelProbePOST(ctx, client, root+"/v1/messages", headers, body, "content")
	}
	body := map[string]any{"model": req.Model, "messages": messages}
	return postCompletionWithTokenField(ctx, client, root+"/v1/chat/completions",
		headers, body, "max_tokens", "max_completion_tokens", "choices")
}

// openAICompatibleRootURL is account.bifrostVLLMBaseURL: api_base without
// trailing slashes and without one trailing /v1. The runtime hands this root
// to Bifrost, and Bifrost adds /v1 for every operation.
func openAICompatibleRootURL(apiBase string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return base[:len(base)-len("/v1")]
	}
	return base
}

// postCompletionWithTokenField sends a completion with a one-token budget in
// the field named first. When the provider refuses that field with a 400 that
// names it, the request is sent once more with the field named second.
//
// OpenAI's reasoning models answer max_tokens with "Unsupported parameter:
// 'max_tokens' is not supported with this model. Use 'max_completion_tokens'
// instead." Older OpenAI-compatible servers refuse max_completion_tokens in
// the same way. Neither answer says anything about the model or the route, so
// neither is a verdict.
func postCompletionWithTokenField(
	ctx context.Context, client *http.Client, rawURL string, headers map[string]string,
	body map[string]any, first, second, expectKey string,
) error {
	body[first] = 1
	err := modelProbePOST(ctx, client, rawURL, headers, body, expectKey)
	if !refusesTokenField(err, first) {
		return err
	}
	delete(body, first)
	body[second] = 1
	return modelProbePOST(ctx, client, rawURL, headers, body, expectKey)
}

// refusesTokenField reports whether err is a 400 whose message names field.
func refusesTokenField(err error, field string) bool {
	var me *modelProbeError
	if !errors.As(err, &me) || me.notCompletion || me.status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(me.message), field)
}

// probeAzureChatCompletion sends POST
// {api_base}/openai/deployments/{model}/chat/completions?api-version=...
func probeAzureChatCompletion(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	apiVersion := strings.TrimSpace(req.APIVersion)
	if apiVersion == "" {
		apiVersion = defaultAzureCompletionAPIVersion
	}
	body := map[string]any{
		"messages": []map[string]string{{"role": "user", "content": checkModelPrompt}},
	}
	first, second := "max_tokens", "max_completion_tokens"
	if azureAcceptsMaxCompletionTokens(apiVersion) {
		first, second = second, first
	}
	return postCompletionWithTokenField(ctx, client, azureDeploymentCompletionURL(req.APIBase, req.Model, apiVersion),
		apiKeyHeader(req.APIKey), body, first, second, "choices")
}

// probeDialCompletion sends a DIAL model probe to the route the runtime sends
// the same model to (legacy issue #6707). account.DialRouteFor decides the
// route for both, from the model's dial_protocol and its name:
//
//   - deployment: {api_base}/openai/deployments/{model}/chat/completions with
//     the api-version and the api-key header. It sends max_tokens, because
//     DIAL forwards the request to adapters that predate
//     max_completion_tokens, and it falls back to max_completion_tokens when a
//     model refuses max_tokens.
//   - responses (the openai protocol): {api_base}/openai/v1/responses with the
//     api-key header and no api-version. DIAL answers 503 there for a model
//     that is not gpt, so the probe reports that model as failing, as a chat
//     would.
//   - messages (the anthropic protocol, or a Claude name on the default):
//     {api_base}/anthropic/v1/messages with the x-api-key header and no
//     api-version.
//
// An unknown protocol value is read as the default, as the runtime reads it.
func probeDialCompletion(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	protocol, _ := account.ParseDialProtocol(req.DialProtocol)
	messages := []map[string]string{{"role": "user", "content": checkModelPrompt}}
	base := strings.TrimRight(strings.TrimSpace(req.APIBase), "/")
	switch account.DialRouteFor(protocol, req.Model) {
	case account.DialRouteResponses:
		headers := apiKeyHeader(req.APIKey)
		// 16 is the smallest max_output_tokens the Responses API accepts.
		body := map[string]any{"model": req.Model, "input": checkModelPrompt, "max_output_tokens": 16}
		return modelProbePOST(ctx, client, base+"/openai/v1/responses", headers, body, "output")
	case account.DialRouteMessages:
		headers := map[string]string{"anthropic-version": anthropicAPIVersion}
		if req.APIKey != "" {
			headers["x-api-key"] = req.APIKey
		}
		body := map[string]any{"model": req.Model, "messages": messages, "max_tokens": 1}
		return modelProbePOST(ctx, client, base+"/anthropic/v1/messages", headers, body, "content")
	default:
		body := map[string]any{"messages": messages}
		return postCompletionWithTokenField(ctx, client,
			account.DialDeploymentURL(req.APIBase, req.Model, "chat/completions", req.APIVersion),
			apiKeyHeader(req.APIKey), body, "max_tokens", "max_completion_tokens", "choices")
	}
}

// probeOllamaChat sends Ollama's native POST {api_base}/api/chat with a
// one-token prediction budget. The OpenAI-compatible route of Ollama ignores
// max_completion_tokens, so the native route is the one that stays short.
func probeOllamaChat(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	body := map[string]any{
		"model":    req.Model,
		"messages": []map[string]string{{"role": "user", "content": checkModelPrompt}},
		"stream":   false,
		"options":  map[string]any{"num_predict": 1},
	}
	return modelProbePOST(ctx, client, checkConnectionJoinURL(req.APIBase, "/api/chat"), nil, body, "message")
}

// checkConnectionBedrockRuntimeTargets names the Bedrock RUNTIME host, which
// serves inference. The credential probe uses the control-plane host instead.
func checkConnectionBedrockRuntimeTargets(req checkConnectionRequest) ([]string, error) {
	region, err := bedrockCheckRegion(req)
	if err != nil {
		return nil, err
	}
	return []string{bedrockRuntimeBaseURL(region)}, nil
}

func bedrockRuntimeBaseURL(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// probeBedrockConverse sends a SigV4-signed POST
// bedrock-runtime.{region}.amazonaws.com/model/{model}/converse. Credentials
// come ONLY from the supplied fields, as for the credential probe.
func probeBedrockConverse(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	region, err := bedrockCheckRegion(req)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"messages":        []map[string]any{{"role": "user", "content": []map[string]string{{"text": checkModelPrompt}}}},
		"inferenceConfig": map[string]any{"maxTokens": 1},
	})
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	target := bedrockRuntimeBaseURL(region) + "/model/" + url.PathEscape(req.Model) + "/converse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])
	httpReq.Header.Set("x-amz-content-sha256", payloadHash)
	creds := aws.Credentials{
		AccessKeyID:     strings.TrimSpace(req.AWSAccessKeyID),
		SecretAccessKey: req.AWSSecretAccessKey,
		SessionToken:    req.AWSSessionToken,
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, httpReq, payloadHash,
		checkConnectionAWSSigningService, region, time.Now().UTC()); err != nil {
		return &checkConnectionProbeError{
			status: http.StatusUnauthorized,
			err:    fmt.Errorf("check_connection: could not sign the bedrock request: %w", err),
		}
	}
	return modelProbeDo(client, httpReq, "output")
}

// probeVertexGenerateContent mints a token exactly as the credential probe
// does, then sends one generateContent (Gemini) or rawPredict (Claude on
// Vertex) request for the model.
func probeVertexGenerateContent(ctx context.Context, client *http.Client, req checkConnectionRequest) error {
	_, location, err := vertexCheckCredential(req)
	if err != nil {
		return err
	}
	accessToken, err := mintVertexAccessToken(ctx, client, req)
	if err != nil {
		return err
	}
	base := "https://" + vertexCheckAPIHost(location) + "/v1/projects/" + url.PathEscape(strings.TrimSpace(req.VertexProject)) +
		"/locations/" + url.PathEscape(location)
	headers := map[string]string{"Authorization": "Bearer " + accessToken}
	if strings.HasPrefix(strings.ToLower(req.Model), "claude") {
		body := map[string]any{
			"anthropic_version": "vertex-2023-10-16",
			"messages":          []map[string]string{{"role": "user", "content": checkModelPrompt}},
			"max_tokens":        1,
		}
		return modelProbePOST(ctx, client, base+"/publishers/anthropic/models/"+url.PathEscape(req.Model)+":rawPredict",
			headers, body, "content")
	}
	body := map[string]any{
		"contents":         []map[string]any{{"role": "user", "parts": []map[string]string{{"text": checkModelPrompt}}}},
		"generationConfig": map[string]any{"maxOutputTokens": 1},
	}
	return modelProbePOST(ctx, client, base+"/publishers/google/models/"+url.PathEscape(req.Model)+":generateContent",
		headers, body, "candidates")
}

// --- helpers ------------------------------------------------------------------

func apiKeyHeader(apiKey string) map[string]string {
	headers := map[string]string{}
	if apiKey != "" {
		headers["api-key"] = apiKey
	}
	return headers
}

func azureDeploymentCompletionURL(apiBase, deployment, apiVersion string) string {
	return checkConnectionJoinURL(apiBase, "/openai/deployments/"+url.PathEscape(deployment)+"/chat/completions") +
		"?api-version=" + url.QueryEscape(apiVersion)
}

// azureAcceptsMaxCompletionTokens reports whether an Azure api-version
// (YYYY-MM-DD[-preview]) is 2024-09-01 or later, the first that knows
// max_completion_tokens. An unparseable version gets the older max_tokens.
func azureAcceptsMaxCompletionTokens(apiVersion string) bool {
	if len(apiVersion) < len("2006-01-02") {
		return false
	}
	date, err := time.Parse("2006-01-02", apiVersion[:len("2006-01-02")])
	if err != nil {
		return false
	}
	return !date.Before(time.Date(2024, time.September, 1, 0, 0, 0, 0, time.UTC))
}

// isOpenAIAPIOrigin reports whether apiBase is OpenAI's own API host.
func isOpenAIAPIOrigin(apiBase string) bool {
	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.openai.com")
}

// modelProbePOST sends one JSON POST to rawURL and checks the answer is a
// completion that carries expectKey.
func modelProbePOST(
	ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body any, expectKey string,
) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &checkConnectionProbeError{err: fmt.Errorf("check_connection: api_base does not yield a valid http(s) url")}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for key, value := range headers {
		if value != "" {
			httpReq.Header.Set(key, value)
		}
	}
	return modelProbeDo(client, httpReq, expectKey)
}

// modelProbeDo sends a built probe request. A non-2xx answer becomes a
// modelProbeError with the provider's message; a 2xx answer must be a JSON
// object that carries expectKey.
func modelProbeDo(client *http.Client, httpReq *http.Request, expectKey string) error {
	resp, err := client.Do(httpReq)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, checkModelMaxErrorBody))
		return &modelProbeError{status: resp.StatusCode, message: providerErrorMessage(raw)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, checkModelMaxSuccessBody))
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return &modelProbeError{status: resp.StatusCode, notCompletion: true}
	}
	if _, ok := document[expectKey]; !ok {
		return &modelProbeError{status: resp.StatusCode, notCompletion: true}
	}
	return nil
}
