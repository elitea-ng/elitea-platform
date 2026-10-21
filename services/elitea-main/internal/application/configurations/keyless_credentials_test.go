package configurations

import (
	"context"
	"errors"
	"testing"
)

func TestCredentialAuthMode(t *testing.T) {
	for _, data := range []map[string]any{{}, {"auth_mode": nil}, {"auth_mode": ""}, {"auth_mode": "key"}} {
		if mode, ok := CredentialAuthMode(data); !ok || mode != CredentialAuthModeKey {
			t.Errorf("CredentialAuthMode(%v) = %q, %v; want the key mode", data, mode, ok)
		}
	}
	if mode, ok := CredentialAuthMode(map[string]any{"auth_mode": "workload_identity"}); !ok ||
		mode != CredentialAuthModeWorkloadIdentity {
		t.Errorf("workload_identity = %q, %v", mode, ok)
	}
	// An unknown value is refused, never read as the key mode.
	for _, raw := range []any{"ambient", "WORKLOAD_IDENTITY", 1, true} {
		if _, ok := CredentialAuthMode(map[string]any{"auth_mode": raw}); ok {
			t.Errorf("auth_mode %v was accepted", raw)
		}
	}
}

// TestMutationServiceRefusesTheKeylessMode: the mutation service serves the
// project routes only, so the mode is refused for every type, on create and on
// update, with a field the API maps to a 400.
func TestMutationServiceRefusesTheKeylessMode(t *testing.T) {
	err := refuseCurrentKeylessCredential("ai_dial", map[string]any{"auth_mode": "workload_identity"})
	var mutationError *CurrentConfigurationMutationError
	if !errors.As(err, &mutationError) || mutationError.Code != CurrentConfigurationMutationPlatformOnly ||
		mutationError.Field != "data.auth_mode" {
		t.Fatalf("err = %v, want the platform-only refusal on data.auth_mode", err)
	}
	err = refuseCurrentKeylessCredential("ai_dial", map[string]any{"auth_mode": "ambient"})
	if !errors.As(err, &mutationError) || mutationError.Code != CurrentConfigurationMutationInvalid {
		t.Fatalf("err = %v, want an invalid-field refusal", err)
	}
	if err := refuseCurrentKeylessCredential("ai_dial", map[string]any{"auth_mode": "key"}); err != nil {
		t.Fatalf("the key mode was refused: %v", err)
	}
	// Another type's own `auth_mode` is not this field (a toolkit's "basic",
	// "oauth", even its own "workload_identity").
	for _, typeName := range []string{"github", "jira", "mcp_custom", ""} {
		for _, mode := range []string{"basic", "oauth", "workload_identity"} {
			if err := refuseCurrentKeylessCredential(typeName, map[string]any{"auth_mode": mode}); err != nil {
				t.Errorf("type %q auth_mode %q refused: %v", typeName, mode, err)
			}
		}
	}
	service := &CurrentConfigurationMutationService{}
	if _, err := service.normalizeData(context.Background(), CurrentConfigurationNormalizationUpdate, 1, 1, "ai_dial", nil,
		map[string]any{"auth_mode": "workload_identity"}); !errors.As(err, &mutationError) {
		t.Fatalf("normalizeData did not refuse the keyless mode: %v", err)
	}
}
