package configurations

import (
	"context"
	"errors"
	"strings"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type targetResolverStub struct {
	target   ValidationTarget
	identity executionapp.AdmissionIdentity
	revision string
}

func (s *targetResolverStub) ResolveValidationTarget(_ context.Context, identity executionapp.AdmissionIdentity, revision string) (ValidationTarget, error) {
	s.identity = identity
	s.revision = revision
	return s.target, nil
}

type bundleFactoryStub struct {
	settings []byte
}

func (s *bundleFactoryStub) BuildValidationInput(_ context.Context, _ string, entryID, entryVersion string, settings []byte) (executiondomain.InputBundle, error) {
	s.settings = settings
	manifest := []byte(`{"entries":["settings"]}`)
	return executiondomain.InputBundle{
		ID:        "bundle-1",
		Version:   "bundle-v1",
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest),
		Manifest:  manifest,
		Entries: []executiondomain.InputEntry{{
			ID:                    entryID,
			Version:               entryVersion,
			SemanticRole:          "configuration.settings",
			ContentID:             "content-1",
			MediaType:             executiondomain.SettingsJSONMediaType,
			Classification:        "synthetic",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
			ContentDigest:         runtimedomain.SHA256(settings),
			ContentLength:         int64(len(settings)),
			Content:               append([]byte(nil), settings...),
		}},
	}, nil
}

type jobSubmitterStub struct {
	request executionapp.SubmitValidationRequest
}

func (s *jobSubmitterStub) SubmitValidation(_ context.Context, request executionapp.SubmitValidationRequest) (executionapp.AdmissionOutcome, error) {
	s.request = request
	return executionapp.AdmissionOutcome{ExecutionID: "execution-1", CommandID: "command-1", Created: true}, nil
}

func TestSubmitValidationSelectsTrustedCatalogAndPreservesSettingsBytes(t *testing.T) {
	settings := []byte(`{ "base_url":"https://github.example", "access_token":"token", "options":{"labels":["bug"]} }`)
	targets := &targetResolverStub{target: ValidationTarget{
		ConfigurationType:      "github",
		CatalogRevision:        "sdk-commit",
		CatalogDigest:          runtimedomain.SHA256([]byte("catalog")),
		SchemaID:               "elitea.configuration.github",
		SchemaRevision:         "schema-v1",
		SchemaDigest:           runtimedomain.SHA256([]byte("schema")),
		SettingsEntryID:        "settings",
		SettingsVersion:        "revision-1",
		ExpectedSettingsDigest: runtimedomain.SHA256(settings),
	}}
	bundles := &bundleFactoryStub{}
	jobs := &jobSubmitterStub{}
	service, err := NewSubmitValidationService(targets, bundles, jobs)
	if err != nil {
		t.Fatal(err)
	}
	identity := executionapp.AdmissionIdentity{
		TenantID:            "tenant-1",
		ResourceProjectID:   "project-1",
		ProjectionProjectID: "project-1",
		ActorID:             "actor-1",
	}
	outcome, err := service.Submit(context.Background(), SubmitValidationRequest{
		Identity:                identity,
		ConfigurationRevisionID: "revision-1",
		IdempotencyKey:          "key-1",
		Settings:                settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Created || jobs.request.Command.ConfigurationType != "github" || jobs.request.Command.CatalogRevision != "sdk-commit" {
		t.Fatalf("unexpected admitted command: %+v", jobs.request.Command)
	}
	if targets.identity != identity || targets.revision != "revision-1" {
		t.Fatal("target resolution did not bind trusted identity and revision")
	}
	settings[0] = '['
	if got := string(bundles.settings); got != `{ "base_url":"https://github.example", "access_token":"token", "options":{"labels":["bug"]} }` {
		t.Fatalf("bundle factory input aliased public request bytes: %q", got)
	}
}

func TestSubmitValidationRejectsSettingsThatDoNotMatchImmutableRevision(t *testing.T) {
	targets := &targetResolverStub{target: ValidationTarget{
		ConfigurationType:      "pgvector",
		CatalogRevision:        "sdk-commit",
		CatalogDigest:          runtimedomain.SHA256([]byte("catalog")),
		SchemaID:               "elitea.configuration.pgvector",
		SchemaRevision:         "schema-v1",
		SchemaDigest:           runtimedomain.SHA256([]byte("schema")),
		SettingsEntryID:        "settings",
		SettingsVersion:        "revision-1",
		ExpectedSettingsDigest: runtimedomain.SHA256([]byte(`{}`)),
	}}
	bundles := &bundleFactoryStub{}
	jobs := &jobSubmitterStub{}
	service, err := NewSubmitValidationService(targets, bundles, jobs)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Submit(context.Background(), SubmitValidationRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-1",
			ResourceProjectID:   "project-1",
			ProjectionProjectID: "project-1",
			ActorID:             "actor-1",
		},
		ConfigurationRevisionID: "revision-1",
		IdempotencyKey:          "key-1",
		Settings:                []byte(`{ }`),
	})
	if !errors.Is(err, ErrInvalidValidationAdmission) {
		t.Fatalf("expected immutable settings mismatch, got %v", err)
	}
	if bundles.settings != nil || jobs.request.Command.ConfigurationRevisionID != "" {
		t.Fatal("immutable settings mismatch reached bundle construction or job submission")
	}
}

func TestSubmitValidationRejectsSettingsAboveBoundBeforeDependencies(t *testing.T) {
	targets := &targetResolverStub{}
	service, err := NewSubmitValidationService(targets, &bundleFactoryStub{}, &jobSubmitterStub{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Submit(context.Background(), SubmitValidationRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-1",
			ResourceProjectID:   "project-1",
			ProjectionProjectID: "project-1",
			ActorID:             "actor-1",
		},
		ConfigurationRevisionID: "revision-1",
		IdempotencyKey:          "key-1",
		Settings:                make([]byte, MaxValidationSettingsBytes+1),
	})
	if err == nil {
		t.Fatal("oversized settings admitted")
	}
	if targets.revision != "" {
		t.Fatal("oversized settings reached target resolver")
	}
}

func TestSettingsJSONAdmissionIsProviderNeutral(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		wantErr  error
	}{
		{name: "empty object", settings: `{}`},
		{name: "arbitrary provider fields", settings: `{"base_url":"https://github.example","access_token":"token"}`},
		{name: "credential fields are configuration owned", settings: `{"api_key":"token","client_secret":null}`},
		{name: "nested object and array", settings: `{"extension":{"headers":[{"name":"X-API-Key","value":"token"}]}}`},
		{name: "json scalars", settings: `{"null":null,"number":123,"decimal":1.25,"boolean":false,"string":"value"}`},
		{name: "duplicate top-level key", settings: `{"type":"github","type":"gitlab"}`, wantErr: ErrInvalidValidationAdmission},
		{name: "escaped duplicate key", settings: `{"type":"github","\u0074ype":"gitlab"}`, wantErr: ErrInvalidValidationAdmission},
		{name: "duplicate nested key", settings: `{"extension":{"enabled":true,"enabled":false}}`, wantErr: ErrInvalidValidationAdmission},
		{name: "duplicate key inside array", settings: `{"rules":[{"name":"one","name":"two"}]}`, wantErr: ErrInvalidValidationAdmission},
		{name: "non object", settings: `[]`, wantErr: ErrInvalidValidationAdmission},
		{name: "scalar", settings: `"github"`, wantErr: ErrInvalidValidationAdmission},
		{name: "non finite token", settings: `{"value":NaN}`, wantErr: ErrInvalidValidationAdmission},
		{name: "non finite exponent", settings: `{"value":1e9999}`, wantErr: ErrInvalidValidationAdmission},
		{name: "trailing value", settings: `{} {}`, wantErr: ErrInvalidValidationAdmission},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSettingsJSON([]byte(test.settings))
			if test.wantErr == nil && err != nil {
				t.Fatalf("bounded provider settings rejected: %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("expected %v, got %v", test.wantErr, err)
			}
		})
	}
}

func TestSettingsJSONStructuralLimitsMatchWorkerPolicy(t *testing.T) {
	maximumDepth := `{"nested":` + strings.Repeat(`{"value":`, maxValidationJSONDepth-1) + `null` + strings.Repeat(`}`, maxValidationJSONDepth)
	deep := `{"nested":` + strings.Repeat(`{"value":`, maxValidationJSONDepth) + `null` + strings.Repeat(`}`, maxValidationJSONDepth+1)
	tests := []struct {
		name     string
		settings string
		wantErr  error
	}{
		{name: "maximum depth", settings: maximumDepth},
		{name: "depth", settings: deep, wantErr: ErrValidationInputLimitExceeded},
		{name: "long key", settings: `{"` + strings.Repeat("k", maxValidationJSONString+1) + `":null}`, wantErr: ErrValidationInputLimitExceeded},
		{name: "long string", settings: `{"value":"` + strings.Repeat("v", maxValidationJSONString+1) + `"}`, wantErr: ErrValidationInputLimitExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSettingsJSON([]byte(test.settings))
			if test.wantErr == nil && err != nil {
				t.Fatalf("maximum bounded settings rejected: %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("expected %v, got %v", test.wantErr, err)
			}
		})
	}
}

func TestSettingsJSONRejectsInvalidUTF8AndOversizedBodies(t *testing.T) {
	if err := validateSettingsJSON([]byte{'{', '"', 0xff, '"', ':', '1', '}'}); !errors.Is(err, ErrInvalidValidationAdmission) {
		t.Fatalf("invalid UTF-8 returned %v", err)
	}
	if err := validateSettingsJSON(make([]byte, MaxValidationSettingsBytes+1)); !errors.Is(err, ErrInvalidValidationAdmission) {
		t.Fatalf("oversized settings returned %v", err)
	}
}
