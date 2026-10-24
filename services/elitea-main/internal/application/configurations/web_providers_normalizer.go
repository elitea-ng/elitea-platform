package configurations

import (
	"context"
	"strings"
)

// maxWebProviderAPIKeyBytes bounds a stored provider key. Every vendor's key
// is well under a kilobyte; the bound only stops a paste of something else.
const maxWebProviderAPIKeyBytes = 8 * 1024

// CurrentWebProviderDataNormalizer owns the create rules of the web search and
// host-intelligence provider types (#1238). There is no SDK model behind
// them: each holds one API key (DuckDuckGo instant answers hold nothing), so
// a create keeps exactly that and drops every other field. Updates are
// delegated, like every other owner's.
type CurrentWebProviderDataNormalizer struct {
	fallback CurrentConfigurationDataNormalizer
}

// NewCurrentWebProviderDataNormalizer composes the rules in front of another
// normalizer.
func NewCurrentWebProviderDataNormalizer(fallback CurrentConfigurationDataNormalizer) *CurrentWebProviderDataNormalizer {
	return &CurrentWebProviderDataNormalizer{fallback: fallback}
}

func (n *CurrentWebProviderDataNormalizer) Normalize(
	ctx context.Context,
	request CurrentConfigurationNormalizationRequest,
) (CurrentConfigurationNormalizationResult, error) {
	if ctx == nil {
		return CurrentConfigurationNormalizationResult{}, ErrInvalidCurrentConfigurationMutation
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationNormalizationResult{}, err
	}
	if request.Operation == CurrentConfigurationNormalizationCreate && currentWebProviderConfigurationType(request.Type) {
		data, err := normalizeCurrentWebProviderCreate(request.Type, request.Data)
		return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
	}
	if n != nil && n.fallback != nil {
		return n.fallback.Normalize(ctx, request)
	}
	return CurrentConfigurationNormalizationResult{Complete: false}, nil
}

func currentWebProviderConfigurationType(typeName string) bool {
	switch typeName {
	case TavilyConfigurationType, FirecrawlConfigurationType, BraveSearchConfigurationType,
		DuckDuckGoInstantConfigurationType, ShodanConfigurationType:
		return true
	default:
		return false
	}
}

func normalizeCurrentWebProviderCreate(typeName string, data map[string]any) (map[string]any, error) {
	if typeName == DuckDuckGoInstantConfigurationType {
		return map[string]any{}, nil
	}
	if data == nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	key, ok := data["api_key"].(string)
	if !ok {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.api_key")
	}
	// A secret reference ({{secret.NAME}}) is stored as given and sealed by
	// the secret extraction that follows; a literal key must be one token of
	// printable ASCII, as every vendor's is.
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || len(trimmed) > maxWebProviderAPIKeyBytes ||
		strings.ContainsFunc(trimmed, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.api_key")
	}
	return map[string]any{"api_key": trimmed}, nil
}

var _ CurrentConfigurationDataNormalizer = (*CurrentWebProviderDataNormalizer)(nil)
