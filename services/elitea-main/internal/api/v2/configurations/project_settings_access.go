package configurations

import (
	"context"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ProjectSettingsEditPermission is the project settings write permission
// (shared 0136). The cloud executor project setting (ADR-0034 decision 11) is
// a configuration row, and its writes take this permission IN ADDITION to the
// configuration route's own: the setting decides whether a project's agents
// get a shell and the network, which is the project admin's decision. The
// worker egress project setting (ADR-0044 decision 2) widens the hosts a
// project's runs reach, and takes the same gate. So does the cloud browser
// project setting (ADR-0049): it lets a project's runs drive a browser. And
// the desktop computer use setting (ADR-0047): it sends screenshots of a
// person's other apps to the platform.
const ProjectSettingsEditPermission = "models.project_settings.edit"

// projectSettingsPermissionMessage is the 403 of a guarded setting write by a
// caller the guarded-type rule refuses, on every implementation of the routes.
const projectSettingsPermissionMessage = "this setting needs the project settings permission"

// guardedProjectSettingTypes are the five guarded settings. The central
// override's allowlist (adminaccess.GuardedSettingTypes) names the same five;
// project_settings_central_test.go fails when the two lists differ.
var guardedProjectSettingTypes = []string{
	configurationapp.CloudExecutorConfigurationType, configurationapp.WorkerEgressConfigurationType,
	configurationapp.AIDisclosureConfigurationType, configurationapp.BrowserConfigurationType,
	configurationapp.ComputerUseConfigurationType,
}

// projectSettingsWriteForbidden reports a write of a type that needs the
// project settings permission by a caller who lacks it.
func (h *Handler) projectSettingsWriteForbidden(ctx context.Context, projectID string, typeNames ...string) bool {
	return projectSettingsWriteForbidden(ctx, h.permissionResolver, projectID, typeNames...)
}

// projectSettingsWriteForbidden is the guarded-type rule of every
// implementation of the project configurations writes: the compatibility
// Handler and the reviewed mutation route (#3024).
func projectSettingsWriteForbidden(
	ctx context.Context, resolver auth.PermissionResolver, projectID string, typeNames ...string,
) bool {
	// A request the gate admitted through the central override writes the
	// guarded types and nothing else, whatever the gate read: the decision is
	// made again on the types the handler is about to write.
	if admittedCentrally(ctx) {
		return !centralSettingsWrite(ctx, resolver, typeNames)
	}
	guarded := false
	for _, name := range typeNames {
		if slices.Contains(guardedProjectSettingTypes, strings.TrimSpace(name)) {
			guarded = true
		}
	}
	if !guarded {
		return false
	}
	if holdsProjectSettingsEdit(ctx, resolver, projectID) {
		return false
	}
	// The central branch (ADR-0059 R0.6b, project_settings_central.go): a
	// holder of projects.settings.manage writes the guarded types of any
	// project, and only those.
	return !centralSettingsWrite(ctx, resolver, typeNames)
}

// holdsProjectSettingsEdit reports whether the caller holds
// models.project_settings.edit in the project.
func holdsProjectSettingsEdit(ctx context.Context, resolver auth.PermissionResolver, projectID string) bool {
	permissions, ok := middleware.ResolvedProjectPermissions(ctx, auth.PermissionModeDefault, projectID)
	if !ok {
		user, hasUser := auth.UserFromContext(ctx)
		if !hasUser || resolver == nil {
			return false
		}
		resolution, err := resolver.ResolvePermissions(ctx, user, auth.PermissionModeDefault, projectID)
		if err != nil {
			return false
		}
		permissions = resolution.Permissions
	}
	return slices.Contains(permissions, ProjectSettingsEditPermission)
}
