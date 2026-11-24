// Package adminaccess serves the console capability document (ADR-0059
// decision 6, story R3.1a):
//
//	GET /api/v2/admin/console
//	GET /api/v2/admin/console/items/{item_id}
//
// The first answers, for the caller, the areas and items of the console they
// can open, each at `read` or `write`, their administration roles and their
// active elevation. It is computed from the item table and the permission
// table of internal/adminaccess and from the caller's administration set,
// resolved by the same resolver call the route gates make
// (middleware.RequireCentralPermissions). It carries no permission name: the
// console must not rebuild gate logic from one.
//
// The second answers, for one item, its label key and the roles that grant
// it, by role name. It names no person: any signed-in user may read it, and
// the access-denied page reads it for an item the caller cannot open.
//
// Neither route is gated. A caller with no administration permission gets
// 200 with no area, so the console can tell "no access" from "failed". The
// document is a statement of what the server will answer, never an
// enforcement: every route the console opens resolves its own gate.
package adminaccess

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The routes, relative to the /api/v2/admin group.
const (
	ConsolePath = "/console"
	ItemPath    = "/console/items/{item_id}"
)

// Role is an administration role, by name.
type Role struct {
	Name string
	// BuiltIn is true when the deployment records the role as built-in
	// (elitea_auth.admin_role_seeds), not because of its name.
	BuiltIn bool
	// Marker is the seed row's marker, empty for a custom role.
	Marker string
}

// GrantingRole is a role that holds at least one of an item's permissions,
// with the ones it holds.
type GrantingRole struct {
	Role
	Permissions []string
}

// RoleReader reads administration roles. *PostgresRoles is the production one.
type RoleReader interface {
	// HeldRoles are the administration roles the user holds.
	HeldRoles(ctx context.Context, userID int64) ([]Role, error)
	// GrantingRoles are the administration roles that hold any of names.
	GrantingRoles(ctx context.Context, names []string) ([]GrantingRole, error)
}

// Elevation is the caller's active elevation (ADR-0059 decision 5).
type Elevation struct {
	ID        string
	Level     string
	ExpiresAt time.Time
}

// ElevationReader reads the caller's active elevation; nil, nil when there is
// none. Story R3.10a (issue 2541) wires it to the elevation ledger of R2.1.
type ElevationReader interface {
	ActiveElevation(ctx context.Context, userID int64) (*Elevation, error)
}

// NoElevation is the ElevationReader until the elevation ledger ships: no
// caller has an active elevation.
type NoElevation struct{}

// ActiveElevation answers none.
func (NoElevation) ActiveElevation(context.Context, int64) (*Elevation, error) { return nil, nil }

// Handler serves the two routes.
type Handler struct {
	resolver   auth.PermissionResolver
	roles      RoleReader
	elevations ElevationReader
}

// NewHandler returns the handler. A nil resolver or role reader answers 503;
// a nil elevation reader is NoElevation.
func NewHandler(resolver auth.PermissionResolver, roles RoleReader, elevations ElevationReader) *Handler {
	if elevations == nil {
		elevations = NoElevation{}
	}
	return &Handler{resolver: resolver, roles: roles, elevations: elevations}
}

// Register mounts the routes on the /api/v2/admin router, behind
// middlewares.
func (h *Handler) Register(r chi.Router, middlewares ...func(http.Handler) http.Handler) {
	r.With(middlewares...).Get(ConsolePath, h.Console)
	r.With(middlewares...).Get(ItemPath, h.Item)
}

/* ── the wire shapes (v2.yaml AdminConsoleDocument, AdminConsoleItem) ──── */

type documentBody struct {
	Areas     []areaBody     `json:"areas"`
	Roles     []roleBody     `json:"roles"`
	Elevation *elevationBody `json:"elevation"`
}

type areaBody struct {
	ID       string     `json:"id"`
	LabelKey string     `json:"label_key"`
	Items    []itemBody `json:"items"`
}

type itemBody struct {
	ID       string `json:"id"`
	LabelKey string `json:"label_key"`
	Level    string `json:"level"`
}

type roleBody struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	BuiltIn     bool   `json:"built_in"`
}

type elevationBody struct {
	ID        string    `json:"id"`
	Level     string    `json:"level"`
	ExpiresAt time.Time `json:"expires_at"`
}

type itemDetailBody struct {
	ID            string             `json:"id"`
	Area          string             `json:"area"`
	LabelKey      string             `json:"label_key"`
	AreaLabelKey  string             `json:"area_label_key"`
	Available     bool               `json:"available"`
	GrantingRoles []grantingRoleBody `json:"granting_roles"`
}

type grantingRoleBody struct {
	roleBody
	Level string `json:"level"`
}

// roleOrder puts the built-in roles first, in console order, then the
// custom roles by name.
func roleOrder(a, b Role) int {
	rank := func(r Role) int {
		if r.BuiltIn {
			for i, declared := range registry.BuiltInRoles() {
				if declared.Name == r.Name {
					return i
				}
			}
		}
		return len(registry.BuiltInRoles())
	}
	if d := rank(a) - rank(b); d != 0 {
		return d
	}
	return strings.Compare(a.Name, b.Name)
}

func role(r Role) roleBody {
	body := roleBody{Name: r.Name, BuiltIn: r.BuiltIn}
	if r.BuiltIn {
		if declared, ok := registry.LookupBuiltInRole(r.Name); ok {
			body.DisplayName = declared.DisplayName
		}
	}
	return body
}

/* ── GET /admin/console ────────────────────────────────────────────────── */

// Console answers the capability document of the caller.
func (h *Handler) Console(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.resolver == nil || h.roles == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the console capability document is not configured")
		return
	}
	body := documentBody{Areas: []areaBody{}, Roles: []roleBody{}}
	// The call the route gates make: the caller's principal, credential
	// included, in the administration mode, with no project.
	resolution, err := h.resolver.ResolvePermissions(r.Context(), user, auth.PermissionModeAdministration, "")
	if errors.Is(err, auth.ErrPermissionDenied) {
		// A refused principal holds no administration permission: an
		// answer, not a failure.
		writeJSON(w, body)
		return
	}
	if err != nil {
		h.fail(w, r, "permission resolution failed", err)
		return
	}
	held := registry.SetOf(resolution.Permissions...)
	// No administration route gate admits a native device session
	// (ADR-0059 decision 7), so no item is open to one.
	if apimw.AdministrationGatesRefuse(user) {
		held = registry.Set{}
	}
	for _, area := range registry.ConsoleAccess(held) {
		out := areaBody{ID: string(area.Area), LabelKey: registry.AreaLabelKey(area.Area), Items: []itemBody{}}
		for _, access := range area.Items {
			out.Items = append(out.Items, itemBody{
				ID: access.Item.ID, LabelKey: access.Item.LabelKey(), Level: string(access.Level),
			})
		}
		body.Areas = append(body.Areas, out)
	}
	if resolution.UserID > 0 {
		roles, err := h.roles.HeldRoles(r.Context(), resolution.UserID)
		if err != nil {
			h.fail(w, r, "the roles could not be read", err)
			return
		}
		slices.SortFunc(roles, roleOrder)
		for _, held := range roles {
			body.Roles = append(body.Roles, role(held))
		}
		elevation, err := h.elevations.ActiveElevation(r.Context(), resolution.UserID)
		if err != nil {
			h.fail(w, r, "the elevation could not be read", err)
			return
		}
		if elevation != nil {
			body.Elevation = &elevationBody{ID: elevation.ID, Level: elevation.Level, ExpiresAt: elevation.ExpiresAt.UTC()}
		}
	}
	writeJSON(w, body)
}

/* ── GET /admin/console/items/{item_id} ────────────────────────────────── */

// Item answers one item and the roles that grant it.
func (h *Handler) Item(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFromContext(r.Context()); !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	item, ok := registry.LookupConsoleItem(chi.URLParam(r, "item_id"))
	if !ok {
		apierr.WriteStatus(w, http.StatusNotFound, "no such console item")
		return
	}
	if h.roles == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the console capability document is not configured")
		return
	}
	granting, err := h.roles.GrantingRoles(r.Context(), item.Permissions)
	if err != nil {
		h.fail(w, r, "the roles could not be read", err)
		return
	}
	body := itemDetailBody{
		ID: item.ID, Area: string(item.Area), LabelKey: item.LabelKey(),
		AreaLabelKey: registry.AreaLabelKey(item.Area), Available: item.Available,
		GrantingRoles: []grantingRoleBody{},
	}
	slices.SortFunc(granting, func(a, b GrantingRole) int { return roleOrder(a.Role, b.Role) })
	for _, grant := range granting {
		// A role that is never assigned opens nothing for anybody.
		if grant.Marker == string(registry.MarkerUnassignable) {
			continue
		}
		level, open := item.Level(registry.SetOf(grant.Permissions...))
		if !open {
			continue
		}
		body.GrantingRoles = append(body.GrantingRoles, grantingRoleBody{roleBody: role(grant.Role), Level: string(level)})
	}
	writeJSON(w, body)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, message string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	slog.ErrorContext(r.Context(), "admin console: "+message, "error", err, "path", r.URL.Path)
	apierr.WriteStatus(w, http.StatusInternalServerError, message)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The document changes with the caller's roles; a cached copy would
	// leave controls that will be refused.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
