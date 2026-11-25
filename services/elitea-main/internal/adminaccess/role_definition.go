package adminaccess

// What a role can do, and whether a caller may hand it out (ADR-0059
// decision 8, "Reading it"; story R3.9a, issue 2540). The Access page reads
// it through GET /api/v2/admin/access/role-definitions, so the browser never
// re-implements a rule of this package.
//
// "As areas" is ConsoleAccess, the function of the capability document,
// applied to a role's set instead of a caller's. The assignment answer is the
// order checkCentralRoles (internal/api/v2/admin/users.go) applies to one
// role: an unassignable role first, then the super_admin guard, then the
// not-yourself rule, then the subset rule. A write still decides for itself;
// this answer is what that write would say for one role, read without the
// role-writes lock.

// RoleStatus is the seed status of an administration role.
type RoleStatus string

// The statuses. A role with a seed row is built-in and its set is locked
// (CodeSetLocked); the marker says how.
const (
	// RoleStatusBuiltIn: the registry declares the set.
	RoleStatusBuiltIn RoleStatus = "built_in"
	// RoleStatusAsFound: locked to the set it held at the upgrade.
	RoleStatusAsFound RoleStatus = "as_found"
	// RoleStatusUnassignable: locked and given to nobody.
	RoleStatusUnassignable RoleStatus = "unassignable"
	// RoleStatusCustom: no seed row; the deployment made it.
	RoleStatusCustom RoleStatus = "custom"
)

// StoredRole is one administration role as the database holds it.
type StoredRole struct {
	Name string
	// Marker is the seed row's marker, "" for a role without one.
	Marker SeedMarker
	// Permissions are the role's grants.
	Permissions []string
}

// Status answers the role's seed status. Without the seed record
// (seedRecord false) no role is built-in, but `system` is still the service
// accounts' role and stays unassignable by name, as the assignment routes
// hold it.
func (r StoredRole) Status(seedRecord bool) RoleStatus {
	switch {
	case !seedRecord && r.Name == RoleSystem:
		return RoleStatusUnassignable
	case r.Marker == MarkerUnassignable:
		return RoleStatusUnassignable
	case r.Marker == MarkerAsFound:
		return RoleStatusAsFound
	case r.Marker == MarkerDeclared:
		return RoleStatusBuiltIn
	}
	return RoleStatusCustom
}

// Locked reports whether the role's set is edited by no caller through the
// API: a role with a seed row (CodeSetLocked).
func (r StoredRole) Locked() bool { return r.Marker != "" }

// Areas answers the role's set as the capability document would show it to
// a caller who held that set alone.
func (r StoredRole) Areas() []ConsoleAreaAccess { return ConsoleAccess(SetOf(r.Permissions...)) }

// Assigner is the caller of an assignment, as the role rules read it: the
// standing administration set, and whether it holds SuperAdminPermission.
type Assigner struct {
	Administration Set
	SuperAdmin     bool
}

// AssignRefusal answers the refusal a write that gives role to ANOTHER
// person would get from caller; nil when the rules admit it.
func (caller Assigner) AssignRefusal(role StoredRole, seedRecord bool) *Refusal {
	if refusal := caller.guards(role, seedRecord); refusal != nil {
		return refusal
	}
	return CheckAssignment(caller.Administration, caller.SuperAdmin, role.Name, role.Permissions)
}

// SelfRefusal answers the refusal a write that gives role to the caller, or
// takes it from the caller, would get; nil when the rules admit it (a
// standing global administrator changing a role other than an unassignable
// one).
func (caller Assigner) SelfRefusal(role StoredRole, seedRecord bool) *Refusal {
	if refusal := caller.guards(role, seedRecord); refusal != nil {
		return refusal
	}
	// The same principal on both sides: the rule refuses it unless the
	// caller is a standing global administrator.
	const self = 1
	return CheckSelfAssignment(self, caller.SuperAdmin, self, role.Name)
}

// guards are the two rules that come before the not-yourself and subset
// rules, in both directions: an unassignable role is given to nobody, and
// the super_admin role needs SuperAdminPermission (checkCentralRoles).
func (caller Assigner) guards(role StoredRole, seedRecord bool) *Refusal {
	if role.Status(seedRecord) == RoleStatusUnassignable {
		return Unassignable(role.Name)
	}
	if role.Name == SuperAdminRole && !caller.SuperAdmin {
		return SuperAdminRequired()
	}
	return nil
}
