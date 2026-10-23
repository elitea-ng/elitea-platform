package repos

import (
	"context"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestConfigurationTargetUsesAuthorizedResourceProjectTenantContext(t *testing.T) {
	catalog := runtimedomain.SHA256([]byte("catalog"))
	schema := runtimedomain.SHA256([]byte("schema"))
	settings := runtimedomain.SHA256([]byte(`{}`))
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"openapi", "catalog-v1", catalog[:], "openapi", "schema-v1", schema[:], "settings", "revision-1", settings[:],
	}}}}
	projects := &scriptedProjectStore{scriptedExecutor: executor}
	repository, err := newConfigurationTargetsRepository(projects)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repository.ResolveValidationTarget(context.Background(), executionapp.AdmissionIdentity{
		TenantID:            "tenant-1",
		ResourceProjectID:   "42",
		ProjectionProjectID: "99",
		ActorID:             "actor-1",
	}, "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if projects.projectID != 42 {
		t.Fatalf("projection project selected tenant schema: got %d", projects.projectID)
	}
	if target.CatalogDigest != catalog || target.SchemaDigest != schema || target.ExpectedSettingsDigest != settings || target.SettingsVersion != "revision-1" {
		t.Fatalf("unexpected immutable target: %+v", target)
	}
}

func TestConfigurationTargetRejectsMalformedStoredSettingsDigest(t *testing.T) {
	catalog := runtimedomain.SHA256([]byte("catalog"))
	schema := runtimedomain.SHA256([]byte("schema"))
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"openapi", "catalog-v1", catalog[:], "openapi", "schema-v1", schema[:], "settings", "revision-1", []byte("short"),
	}}}}
	repository, err := newConfigurationTargetsRepository(&scriptedProjectStore{scriptedExecutor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ResolveValidationTarget(context.Background(), executionapp.AdmissionIdentity{
		TenantID:            "tenant-1",
		ResourceProjectID:   "42",
		ProjectionProjectID: "99",
		ActorID:             "actor-1",
	}, "revision-1")
	if err == nil {
		t.Fatal("malformed stored settings digest was accepted")
	}
}
