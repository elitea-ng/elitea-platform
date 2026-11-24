package admin

// The project_permissions_pushed events of the estate-wide permission pushes
// (ADR-0059 R1.5 follow-up, issue 2994): the personal-project and
// team-project matrix save (PUT /admin/user_project_permissions/administration)
// and Apply to Projects (POST /admin/permissions/administration/default).
//
// A push writes the same role in every project of its target set at once, on
// the reference deployment about 15k projects. One event per project would be
// 15k chained appends in the push's transaction for one decision, so the
// push appends ONE summary event per role whose grants or assignments it
// changed, on the platform partition: before names the permissions it
// removed from the role somewhere, after those it added somewhere. The event
// is appended in the push's transaction, so a failed append fails the push.

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The target sets of a push, the first half of the subject id.
const (
	pushTargetPersonal = "personal"
	pushTargetTeam     = "team"
)

// pushRolesAssignedField is the Fields value of a push that also gave the
// role to every member of the target projects (append_user_role).
const pushRolesAssignedField = "roles_assigned"

// rolePush is what one push did to one role, across its target projects.
type rolePush struct {
	removed, added []string
	assigned       bool
}

// pushChanges collects the rolePush of each role a push touched.
type pushChanges map[string]*rolePush

func (p pushChanges) role(name string) *rolePush {
	if p[name] == nil {
		p[name] = &rolePush{}
	}
	return p[name]
}

// pushedGrants wraps a grant INSERT or DELETE whose RETURNING is
// (role_id, permission) so the statement answers one (role name, permission,
// rows) row per distinct pair, not one row per project: the push's result
// stays bounded by roles times permissions however many projects it reaches.
func pushedGrants(statement string) string {
	return `WITH changed AS (` + statement + `)
SELECT role.name, changed.permission, count(*)
FROM changed JOIN public.auth_core__project_role role ON role.id = changed.role_id
GROUP BY role.name, changed.permission`
}

// changedPermissionsByRole runs a pushedGrants statement and records its
// pairs on p, as added or removed. It answers the number of grants written.
func (p pushChanges) changedPermissionsByRole(
	ctx context.Context, tx pgx.Tx, added bool, statement string, args ...any,
) (int64, error) {
	rows, err := tx.Query(ctx, pushedGrants(statement), args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var role, permission string
		var written int64
		if err := rows.Scan(&role, &permission, &written); err != nil {
			return 0, err
		}
		count += written
		push := p.role(role)
		if added {
			push.added = append(push.added, permission)
		} else {
			push.removed = append(push.removed, permission)
		}
	}
	return count, rows.Err()
}

// appendPushEvents appends the summary event of every role the push changed,
// in role name order, in tx. mode is "default" for Apply to Projects.
func (h *Handler) appendPushEvents(
	ctx context.Context, tx pgx.Tx, r *http.Request, actorUserID int64, target, mode string, changes pushChanges,
) error {
	roles := make([]string, 0, len(changes))
	for role := range changes {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	for _, role := range roles {
		push := changes[role]
		removed, added := sortedSet(push.removed), sortedSet(push.added)
		if len(removed) == 0 && len(added) == 0 && !push.assigned {
			continue
		}
		change := securityaudit.AdminAccessChanged{
			ActorUserID: actorUserID,
			SubjectType: "project_roles",
			SubjectID:   target + ":" + role,
			Change: securityaudit.AccessChange{
				Kind:   securityaudit.AccessProjectPermissionsPushed,
				Mode:   mode,
				Before: removed,
				After:  added,
			},
		}
		if push.assigned {
			change.Change.Fields = []string{pushRolesAssignedField}
		}
		if err := h.appendAccessChange(ctx, tx, r, change); err != nil {
			return err
		}
	}
	return nil
}
