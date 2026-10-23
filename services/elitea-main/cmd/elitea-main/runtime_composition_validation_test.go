package main

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func TestValidateRuntimeCompositionRejectsIndexIngestWithoutConfigurations(t *testing.T) {
	t.Parallel()

	err := validateRuntimeComposition(
		currentConfigurationsConfig{},
		runtimecomposition.Config{Enabled: true, IndexIngestDispatchEnabled: true},
	)
	if err == nil {
		t.Fatal("index ingest was accepted without the Configurations chain")
	}
}

// TestValidateRuntimeCompositionComposesIndexIngestWithoutLiteLLM pins the
// Bifrost contract: the embedding binding is resolved from the Configurations
// rows the gateway itself reads, so the index plane must compose with no LLM
// facade configured at all. This test used to assert the opposite — that a
// missing ELITEA_LITELLM_BASE_URL (or its master key) blocked index ingest —
// which is exactly what kept the index plane off on a gateway-only stack. Those
// settings no longer exist on this config at all, so the remaining cases pin
// that the Configurations flag is the ONLY input to the index-ingest decision.
func TestValidateRuntimeCompositionComposesIndexIngestWithoutLiteLLM(t *testing.T) {
	t.Parallel()

	for name, configurations := range map[string]currentConfigurationsConfig{
		"configurations only": {
			Enabled: true,
		},
		"configurations with project-own LLMs denied": {
			Enabled:             true,
			AllowProjectOwnLLMs: false,
		},
		"configurations with a vault key": {
			Enabled:            true,
			VaultMasterKeyFile: "/run/secrets/centry-vault-master-key",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateRuntimeComposition(
				configurations,
				runtimecomposition.Config{Enabled: true, IndexIngestDispatchEnabled: true},
			)
			if err != nil {
				t.Fatalf("index ingest rejected without a LiteLLM facade: %v", err)
			}
		})
	}
}

func TestValidateRuntimeCompositionPreservesIndependentRuntimeModes(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		configurations currentConfigurationsConfig
		runtime        runtimecomposition.Config
		wantErr        bool
	}{
		"all disabled": {},
		"validation runtime only": {
			runtime: runtimecomposition.Config{Enabled: true},
		},
		"read-only configurations": {
			configurations: currentConfigurationsConfig{Enabled: true},
		},
		"mutation without runtime": {
			configurations: currentConfigurationsConfig{Enabled: true, MutationEnabled: true},
			wantErr:        true,
		},
		"mutation with runtime": {
			configurations: currentConfigurationsConfig{Enabled: true, MutationEnabled: true},
			runtime:        runtimecomposition.Config{Enabled: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateRuntimeComposition(test.configurations, test.runtime)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}
