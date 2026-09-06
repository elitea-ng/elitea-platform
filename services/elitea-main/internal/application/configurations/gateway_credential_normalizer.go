package configurations

import "context"

// CurrentGatewayCredentialDataNormalizer owns the create-time rules for the
// three provider credential types the LLM data plane can dispatch to and the
// baseline platform never registered: `anthropic`, `open_ai_azure` and `vllm`.
//
// WHY IT IS NOT CurrentLiteLLMDataNormalizer. That normalizer ports the six
// Pydantic credential models `runtime_interface_litellm` registers, and the
// three here have no such model: they come from `providerConfigTypes` in
// services/elitea-llm-gateway/internal/account/credentials.go. Putting them in
// the LiteLLM owner would make its name state something untrue about where its
// contract comes from.
//
// Every type in the catalogue must have EXACTLY ONE create-time owner
// (NewCurrentConfigurationDataNormalizer fails closed otherwise), so this file
// is what lets those three types exist in the pinned catalogue at all. They
// must exist there: without a catalogue entry `sectionFor` stores an empty
// `section` and the gateway never reads the row, and the write route has no
// data schema, so it cannot tell which field is a password.
//
// Updates are delegated, exactly as the LiteLLM owner delegates them: the
// current update contract applies only shallow coercion.
type CurrentGatewayCredentialDataNormalizer struct {
	fallback CurrentConfigurationDataNormalizer
}

// NewCurrentGatewayCredentialDataNormalizer composes the gateway credential
// create rules in front of another normalizer. A nil fallback leaves unhandled
// requests incomplete.
func NewCurrentGatewayCredentialDataNormalizer(
	fallback CurrentConfigurationDataNormalizer,
) *CurrentGatewayCredentialDataNormalizer {
	return &CurrentGatewayCredentialDataNormalizer{fallback: fallback}
}

func (n *CurrentGatewayCredentialDataNormalizer) Normalize(
	ctx context.Context,
	request CurrentConfigurationNormalizationRequest,
) (CurrentConfigurationNormalizationResult, error) {
	if ctx == nil {
		return CurrentConfigurationNormalizationResult{}, ErrInvalidCurrentConfigurationMutation
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationNormalizationResult{}, err
	}
	if request.Operation == CurrentConfigurationNormalizationCreate &&
		currentGatewayCredentialType(request.Type) {
		data, err := normalizeCurrentGatewayCredentialCreate(request.Type, request.Data)
		return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
	}
	if n != nil && n.fallback != nil {
		return n.fallback.Normalize(ctx, request)
	}
	return CurrentConfigurationNormalizationResult{Complete: false}, nil
}

// currentGatewayCredentialType lists the credential types this normalizer owns.
//
// It is the difference between currentProviderCredentialType (the nine types
// the gateway dispatches to) and currentLiteLLMCredentialType (the six the
// baseline registered). TestEveryGatewayCredentialTypeHasOneCreateOwner keeps
// the three lists consistent.
func currentGatewayCredentialType(typeName string) bool {
	switch typeName {
	case "anthropic", "open_ai_azure", "vllm":
		return true
	default:
		return false
	}
}

// normalizeCurrentGatewayCredentialCreate keeps the stored field names the
// gateway's credentialData struct reads. A field renamed for readability would
// be written into the row and ignored by the gateway.
func normalizeCurrentGatewayCredentialCreate(
	typeName string,
	data map[string]any,
) (map[string]any, error) {
	switch typeName {
	case "anthropic":
		return normalizeCurrentAnthropicCredential(data)
	case "open_ai_azure":
		return normalizeCurrentAzureCredential(data)
	case "vllm":
		return normalizeCurrentVLLMCredential(data)
	default:
		return nil, currentMutationFieldError(CurrentConfigurationMutationNormalizationRequired, "data")
	}
}

// normalizeCurrentAnthropicCredential keeps the key and the optional endpoint.
// Anthropic authenticates with the key alone; api_base exists for an
// Anthropic-compatible proxy, so it is optional.
func normalizeCurrentAnthropicCredential(data map[string]any) (map[string]any, error) {
	apiKey, err := currentCredentialRequiredString(data, "api_key")
	if err != nil {
		return nil, err
	}
	apiBase, err := currentCredentialOptionalString(data, "api_base")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"api_base": apiBase,
		"api_key":  apiKey,
	}, nil
}

// normalizeCurrentVLLMCredential keeps the endpoint, the optional key and the
// Anthropic-dialect switch. use_anthropic_endpoints is read straight into
// schemas.Key.UseAnthropicEndpoints by the gateway; dropping it here would
// route an Anthropic-dialect upstream through the OpenAI-compatible surface.
//
// The key is optional on purpose: a vLLM deployment behind a private network
// commonly has none.
func normalizeCurrentVLLMCredential(data map[string]any) (map[string]any, error) {
	apiBase, err := currentCredentialRequiredString(data, "api_base")
	if err != nil {
		return nil, err
	}
	apiKey, err := currentCredentialOptionalString(data, "api_key")
	if err != nil {
		return nil, err
	}
	anthropicEndpoints, err := currentCredentialOptionalBool(data, "use_anthropic_endpoints")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"api_base":                apiBase,
		"api_key":                 apiKey,
		"use_anthropic_endpoints": anthropicEndpoints,
	}, nil
}

func currentCredentialOptionalBool(data map[string]any, field string) (any, error) {
	raw, present := data[field]
	if !present || raw == nil {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil, currentCredentialInvalidField(field)
	}
	return value, nil
}
