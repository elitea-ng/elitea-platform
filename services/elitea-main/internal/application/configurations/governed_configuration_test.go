package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

const governedTestReference = "0192f0aa-0000-7000-8000-0000000000c1"

type governedTargetsStub struct {
	references map[string]credentialgrant.CredentialReference
	targets    map[string]credentialgrant.TargetBinding
}

func (s governedTargetsStub) CredentialReference(_ context.Context, tenantProjectID int64, id string) (credentialgrant.CredentialReference, error) {
	ref, ok := s.references[id]
	if !ok || ref.TenantProjectID != tenantProjectID {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	return ref, nil
}

func (s governedTargetsStub) TargetBinding(_ context.Context, tenantProjectID int64, id string) (credentialgrant.TargetBinding, error) {
	target, ok := s.targets[id]
	if !ok || target.TenantProjectID != tenantProjectID {
		return credentialgrant.TargetBinding{}, credentialgrant.ErrTargetNotFound
	}
	return target, nil
}

func governedTargets(t *testing.T, provider, identity string, state credentialgrant.ReferenceState) *GovernedConfigurationTargets {
	t.Helper()
	stub := governedTargetsStub{
		references: map[string]credentialgrant.CredentialReference{governedTestReference: {
			ID: governedTestReference, TenantProjectID: 7, Class: credentialgrant.ClassOAuthDelegated,
			ProviderType: provider, TargetBindingID: "target-1", State: state,
		}},
		targets: map[string]credentialgrant.TargetBinding{"target-1": {
			ID: "target-1", TenantProjectID: 7, ProviderType: provider, TargetIdentity: identity,
		}},
	}
	source, err := NewGovernedConfigurationTargets(stub, stub)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// governedConfigurationToolkit resolves one toolkit of type provider whose stored
// reference is ref, under ctx, with the REAL legacy expansion composed
// (CurrentExpansionService over a finder stub that holds the title). It
// answers the frozen settings, the finder's lookups and the error.
func governedConfigurationToolkit(t *testing.T, ctx context.Context, provider string, ref map[string]any, source GovernedConfigurationSource, mode CurrentToolkitSettingsMode) (map[string]any, []currentExpansionLookup, error) {
	t.Helper()
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "prod"}: {
			UUID: "33333333-3333-3333-3333-333333333333", ProjectID: 7, Type: provider,
			Data: map[string]any{"base_url": "https://legacy.example.test", "api_key": "{{secret.KEY}}"},
		},
	}}
	expander := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})
	field := provider + "_configuration"
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		provider: {Properties: map[string]any{
			field:   map[string]any{"configuration_types": []any{provider}},
			"limit": map[string]any{},
		}},
	}}
	deps := currentToolkitSettingsTestDependencies()
	resolver := newCurrentToolkitSettingsResolverForTest(t, schemas, deps.toolkits, expander, deps.models, deps.unsecreter)
	if source != nil {
		resolver.WithGovernedConfigurations(source)
	}
	settings, err := resolver.Resolve(ctx, CurrentToolkitSettingsRequest{
		ToolkitType: provider, ProjectID: 7, UserID: 42, Mode: mode,
		Settings: map[string]any{field: ref, "limit": json.Number("5")},
	})
	return settings, finder.calls, err
}

// Review of #1556: a governed run (WithGovernedRun) never calls the legacy
// title expansion. A reference that names its exact credential reference
// freezes only that reference's target, with no title lookup and no secret.
func TestGovernedRunResolvesTheConfigurationToItsTargetOnly(t *testing.T) {
	ctx := WithGovernedRun(context.Background())
	for _, c := range []struct{ provider, key, identity string }{
		{"jira", "base_url", "https://acme.atlassian.net"},
		{"confluence", "base_url", "https://acme.atlassian.net"},
		{"sharepoint", "site_url", "https://acme.sharepoint.com/sites/docs"},
	} {
		t.Run(c.provider, func(t *testing.T) {
			ref := map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: governedTestReference}
			for _, mode := range []CurrentToolkitSettingsMode{CurrentToolkitSettingsReferenceMode, CurrentToolkitSettingsClaimMode} {
				settings, lookups, err := governedConfigurationToolkit(t, ctx, c.provider,
					ref, governedTargets(t, c.provider, c.identity, credentialgrant.ReferenceActive), mode)
				if err != nil {
					t.Fatal(err)
				}
				if len(lookups) != 0 {
					t.Fatalf("the legacy title lookup ran: %v", lookups)
				}
				got, err := json.Marshal(settings[c.provider+"_configuration"])
				if err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(map[string]any{c.key: c.identity, CredentialReferenceIDField: governedTestReference})
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Fatalf("%s: frozen %s, want %s", mode, got, want)
				}
			}
		})
	}
}

// Without the governed source the legacy expansion still never runs: the
// governed run fails closed with ErrLegacyResolverInGovernedRun. Outside a
// governed run the legacy path is unchanged.
func TestGovernedRunWithoutTheSourceRefusesInsteadOfExpanding(t *testing.T) {
	ref := map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: governedTestReference}
	_, lookups, err := governedConfigurationToolkit(t, WithGovernedRun(context.Background()), "jira", ref, nil, CurrentToolkitSettingsReferenceMode)
	if !errors.Is(err, ErrLegacyResolverInGovernedRun) || len(lookups) != 0 {
		t.Fatalf("err = %v lookups %v", err, lookups)
	}
	_, lookups, err = governedConfigurationToolkit(t, context.Background(), "jira", ref,
		governedTargets(t, "jira", "https://acme.atlassian.net", credentialgrant.ReferenceActive), CurrentToolkitSettingsReferenceMode)
	if err != nil || len(lookups) != 1 {
		t.Fatalf("legacy run: err = %v lookups %v", err, lookups)
	}
}

// A title-only, private or malformed reference, a reference that is not
// active or not in the tenant, and one of another provider are violations:
// the run fails closed and no title is looked up.
func TestGovernedRunRefusesAReferenceItCannotUse(t *testing.T) {
	ctx := WithGovernedRun(context.Background())
	active := governedTargets(t, "jira", "https://acme.atlassian.net", credentialgrant.ReferenceActive)
	for name, c := range map[string]struct {
		ref    map[string]any
		source GovernedConfigurationSource
		code   CurrentToolkitSettingsViolationCode
	}{
		"title only": {map[string]any{"elitea_title": "prod", "private": false}, active, CurrentToolkitConfigurationReferenceInvalidCode},
		"malformed": {map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: "nope"}, active,
			CurrentToolkitConfigurationReferenceInvalidCode},
		"private": {map[string]any{"elitea_title": "prod", "private": true, CredentialReferenceIDField: governedTestReference}, active,
			CurrentToolkitConfigurationForbiddenCode},
		"retired": {map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: governedTestReference},
			governedTargets(t, "jira", "https://acme.atlassian.net", credentialgrant.ReferenceRetired), CurrentToolkitConfigurationNotFoundCode},
		"unknown": {map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: "0192f0aa-0000-7000-8000-0000000000c2"},
			active, CurrentToolkitConfigurationNotFoundCode},
		"other provider": {map[string]any{"elitea_title": "prod", "private": false, CredentialReferenceIDField: governedTestReference},
			governedTargets(t, "confluence", "https://acme.atlassian.net", credentialgrant.ReferenceActive), CurrentToolkitConfigurationTypeMismatchCode},
	} {
		t.Run(name, func(t *testing.T) {
			_, lookups, err := governedConfigurationToolkit(t, ctx, "jira", c.ref, c.source, CurrentToolkitSettingsReferenceMode)
			var validation *CurrentToolkitSettingsValidationError
			if !errors.As(err, &validation) || len(validation.Violations) != 1 || validation.Violations[0].Code != c.code || len(lookups) != 0 {
				t.Fatalf("err = %v lookups %v", err, lookups)
			}
		})
	}
}
