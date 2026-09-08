package projects

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Handler struct {
	pool        *pgxpool.Pool
	projects    CurrentProjectLister
	resolver    auth.PermissionResolver
	provisioner ProjectProvisioner
}

// ProjectProvisioner creates a project and its tenant. It is an interface at
// the consumer so the create route can be tested without a database, and so
// this package does not depend on the provisioner's concrete constructor.
type ProjectProvisioner interface {
	Provision(ctx context.Context, request projectprovisioning.Request) (projectprovisioning.Result, error)
	Deprovision(ctx context.Context, projectID int64) (projectprovisioning.Result, error)
}

// WithProvisioner supplies the project-create pipeline. Without it the create
// route answers 503 rather than 404, so a misconfigured deployment reports a
// missing dependency instead of a missing endpoint.
func WithProvisioner(provisioner ProjectProvisioner) Option {
	return func(h *Handler) { h.provisioner = provisioner }
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver the group WRITE routes are gated
// on. It is an Option rather than middleware applied in router.go because this
// package is Mount()ed as a subrouter, and chi cannot carry a per-route gate
// across a mount boundary.
//
// Fail-closed: `RequireResolvedPermissionsForProject` answers 403 when its
// resolver is nil, so a Handler built without this option serves the reads and
// refuses every write rather than running ungated.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.resolver = resolver }
}

const (
	CurrentProjectListPath = "/api/v2/projects/project/default/1"
	CurrentProjectListMode = auth.PermissionModeDefault
	// CurrentProjectListPublicProjectID is the public project the `default/1`
	// path segment names. It reaches the QUERY, as the `check_public_role`
	// filter's subject; it is no longer the project the route's permission gate
	// resolves against (#830).
	CurrentProjectListPublicProjectID = "1"
	CurrentProjectListPermission      = "projects.projects.project.view"
)

func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	var projects CurrentProjectLister
	if pool != nil {
		projects = sqlcgen.New(pool)
	}
	handler := &Handler{pool: pool, projects: projects}
	for _, option := range options {
		option(handler)
	}
	return handler
}

// CurrentProjectLister is the generated query surface consumed by the one
// current-compatible project-list route. Keeping this interface at the
// consumer makes the HTTP contract testable without replacing PostgreSQL in
// production.
type CurrentProjectLister interface {
	ListCurrentUserProjects(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error)
}

func NewCurrentProjectListHandler(projects CurrentProjectLister) *Handler {
	return &Handler{projects: projects}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/project/{mode}/{projectID}", h.GetProject)
	r.Get("/groups/prompt_lib", h.GroupList)
	// Project CREATE (#333). Gated CENTRALLY, not per project: there is no
	// project in a create path, so RequireResolvedPermissions' extractor would
	// reject the empty id and answer 403 for everyone. `administration` is the
	// mode the reference resolves this permission in, and the handler refuses
	// any other `{mode}` segment with a 404 to match its route table.
	r.With(apimw.RequireCentralPermissions(
		h.resolver, auth.PermissionModeAdministration, CreateProjectPermission,
	)).Post("/project/{mode}", h.CreateProject)
	// Project DELETE (#333), the symmetric half. Same central gate, same mode
	// restriction. It drops the tenant schema with CASCADE.
	r.With(apimw.RequireCentralPermissions(
		h.resolver, auth.PermissionModeAdministration, DeleteProjectPermission,
	)).Delete("/project/{mode}/{projectID}", h.DeleteProject)
	// The three group WRITES, gated on the permissions their pylon originals
	// declare — `projects.projects.groups.edit` for the set-replacement PUT
	// (groups.py) and `projects.projects.group.create` / `.delete` for the
	// singular create and detach (group.py). Resolved in DEFAULT mode against
	// the project in the path, which is the mode pylon's `recommended_roles`
	// names for these handlers and the one the `prompt_lib` segment reaches.
	r.With(h.requireProjectPermission("projects.projects.groups.edit")).
		Put("/groups/prompt_lib/{projectID}", h.PutProjectGroups)
	r.With(h.requireProjectPermission("projects.projects.group.create")).
		Post("/group/prompt_lib/{projectID}", h.GroupCreate)
	r.With(h.requireProjectPermission("projects.projects.group.delete")).
		Delete("/group/prompt_lib/{projectID}/{groupID}", h.GroupDelete)
	// Quota and statistics (issue #246, quota.go). Gated on the permissions
	// quota.py and statistics.py declare — `projects.projects.project.view` for
	// the two reads and `…project.edit` for the write — resolved in DEFAULT
	// mode against the project in the path. Neither pylon module carries a
	// `{mode}` segment, so neither does the route.
	r.With(h.requireProjectPermission("projects.projects.project.view")).
		Get("/quota/{projectID}", h.GetQuota)
	r.With(h.requireProjectPermission("projects.projects.project.edit")).
		Put("/quota/{projectID}", h.PutQuota)
	r.With(h.requireProjectPermission("projects.projects.project.view")).
		Get("/statistics/{projectID}", h.GetStatistics)
	return r
}

func (h *Handler) requireProjectPermission(permission string) func(http.Handler) http.Handler {
	return apimw.RequireResolvedPermissions(h.resolver, auth.PermissionModeDefault, permission)
}

type Project struct {
	ID             int32           `json:"id"`
	Name           string          `json:"name"`
	OwnerID        int32           `json:"owner_id"`
	Plugins        []string        `json:"plugins"`
	KeycloakGroups json.RawMessage `json:"keycloak_groups"`
	CreateSuccess  bool            `json:"create_success"`
	Suspended      bool            `json:"suspended"`
	Groups         []Group         `json:"groups"`
}

func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	publicProjectID, err := parseInt32(chi.URLParam(r, "projectID"))
	if err != nil || publicProjectID <= 0 {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	h.getCurrentProjects(w, r, publicProjectID)
}

// GetCurrentProjectList owns only the exact route used by the current UI. The
// public-project identifier is part of that compatibility contract, not a
// caller-selected tenant scope.
func (h *Handler) GetCurrentProjectList(w http.ResponseWriter, r *http.Request) {
	h.getCurrentProjects(w, r, 1)
}

func (h *Handler) getCurrentProjects(w http.ResponseWriter, r *http.Request, publicProjectID int32) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	userID, ok := user.OwningUserID()
	if !ok || userID > int64(^uint32(0)>>1) {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.projects == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	limit, err := optionalInt32(r, "limit")
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	offset, err := optionalInt32(r, "offset")
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}

	var search *string
	if value := r.URL.Query().Get("search"); value != "" {
		search = &value
	}
	checkPublicRole := r.URL.Query().Get("check_public_role") != ""
	rows, err := h.projects.ListCurrentUserProjects(r.Context(), sqlcgen.ListCurrentUserProjectsParams{
		CheckPublicRole: checkPublicRole,
		PublicProjectID: publicProjectID,
		UserID:          int32(userID),
		Search:          search,
		Offset:          offset,
		Limit:           limit,
	})
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}

	projects := assembleProjects(rows)
	writeJSON(w, http.StatusOK, projects)
}

func optionalInt32(r *http.Request, name string) (*int32, error) {
	values, present := r.URL.Query()[name]
	if !present {
		return nil, nil
	}
	value, err := parseInt32(values[0])
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func parseInt32(value string) (int32, error) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err
}

func assembleProjects(rows []sqlcgen.ListCurrentUserProjectsRow) []Project {
	projects := make([]Project, 0)
	for _, row := range rows {
		if len(projects) == 0 || projects[len(projects)-1].ID != row.ID {
			projects = append(projects, Project{
				ID:             row.ID,
				Name:           row.Name,
				OwnerID:        row.OwnerID,
				Plugins:        row.Plugins,
				KeycloakGroups: json.RawMessage(row.KeycloakGroups),
				CreateSuccess:  row.CreateSuccess,
				Suspended:      row.Suspended,
				Groups:         make([]Group, 0),
			})
		}
		if row.GroupID == nil || row.GroupName == nil {
			continue
		}
		project := &projects[len(projects)-1]
		if len(project.Groups) != 0 && project.Groups[len(project.Groups)-1].ID == int(*row.GroupID) {
			continue
		}
		project.Groups = append(project.Groups, Group{ID: int(*row.GroupID), Name: *row.GroupName})
	}
	return projects
}

type Group struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (h *Handler) GroupList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.pool.Query(ctx, `SELECT id, name FROM centry.project_group ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0})
		return
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
			return
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if groups == nil {
		groups = []Group{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": groups, "total": len(groups)})
}

// PutProjectGroups, GroupCreate and GroupDelete live in groups.go. The PUT used
// to sit here as a body echo: it decoded the request and wrote it back as the
// response without touching a table, so every group edit reported success and
// changed nothing.

func writeJSON(w http.ResponseWriter, code int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(payload); err != nil {
		return
	}
}
