package eliteacore

// The no-escalation rule on project membership (issue 2488, ADR-0059 R0.8).
//
// The three write verbs give and take PROJECT roles. Project roles never
// resolve in the administration plane (internal/adminaccess), so the risk here
// is inside the project. The rule depends on how the caller reached the write:
//
//   - Through the `{mode}` routes, the gate resolves the caller's permissions
//     IN THIS PROJECT, so the caller is a member acting in it. The caller then
//     holds every permission of every role given and of every role taken away,
//     measured in this project. A member with `configuration.users.users.edit`
//     cannot make anybody, the caller included, a project admin unless the
//     caller holds all of the admin role.
//   - Through the static `administration` routes (the admin Projects page), the
//     gate is central. Managing the members of every project is the authority
//     that central gate grants, and ADR-0059 keeps it a central-operator power
//     (R0.9 splits it out with its own roles later). An operator usually holds
//     no role in the project at all, so a subset rule there would refuse every
//     member write it is entitled to. The rule there is the effect on the
//     caller: an operator cannot give its OWN account a role whose permissions
//     it does not already hold in the project.
//
// A holder of admin.auth.users.super_admin is exempt from both.

import (
	"net/http"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// membershipChange is one write's effect on project roles.
type membershipChange struct {
	projectID int
	// asMember is true for the `{mode}` routes (default-mode gate).
	asMember bool
	// assigned are the project role ids the write gives.
	assigned []int
	// targets are the users whose current roles the write removes (update and
	// delete).
	targets []int
	// invited are the addresses a create adds. Only the operator path reads
	// them, to find the caller's own account among them.
	invited []string
}

// checkMembershipChange returns the refusal of a change, or nil. A super_admin
// caller is answered before any query.
//
// `q` is a transaction that holds adminaccess.LockRoleWrites: the write's own
// transaction (update, delete) or a adminaccess.HoldRoleWrites hold that spans
// the per-address transactions (create). The caller is resolved here, after
// the lock, so the check and the write see the same state.
func (h *Handler) checkMembershipChange(
	r *http.Request, q adminaccess.Querier, change membershipChange,
) *adminaccess.Refusal {
	caller, refusal := adminaccess.ResolveCaller(r, h.permissionResolver)
	if refusal != nil {
		return refusal
	}
	if caller.SuperAdmin {
		return nil
	}
	ctx := r.Context()
	unreadable := adminaccess.Unresolved("the project roles could not be read, so the role change was refused")

	roleIDs := append([]int{}, change.assigned...)
	if change.asMember {
		if len(change.targets) > 0 {
			current, err := adminaccess.MembersProjectRoleIDs(ctx, q, change.projectID, change.targets)
			if err != nil {
				return unreadable
			}
			roleIDs = append(roleIDs, current...)
		}
	} else {
		self, err := callerIsAmong(r, q, caller.UserID, change)
		if err != nil {
			return unreadable
		}
		if !self {
			return nil
		}
	}

	required, err := adminaccess.ProjectRolesPermissions(ctx, q, change.projectID, roleIDs)
	if err != nil {
		return unreadable
	}
	held, err := caller.ProjectSet(ctx, change.projectID)
	if err != nil {
		return adminaccess.Unresolved("your own permissions could not be read, so the role change was refused")
	}
	missing := adminaccess.Missing(held, required)
	if len(missing) == 0 {
		return nil
	}
	if !change.asMember {
		return adminaccess.SelfEscalation(adminaccess.ProjectGain{
			ProjectID: int64(change.projectID), Permissions: missing,
		})
	}
	refusal = adminaccess.NotHeld(
		"you can give or take only project roles whose permissions you all hold in this project", "", missing)
	refusal.ProjectID = int64(change.projectID)
	return refusal
}

// callerIsAmong is true when the change reaches the caller's own account: as
// a target, or as one of the invited addresses. The address lookup runs only
// when there are addresses.
func callerIsAmong(r *http.Request, q adminaccess.Querier, callerID int64, change membershipChange) (bool, error) {
	if slices.Contains(change.targets, int(callerID)) {
		return true, nil
	}
	if len(change.invited) == 0 {
		return false, nil
	}
	normalized := make([]string, 0, len(change.invited))
	for _, email := range change.invited {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(email)))
	}
	var self bool
	err := q.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM auth_core__user WHERE id = $1 AND lower(email) = ANY($2::text[]))`,
		callerID, normalized).Scan(&self)
	return self, err
}
