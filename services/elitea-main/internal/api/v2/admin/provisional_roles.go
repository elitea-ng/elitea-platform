package admin

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
)

// centralAssignment is one write of central roles, after its rules passed
// under the role-writes lock: the roles given and taken. replace, when not
// nil, is the whole requested set of a set_admin_role (rolegrants.Replace).
type centralAssignment struct {
	userID  int
	mode    string
	given   []string
	taken   []string
	replace []string
}

// assignCentralRoles is the one write of central roles that both assignment
// routes call (set_admin_role and POST /admin/modes/administration).
//
// The caller under an elevation (ADR-0059 decision 5; story R2.12a): the
// administration roles it gives are provisional, rows of
// elitea_auth.admin_provisional_grants bound to the elevation, and are not
// written to auth_core__user_role (adminaccess.GiveProvisionalRoles). A
// standing global administrator in an elevated session is intercepted too:
// the session is elevated. A role taken is a standing removal, flagged for
// the review, as before. It answers the roles given provisionally, nil when
// none.
//
// Any other caller writes the standing assignment with its grant history, as
// before.
func assignCentralRoles(
	ctx context.Context, tx pgx.Tx, caller adminaccess.Caller, change centralAssignment, origin rolegrants.Origin,
) ([]string, error) {
	if caller.Elevated && change.mode == adminaccess.ModeAdministration {
		for _, role := range change.taken {
			if _, err := rolegrants.Revoke(ctx, tx, int64(change.userID), change.mode, role, origin); err != nil {
				return nil, err
			}
		}
		if len(change.given) == 0 {
			return nil, nil
		}
		return adminaccess.GiveProvisionalRoles(ctx, tx, caller, int64(change.userID), change.mode,
			change.given, time.Now())
	}
	if change.replace != nil {
		_, err := rolegrants.Replace(ctx, tx, int64(change.userID), change.mode, change.replace, origin)
		return nil, err
	}
	for _, role := range change.given {
		if _, err := rolegrants.Grant(ctx, tx, int64(change.userID), change.mode, role, origin); err != nil {
			return nil, err
		}
	}
	for _, role := range change.taken {
		if _, err := rolegrants.Revoke(ctx, tx, int64(change.userID), change.mode, role, origin); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
