package configurations

import (
	"context"
	"net/http"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// keyless_credentials.go — the write-side rules for `data.auth_mode`.
//
// "workload_identity" makes the LLM gateway sign in with the POD's Google
// identity (GKE Workload Identity) instead of a stored key. That identity is
// the platform's, so a project credential must never select it: a tenant that
// could would spend the platform's Vertex AI quota, or send the platform's ID
// token to an AI DIAL endpoint the tenant controls. The gateway refuses such a
// row on every read; these rules refuse it on save, with a reason the operator
// can act on.
//
// The compatibility handlers serve BOTH the project routes and the admin
// providers surface (CreateGlobalProvider / UpdateGlobalProvider delegate to
// them). The surface marks its request context, and only a marked request may
// store the mode. A body field could not carry that decision: the body is the
// caller's.

const keylessCredentialPlatformOnlyMessage = `data.auth_mode "workload_identity" is accepted only for a ` +
	`platform provider (Administration, LLM providers): a project credential must hold its own key`

type platformCredentialWriteKey struct{}

// withPlatformCredentialWrite marks a request that the admin providers surface
// has pinned to the public project and forced to shared.
func withPlatformCredentialWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, platformCredentialWriteKey{}, true)
}

func platformCredentialWrite(ctx context.Context) bool {
	marked, _ := ctx.Value(platformCredentialWriteKey{}).(bool)
	return marked
}

// keylessModeApplies reports whether `data.auth_mode` is the LLM credential
// field this file rules on. It is for the two keyless types, and for every
// write of the admin providers surface (which writes LLM provider credentials
// only, and must refuse the mode on any other provider type). Every other type
// is left alone: a toolkit or MCP configuration may carry an `auth_mode` of
// its own ("basic", "oauth", ...) that means something else entirely.
func keylessModeApplies(ctx context.Context, configType string) bool {
	if _, keyless := configurationapp.KeylessCredentialKeyField(configType); keyless {
		return true
	}
	return platformCredentialWrite(ctx)
}

// refuseKeylessCredentialWrite refuses an unknown mode, and the keyless mode
// on an unmarked request. configType may be "" (an update body that omits the
// type): the rule then applies on the admin surface only, and the update path
// runs it again once the stored type is read.
func refuseKeylessCredentialWrite(ctx context.Context, configType string, data map[string]any) *configurationWriteFailure {
	if data == nil || !keylessModeApplies(ctx, configType) {
		return nil
	}
	mode, ok := configurationapp.CredentialAuthMode(data)
	if !ok {
		return &configurationWriteFailure{
			status:  http.StatusBadRequest,
			message: `data.auth_mode must be "key" or "workload_identity"`,
		}
	}
	if mode == configurationapp.CredentialAuthModeWorkloadIdentity && !platformCredentialWrite(ctx) {
		return &configurationWriteFailure{status: http.StatusBadRequest, message: keylessCredentialPlatformOnlyMessage}
	}
	if raw, present := data[configurationapp.CredentialTokenAudienceField]; present && raw != nil {
		if _, isString := raw.(string); !isString {
			return &configurationWriteFailure{status: http.StatusBadRequest, message: "data.token_audience must be a string"}
		}
	}
	return nil
}

// unsavedKeylessCheckReason is the gateway's own reason for the refusal, so a
// client reads one code for one cause.
const unsavedKeylessCheckReason = "workload_identity_not_allowed"

// refuseUnsavedKeylessCheck refuses a keyless payload on the UNSAVED check
// routes. Those routes take `api_base` from the caller and sign the path's
// project. If that project is the public one, the gateway would mint the
// platform's ID token for an endpoint the caller chose. A keyless provider is
// tested after it is saved, through the admin providers surface, which tests
// the STORED row.
//
// Only the two keyless types are read: the gateway admits the mode for no
// other type, and another type's `auth_mode` is its own field.
func refuseUnsavedKeylessCheck(configType string, data map[string]any) (string, bool) {
	if _, keyless := configurationapp.KeylessCredentialKeyField(configType); !keyless {
		return "", false
	}
	mode, ok := configurationapp.CredentialAuthMode(data)
	if !ok {
		return `data.auth_mode must be "key" or "workload_identity"`, true
	}
	if mode == configurationapp.CredentialAuthModeWorkloadIdentity {
		return "A keyless (workload identity) provider is tested after it is saved, " +
			"from Administration, LLM providers.", true
	}
	return "", false
}

// admitKeylessCredentialData runs once the type is known. It refuses the mode
// on a type without one, and it CLEARS the key fields of a keyless row.
//
// Clearing is what makes a switch to the keyless mode safe. An update writes
// `data` whole, and an edit form can send back the stored `{{secret.NAME}}`
// reference of the old key; a row that kept it would be ambiguous, and the
// gateway refuses an ambiguous row (INVALID_CREDENTIAL_AUTH_MODE). So an empty
// value or a stored reference is removed. A NEW literal key is refused
// instead: the operator typed a key into a keyless provider, and silently
// dropping it would hide that mistake.
func admitKeylessCredentialData(ctx context.Context, configType string, data map[string]any) *configurationWriteFailure {
	if data == nil || !keylessModeApplies(ctx, configType) {
		return nil
	}
	if mode, ok := configurationapp.CredentialAuthMode(data); !ok || mode != configurationapp.CredentialAuthModeWorkloadIdentity {
		return nil
	}
	if _, supported := configurationapp.KeylessCredentialKeyField(configType); !supported {
		return &configurationWriteFailure{
			status:  http.StatusBadRequest,
			message: `data.auth_mode "workload_identity" is supported only for the vertex_ai and ai_dial types`,
		}
	}
	for _, field := range globalProviderSecretFields {
		raw, present := data[field]
		if !present {
			continue
		}
		value, isString := raw.(string)
		switch {
		case raw == nil, isString && strings.TrimSpace(value) == "",
			isString && configurationapp.IsCurrentSecretReference(value):
			delete(data, field)
		default:
			return &configurationWriteFailure{
				status: http.StatusBadRequest,
				message: "data." + field + ` must be empty when data.auth_mode is "workload_identity": ` +
					"a keyless provider holds no key",
			}
		}
	}
	return nil
}
