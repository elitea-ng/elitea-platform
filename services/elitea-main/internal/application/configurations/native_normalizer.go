package configurations

import (
	"context"
	"strings"
)

// maxNativeStringBytes bounds one stored string field of a native type that
// declares no maxLength of its own. A token or key is well under it; the bound
// stops a paste of something else.
const maxNativeStringBytes = 8 * 1024

// CurrentNativeDataNormalizer is the DEFAULT create-time owner of a native
// credential type (native_types.go). Every type in the catalog needs exactly
// one create owner or Main refuses to boot, so a native type is owned by this
// normalizer unless its definition says OwnNormalizer, in which case its story
// adds its own owner to NewCurrentConfigurationDataNormalizer.
//
// A create keeps exactly the fields the definition declares and drops every
// other key; every required field must be present, non-null and of the declared
// JSON type (a string must be non-empty and bounded). A secret reference
// ({{secret.NAME}}) is a string and is stored as given, sealed by the secret
// extraction that follows. Updates are delegated, like every other owner's.
type CurrentNativeDataNormalizer struct {
	fallback CurrentConfigurationDataNormalizer
}

// NewCurrentNativeDataNormalizer composes the rules in front of another
// normalizer.
func NewCurrentNativeDataNormalizer(fallback CurrentConfigurationDataNormalizer) *CurrentNativeDataNormalizer {
	return &CurrentNativeDataNormalizer{fallback: fallback}
}

func (n *CurrentNativeDataNormalizer) Normalize(
	ctx context.Context,
	request CurrentConfigurationNormalizationRequest,
) (CurrentConfigurationNormalizationResult, error) {
	if ctx == nil {
		return CurrentConfigurationNormalizationResult{}, ErrInvalidCurrentConfigurationMutation
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationNormalizationResult{}, err
	}
	if request.Operation == CurrentConfigurationNormalizationCreate && currentNativeConfigurationType(request.Type) {
		data, err := normalizeCurrentNativeCreate(request.Type, request.Data)
		return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
	}
	if n != nil && n.fallback != nil {
		return n.fallback.Normalize(ctx, request)
	}
	return CurrentConfigurationNormalizationResult{Complete: false}, nil
}

// currentNativeConfigurationType reports a native type owned by the default
// normalizer (one that did not declare its own).
func currentNativeConfigurationType(typeName string) bool {
	for _, native := range nativeConfigurationTypes {
		if native.Type == typeName {
			return !native.OwnNormalizer
		}
	}
	return false
}

func normalizeCurrentNativeCreate(typeName string, data map[string]any) (map[string]any, error) {
	var definition NativeConfigurationType
	for _, native := range nativeConfigurationTypes {
		if native.Type == typeName {
			definition = native
		}
	}
	if data == nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	required := make(map[string]bool, len(definition.Required))
	for _, name := range definition.Required {
		required[name] = true
	}
	kept := make(map[string]any, len(definition.Properties))
	for name, rawSchema := range definition.Properties {
		value, present := data[name]
		if !present || value == nil {
			if required[name] {
				return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data."+name)
			}
			continue
		}
		schema, _ := rawSchema.(map[string]any)
		if !nativeValueMatches(schema, value) {
			return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data."+name)
		}
		kept[name] = value
	}
	return kept, nil
}

// nativeValueMatches checks one value against the declared JSON type. A schema
// with no plain `type` (an anyOf) accepts any non-null value: its owner needs a
// rule of its own, which OwnNormalizer is for.
func nativeValueMatches(schema map[string]any, value any) bool {
	switch schema["type"] {
	case "string":
		text, ok := value.(string)
		if !ok {
			return false
		}
		limit := maxNativeStringBytes
		if declared, ok := schema["maxLength"].(int); ok && declared > 0 {
			limit = declared
		}
		return strings.TrimSpace(text) != "" && len(text) <= limit
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch number := value.(type) {
		case int, int64:
			return true
		case float64:
			return number == float64(int64(number))
		}
		return false
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return true
}

var _ CurrentConfigurationDataNormalizer = (*CurrentNativeDataNormalizer)(nil)
