package adminaccess

import (
	"net/http"
	"slices"
	"strings"
)

// CodePermissionDeprecated: the write adds a legacy permission (Legacy in the
// registry: runtime.plugins, configuration.governance) to a role that does not
// hold it (ADR-0059 decision 2, story R0.14a). It is a 400, not a 403: the
// caller may be fully authorized, and the request itself is invalid.
const CodePermissionDeprecated = "permission_deprecated"

// CheckDeprecated refuses a role write that adds a legacy name to a role.
//
// `granted` are the NEW cells of the write: a cell the role already holds is
// not in it, so a save that keeps an existing grant passes, and the existing
// grants keep working until the sunset date. It is decided before every other
// rule of a role write, so a global administrator gets it too, and it binds
// every scope and mode: no role newly receives a legacy name anywhere.
func CheckDeprecated(granted []Cell) *Refusal {
	var names, roles []string
	for _, cell := range granted {
		if !IsLegacy(cell.Permission) {
			continue
		}
		if !slices.Contains(names, cell.Permission) {
			names = append(names, cell.Permission)
		}
		if !slices.Contains(roles, cell.Role) {
			roles = append(roles, cell.Role)
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	slices.Sort(roles)
	return &Refusal{
		Code: CodePermissionDeprecated,
		Message: strings.Join(names, ", ") + " ends on the sunset date and can no longer be added to a role; " +
			"grant the narrow permission that replaces it",
		Role:        roles[0],
		Roles:       roles,
		Permissions: names,
		Status:      http.StatusBadRequest,
	}
}
