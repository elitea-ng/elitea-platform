package admin

// The admin Projects surface: one listing read and one suspend write.
//
//	GET /admin/projects/{mode}                    — the paginated project list
//	PUT /admin/project_suspend/{mode}/{projectID} — toggle the suspended flag
//
// ## What was here before unit A14
//
// `Projects` had a route and did read the database, so it was not one of the
// package's eight `_ *http.Request` stubs — but it answered a DIFFERENT
// question from the one the admin Projects page asks. It ignored `search`,
// `sort_by`, `sort_order` and `project_type` entirely, emitted `owner_id`
// instead of `owner_name`, carried no `admin_names`, no `status`, no `counts`,
// and reported `total` as the count of ALL projects regardless of the filters
// the client sent. The page's two tabs, its search box, its sortable columns
// and three of its five columns had nothing behind them.
//
// `ProjectSuspend` was worse in the way #126/#129/#134 keep producing: a
// complete handler mounted on NO ROUTE. Nothing could reach it.
//
// ## The contract is not invented
//
// It mirrors the legacy handlers the existing admin_ui client already speaks
// to:
//
//	legacy/plugins/admin/api/v2/projects.py        (GET, the enrichment loop)
//	legacy/plugins/projects/models/project.py      (list_projects_paginated)
//	legacy/plugins/admin/api/v2/project_suspend.py (PUT)
//
// — same paths, same query parameters, same body keys, same row fields —
// because guessing at a shape is how #137 broke elitea-sdk, admin_ui and the
// QA suite at once.
//
// ## Deliberate divergences, called out so they are not read as porting slips
//
//   - ORDER BY carries a `p.id` TIEBREAKER. `ORDER BY name` alone is not a
//     total order (personal projects are named `project_user_<n>` and team
//     names are not unique), so PostgreSQL may return tied rows in a different
//     order per LIMIT/OFFSET page — rows repeat on one page and vanish from
//     another while paging through an unchanged table.
//   - `total` counts the FILTERED set. The legacy runtime does too; the Go
//     implementation this replaces did not, so its pagination advertised pages
//     that did not exist as soon as a filter was applied.
//   - The owner and admin lookups are LATERAL sub-selects, not JOINs. A plain
//     LEFT JOIN onto project roles multiplies a project out once per admin,
//     which is exactly what the pre-A14 admin USER listing did.
//
// ## Authorisation
//
// Both routes are gated in internal/api/router.go on the permission their
// legacy counterparts declare — `projects.projects.projects.view` for the read,
// `projects.projects.projects.edit` for the write — resolved from
// auth_core__user_role per request. The admin SPA's
// `window.admin_ui_config.permissions` is PRESENTATION state and is never
// consulted here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

// personalProjectPredicate classifies a project as PERSONAL. Copied verbatim
// from `list_projects_paginated`'s `is_personal`, underscores included:
// SQLAlchemy's `.like()` treats `_` as a single-character wildcard just as SQL
// LIKE does, so this is the same predicate and not a stricter one.
const personalProjectPredicate = `(p.name LIKE 'project_user_%')`

// systemProjectMemberPredicate excludes each project's own service account from
// the admin-name list, matching `get_users_roles_in_project(filter_system_user=True)`.
// The address shape is the one internal/api/v2/eliteacore/handler.go's project
// member listing already filters on.
const systemProjectMemberPredicate = `(u.email NOT LIKE '%@centry.user')`

// projectStatusExpression ranks the four statuses for `sort_by=status`, in the
// order `list_projects_paginated`'s `sort_map` gives them: active, pending,
// failed, suspended.
const projectStatusExpression = `CASE WHEN p.suspended THEN 3 WHEN p.create_success THEN 0 ELSE 2 END`

// sortableProjectColumns mirrors `list_projects_paginated`'s `sort_map`. The
// value is interpolated into the ORDER BY, so this allow-list is also what
// keeps the query injection-free — an unknown column falls back to `name`.
var sortableProjectColumns = map[string]string{
	"name":           "p.name",
	"id":             "p.id",
	"create_success": "p.create_success",
	"status":         projectStatusExpression,
	// The storage the project's cloud copies use (ADR-0034 decision 3).
	"workspace_bytes": "workspace.bytes",
}

// projectRow is one row of the listing, carrying every field
// legacy/plugins/admin/api/v2/projects.py's enrichment loop attaches.
//
// `project_name` and `admin_name` duplicate `name` and `owner_name`: they are
// legacy-era aliases the existing admin_ui client and the QA suite read, kept
// so this endpoint stays drop-in.
type projectRow struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	ProjectName  string   `json:"project_name"`
	OwnerID      int      `json:"owner_id"`
	OwnerName    string   `json:"owner_name"`
	AdminName    string   `json:"admin_name"`
	AdminNames   []string `json:"admin_names"`
	Status       string   `json:"status"`
	Suspended    bool     `json:"suspended"`
	CreateSucces bool     `json:"create_success"`
	IsPersonal   bool     `json:"is_personal"`
	// WorkspaceBytes is the storage the project's cloud copies of desktop
	// Local work threads use: its live workspace segments (ADR-0034
	// decision 3). It is apart from artifact usage and does not count
	// against the artifact quota.
	WorkspaceBytes int64 `json:"workspace_bytes"`
}

type projectListing struct {
	Rows   []projectRow   `json:"rows"`
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
}

type projectListingParams struct {
	limit       int
	offset      int
	search      string
	projectType string
	sortBy      string
	sortOrder   string
}

// Projects serves `GET /admin/projects/{mode}`.
//
// Response body: {rows, total, counts:{team,personal}} — `counts` is computed
// over ALL projects, unfiltered, because it labels the page's two tabs, while
// `total` describes the filtered set the rows come from.
func (h *Handler) Projects(w http.ResponseWriter, r *http.Request) {
	limit, offset := paginationParams(r)
	query := r.URL.Query()

	listing, err := h.listProjects(r.Context(), projectListingParams{
		limit:       limit,
		offset:      offset,
		search:      strings.TrimSpace(query.Get("search")),
		projectType: query.Get("project_type"),
		sortBy:      query.Get("sort_by"),
		sortOrder:   query.Get("sort_order"),
	})
	if err != nil {
		// A read failure is reported as one. The implementation this replaces
		// swallowed every error into an empty page, which renders exactly like
		// "this deployment has no projects" — the shape #130's post-mortem
		// named as worse than a 404.
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list projects")
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (h *Handler) listProjects(ctx context.Context, params projectListingParams) (*projectListing, error) {
	if h.pool == nil {
		return &projectListing{
			Rows:   []projectRow{},
			Total:  0,
			Counts: map[string]int{"team": 0, "personal": 0},
		}, nil
	}

	counts, err := h.projectCounts(ctx)
	if err != nil {
		return nil, err
	}

	where, args := projectFilters(params)

	var total int
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM centry.project p`+where, args...,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("count filtered projects: %w", err)
	}

	rows, err := h.queryProjectPage(ctx, params, where, args)
	if err != nil {
		return nil, err
	}
	return &projectListing{Rows: rows, Total: total, Counts: counts}, nil
}

// projectCounts labels the two tabs and is deliberately NOT narrowed by the
// filters: a tab whose count changed with the search box would be reporting
// how many rows the OTHER tab currently shows.
func (h *Handler) projectCounts(ctx context.Context) (map[string]int, error) {
	var all, personal int
	if err := h.pool.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE `+personalProjectPredicate+`)
FROM centry.project p`).Scan(&all, &personal); err != nil {
		return nil, fmt.Errorf("count projects: %w", err)
	}
	return map[string]int{"team": all - personal, "personal": personal}, nil
}

// projectFilters renders `project_type` and `search` as SQL, mirroring
// `list_projects_paginated`'s condition set.
//
// The search matches the project name, the project id AS TEXT, and the OWNER's
// name or email — the legacy runtime resolves the third by calling
// `auth_search_users` and ORing the returned ids into the query; the sub-select
// below is the same question asked in one round trip.
func projectFilters(params projectListingParams) (where string, args []any) {
	conditions := make([]string, 0, 2)
	switch params.projectType {
	case "personal":
		conditions = append(conditions, personalProjectPredicate)
	case "team":
		conditions = append(conditions, "NOT "+personalProjectPredicate)
	}
	if params.search != "" {
		args = append(args, "%"+params.search+"%")
		placeholder := "$" + strconv.Itoa(len(args))
		conditions = append(conditions, fmt.Sprintf(`(
    p.name ILIKE %[1]s
 OR p.id::text ILIKE %[1]s
 OR EXISTS (
        SELECT 1 FROM public.auth_core__user owner
        WHERE owner.id = p.owner_id
          AND (owner.name ILIKE %[1]s OR owner.email ILIKE %[1]s)
    )
)`, placeholder))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// projectPageSQL resolves the owner and the admin list with LATERAL sub-selects.
//
// Both are aggregate/LIMIT-1 sub-selects, so each returns exactly one row and
// the project cannot be multiplied out — the failure mode the pre-A14 admin
// user listing shipped, where a user with two roles appeared twice while a
// separate COUNT disagreed.
//
// The admin set follows projects.py exactly: project members holding `admin`,
// plus members holding `editor` on a PERSONAL project, minus the owner (who is
// reported separately as `owner_name`) and minus the project's own service
// account.
const projectPageSQL = `
SELECT p.id,
       p.name,
       p.owner_id,
       p.create_success,
       p.suspended,
       ` + personalProjectPredicate + ` AS is_personal,
       COALESCE(owner.display_name, '') AS owner_name,
       COALESCE(admins.names, '{}')     AS admin_names,
       workspace.bytes                  AS workspace_bytes
FROM centry.project p
LEFT JOIN LATERAL (
    SELECT COALESCE(u.name, u.email, '') AS display_name
    FROM public.auth_core__user u
    WHERE u.id = p.owner_id
    LIMIT 1
) AS owner ON TRUE
LEFT JOIN LATERAL (
    SELECT array_agg(DISTINCT COALESCE(u.name, u.email, '')) AS names
    FROM public.auth_core__project_user_role assignment
    JOIN public.auth_core__project_role project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    JOIN public.auth_core__user u ON u.id = assignment.user_id
    WHERE assignment.project_id = p.id
      AND u.id <> p.owner_id
      AND ` + systemProjectMemberPredicate + `
      AND (
            project_role.name = 'admin'
         OR (project_role.name = 'editor' AND ` + personalProjectPredicate + `)
      )
) AS admins ON TRUE
LEFT JOIN LATERAL (
    SELECT COALESCE(sum(segment.byte_length), 0)::bigint AS bytes
    FROM elitea_runtime.workspace_segments segment
    WHERE segment.project_id = p.id AND segment.superseded_at IS NULL
) AS workspace ON TRUE`

func (h *Handler) queryProjectPage(
	ctx context.Context, params projectListingParams, where string, args []any,
) ([]projectRow, error) {
	sortColumn, ok := sortableProjectColumns[params.sortBy]
	if !ok {
		sortColumn = sortableProjectColumns["name"]
	}
	direction := "ASC"
	if strings.EqualFold(params.sortOrder, "desc") {
		direction = "DESC"
	}

	limitPlaceholder := "$" + strconv.Itoa(len(args)+1)
	offsetPlaceholder := "$" + strconv.Itoa(len(args)+2)
	pageArgs := append(append([]any{}, args...), params.limit, params.offset)

	rows, err := h.pool.Query(ctx, projectPageSQL+where+`
ORDER BY `+sortColumn+` `+direction+` NULLS LAST, p.id `+direction+`
LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder, pageArgs...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	items := make([]projectRow, 0, params.limit)
	for rows.Next() {
		var row projectRow
		var adminNames []string
		if err := rows.Scan(
			&row.ID, &row.Name, &row.OwnerID, &row.CreateSucces, &row.Suspended,
			&row.IsPersonal, &row.OwnerName, &adminNames, &row.WorkspaceBytes,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		row.ProjectName = row.Name
		row.AdminName = row.OwnerName
		row.AdminNames = adminNames
		if row.AdminNames == nil {
			row.AdminNames = []string{}
		}
		row.Status = projectStatus(row.Suspended, row.CreateSucces)
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	return items, nil
}

// projectStatus mirrors projects.py's status ladder.
//
// The legacy runtime has a fourth value, `pending`, for `create_success IS
// NULL`. That branch is UNREACHABLE against this schema:
// `centry.project.create_success` is NOT NULL both in
// internal/infra/db/migrations/001_initial.sql and in the running legacy
// database. It is not reproduced here rather than emitted as a constant nobody
// can trigger — the defect the admin Users reference page shipped, where a
// `status` column that does not exist rendered as a permanent "Active" chip.
func projectStatus(suspended, createSuccess bool) string {
	switch {
	case suspended:
		return "suspended"
	case createSuccess:
		return "active"
	default:
		return "failed"
	}
}

/* ── write ─────────────────────────────────────────────────────────────── */

// ProjectSuspend serves `PUT /admin/project_suspend/{mode}/{projectID}`.
//
// This handler existed in this package before unit A14 but was mounted on NO
// route — dead code with no caller, the pattern #126/#129/#134/#136/#138/#149
// keep producing, and the same one `UserSuspend` was in before the Users port.
// A14 mounts it and hardens it: the mode is checked, the id is validated, a
// missing `suspended` field is a 400 rather than a silent `false`, and an id
// that matches no project is a 404 rather than a 200 that changed nothing.
//
// Suspension is the only project write this unit implements. It is a reversible
// boolean on one row and touches no tenant data; project CREATE and DELETE are
// multi-system provisioning pipelines (tenant schema, object storage, vault,
// RabbitMQ, InfluxDB, a system user and its token — see
// legacy/plugins/projects/utils/project_steps.py) and are rendered unavailable
// in the UI rather than half-implemented here.
func (h *Handler) ProjectSuspend(w http.ResponseWriter, r *http.Request) {
	if !isAdministrationMode(r) {
		apierr.WriteStatus(w, http.StatusNotFound, "not found")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return
	}

	var body struct {
		Suspended *bool `json:"suspended"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Suspended == nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "suspended field is required")
		return
	}

	tag, err := h.pool.Exec(r.Context(),
		`UPDATE centry.project SET suspended = $1 WHERE id = $2`, *body.Suspended, projectID)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update project")
		return
	}
	if tag.RowsAffected() == 0 {
		apierr.WriteStatus(w, http.StatusNotFound, "project not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": projectID, "suspended": *body.Suspended})
}
