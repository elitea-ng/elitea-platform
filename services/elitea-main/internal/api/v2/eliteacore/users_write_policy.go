package eliteacore

// The no-escalation rule on project membership (issue 2488, ADR-0059 R0.8).
//
// The three write verbs give and take PROJECT roles. Project roles never
// resolve in the administration plane (internal/rolepolicy), so the risk here
// is inside the project. The rule depends on how the caller reached the write:
//
//   - Through the `{mode}` routes, the gate resolves the caller's permissions
//     IN THIS PROJECT, so the caller is a member acting in it. The caller then
//     holds every permission of every role given and of every role taken away,
//     measured in this project. A member with `configuration.users.users.edit`
//     cannot make anybody, the caller included, a project admin unless the
//     caller holds all of the admin role.
//   - Through the static `administration` routes, the gate is central and the
//     caller is an operator of every project, who holds no project permission
//     by role. There the rule is the effect on the caller: an operator cannot
//     give the caller's own account a role whose permissions the caller does
//     not already hold in the project.
//
// A holder of admin.auth.users.super_admin is exempt from both.

import (
	"net/http"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
)

// membershipChange is one write's effect on project roles.
type membershipChange struct {
	projectID int
	// asMember is true for the `{mode}` routes (default-mode gate).
	asMember bool
	// assigned are the project role ids the write gives.
	assigned []int
	// targets are the users the write changes.
	targets []int
	// takesCurrent is true when the write removes the targets' current roles
	// in the project (update and delete).
	takesCurrent bool
}

// checkMembershipChange returns the refusal of a change, or nil.
func (h *Handler) checkMembershipChange(r *http.Request, change membershipChange) *rolepolicy.Refusal {
	caller, refusal := rolepolicy.ResolveCaller(r, h.permissionResolver)
	if refusal != nil {
		return refusal
	}
	if caller.SuperAdmin {
		return nil
	}
	ctx := r.Context()
	unreadable := rolepolicy.Unresolved("the project roles could not be read, so the role change was refused")

	roleIDs := append([]int{}, change.assigned...)
	if !change.asMember {
		// The operator path checks the effect on the caller only.
		self := false
		for _, target := range change.targets {
			if int64(target) == caller.UserID {
				self = true
				break
			}
		}
		if !self {
			return nil
		}
	} else if change.takesCurrent {
		current, err := rolepolicy.MembersProjectRoleIDs(ctx, h.pool, change.projectID, change.targets)
		if err != nil {
			return unreadable
		}
		roleIDs = append(roleIDs, current...)
	}

	required, err := rolepolicy.ProjectRolesPermissions(ctx, h.pool, change.projectID, roleIDs)
	if err != nil {
		return unreadable
	}
	held, err := caller.ProjectSet(ctx, change.projectID)
	if err != nil {
		return rolepolicy.Unresolved("your own permissions could not be read, so the role change was refused")
	}
	missing := rolepolicy.Missing(held, required)
	if len(missing) == 0 {
		return nil
	}
	if !change.asMember {
		return rolepolicy.SelfEscalation(rolepolicy.ProjectGain{
			ProjectID: int64(change.projectID), Permissions: missing,
		})
	}
	refusal = rolepolicy.NotHeld(
		"you can give or take only project roles whose permissions you all hold in this project", "", missing)
	refusal.ProjectID = int64(change.projectID)
	return refusal
}

// userIDsByEmail resolves the invited addresses that already have an account.
// An address with no account cannot be the caller.
func (h *Handler) userIDsByEmail(r *http.Request, emails []string) ([]int, error) {
	normalized := make([]string, 0, len(emails))
	for _, email := range emails {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(email)))
	}
	rows, err := h.pool.Query(r.Context(),
		`SELECT id FROM auth_core__user WHERE lower(email) = ANY($1::text[])`, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
