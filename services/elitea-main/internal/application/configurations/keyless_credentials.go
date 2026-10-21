package configurations

import "strings"

// keyless_credentials.go — the `auth_mode` field of a provider credential.
//
// A credential in "workload_identity" mode holds NO key. The LLM gateway signs
// in to the provider with its own Google identity (GKE Workload Identity)
// instead. That identity belongs to the PLATFORM, so only a platform provider
// may use it: a row in the public project, shared, written through the admin
// providers surface. The gateway enforces the same rule on every read
// (services/elitea-llm-gateway/internal/account/workload_identity.go); the
// write side refuses early so that an operator sees the reason on save and not
// as a refused chat later.

const (
	// CredentialAuthModeField is the `data` key that selects the mode.
	CredentialAuthModeField = "auth_mode"
	// CredentialTokenAudienceField is the optional AI DIAL ID-token audience.
	CredentialTokenAudienceField = "token_audience"
	// CredentialAuthModeKey is the default: the row holds a key.
	CredentialAuthModeKey = "key"
	// CredentialAuthModeWorkloadIdentity uses the gateway's Google identity.
	CredentialAuthModeWorkloadIdentity = "workload_identity"
)

// keylessCredentialKeyFields maps each type that supports the keyless mode to
// the key field that the mode replaces. Only these two types: the gateway
// mints a Google access token for Vertex AI and a Google ID token for AI DIAL,
// and no other provider accepts either.
var keylessCredentialKeyFields = map[string]string{
	"vertex_ai": "vertex_credentials",
	"ai_dial":   "api_key",
}

// KeylessCredentialKeyField returns the key field the keyless mode replaces
// for configType, and false for a type that has no keyless mode.
func KeylessCredentialKeyField(configType string) (string, bool) {
	field, ok := keylessCredentialKeyFields[configType]
	return field, ok
}

// CredentialAuthMode reads `data.auth_mode`. An absent, null or empty value is
// the key mode. ok is false for any other value, and the caller must refuse
// it: an unknown mode read as "key" would be a silent fallback.
func CredentialAuthMode(data map[string]any) (mode string, ok bool) {
	raw, present := data[CredentialAuthModeField]
	if !present || raw == nil {
		return CredentialAuthModeKey, true
	}
	value, isString := raw.(string)
	if !isString {
		return "", false
	}
	switch strings.TrimSpace(value) {
	case "", CredentialAuthModeKey:
		return CredentialAuthModeKey, true
	case CredentialAuthModeWorkloadIdentity:
		return CredentialAuthModeWorkloadIdentity, true
	default:
		return "", false
	}
}

// refuseCurrentKeylessCredential is the project-scope rule of the mutation
// service. This service serves the PROJECT routes only, so a keyless mode is
// always refused here. The admin providers surface writes through the
// compatibility handler, which has the platform-write marker.
//
// It reads `auth_mode` of the two keyless types only: a toolkit or MCP
// configuration may carry an `auth_mode` of its own with another meaning.
func refuseCurrentKeylessCredential(typeName string, data map[string]any) error {
	if _, keyless := KeylessCredentialKeyField(typeName); !keyless {
		return nil
	}
	mode, ok := CredentialAuthMode(data)
	if !ok {
		return currentMutationFieldError(CurrentConfigurationMutationInvalid, "data."+CredentialAuthModeField)
	}
	if mode == CredentialAuthModeWorkloadIdentity {
		return currentMutationFieldError(CurrentConfigurationMutationPlatformOnly, "data."+CredentialAuthModeField)
	}
	return nil
}
