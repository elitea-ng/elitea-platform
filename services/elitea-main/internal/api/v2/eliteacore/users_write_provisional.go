package eliteacore

// The member writes of a central route under an elevation (ADR-0059
// decision 5, "A project membership given under an elevation is
// provisional"; story R2.12b, issue 2606). See adminaccess/provisional.go.

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// provisionalMember is one person a membership was given to under an
// elevation that is still active, in the project's member listing: the
// project's own administrators see who was let in, marked. Not paged: the
// listing's rows and total stay the standing members.
type provisionalMember struct {
	ID          string   `json:"id"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	Roles       []string `json:"roles"`
	ElevationID string   `json:"elevation_id"`
	GrantedBy   int64    `json:"granted_by"`
	Provisional bool     `json:"provisional"`
}

// provisionalMembers answers the provisional members of the project, by
// person; a read failure answers none (the listing degrades as its own reads
// do).
func (h *Handler) provisionalMembers(ctx context.Context, projectID int) []provisionalMember {
	out := []provisionalMember{}
	grants, err := adminaccess.ProvisionalProjectMembers(ctx, h.pool, int64(projectID), time.Now())
	if err != nil || len(grants) == 0 {
		return out
	}
	byUser := map[int64]int{}
	ids := []int64{}
	for _, grant := range grants {
		i, seen := byUser[grant.UserID]
		if !seen {
			i = len(out)
			byUser[grant.UserID] = i
			ids = append(ids, grant.UserID)
			out = append(out, provisionalMember{ID: strconv.FormatInt(grant.UserID, 10), Roles: []string{},
				ElevationID: grant.ElevationID.String(), GrantedBy: grant.GrantedBy, Provisional: true})
		}
		if !slices.Contains(out[i].Roles, grant.ProjectRoleName) {
			out[i].Roles = append(out[i].Roles, grant.ProjectRoleName)
		}
	}
	rows, err := h.pool.Query(ctx,
		`SELECT id, COALESCE(email, ''), COALESCE(name, '') FROM auth_core__user WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var email, name string
		if rows.Scan(&id, &email, &name) == nil {
			out[byUser[id]].Email, out[byUser[id]].Name = email, name
		}
	}
	return out
}

// replaceMemberRoles makes roleIDs the user's roles in the project, the
// standing write of the update: the rows go and come straight back in the
// transaction, so the user never stops being a member.
func replaceMemberRoles(ctx context.Context, tx pgx.Tx, projectID, userID int, roleIDs []int) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_core__project_user_role WHERE project_id = $1 AND user_id = $2`,
		projectID, userID); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
			 VALUES ($1, $2, $3) ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
			projectID, userID, roleID); err != nil {
			return err
		}
	}
	return nil
}

// replaceMemberRolesProvisionally is the update under an elevation: a role
// of roleIDs the user does not hold in the project is a provisional grant of
// the caller's elevation, and a held role that is not in roleIDs is removed,
// a standing removal that the restriction rule flags for the review. A role
// held and asked for is left as it is.
func replaceMemberRolesProvisionally(
	ctx context.Context, tx pgx.Tx, caller adminaccess.Caller, projectID, userID int, roleIDs []int,
) error {
	held, err := adminaccess.MembersProjectRoleIDs(ctx, tx, projectID, []int{userID})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_core__project_user_role
		 WHERE project_id = $1 AND user_id = $2 AND NOT (role_id = ANY($3::int[]))`,
		projectID, userID, roleIDs); err != nil {
		return err
	}
	given := slices.DeleteFunc(slices.Clone(roleIDs), func(id int) bool { return slices.Contains(held, id) })
	return adminaccess.GiveProvisionalMembership(ctx, tx, caller, int64(projectID), int64(userID), given, time.Now())
}
