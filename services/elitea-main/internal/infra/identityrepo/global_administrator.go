package identityrepo

import (
	"context"
	"fmt"
	"log/slog"

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

// initialAdministrationRoleDue reports whether the initial administration
// role is still to be granted to userID: the account holds no role in the
// mode (a demotion is never undone), and no global administrator exists. It
// logs the skip of a listed entry once the bootstrap is over, naming the
// entry's shape and never the address.
func initialAdministrationRoleDue(
	ctx context.Context,
	db sqlcgen.DBTX,
	userID int32,
	mode string,
	entryShape string,
) (bool, error) {
	count, err := sqlcgen.New(db).CountAuthUserRolesInMode(ctx, sqlcgen.CountAuthUserRolesInModeParams{
		UserID: userID,
		Mode:   mode,
	})
	if err != nil {
		return false, fmt.Errorf("identityrepo: count administration roles: %w", err)
	}
	if count != 0 {
		return false, nil
	}
	exists, err := GlobalAdministratorExists(ctx, db)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	slog.WarnContext(ctx,
		"identity.initial_global_admins entry ignored: the deployment already has a global administrator, "+
			"and the list applies only while there is none. An existing administrator assigns the role "+
			"(the admin console, or POST /api/v2/admin/auth_users/administration); "+
			"`elitea-main admin-role grant` covers a deployment with none",
		"entry_shape", entryShape, "user_id", userID)
	return false, nil
}
