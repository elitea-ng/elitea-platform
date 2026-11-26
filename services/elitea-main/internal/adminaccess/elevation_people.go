package adminaccess

// The people an elevation cannot remove (ADR-0059 decision 5, "What an
// elevation cannot do", "Five rules are not per action"; story R2.8b, issue
// 2602).
//
// An elevated person cannot demote, suspend or delete a holder of
// access.breakglass.approve or access.breakglass.review: the people who can
// stop and review the elevation stay. The rule is about PEOPLE, so it is not a
// value of the action table: the same suspension route that an elevated
// person may use on anybody else is refused for these.
//
// It is one check, called by every handler that demotes, suspends or deletes
// a person: both assignment routes (through the shared checkCentralRoles of
// internal/api/v2/admin), user suspension and user deletion. A matrix edit
// that would strip the permission from a role is already `deny` under an
// elevation, and a SCIM push cannot carry an elevation.
//
// The holders are read from STANDING roles, under the role-writes lock the
// caller holds, so a concurrent assignment cannot make a protected person
// unprotected between the check and the write.

import (
	"context"
	"fmt"
)

// CodeElevationProtectedPerson is the stable code of the refusal.
const CodeElevationProtectedPerson = "elevation_protected_person"

// ElevationProtectedPermissions are the permissions whose holders an elevated
// person cannot remove.
var ElevationProtectedPermissions = []string{AccessBreakglassApprove, AccessBreakglassReview}

// ElevationProtectedPersonRefusal is the 403 of the rule.
func ElevationProtectedPersonRefusal() *Refusal {
	return &Refusal{
		Code: CodeElevationProtectedPerson,
		Message: "an elevated session cannot demote, suspend or delete a person who can approve or review " +
			"break-glass access",
		Permissions: append([]string(nil), ElevationProtectedPermissions...),
	}
}

// ElevationProtectedTargets refuses a demotion, suspension or deletion of any
// of targetIDs by an elevated caller when one of them holds
// access.breakglass.approve or access.breakglass.review through a standing
// role. A caller that is not elevated is never refused here. q is the
// transaction of the write, holding the role-writes lock.
func ElevationProtectedTargets(ctx context.Context, q Querier, caller Caller, targetIDs ...int64) (*Refusal, error) {
	if !caller.Elevated || len(targetIDs) == 0 {
		return nil, nil
	}
	protected, err := holdsStandingAny(ctx, q, targetIDs, ElevationProtectedPermissions)
	if err != nil {
		return nil, err
	}
	if protected {
		return ElevationProtectedPersonRefusal(), nil
	}
	return nil, nil
}

// holdsStandingAny reports whether one of the accounts holds one of the
// permissions through a standing administration role.
func holdsStandingAny(ctx context.Context, q Querier, userIDs []int64, permissions []string) (bool, error) {
	var held bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user account
    JOIN public.auth_core__user_role assignment ON assignment.user_id = account.id
    JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = $3
    JOIN public.auth_core__role_permission grant_row
      ON grant_row.role_id = role.id AND grant_row.permission = ANY($2::text[])
    WHERE account.id = ANY($1::bigint[]))`, userIDs, permissions, ModeAdministration).Scan(&held); err != nil {
		return false, fmt.Errorf("read the protected people: %w", err)
	}
	return held, nil
}
