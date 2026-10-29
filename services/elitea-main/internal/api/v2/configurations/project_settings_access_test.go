package configurations

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type grantedPermissions []string

func (g grantedPermissions) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 3, Permissions: g}, nil
}

// The cloud executor project setting (ADR-0034 decision 11) is written under
// models.project_settings.edit as well as the configuration permission; every
// other type is not affected.
func TestCloudExecutorSettingNeedsTheProjectSettingsPermission(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), auth.User{UserID: "3", AuthType: "token"})
	editor := &Handler{permissionResolver: grantedPermissions{"configurations.configuration.create"}}
	admin := &Handler{permissionResolver: grantedPermissions{"configurations.configuration.create", ProjectSettingsEditPermission}}
	if !editor.projectSettingsWriteForbidden(ctx, "7", "cloud_executor") {
		t.Fatalf("an editor without models.project_settings.edit may write the cloud executor setting")
	}
	if editor.projectSettingsWriteForbidden(ctx, "7", "github") {
		t.Fatalf("an ordinary type was gated")
	}
	if !editor.projectSettingsWriteForbidden(ctx, "7", "github", "cloud_executor") {
		t.Fatalf("an update that changes a row INTO cloud_executor was not gated")
	}
	if admin.projectSettingsWriteForbidden(ctx, "7", "cloud_executor") {
		t.Fatalf("a project admin was refused")
	}
	if !(&Handler{}).projectSettingsWriteForbidden(context.Background(), "7", "cloud_executor") {
		t.Fatalf("a caller with no principal was allowed")
	}
}

// The worker egress project setting (ADR-0044 decision 2) widens what a
// project's runs reach, so its writes take the same gate.
func TestWorkerEgressSettingNeedsTheProjectSettingsPermission(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), auth.User{UserID: "3", AuthType: "token"})
	editor := &Handler{permissionResolver: grantedPermissions{"configurations.configuration.create"}}
	admin := &Handler{permissionResolver: grantedPermissions{"configurations.configuration.create", ProjectSettingsEditPermission}}
	if !editor.projectSettingsWriteForbidden(ctx, "7", "worker_egress") {
		t.Fatalf("an editor without models.project_settings.edit may write the worker egress setting")
	}
	if admin.projectSettingsWriteForbidden(ctx, "7", "worker_egress") {
		t.Fatalf("a project admin was refused")
	}
}
