package configurations

import (
	"context"
	"slices"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// ProjectSettingsEditPermission is the project settings write permission
// (shared 0136). The cloud executor project setting (ADR-0034 decision 11) is
// a configuration row, and its writes take this permission IN ADDITION to the
// configuration route's own: the setting decides whether a project's agents
// get a shell and the network, which is the project admin's decision.
const ProjectSettingsEditPermission = "models.project_settings.edit"

// projectSettingsWriteForbidden reports a write of a type that needs the
// project settings permission by a caller who lacks it.
func (h *Handler) projectSettingsWriteForbidden(ctx context.Context, projectID string, typeNames ...string) bool {
	guarded := false
	for _, name := range typeNames {
		if strings.TrimSpace(name) == configurationapp.CloudExecutorConfigurationType {
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
