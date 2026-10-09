// Package projectaccess owns the SQL meaning of "this user may act inside this
// project". It is the one definition shared by the HTTP gate
// (internal/api/middleware RequireProjectAccess) and by writes that must make
// the same decision inside their own statement, so the two cannot drift.
//
// The rule: the user holds a role assignment in the project, or holds the
// central `super_admin` role in the `administration` mode. auth_core__role is
// UNIQUE (name, mode), so a database also carries `super_admin` in the
// `default` and `developer` modes; only the administration mode grants
// cross-project access.
package projectaccess

import "fmt"

// Membership returns a boolean SQL expression that is true when the user bound
// at placeholder userParam may act inside the project bound at projectParam.
// Both placeholders are integers. The expression reads only shared auth tables;
// it never names a tenant schema, so it is safe to evaluate for a project whose
// schema does not exist.
func Membership(projectParam, userParam int) string {
	return fmt.Sprintf(`EXISTS (
					SELECT 1 FROM auth_core__project_user_role
					WHERE project_id = $%[1]d AND user_id = $%[2]d
					UNION ALL
					SELECT 1
					FROM auth_core__user_role ur
					JOIN auth_core__role role ON role.id = ur.role_id
					WHERE ur.user_id = $%[2]d AND role.name = 'super_admin'
						AND role.mode = 'administration'
				)`, projectParam, userParam)
}

// ProjectExists returns a boolean SQL expression that is true when
// centry.project holds a row for the project bound at projectParam. A member
// always implies existence, so it only matters on the administrator branch,
// which admits every project id.
func ProjectExists(projectParam int) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM centry.project WHERE id = $%d)`, projectParam)
}
