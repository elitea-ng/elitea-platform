package configurations

import (
	"context"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
)

// CurrentMicrosoftDataNormalizer owns the save rules of the four Microsoft
// delegated credential types (outlook, teams, ms_graph, dataverse; ADR-0057).
// There is no SDK validation boundary behind them: they are native types
// (native_types_microsoft.go), so the rules of the SDK's pydantic models are
// applied here, by the same functions the connection check and the sign-in use
// (internal/microsoftauth), so a stored value and a requested value cannot
// differ:
//
//   - a field the type does not define is dropped. That is how a `refresh_token`
//     or `access_token` in a request never reaches the credential's data
//     (ADR-0057 decision 4);
//   - the scopes are normalized (a fixed list keeps what it offers, in its
//     order, and falls back to its default; Microsoft Graph free text is
//     validated);
//   - an OAuth discovery endpoint must be an Entra authority, and a Dataverse
//     url and tenant_id must be valid (https, *.dynamics.com, port 443; an
//     update validates the ones it carries);
//   - a create needs either a Default app (oauth_app_id) or a Custom app's
//     client_id and client_secret. An update does not re-check this: its data
//     carries the stored secret as a reference.
type CurrentMicrosoftDataNormalizer struct {
	fallback CurrentConfigurationDataNormalizer
}

// NewCurrentMicrosoftDataNormalizer composes the rules in front of another
// normalizer.
func NewCurrentMicrosoftDataNormalizer(fallback CurrentConfigurationDataNormalizer) *CurrentMicrosoftDataNormalizer {
	return &CurrentMicrosoftDataNormalizer{fallback: fallback}
}

func (n *CurrentMicrosoftDataNormalizer) Normalize(
	ctx context.Context,
	request CurrentConfigurationNormalizationRequest,
) (CurrentConfigurationNormalizationResult, error) {
	if ctx == nil {
		return CurrentConfigurationNormalizationResult{}, ErrInvalidCurrentConfigurationMutation
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationNormalizationResult{}, err
	}
	if currentMicrosoftConfigurationType(request.Type) {
		data, err := normalizeCurrentMicrosoftData(request.Type, request.Operation, request.Data)
		return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
	}
	if n != nil && n.fallback != nil {
		return n.fallback.Normalize(ctx, request)
	}
	return CurrentConfigurationNormalizationResult{Complete: false}, nil
}

// currentMicrosoftConfigurationType is true for the Microsoft types this build
// defines natively. SharePoint is a pinned SDK type and keeps its own owner.
func currentMicrosoftConfigurationType(typeName string) bool {
	switch typeName {
	case microsoftauth.Outlook, microsoftauth.Teams, microsoftauth.MSGraph, microsoftauth.Dataverse:
		return true
	default:
		return false
	}
}

// microsoftFields is the set of fields each Microsoft type defines, taken once
// from the definitions so the live registry is never read (or changed) here.
var microsoftFields = func() map[string]map[string]bool {
	fields := map[string]map[string]bool{}
	for _, native := range microsoftNativeTypes() {
		fields[native.Type] = map[string]bool{}
		for field := range native.Properties {
			fields[native.Type][field] = true
		}
	}
	return fields
}()

func microsoftFieldError(field string) error {
	return currentMutationFieldError(CurrentConfigurationMutationInvalid, "data."+field)
}

func normalizeCurrentMicrosoftData(
	typeName string,
	operation CurrentConfigurationNormalizationOperation,
	data map[string]any,
) (map[string]any, error) {
	if data == nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	descriptor, ok := microsoftauth.For(typeName)
	if !ok {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	kept := map[string]any{}
	for field := range microsoftFields[typeName] {
		if value, present := data[field]; present {
			kept[field] = value
		}
	}
	if operation == CurrentConfigurationNormalizationCreate {
		if err := requireMicrosoftAppOrClient(kept); err != nil {
			return nil, err
		}
	}
	if endpoint, _ := kept[microsoftauth.FieldDiscoveryEndpoint].(string); strings.TrimSpace(endpoint) != "" {
		authority, err := microsoftauth.NormalizeEntraAuthority(endpoint)
		if err != nil {
			return nil, microsoftFieldError(microsoftauth.FieldDiscoveryEndpoint)
		}
		kept[microsoftauth.FieldDiscoveryEndpoint] = authority
	}
	if typeName == microsoftauth.Dataverse {
		// An update may carry only the fields it changes; a create needs both.
		_, hasURL := kept[microsoftauth.FieldURL]
		_, hasTenant := kept[microsoftauth.FieldTenantID]
		if hasURL || operation == CurrentConfigurationNormalizationCreate {
			org, err := microsoftauth.NormalizeDataverseURL(stringField(kept, microsoftauth.FieldURL))
			if err != nil {
				return nil, microsoftFieldError(microsoftauth.FieldURL)
			}
			kept[microsoftauth.FieldURL] = org
		}
		if hasTenant || operation == CurrentConfigurationNormalizationCreate {
			if _, err := descriptor.Authority(kept); err != nil {
				return nil, microsoftFieldError(microsoftauth.FieldTenantID)
			}
		}
		return kept, nil
	}
	// A create stores the normalized selection even when none was sent, so the
	// stored value is what a sign-in will ask for.
	if _, present := kept[microsoftauth.FieldScopes]; present || operation == CurrentConfigurationNormalizationCreate {
		selected, _, err := descriptor.Scopes(kept)
		if err != nil {
			return nil, microsoftFieldError(microsoftauth.FieldScopes)
		}
		kept[microsoftauth.FieldScopes] = stringsAsAny(selected)
	}
	return kept, nil
}

// requireMicrosoftAppOrClient is the SDK's require_app_client: a credential needs
// a Default app or the client of its own Custom app.
func requireMicrosoftAppOrClient(data map[string]any) error {
	if stringField(data, microsoftauth.FieldAppID) != "" {
		return nil
	}
	if stringField(data, "client_id") == "" {
		return microsoftFieldError("client_id")
	}
	if stringField(data, "client_secret") == "" {
		return microsoftFieldError("client_secret")
	}
	return nil
}

func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

var _ CurrentConfigurationDataNormalizer = (*CurrentMicrosoftDataNormalizer)(nil)
