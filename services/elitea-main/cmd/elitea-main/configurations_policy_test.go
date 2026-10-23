package main

import (
	"context"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// currentPolicyEffects records what the lifecycle actually wrote. It is a
// deliberately dumb double: every collaborator succeeds, so the ONLY thing that
// can keep status_ok false is the policy itself.
type currentPolicyEffects struct {
	resolutions int
	statuses    []configurationapp.CurrentConfigurationLifecycleStatusUpdate
}

func (e *currentPolicyEffects) ResolveCurrentProviderConfiguration(
	context.Context,
	configurationapp.CurrentProviderConfigurationResolution,
) error {
	e.resolutions++
	return nil
}

func (e *currentPolicyEffects) SetCurrentConfigurationLifecycleStatus(
	_ context.Context,
	update configurationapp.CurrentConfigurationLifecycleStatusUpdate,
) error {
	e.statuses = append(e.statuses, update)
	return nil
}

func (e *currentPolicyEffects) RenameCurrentConfigurationReferences(
	context.Context,
	configurationapp.CurrentConfigurationRenameEffect,
) error {
	return nil
}

func (e *currentPolicyEffects) RepairCurrentDeletedLLMReferences(
	context.Context,
	configurationapp.CurrentDeletedLLMEffect,
) error {
	return nil
}

// The project-own-LLM policy survived LiteLLM's removal; only its env var was
// renamed off the retired ELITEA_LITELLM_ prefix. Parsing the new name is not
// enough to call the rename done — the parsed flag has to still reach the
// lifecycle and still decide status_ok, because status_ok is the read filter
// the Bifrost gateway's credential loader, the model catalogue and the
// embedding-configuration query all select on.
//
// So this walks the whole path in one test: env value -> currentConfigurationsConfig
// -> CurrentProviderProjectPolicy -> the reconcile of a private project's
// provider credential -> the status_ok that is written. It fails if the env
// name stops being read, if the boolean stops being carried, or if the
// enforcement is dropped — three separate regressions a parse-only assertion
// would let through.
func TestProjectOwnLLMPolicyFromEnvDecidesStatusOK(t *testing.T) {
	t.Parallel()

	const publicProjectID = int32(1)
	const privateProjectID = int32(7)

	tests := []struct {
		name         string
		env          map[string]string
		projectID    int32
		wantStatusOK bool
	}{
		{
			name: "denied private project is never admitted",
			env: map[string]string{
				"ELITEA_CONFIGURATIONS_ENABLED": "true",
				"ELITEA_AI_PROJECT_ID":          "1",
				"ELITEA_ALLOW_PROJECT_OWN_LLMS": "false",
			},
			projectID:    privateProjectID,
			wantStatusOK: false,
		},
		{
			// The control: same row, same doubles, only the policy differs. Without
			// it a broken policy read that always denied would look like a pass.
			name: "allowed private project is admitted",
			env: map[string]string{
				"ELITEA_CONFIGURATIONS_ENABLED": "true",
				"ELITEA_AI_PROJECT_ID":          "1",
				"ELITEA_ALLOW_PROJECT_OWN_LLMS": "true",
			},
			projectID:    privateProjectID,
			wantStatusOK: true,
		},
		{
			// The public project defines the shared LLMs, so it is admitted even
			// when every other project is denied.
			name: "public project is admitted despite the denial",
			env: map[string]string{
				"ELITEA_CONFIGURATIONS_ENABLED": "true",
				"ELITEA_AI_PROJECT_ID":          "1",
				"ELITEA_ALLOW_PROJECT_OWN_LLMS": "false",
			},
			projectID:    publicProjectID,
			wantStatusOK: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			config, err := currentConfigurationsConfigFromEnv(func(name string) (string, bool) {
				value, ok := test.env[name]
				return value, ok
			})
			if err != nil {
				t.Fatalf("parse environment: %v", err)
			}

			effects := &currentPolicyEffects{}
			reconciler, err := configurationapp.NewCurrentConfigurationLifecycleEffectsReconciler(
				effects, effects, effects, effects,
				// Exactly the policy cmd/elitea-main composes from the parsed
				// configuration when it builds the lifecycle reconciler.
				configurationapp.CurrentProviderProjectPolicy{
					AllowProjectOwnLLMs: config.AllowProjectOwnLLMs,
					PublicProjectID:     config.PublicProjectID,
				},
			)
			if err != nil {
				t.Fatalf("compose lifecycle effects reconciler: %v", err)
			}

			after := configurationapp.CurrentConfigurationLifecycleSnapshot{
				ID: 9, UUID: "configuration-uuid", ProjectID: test.projectID,
				EliteaTitle: "OpenAI", Type: "open_ai", Section: "ai_credentials",
				StatusOK: true, Source: configurationapp.CurrentConfigurationSourceUser,
				Data: map[string]any{},
			}
			event := configurationapp.CurrentConfigurationLifecycleEvent{
				EventID: "event-1", ProjectID: after.ProjectID, ConfigurationUUID: after.UUID,
				Revision: 4, Operation: configurationapp.CurrentConfigurationCreated,
				ActorID: 13, AttemptCount: 1,
			}
			intent := configurationapp.CurrentConfigurationLifecycleIntent{
				ID: event.EventID, Operation: event.Operation, ActorID: event.ActorID, After: &after,
			}

			if _, err := reconciler.ReconcileCurrentConfigurationLifecycle(
				context.Background(), event, intent,
			); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			if len(effects.statuses) == 0 {
				t.Fatal("no status was written — the reconcile did not reach the policy")
			}
			admitted := false
			for _, status := range effects.statuses {
				if status.StatusOK {
					admitted = true
				}
			}
			if admitted != test.wantStatusOK {
				t.Fatalf("status_ok=true written=%v want=%v; statuses=%#v",
					admitted, test.wantStatusOK, effects.statuses)
			}
			if !test.wantStatusOK && effects.resolutions != 0 {
				t.Fatalf("denied project was still resolved %d time(s)", effects.resolutions)
			}
		})
	}
}
