package admin

// The admin surface of the SCIM GROUP BINDING — the half of a group push that
// an identity provider cannot carry.
//
// # Why a binding exists at all
//
// Membership on this platform is always (user, PROJECT, ROLE). A SCIM group
// carries a name and a list of members and says nothing about either. The first
// revision of the SCIM tree refused `/Groups` for exactly that reason; this
// surface is what removes the refusal, by making the missing half something an
// administrator AUTHORS here rather than something a push invents.
//
// One binding names one group, one project and one role. A push then supplies
// only the members, and:
//
//   - it cannot create a project — an unbound group is refused by name;
//   - it cannot choose a role — the role is on this row;
//   - it cannot delete a project — a group deletion withdraws the access the
//     group granted and removes the binding, nothing else.
//
// # The permission is the one that already governs project membership
//
// `configuration.roles.user_project_permissions.edit` — the string
// `user_project_permissions.py` declares, and the same one the project
// permission editor beside this file carries. Writing a binding is authorising
// a directory to put people into a project with a role, which is what that
// permission governs; the read is gated on the `.view` half. No new permission
// string arrives, so the grant gate in
// `internal/api/router_permission_grant_gate_test.go` stays untripped.
//
// The SCIM tree itself keeps `admin.auth.users`: an identity provider presents
// ONE credential for `/Users` and `/Groups`, and splitting the gate would stop
// every SCIM client already configured against this deployment.
//
// # Every value is verified against the table that owns it
//
// A binding that named a missing project, or a role the project does not have,
// would be accepted here and refused later — during a push, in a log an
// operator does not read. Both are checked at the write, and the refusal names
// what was wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// SCIMGroupBindingStore is the store seam.
//
// An INTERFACE rather than the concrete `*scimdirectory.Store`, for the reason
// identity_providers.go states beside it: this handler's contract includes the
// ORDER of its refusals, and a concrete store over a nil pool can only prove
// the ones that come before the database is read.
type SCIMGroupBindingStore interface {
	ListGroups(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]scimdirectory.Group, int, error)
	ProjectRoleNames(ctx context.Context, projectID int) ([]string, error)
	CreateBinding(ctx context.Context, displayName string, projectID int, roleName string) (scimdirectory.Group, error)
	UpdateBinding(ctx context.Context, id int64, displayName string, projectID int, roleName string) (scimdirectory.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
}

// WithSCIMGroupBindings supplies the binding store. Without it every route here
// answers 503 rather than an empty list: an empty list would read as "no group
// is bound", which is the answer that sends an operator to author bindings that
// already exist.
func WithSCIMGroupBindings(store SCIMGroupBindingStore) Option {
	return func(h *Handler) {
		// A nil interface is not stored; see WithIdentityProviders for why the
		// check is here and not at the call site.
		if store == nil {
			return
		}
		h.scimGroupBindings = store
	}
}

// scimGroupBindingPageSize is the DEFAULT page, and scimGroupBindingMaxPage the
// largest one a caller may ask for. The maximum is the SCIM directory's own, so
// this screen and a SCIM client see the same set.
//
// The listing is PAGED rather than capped. A capped listing renders its first
// page and reports a larger total, and every binding past the cap is then
// unreachable from any screen — an operator looking for one concludes it does
// not exist and authors a duplicate, which the unique index refuses for a
// reason they cannot see.
const (
	scimGroupBindingPageSize = 100
	scimGroupBindingMaxPage  = 500
)

// scimGroupBindingBody is the wire shape of one authored binding.
type scimGroupBindingBody struct {
	DisplayName string `json:"display_name"`
	ProjectID   int    `json:"project_id"`
	RoleName    string `json:"role_name"`
}

// scimGroupBindingView is what a read returns.
//
// `members` is included because it is the only place an operator can see what a
// push actually did — the project's own members screen shows the membership but
// not which group granted it. `granted` distinguishes a membership this group
// created from one it merely found, which is the difference between a person
// who loses access when they leave the group and one who does not.
type scimGroupBindingView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	ExternalID  string `json:"external_id,omitempty"`
	ProjectID   int    `json:"project_id"`
	ProjectName string `json:"project_name,omitempty"`
	RoleName    string `json:"role_name"`
	Members     []struct {
		UserID      int    `json:"user_id"`
		UserName    string `json:"user_name"`
		DisplayName string `json:"display_name,omitempty"`
		Granted     bool   `json:"granted"`
	} `json:"members"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func scimGroupBindingToView(group scimdirectory.Group) scimGroupBindingView {
	view := scimGroupBindingView{
		ID:          strconv.FormatInt(group.ID, 10),
		DisplayName: group.DisplayName,
		ExternalID:  group.ExternalID,
		ProjectID:   group.ProjectID,
		ProjectName: group.ProjectName,
		RoleName:    group.RoleName,
	}
	view.Members = make([]struct {
		UserID      int    `json:"user_id"`
		UserName    string `json:"user_name"`
		DisplayName string `json:"display_name,omitempty"`
		Granted     bool   `json:"granted"`
	}, 0, len(group.Members))
	for _, member := range group.Members {
		view.Members = append(view.Members, struct {
			UserID      int    `json:"user_id"`
			UserName    string `json:"user_name"`
			DisplayName string `json:"display_name,omitempty"`
			Granted     bool   `json:"granted"`
		}{
			UserID: member.UserID, UserName: member.UserName,
			DisplayName: member.DisplayName, Granted: member.Granted,
		})
	}
	if !group.UpdatedAt.IsZero() {
		view.UpdatedAt = group.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return view
}

func (h *Handler) scimGroupBindingsReady(w http.ResponseWriter) bool {
	if h.scimGroupBindings != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable,
		map[string]any{"error": "SCIM group provisioning is not available on this deployment"})
	return false
}

// SCIMGroupBindingList answers `GET /admin/scim_group_bindings/administration`.
//
// `limit` and `offset` page it, and the response carries `total`, `limit` and
// `offset` so the caller can tell a full listing from a page of one.
func (h *Handler) SCIMGroupBindingList(w http.ResponseWriter, r *http.Request) {
	if !h.scimGroupBindingsReady(w) {
		return
	}
	limit, offset := scimGroupBindingPaging(r)
	// The store's index is ONE-BASED, as SCIM's is.
	groups, total, err := h.scimGroupBindings.ListGroups(
		r.Context(), scimdirectory.Filter{}, offset+1, limit)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the group bindings could not be read"})
		return
	}
	views := make([]scimGroupBindingView, 0, len(groups))
	for _, group := range groups {
		views = append(views, scimGroupBindingToView(group))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bindings": views, "total": total, "limit": limit, "offset": offset,
	})
}

// scimGroupBindingPaging reads `limit` and `offset`, clamped.
//
// An unreadable or negative value falls back to the default rather than
// refusing: a paging parameter is the caller's convenience, and a 400 here
// would make the screen unusable over a typo in a URL.
func scimGroupBindingPaging(r *http.Request) (limit, offset int) {
	limit, offset = scimGroupBindingPageSize, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, scimGroupBindingMaxPage)
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			offset = parsed
		}
	}
	return limit, offset
}

// SCIMGroupBindingProjectRoles answers
// `GET /admin/scim_group_bindings/administration/project_roles/{projectID}`.
//
// # Why this exists beside `/admin/roles/{mode}/{projectID}`
//
// That listing answers a HARDCODED admin/editor/viewer when a project carries
// no role rows (internal/api/v2/eliteacore/handler.go). It is a reasonable
// default where it came from, and it is the wrong source for this screen: a
// picker fed by it offers a role the project does not have, the operator
// chooses it, and the save is refused by a value the control supplied.
//
// This route answers what the project HAS, empty list included.
func (h *Handler) SCIMGroupBindingProjectRoles(w http.ResponseWriter, r *http.Request) {
	if !h.scimGroupBindingsReady(w) {
		return
	}
	projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such project"})
		return
	}
	roles, err := h.scimGroupBindings.ProjectRoleNames(r.Context(), projectID)
	if err != nil {
		var unknownProject scimdirectory.UnknownProjectError
		if errors.As(err, &unknownProject) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such project"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the project roles could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles, "total": len(roles)})
}

// SCIMGroupBindingCreate answers `POST /admin/scim_group_bindings/administration`.
func (h *Handler) SCIMGroupBindingCreate(w http.ResponseWriter, r *http.Request) {
	if !h.scimGroupBindingsReady(w) {
		return
	}
	body, ok := decodeSCIMGroupBinding(w, r)
	if !ok {
		return
	}
	group, err := h.scimGroupBindings.CreateBinding(
		r.Context(), body.DisplayName, body.ProjectID, body.RoleName)
	if err != nil {
		writeSCIMGroupBindingFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"binding": scimGroupBindingToView(group)})
}

// SCIMGroupBindingSave answers `PUT /admin/scim_group_bindings/administration/{id}`.
//
// A binding whose project or role changes MOVES the access it granted: the
// store revokes under the old pair and re-grants under the new one in one
// transaction. The alternative — leaving the old memberships standing — is
// access that matches no authored rule and that no screen would explain.
func (h *Handler) SCIMGroupBindingSave(w http.ResponseWriter, r *http.Request) {
	if !h.scimGroupBindingsReady(w) {
		return
	}
	id, ok := scimGroupBindingID(w, r)
	if !ok {
		return
	}
	body, ok := decodeSCIMGroupBinding(w, r)
	if !ok {
		return
	}
	group, err := h.scimGroupBindings.UpdateBinding(
		r.Context(), id, body.DisplayName, body.ProjectID, body.RoleName)
	if err != nil {
		writeSCIMGroupBindingFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"binding": scimGroupBindingToView(group)})
}

// SCIMGroupBindingDelete answers `DELETE /admin/scim_group_bindings/administration/{id}`.
//
// It withdraws every membership the group granted and removes the binding. The
// project is untouched: deleting a project is a decision taken on the admin
// Projects page, where what it destroys is on screen.
func (h *Handler) SCIMGroupBindingDelete(w http.ResponseWriter, r *http.Request) {
	if !h.scimGroupBindingsReady(w) {
		return
	}
	id, ok := scimGroupBindingID(w, r)
	if !ok {
		return
	}
	if err := h.scimGroupBindings.DeleteGroup(r.Context(), id); err != nil {
		writeSCIMGroupBindingFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func scimGroupBindingID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such group binding"})
		return 0, false
	}
	return id, true
}

func decodeSCIMGroupBinding(w http.ResponseWriter, r *http.Request) (scimGroupBindingBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, scimGroupBindingMaxBytes)
	var body scimGroupBindingBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the binding could not be read"})
		return scimGroupBindingBody{}, false
	}
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	body.RoleName = strings.TrimSpace(body.RoleName)
	switch {
	case body.DisplayName == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "a group name is required: it is what an identity provider's push is matched on",
		})
		return scimGroupBindingBody{}, false
	case body.ProjectID <= 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "a project is required: a group push never creates one",
		})
		return scimGroupBindingBody{}, false
	case body.RoleName == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "a role is required: membership on this platform is always a role on a project",
		})
		return scimGroupBindingBody{}, false
	}
	return body, true
}

// scimGroupBindingMaxBytes bounds one write. A binding is three short values;
// the bound is stated rather than assumed.
const scimGroupBindingMaxBytes = 64 << 10

// writeSCIMGroupBindingFailure maps a store outcome onto a response that names
// what an operator has to change.
func writeSCIMGroupBindingFailure(w http.ResponseWriter, err error) {
	var (
		unknownProject scimdirectory.UnknownProjectError
		roleMissing    scimdirectory.RoleMissingError
	)
	switch {
	case errors.As(err, &unknownProject):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "no project " + strconv.Itoa(unknownProject.ProjectID) + " exists",
		})
	case errors.As(err, &roleMissing):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "project " + strconv.Itoa(roleMissing.ProjectID) + " has no role " +
				strconv.Quote(roleMissing.RoleName) +
				"; a project is provisioned with admin, editor, viewer and system",
		})
	case errors.Is(err, scimdirectory.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "another binding already uses that group name",
		})
	case errors.Is(err, scimdirectory.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such group binding"})
	default:
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the group binding could not be written"})
	}
}
