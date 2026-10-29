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
// project's runs reach, and takes the same gate.
const ProjectSettingsEditPermission = "models.project_settings.edit"

// projectSettingsWriteForbidden reports a write of a type that needs the
// project settings permission by a caller who lacks it.
func (h *Handler) projectSettingsWriteForbidden(ctx context.Context, projectID string, typeNames ...string) bool {
	guarded := false
	for _, name := range typeNames {
		switch strings.TrimSpace(name) {
		case configurationapp.CloudExecutorConfigurationType, configurationapp.WorkerEgressConfigurationType:
			guarded = true
		}
	}
	if !guarded {
		return false
	}
	permissions, ok := middleware.ResolvedProjectPermissions(ctx, auth.PermissionModeDefault, projectID)
	if !ok {
		user, hasUser := auth.UserFromContext(ctx)
		if !hasUser || h.permissionResolver == nil {
			return true
		}
		resolution, err := h.permissionResolver.ResolvePermissions(ctx, user, auth.PermissionModeDefault, projectID)
		if err != nil {
			return true
		}
		permissions = resolution.Permissions
	}
	return !slices.Contains(permissions, ProjectSettingsEditPermission)
}
