package runtimecomposition

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentProviderConfigurationResolutionUsesOwningVaultThenGenericExpansion(t *testing.T) {
	callOrder := []string{}
	unsecreter := currentLifecycleUnsecreterStub{unsecret: func(
		_ context.Context,
		projectID int32,
		data map[string]any,
	) (map[string]any, error) {
		callOrder = append(callOrder, "unsecret")
		if projectID != 7 || data["api_key"] != "{{secret.key}}" {
			t.Fatalf("project=%d data=%#v", projectID, data)
		}
		return map[string]any{
			"api_key": "resolved-root",
			"ai_credentials": map[string]any{
				"elitea_title": "shared-openai", "private": false,
			},
		}, nil
	}}
	expander := currentLifecycleExpanderStub{expand: func(
		_ context.Context,
		request configurationapp.CurrentExpansionRequest,
	) (map[string]any, error) {
		callOrder = append(callOrder, "expand")
		if request.CurrentProjectID != 7 || !request.Unsecret || request.UserID == nil || *request.UserID != 13 ||
			request.Payload["api_key"] != "resolved-root" {
			t.Fatalf("request=%#v", request)
		}
		return map[string]any{
			"api_key": "resolved-root",
			"ai_credentials": map[string]any{
				"configuration_uuid":       "credential-uuid",
				"configuration_project_id": 1,
				"configuration_type":       "open_ai",
				"api_key":                  "resolved-nested",
			},
		}, nil
	}}
	resolution, err := newCurrentProviderConfigurationResolution(expander, unsecreter)
	if err != nil {
		t.Fatal(err)
	}
	authorID := int32(13)
	snapshot := configurationapp.CurrentConfigurationLifecycleSnapshot{
		UUID: "model-uuid", ProjectID: 7, Type: "llm_model", AuthorID: &authorID,
		Data: map[string]any{
			"api_key": "{{secret.key}}",
			"ai_credentials": map[string]any{
				"elitea_title": "shared-openai", "private": false,
			},
		},
	}
	if err := resolution.ResolveCurrentProviderConfiguration(
		context.Background(),
		configurationapp.CurrentProviderConfigurationResolution{
			EffectID: "event-1:provider:resolve", Revision: 4, ProjectID: 7,
			ConfigurationUUID: "model-uuid", Section: "llm", Configuration: snapshot,
		},
	); err != nil {
		t.Fatal(err)
	}
	// Owning-project unsecreting must precede expansion, so each nested
	// configuration is redeemed through its own project's vault.
	if !reflect.DeepEqual(callOrder, []string{"unsecret", "expand"}) {
		t.Fatalf("calls=%#v", callOrder)
	}
	if snapshot.Data["api_key"] != "{{secret.key}}" {
		t.Fatalf("snapshot was mutated: %#v", snapshot.Data)
	}
}

// The check is the reason a row reaches status_ok at all, so an unresolvable
// reference or an unredeemable secret has to surface as a failure — and it must
// surface without carrying provider text out of the boundary.
func TestCurrentProviderConfigurationResolutionRedactsDependenciesAndPreservesCancellation(t *testing.T) {
	secret := "provider-token-must-not-escape"
	for name, test := range map[string]struct {
		expander   currentConfigurationLifecycleExpander
		unsecreter currentConfigurationLifecycleUnsecreter
		want       error
	}{
		"unsecret failure": {
			expander: currentLifecycleExpanderStub{expand: func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error) {
				return map[string]any{}, nil
			}},
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(context.Context, int32, map[string]any) (map[string]any, error) {
				return nil, errors.New(secret)
			}},
			want: errCurrentProviderConfigurationResolution,
		},
		"expansion failure": {
			expander: currentLifecycleExpanderStub{expand: func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error) {
				return nil, errors.New(secret)
			}},
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(_ context.Context, _ int32, input map[string]any) (map[string]any, error) {
				return input, nil
			}},
			want: errCurrentProviderConfigurationResolution,
		},
		"missing expansion result": {
			expander: currentLifecycleExpanderStub{expand: func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error) {
				return nil, nil
			}},
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(_ context.Context, _ int32, input map[string]any) (map[string]any, error) {
				return input, nil
			}},
			want: errCurrentProviderConfigurationResolution,
		},
		"dependency cancellation": {
			expander: currentLifecycleExpanderStub{expand: func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error) {
				return nil, context.DeadlineExceeded
			}},
			unsecreter: currentLifecycleUnsecreterStub{unsecret: func(_ context.Context, _ int32, input map[string]any) (map[string]any, error) {
				return input, nil
			}},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolution, err := newCurrentProviderConfigurationResolution(test.expander, test.unsecreter)
			if err != nil {
				t.Fatal(err)
			}
			err = resolution.ResolveCurrentProviderConfiguration(
				context.Background(),
				configurationapp.CurrentProviderConfigurationResolution{
					ProjectID: 7, ConfigurationUUID: "uuid", Section: "ai_credentials",
					Configuration: configurationapp.CurrentConfigurationLifecycleSnapshot{
						UUID: "uuid", ProjectID: 7, Type: "open_ai", Data: map[string]any{},
					},
				},
			)
			if !errors.Is(err, test.want) || (err != nil && strings.Contains(err.Error(), secret)) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestCurrentProviderConfigurationResolutionRejectsIncompleteGraphAndSnapshot(t *testing.T) {
	validExpander := currentLifecycleExpanderStub{expand: func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error) {
		return map[string]any{}, nil
	}}
	validUnsecreter := currentLifecycleUnsecreterStub{unsecret: func(_ context.Context, _ int32, input map[string]any) (map[string]any, error) {
		return input, nil
	}}
	if resolution, err := newCurrentProviderConfigurationResolution(nil, validUnsecreter); err == nil || resolution != nil {
		t.Fatalf("resolution=%#v error=%v", resolution, err)
	}
	if resolution, err := newCurrentProviderConfigurationResolution(validExpander, nil); err == nil || resolution != nil {
		t.Fatalf("resolution=%#v error=%v", resolution, err)
	}
	resolution, err := newCurrentProviderConfigurationResolution(validExpander, validUnsecreter)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolution.ResolveCurrentProviderConfiguration(
		context.Background(),
		configurationapp.CurrentProviderConfigurationResolution{},
	); !errors.Is(err, errCurrentProviderConfigurationResolution) {
		t.Fatalf("error=%v", err)
	}
}

type currentLifecycleExpanderStub struct {
	expand func(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error)
}

func (stub currentLifecycleExpanderStub) Expand(
	ctx context.Context,
	request configurationapp.CurrentExpansionRequest,
) (map[string]any, error) {
	return stub.expand(ctx, request)
}

type currentLifecycleUnsecreterStub struct {
	unsecret func(context.Context, int32, map[string]any) (map[string]any, error)
}

func (stub currentLifecycleUnsecreterStub) Unsecret(
	ctx context.Context,
	projectID int32,
	data map[string]any,
) (map[string]any, error) {
	return stub.unsecret(ctx, projectID, data)
}
