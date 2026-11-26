package admin

// The ADMIN cross-project bulk membership invite —
// `POST /admin/invites_bulk/administration`.
//
// ## What it replaces
//
// The legacy runtime ships this capability as TWO admin console pages, each a
// free-text form over the same write:
//
//   - legacy/plugins/admin/api/v2/invites_bulkusers.py:36-67 — "add EVERY
//     platform user to ONE project with these roles". Body
//     `{project_id, roles: "admin,editor"}`; it walks `auth.list_users()`,
//     skips a user whose name starts with `:system:project:`
//     (invites_bulkusers.py:57), and calls
//     `admin_update_roles_for_user(project_id, user_id, roles)` per user.
//   - legacy/plugins/admin/api/v2/invites_bulkprojects.py:36-64 — "add ONE
//     user to MANY selected projects with these roles". Body
//     `{user_id, roles, projects: [{id, name}]}`, the same per-pair call.
//
// Both answer `{"ok": true, "logs": "<newline-joined English>"}` and both
// declare their own permission — `invites.bulkusers` and
// `invites.bulkprojects` (invites_bulkusers.py:35, invites_bulkprojects.py:35,
// and the console subsections at legacy/plugins/admin/module.py:507-535).
//
// One route serves both, because both are the same cross product with one side
// pinned: users x projects x role. A console that can select many of each does
// not need two pages, and two routes would be two places for the outcome
// reporting below to drift.
//
// ## Four deliberate divergences
//
//  1. IDS, NOT FREE TEXT. The legacy runtime posts `roles` as a comma-separated
//     string and `projects` as whatever rows the table widget had selected.
//     This takes `users` and `projects` as id arrays and `role` as ONE role
//     name, which is what the platform's typed role vocabulary already is
//     (auth_core__project_role). A textarea of permission strings is the shape
//     decisions.md §2.5 refuses to carry forward.
//
//  2. THE ROLE SET IS ADDED TO, NOT REPLACED. The legacy
//     `update_roles_for_user` (legacy/plugins/admin/rpc/roles.py:137-148) calls
//     `auth.update_project_user_roles`, which REPLACES every role the user
//     holds in the project. Run against "all users" that silently demotes every
//     existing project admin to the role the operator typed. This adds the one
//     role and leaves the rest alone, exactly as the project invite already
//     does (internal/api/v2/eliteacore/users_write.go:387-395).
//
//  3. PER-PAIR OUTCOMES, NOT A LOG BLOB. The legacy runtime returns `logs` as
//     one English string, so a caller cannot tell "added" from "was already
//     there" from "that project has no such role" without matching on prose.
//     Each (user, project) pair reports a machine-readable `outcome` here,
//     following the inviteResult precedent in users_write.go:94-103.
//
//  4. PERSONAL PROJECTS ARE REFUSED. The legacy runtime has no such guard: its
//     "add all users" form would happily write every account into
//     `project_user_9`. A personal project is one user's own tenant space — the
//     platform provisions exactly one per account and every resolver treats it
//     as that account's — so a bulk form may not put a stranger in it. The pair
//     is refused and says so, rather than being skipped in silence.
//
// ## Transactionality
//
// One transaction PER PAIR, not one for the batch. A batch-wide transaction
// would roll back forty good writes because the forty-first project does not
// define the role, and the operator would be told "nothing happened" for a
// request that was 97% valid. Per-pair matches the partial-success shape the
// result array reports, and is the same choice `inviteOne` makes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The permissions the legacy runtime declares on the two forms this route
// replaces. Both are accepted: the gate is an ANY-of intersection
// (internal/api/middleware/rbac.go hasIntersection), so an operator who held
// either console page holds this one. Shared migration 0121 grants both to the
// administration-mode `super_admin` and `admin`, which is the split
// legacy/plugins/admin/module.py:511-517 and :527-533 give them.
const (
	BulkInviteUsersPermission    = adminaccess.InvitesBulkUsers
	BulkInviteProjectsPermission = adminaccess.InvitesBulkProjects
)

// The values of bulkInviteResult.Outcome. Each is a different thing for an
// operator to do next, which is why they are not one "error".
const (
	bulkInviteOutcomeAdded           = "added"
	bulkInviteOutcomeAlreadyMember   = "already_member"
	bulkInviteOutcomeUnknownUser     = "unknown_user"
	bulkInviteOutcomeUnknownProject  = "unknown_project"
	bulkInviteOutcomeUnknownRole     = "unknown_role"
	bulkInviteOutcomeSystemUser      = "system_user"
	bulkInviteOutcomePersonalProject = "personal_project"
	bulkInviteOutcomeFailed          = "failed"
)

// maxBulkInvitePairs caps the cross product. Twenty thousand pairs is twenty
// thousand transactions on one request, which is a request that never returns.
// The limit is on the PRODUCT rather than on either list, because 500 users
// times 1 project is fine and 200 times 200 is not.
const maxBulkInvitePairs = 2000

// bulkInviteRequest is the console's submission: which accounts, which
// projects, which role.
type bulkInviteRequest struct {
	Users    []int64 `json:"users"`
	Projects []int64 `json:"projects"`
	Role     string  `json:"role"`
}

// bulkInviteResult is one (user, project) pair's outcome.
//
// `user_email` and `project_name` are carried so a console can render the
// result without holding both source lists — the same reason the legacy `logs`
// spelled the names out.
type bulkInviteResult struct {
	UserID      int64  `json:"user_id"`
	UserEmail   string `json:"user_email"`
	ProjectID   int64  `json:"project_id"`
	ProjectName string `json:"project_name"`
	Status      string `json:"status"`
	Outcome     string `json:"outcome"`
	Msg         string `json:"msg"`
	// Provisional says the membership was given under the caller's
	// elevation (ADR-0059 R2.12b): in effect while it is active, written to
	// the project only once its review confirms it.
	Provisional bool `json:"provisional"`
}

// bulkInviteSummary counts each outcome once, so a console can say "38 added,
// 2 already members" without walking the array. `ok` is false as soon as one
// pair did not end in `added` or `already_member`: those two are both "the
// user is in the project now", and everything else is not.
type bulkInviteSummary struct {
	OK        bool               `json:"ok"`
	Role      string             `json:"role"`
	Requested int                `json:"requested"`
	Added     int                `json:"added"`
	Skipped   int                `json:"skipped"`
	Failed    int                `json:"failed"`
	Results   []bulkInviteResult `json:"results"`
}

// projectTarget is one resolved project row.
type projectTarget struct {
	id         int64
	name       string
	isPersonal bool
	roleID     *int
}

// userTarget is one resolved account.
type userTarget struct {
	id       int64
	email    string
	name     string
	isSystem bool
}

// BulkInviteMembers serves `POST /admin/invites_bulk/administration`.
func (h *Handler) BulkInviteMembers(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	var body bulkInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	users := dedupeIDs(body.Users)
	projects := dedupeIDs(body.Projects)
	role := strings.TrimSpace(body.Role)
	switch {
	case len(users) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "users is required"})
		return
	case len(projects) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "projects is required"})
		return
	case role == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role is required"})
		return
	case len(users)*len(projects) > maxBulkInvitePairs:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("too many pairs: %d users x %d projects exceeds the limit of %d",
				len(users), len(projects), maxBulkInvitePairs),
		})
		return
	}

	ctx := r.Context()
	resolvedUsers, err := h.resolveBulkInviteUsers(ctx, users)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the platform users"})
		return
	}
	resolvedProjects, err := h.resolveBulkInviteProjects(ctx, projects, role)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the projects"})
		return
	}

	// Each pair commits in its own transaction (addProjectMembership), so the
	// check and the loop run under a hold of the role-writes lock that spans them.
	hold, release, err := adminaccess.HoldRoleWrites(ctx, h.pool)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read the projects"})
		return
	}
	defer release()
	caller, refusal := h.checkBulkInviteSelf(r, hold, users, resolvedProjects)
	if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}

	// The route's gate is central: under an elevation every membership it
	// writes is provisional (ADR-0059 R2.12b).
	var provisional *adminaccess.Caller
	if caller.Elevated {
		provisional = &caller
	}

	summary := bulkInviteSummary{
		OK:        true,
		Role:      role,
		Requested: len(users) * len(projects),
		Results:   make([]bulkInviteResult, 0, len(users)*len(projects)),
	}
	for _, userID := range users {
		for _, projectID := range projects {
			result := h.bulkInvitePair(r, resolvedUsers[userID], userID, resolvedProjects[projectID], projectID, role,
				provisional)
			switch result.Outcome {
			case bulkInviteOutcomeAdded:
				summary.Added++
			case bulkInviteOutcomeAlreadyMember:
				summary.Skipped++
			default:
				summary.Failed++
				summary.OK = false
			}
			summary.Results = append(summary.Results, result)
		}
	}

	// 200 EVEN WHEN A PAIR FAILED. The batch is a report, not a single write:
	// a 400 would tell a console that nothing landed, and something did. `ok`
	// and `failed` carry the verdict. This is the one place this file departs
	// from usersCreate, which answers 400 on any failed address — that route
	// writes ONE project and its client treats the batch as atomic.
	writeJSON(w, http.StatusOK, summary)
}

// bulkInvitePair applies one (user, project) pair and describes what happened.
func (h *Handler) bulkInvitePair(
	r *http.Request, user *userTarget, userID int64, project *projectTarget, projectID int64, role string,
	provisional *adminaccess.Caller,
) bulkInviteResult {
	result := bulkInviteResult{UserID: userID, ProjectID: projectID}
	if user != nil {
		result.UserEmail = user.email
	}
	if project != nil {
		result.ProjectName = project.name
	}

	refuse := func(outcome, msg string) bulkInviteResult {
		result.Status = "error"
		result.Outcome = outcome
		result.Msg = msg
		return result
	}
	switch {
	case user == nil:
		return refuse(bulkInviteOutcomeUnknownUser, fmt.Sprintf("no platform user with id %d", userID))
	case user.isSystem:
		return refuse(bulkInviteOutcomeSystemUser,
			fmt.Sprintf("%s is a service account and is not invited", user.label()))
	case project == nil:
		return refuse(bulkInviteOutcomeUnknownProject, fmt.Sprintf("no project with id %d", projectID))
	case project.isPersonal:
		return refuse(bulkInviteOutcomePersonalProject,
			fmt.Sprintf("%s is a personal project and takes no invited members", project.name))
	case project.roleID == nil:
		return refuse(bulkInviteOutcomeUnknownRole,
			fmt.Sprintf("project %s does not define the role %q", project.name, role))
	}

	added, err := h.addProjectMembership(r, projectID, userID, *project.roleID, provisional)
	result.Provisional = provisional != nil
	switch {
	case err != nil:
		return refuse(bulkInviteOutcomeFailed,
			fmt.Sprintf("failed to add %s to %s", user.label(), project.name))
	case added:
		result.Status = "ok"
		result.Outcome = bulkInviteOutcomeAdded
		result.Msg = fmt.Sprintf("added %s to %s as %s", user.label(), project.name, role)
	default:
		// Not an error: a re-run of the same batch is expected, and the
		// operator asked for the user to BE in the project.
		result.Status = "ok"
		result.Outcome = bulkInviteOutcomeAlreadyMember
		result.Msg = fmt.Sprintf("%s already holds %s in %s", user.label(), role, project.name)
	}
	return result
}

// addProjectMembership writes ONE role grant in its own transaction and reports
// whether the row is new.
//
// `added` is derived from the INSERT's own row count rather than from a
// preceding SELECT: two operators submitting overlapping batches at the same
// time would both read "not a member" and one of them would then report an
// insert it did not make. ON CONFLICT DO NOTHING makes the second one report
// `already_member`, which is what happened.
//
// provisional is the elevated caller (ADR-0059 R2.12b), nil otherwise: the
// role is then a provisional grant of the caller's elevation, not a row of
// auth_core__project_user_role, and a person who already holds it is
// already a member.
func (h *Handler) addProjectMembership(
	r *http.Request, projectID, userID int64, roleID int, provisional *adminaccess.Caller,
) (bool, error) {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if provisional != nil {
		var held bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role
               WHERE project_id = $1 AND user_id = $2 AND role_id = $3)`,
			projectID, userID, roleID).Scan(&held); err != nil {
			return false, err
		}
		if held {
			return false, nil
		}
		if err := adminaccess.GiveProvisionalMembership(ctx, tx, *provisional, projectID, userID,
			[]int{roleID}, time.Now()); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}

	before, err := projectMemberRoles(ctx, tx, projectID, userID)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
		 VALUES ($1, $2, $3) ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
		projectID, userID, roleID)
	if err != nil {
		return false, err
	}
	// A membership written through a central route is a security audit
	// event (ADR-0059 R1.5), in this transaction: a failed append fails the
	// pair. A pair that changed nothing appends none.
	if tag.RowsAffected() > 0 {
		if err := h.appendMembershipChange(ctx, tx, r, projectID, userID, before, true); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// resolveBulkInviteUsers reads every requested account in one round trip. An id
// with no row is simply absent from the map, which is how the pair loop
// reports `unknown_user`.
//
// `is_system` reproduces the legacy skip. invites_bulkusers.py:57 tests
// `name.startswith(":system:project:")`; this service ALSO carries the
// `%@centry.user` address the rest of the admin surface filters on
// (internal/api/v2/admin/projects.go:79's systemProjectMemberPredicate), and
// both are service accounts nobody invites.
func (h *Handler) resolveBulkInviteUsers(ctx context.Context, ids []int64) (map[int64]*userTarget, error) {
	rows, err := h.pool.Query(ctx, `
SELECT id,
       COALESCE(email, ''),
       COALESCE(name, ''),
       (COALESCE(name, '') LIKE ':system:project:%' OR COALESCE(email, '') LIKE '%@centry.user') AS is_system
FROM public.auth_core__user
WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resolved := make(map[int64]*userTarget, len(ids))
	for rows.Next() {
		target := &userTarget{}
		if err := rows.Scan(&target.id, &target.email, &target.name, &target.isSystem); err != nil {
			return nil, err
		}
		resolved[target.id] = target
	}
	return resolved, rows.Err()
}

// resolveBulkInviteProjects reads every requested project together with the id
// of the named role IN THAT PROJECT.
//
// The role is resolved per project on purpose: auth_core__project_role is keyed
// on (project_id, name), so "editor" is a different row in every project and a
// project that does not define it must be refused rather than given some other
// project's row. A LEFT JOIN keeps the project in the result with a NULL role
// id, which is what makes `unknown_role` distinguishable from
// `unknown_project`.
//
// `is_personal` is STRICTER than the admin listing's own predicate, and
// deliberately. projects.go:70's `p.name LIKE 'project_user_%'` reads `_` as a
// single-character wildcard, so it also matches a team project named
// `projectAuserB-team` — the case roles_write_postgres_integration_test.go
// seeds on purpose. Mislabelling a row in a listing is cosmetic; REFUSING a
// membership write on the same mistake leaves an operator with a team project
// they cannot fill and no way around it. Personal projects are provisioned as
// `project_user_<user id>` and nothing else
// (internal/db/queries/auth_projects.sql:9 builds the name by concatenation),
// so the anchored digits match every one of them and nothing else.
func (h *Handler) resolveBulkInviteProjects(
	ctx context.Context, ids []int64, role string,
) (map[int64]*projectTarget, error) {
	rows, err := h.pool.Query(ctx, `
SELECT p.id, p.name, (p.name ~ '^project_user_[0-9]+$') AS is_personal, role.id
FROM centry.project p
LEFT JOIN public.auth_core__project_role role
       ON role.project_id = p.id AND role.name = $2
WHERE p.id = ANY($1::bigint[])`, ids, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resolved := make(map[int64]*projectTarget, len(ids))
	for rows.Next() {
		target := &projectTarget{}
		if err := rows.Scan(&target.id, &target.name, &target.isPersonal, &target.roleID); err != nil {
			return nil, err
		}
		resolved[target.id] = target
	}
	return resolved, rows.Err()
}

// label is what an operator recognises the account by. The e-mail is the
// identifier every admin screen shows; the id is the fallback for a row that
// somehow has none.
func (u *userTarget) label() string {
	if u.email != "" {
		return u.email
	}
	if u.name != "" {
		return u.name
	}
	return fmt.Sprintf("user %d", u.id)
}

// dedupeIDs drops duplicates and non-positive ids, and sorts what is left.
//
// Sorting is not cosmetic: it fixes the order the result array reports and the
// order rows are written in, so two operators submitting the same pair set take
// row locks in the same order and cannot deadlock each other.
func dedupeIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

// checkBulkInviteSelf applies the no-escalation rule (issue 2488) to the one
// account this route can widen for the caller: the caller's own. The gate is
// central, and placing people into any project is the authority it grants
// (ADR-0059 keeps it a central-operator power), so an invite of OTHER accounts
// is not measured against the operator's own project sets, which are usually
// empty. Inviting the caller's own account into a project is refused when the
// role carries a permission that the caller does not already hold there.
// Nothing is written for any pair. A super_admin holder is exempt.
//
// A batch that passes and names a project the caller is not a member of marks
// its audit record central_override (ADR-0059 R0.6a).
func (h *Handler) checkBulkInviteSelf(
	r *http.Request, q adminaccess.Querier, users []int64, projects map[int64]*projectTarget,
) (adminaccess.Caller, *adminaccess.Refusal) {
	caller, refusal := adminaccess.ResolveCaller(r, h.resolver)
	if refusal != nil {
		return adminaccess.Caller{}, refusal
	}
	ids := make([]int64, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	if refusal := bulkInviteSelfRefusal(r, q, caller, users, ids, projects); refusal != nil {
		return adminaccess.Caller{}, refusal
	}
	for _, id := range ids {
		if projects[id] == nil {
			continue
		}
		member, err := adminaccess.IsProjectMember(r.Context(), q, caller.UserID, id)
		if err != nil {
			return adminaccess.Caller{}, adminaccess.Unresolved("the project roles could not be read, so the role change was refused")
		}
		if !member {
			adminaccess.MarkCentralOverride(r.Context())
			break
		}
	}
	return caller, nil
}

// bulkInviteSelfRefusal is the rule itself, for a resolved caller and the
// sorted project ids.
func bulkInviteSelfRefusal(
	r *http.Request, q adminaccess.Querier, caller adminaccess.Caller,
	users []int64, ids []int64, projects map[int64]*projectTarget,
) *adminaccess.Refusal {
	if caller.SuperAdmin || !slices.Contains(users, caller.UserID) {
		return nil
	}
	ctx := r.Context()
	for _, id := range ids {
		project := projects[id]
		if project == nil || project.roleID == nil || project.isPersonal {
			continue
		}
		required, err := adminaccess.ProjectRolesPermissions(ctx, q, int(id), []int{*project.roleID})
		if err != nil {
			return adminaccess.Unresolved("the project roles could not be read, so the role change was refused")
		}
		held, err := caller.ProjectSet(ctx, int(id))
		if err != nil {
			return adminaccess.Unresolved("your own permissions could not be read, so the role change was refused")
		}
		if missing := adminaccess.Missing(held, required); len(missing) > 0 {
			return adminaccess.SelfEscalation(adminaccess.ProjectGain{ProjectID: id, Permissions: missing})
		}
	}
	return nil
}
