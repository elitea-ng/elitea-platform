// The role policy: the no-escalation rule for every write that changes
// what a role grants or who holds a role (issue 2488, ADR-0059 R0.8).
//
// # The defect it closes
//
// Before this package, only the ASSIGNMENT of `super_admin` was guarded. The
// role matrix save (`PUT /admin/permissions/{scope}/{mode}`) had no check
// against the caller's own set, so a holder of the administration `admin` role
// could add `admin.auth.users.super_admin` to `admin` and become a global
// administrator in one request. `POST|DELETE /admin/modes/administration`
// assigned any role in the mode. Two ADR-0059 reviews (A-2, B-1) found it.
//
// # The rule, per plane
//
// A role resolves in exactly one plane, and the rule is stated in that plane.
//
// The ADMINISTRATION plane: a central role in the `administration` or
// `developer` mode. legacyrbac resolves administration permissions ONLY from
// these roles (auth_core__user_role joined to auth_core__role by mode). Here:
//
//   - a caller adds a permission to a role, or removes one from it, only if
//     the caller holds it in that mode (CodePermissionNotHeld). A removal
//     counts: a caller who could strip a permission it does not hold could
//     empty a role above it, super_admin included, beyond repair;
//   - a caller does not edit the set of a role the caller holds in that mode
//     (CodeOwnRoleLocked);
//   - a caller does not give or take the caller's OWN central roles, on
//     either assignment route (CodeSelfAssignment, ADR-0059 R0.8a). It is
//     decided before the subset rule, so the caller gets the specific code;
//   - a caller assigns or removes an administration role only if the caller
//     holds every permission of that role (CodePermissionNotHeld). The retired
//     developer mode grants nothing a route reads, so clearing one of its rows
//     needs only the super_admin guard.
//
// The PROJECT plane: a central `default`-mode role (the template every project
// inherits by role name) and an auth_core__project_role. These never resolve in
// the administration plane, so no project write can make anyone a global
// administrator. Inside a project:
//
//   - a project MEMBER who gives or takes project roles holds every permission
//     of those roles in that project (CodePermissionNotHeld);
//   - a SCIM group binding gives its role to members the directory pushes
//     later, the caller possibly among them, so its effect cannot be measured
//     when it is written. Creating, moving or deleting one needs every
//     permission of the role in its project (CodePermissionNotHeld);
//   - a central OPERATOR manages the members and roles of every project: that
//     is the authority its central gate grants, and ADR-0059 keeps it (R0.9
//     splits it out with its own roles). An operator usually holds no role in
//     the project, so the rule there is the effect on the caller: a membership
//     write, a bulk invite or a write of role CONTENTS (the template, Apply to
//     Projects, a project matrix, a rename) may not widen the caller's OWN set
//     in any project (CodeSelfEscalation). The contents writes are measured by
//     their effect, the caller's per-project set before and after, inside the
//     same transaction.
//
// The checks that a concurrent role write could outdate run under
// LockRoleWrites, inside the write's own transaction.
//
// In BOTH planes:
//
//   - a permission the registry marks `seeded_only` (GrantableSeededOnly:
//     `admin.auth.users.super_admin`, `identity.sso.trust.manage` and the four
//     `access.breakglass.*` names) is added to no role through the API
//     (CodeReservedPermission); only migrations grant them. The one exception
//     is the one #2555 kept: `admin.auth.users.super_admin` on the
//     administration `super_admin` role, which on a database with the seed
//     record is a locked built-in set anyway (below).
//   - A holder of `admin.auth.users.super_admin` is exempt from the subset
//     rule, the not-yourself rule and the own-role lock. super_admin keeps full
//     power, and a deployment can still repair any custom role.
//
// Bound to every caller, a global administrator included (ADR-0059 decision
// 3, rules 4 and 5, and the seed record of R0.7):
//
//   - the seeded_only names above;
//   - the set of a built-in role (a row in elitea_auth.admin_role_seeds) is
//     not edited through the API (CodeSetLocked);
//   - a role whose seed row is marked `unassignable` (the legacy `system`) is
//     assigned to nobody (CodeSetLocked);
//   - `break_glass` is not a role name (ReservedRoleName, a 400 of the
//     roles API).
//
// Every refusal is a 403 with a stable `code`, and an annotation on the audit
// row the request middleware writes for every refused admin write (event type
// `api`, action RefusalAuditAction).

package adminaccess

import (
	"sort"
	"strconv"
	"strings"
)

// SuperAdminPermission is the permission that separates `super_admin` from
// `admin`. Holding it exempts a caller from the subset and own-role rules.
const SuperAdminPermission = AdminAuthUsersSuperAdmin

// SuperAdminRole is the only role that may hold SuperAdminPermission.
const SuperAdminRole = "super_admin"

// ModeAdministration and ModeDeveloper are the central modes of the
// administration plane. ModeDefault is the project-plane template mode.
const (
	ModeAdministration = "administration"
	ModeDeveloper      = "developer"
	ModeDefault        = "default"
)

// The refusal codes. Clients key their message on these, so they are stable.
const (
	// CodePermissionNotHeld: the write adds, assigns or removes a permission
	// that the caller does not hold.
	CodePermissionNotHeld = "role_permission_not_held"
	// CodeOwnRoleLocked: the write changes the set of a role that the caller
	// holds.
	CodeOwnRoleLocked = "role_own_set_locked"
	// CodeReservedPermission: the write adds SuperAdminPermission to a role
	// other than the administration super_admin role.
	CodeReservedPermission = "role_permission_reserved"
	// CodeSuperAdminRequired: the write assigns or revokes the super_admin
	// role, and the caller does not hold SuperAdminPermission.
	CodeSuperAdminRequired = "role_super_admin_required"
	// CodeSelfEscalation: the write widens the caller's own permission set in
	// a project.
	CodeSelfEscalation = "role_self_escalation"
	// CodeCallerUnresolved: the caller's own permissions could not be read.
	// The rule fails closed.
	CodeCallerUnresolved = "role_caller_unresolved"
	// CodeSelfAssignment: the write gives or takes one of the caller's own
	// central roles, and the caller is not a standing global administrator.
	CodeSelfAssignment = "role_self_assignment"
	// CodeSetLocked: the write edits the set of a built-in role, or assigns a
	// built-in role marked unassignable. It binds a global administrator too.
	CodeSetLocked = "role_set_locked"
)

// ReservedRoleName is refused as the name of a new role, or as the new name of
// a rename (ADR-0059 decision 3): it is the name of the break-glass section.
const ReservedRoleName = "break_glass"

// SeededOnly reports whether the registry marks a permission seeded_only, so
// that no write through the API adds it to a role.
func SeededOnly(permission string) bool {
	row, ok := Lookup(permission)
	return ok && row.Grantable == GrantableSeededOnly
}

// RefusalAuditAction is the action the audit row of a refused write carries.
const RefusalAuditAction = "role.escalation_refused"

// AdministrationPlane reports whether a central role mode resolves in the
// administration plane.
func AdministrationPlane(mode string) bool {
	return mode == ModeAdministration || mode == ModeDeveloper
}

// Cell is one (role, permission) grant.
type Cell struct {
	Role       string
	Permission string
}

// Set is a permission set.
type Set map[string]bool

// SetOf builds a Set from names.
func SetOf(names ...string) Set {
	set := make(Set, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = true
		}
	}
	return set
}

// Missing returns the names in required that held does not contain, sorted
// and without duplicates.
func Missing(held Set, required []string) []string {
	seen := map[string]bool{}
	missing := []string{}
	for _, name := range required {
		if name == "" || held[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// Names returns the members of a set, sorted.
func (s Set) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckReserved refuses a grant of a seeded_only permission (SeededOnly) to
// any role. `mode` is the central mode of the roles, or "" for project roles.
// It applies to every caller. The one grant it lets through is the one #2555
// let through: SuperAdminPermission on the administration super_admin role.
func CheckReserved(mode string, granted []Cell) *Refusal {
	for _, cell := range granted {
		if !SeededOnly(cell.Permission) {
			continue
		}
		if cell.Permission == SuperAdminPermission {
			if mode == ModeAdministration && cell.Role == SuperAdminRole {
				continue
			}
			return &Refusal{
				Code: CodeReservedPermission,
				Message: SuperAdminPermission + " can be granted only to the administration " +
					SuperAdminRole + " role",
				Role:        cell.Role,
				Permissions: []string{SuperAdminPermission},
			}
		}
		return &Refusal{
			Code: CodeReservedPermission,
			Message: cell.Permission + " is granted by a migration only; " +
				"no change through the API adds it to a role",
			Role:        cell.Role,
			Permissions: []string{cell.Permission},
		}
	}
	return nil
}

// CentralEdit is one matrix save against an administration-plane mode.
type CentralEdit struct {
	// Caller is the caller's own set in the edited mode.
	Caller Set
	// SuperAdmin is true when the caller holds SuperAdminPermission.
	SuperAdmin bool
	// HeldRoles are the roles of the edited mode that the caller holds.
	HeldRoles map[string]bool
	Mode      string
	Granted   []Cell
	Revoked   []Cell
}

// CheckCentralEdit applies the administration-plane rules to one matrix save.
// The order is fixed: the reserved permission first (it binds super_admin
// too), then the own-role lock, then the subset rule.
func CheckCentralEdit(edit CentralEdit) *Refusal {
	if refusal := CheckReserved(edit.Mode, edit.Granted); refusal != nil {
		return refusal
	}
	if edit.SuperAdmin {
		return nil
	}
	for _, cells := range [][]Cell{edit.Granted, edit.Revoked} {
		for _, cell := range cells {
			if edit.HeldRoles[cell.Role] {
				return &Refusal{
					Code:    CodeOwnRoleLocked,
					Message: "you hold the " + cell.Role + " role, so you cannot change its permissions",
					Role:    cell.Role,
				}
			}
		}
	}
	// Revokes count too: a caller who could strip a permission it does not
	// hold could empty a role above it (super_admin included) beyond repair.
	cells := append(append([]Cell{}, edit.Granted...), edit.Revoked...)
	required := make([]string, 0, len(cells))
	for _, cell := range cells {
		required = append(required, cell.Permission)
	}
	if missing := Missing(edit.Caller, required); len(missing) > 0 {
		role := ""
		for _, cell := range cells {
			if !edit.Caller[cell.Permission] {
				role = cell.Role
				break
			}
		}
		return NotHeld("you can grant or revoke only permissions that you hold", role, missing)
	}
	return nil
}

// CheckAssignment applies the subset rule to an assignment or a removal: the
// caller holds every permission of every role that the write gives or takes.
// `roles` names the roles for the message; `required` is their union.
func CheckAssignment(caller Set, superAdmin bool, role string, required []string) *Refusal {
	if superAdmin {
		return nil
	}
	if missing := Missing(caller, required); len(missing) > 0 {
		return NotHeld("you can assign or remove only a role whose permissions you all hold", role, missing)
	}
	return nil
}

// NotHeld builds a CodePermissionNotHeld refusal.
func NotHeld(message, role string, missing []string) *Refusal {
	return &Refusal{
		Code:        CodePermissionNotHeld,
		Message:     message,
		Role:        role,
		Permissions: missing,
	}
}

// SuperAdminRequired is the refusal of a super_admin assignment or removal by
// a caller who does not hold SuperAdminPermission. The message is the one the
// routes have always returned, so existing clients keep showing it.
func SuperAdminRequired() *Refusal {
	return &Refusal{
		Code:    CodeSuperAdminRequired,
		Message: "only super_admin can assign or revoke the super_admin role",
		Role:    SuperAdminRole,
	}
}

// CheckSelfAssignment refuses an assignment or a removal whose target is the
// caller, unless the caller is a standing global administrator. `role` names
// the role the write gives or takes, for the message.
func CheckSelfAssignment(callerID int64, superAdmin bool, targetID int64, role string) *Refusal {
	if superAdmin || callerID <= 0 || callerID != targetID {
		return nil
	}
	return &Refusal{
		Code:    CodeSelfAssignment,
		Message: "you cannot change your own administration roles; another administrator must make this change",
		Role:    role,
	}
}

// SetLocked is the refusal of an edit to the set of the built-in roles
// `roles` (at least one, sorted). It binds a global administrator too.
func SetLocked(roles []string) *Refusal {
	quoted := make([]string, len(roles))
	for i, name := range roles {
		quoted[i] = strconv.Quote(name)
	}
	subject := quoted[0] + " is a built-in role"
	if len(quoted) > 1 {
		subject = strings.Join(quoted, ", ") + " are built-in roles"
	}
	return &Refusal{
		Code: CodeSetLocked,
		Message: subject + " and cannot be edited; " +
			"a built-in set is locked, so create a custom role for a different set",
		Role: roles[0],
	}
}

// Unassignable is the refusal of an assignment of a built-in role whose seed
// row is marked unassignable. It binds a global administrator too.
func Unassignable(role string) *Refusal {
	return &Refusal{
		Code:    CodeSetLocked,
		Message: strconv.Quote(role) + " is a locked built-in role that is never assigned",
		Role:    role,
	}
}

// Unresolved is the fail-closed refusal of a write whose check could not read
// what it needs.
func Unresolved(message string) *Refusal {
	return &Refusal{Code: CodeCallerUnresolved, Message: message}
}

// ProjectGain is a widening of the caller's own set in one project.
type ProjectGain struct {
	ProjectID   int64
	Permissions []string
}

// Widened compares the caller's per-project sets before and after a write. It
// returns the first project (lowest id) where the caller gained a permission.
func Widened(before, after map[int64]Set) (ProjectGain, bool) {
	ids := make([]int64, 0, len(after))
	for id := range after {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, id := range ids {
		gained := Missing(before[id], after[id].Names())
		if len(gained) > 0 {
			return ProjectGain{ProjectID: id, Permissions: gained}, true
		}
	}
	return ProjectGain{}, false
}

// SelfEscalation builds the CodeSelfEscalation refusal for a gain.
func SelfEscalation(gain ProjectGain) *Refusal {
	return &Refusal{
		Code:        CodeSelfEscalation,
		Message:     "this change would give you permissions that you do not hold",
		Permissions: gain.Permissions,
		ProjectID:   gain.ProjectID,
	}
}
