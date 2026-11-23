package identityrepo

import (
	"context"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// GlobalAdministratorExists reports whether an unsuspended account holds
// admin.auth.users.super_admin through an administration-mode role: the
// condition that ends identity.initial_global_admins as a bootstrap (issue
// 2589). A suspended holder does not count: a deployment whose only global
// administrator is suspended still needs the list to recover.
func GlobalAdministratorExists(ctx context.Context, db sqlcgen.DBTX) (bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role
      ON role.id = assignment.role_id AND role.mode = $1
    JOIN public.auth_core__role_permission grant_row
      ON grant_row.role_id = role.id AND grant_row.permission = $2
    JOIN public.auth_core__user account
      ON account.id = assignment.user_id AND NOT account.suspended
)`, adminaccess.ModeAdministration, adminaccess.SuperAdminPermission).Scan(&exists); err != nil {
		return false, fmt.Errorf("identityrepo: look for a global administrator: %w", err)
	}
	return exists, nil
}

// InitialGlobalAdminsIgnored reports that `identity.initial_global_admins`
// is configured while a global administrator exists, so every entry is
// ignored at sign-in. It is for the access posture list (ADR-0059 R2.14),
// which warns about a list left configured after the bootstrap.
func InitialGlobalAdminsIgnored(ctx context.Context, db sqlcgen.DBTX, admins []string) (bool, error) {
	if len(admins) == 0 {
		return false, nil
	}
	return GlobalAdministratorExists(ctx, db)
}
