package adminaccess

// What each administration role can do (ADR-0059 decision 8, "Reading it";
// story R3.9a, issue 2540):
//
//	GET /api/v2/admin/access/role-definitions
//	GET /api/v2/admin/access/role-definitions/compare?a=&b=
//
// The first answers every administration role as areas, each item at `read`
// or `write` (registry.ConsoleAccess applied to the role's set), its seed
// status, its raw permission count, and whether the caller may hand it out:
// `assignable_by_caller` with the refusal code a write giving it to another
// person would get, and `self_refusal_code`, the code a write changing the
// caller's own holding would get. Both are the rule package's answers
// (registry.Assigner), so the Access page re-implements no rule.
//
// The second puts two roles side by side, by item and by permission.
//
// Role names only: neither route names a person. Both sit behind the gate of
// the other Access reads, access.roles.view and the names it narrows.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The routes, relative to the /api/v2/admin group.
const (
	RoleDefinitionsPath = "/access/role-definitions"
	RoleComparePath     = "/access/role-definitions/compare"
)

// RoleDefinitionReader reads every administration role with its grants, and
// whether the database keeps the seed record. *PostgresRoles is the
// production one.
type RoleDefinitionReader interface {
	RoleDefinitions(ctx context.Context) ([]registry.StoredRole, bool, error)
}

// RoleDefinitionsHandler serves the two routes.
type RoleDefinitionsHandler struct {
	resolver auth.PermissionResolver
	roles    RoleDefinitionReader
}

// NewRoleDefinitionsHandler returns the handler. A nil resolver or reader
// answers 503.
func NewRoleDefinitionsHandler(resolver auth.PermissionResolver, roles RoleDefinitionReader) *RoleDefinitionsHandler {
	return &RoleDefinitionsHandler{resolver: resolver, roles: roles}
}

// Register mounts the routes on the /api/v2/admin router, behind middlewares
// (the gate of the Access reads).
func (h *RoleDefinitionsHandler) Register(r chi.Router, middlewares ...func(http.Handler) http.Handler) {
	r.With(middlewares...).Get(RoleDefinitionsPath, h.List)
	r.With(middlewares...).Get(RoleComparePath, h.Compare)
}

/* ── the wire shapes (v2.yaml AccessRoleDefinitions, AccessRoleComparison) */

type roleDefinitionsBody struct {
	Roles []roleDefinitionBody `json:"roles"`
}

type roleDefinitionBody struct {
	roleBody
	Status          string     `json:"status"`
	Locked          bool       `json:"locked"`
	PermissionCount int        `json:"permission_count"`
	Areas           []areaBody `json:"areas"`
	HeldByCaller    bool       `json:"held_by_caller"`
	Assignable      bool       `json:"assignable_by_caller"`
	RefusalCode     string     `json:"refusal_code,omitempty"`
	SelfRefusalCode string     `json:"self_refusal_code,omitempty"`
}

type roleComparisonBody struct {
	A           roleSummaryBody       `json:"a"`
	B           roleSummaryBody       `json:"b"`
	Items       []comparedItemBody    `json:"items"`
	Permissions comparedPermissionSet `json:"permissions"`
}

type roleSummaryBody struct {
	roleBody
	Status          string `json:"status"`
	PermissionCount int    `json:"permission_count"`
}

type comparedItemBody struct {
	ID       string `json:"id"`
	Area     string `json:"area"`
	LabelKey string `json:"label_key"`
	// LevelA and LevelB are "" when the role does not open the item.
	LevelA string `json:"level_a"`
	LevelB string `json:"level_b"`
}

type comparedPermissionSet struct {
	Both  []string `json:"both"`
	OnlyA []string `json:"only_a"`
	OnlyB []string `json:"only_b"`
}

/* ── reading ───────────────────────────────────────────────────────────── */

// read answers the roles, the seed-record flag and the caller; it writes the
// failure itself and answers ok false.
func (h *RoleDefinitionsHandler) read(w http.ResponseWriter, r *http.Request) (
	[]registry.StoredRole, bool, auth.PermissionResolution, bool,
) {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return nil, false, auth.PermissionResolution{}, false
	}
	if h.resolver == nil || h.roles == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the role definitions are not configured")
		return nil, false, auth.PermissionResolution{}, false
	}
	roles, seedRecord, err := h.roles.RoleDefinitions(r.Context())
	if err != nil {
		h.fail(w, r, "the role definitions could not be read", err)
		return nil, false, auth.PermissionResolution{}, false
	}
	// The STANDING set, as the assignment routes read it
	// (registry.ResolveCaller): no elevation assigns a role. It decides
	// nothing here (the route gate did): it only says who may assign.
	caller, err := auth.ResolveStandingAdministration(r.Context(), h.resolver, principal)
	if err != nil {
		h.fail(w, r, "your own permissions could not be read", err)
		return nil, false, auth.PermissionResolution{}, false
	}
	slices.SortFunc(roles, func(a, b registry.StoredRole) int { return roleOrder(storedRole(a), storedRole(b)) })
	return roles, seedRecord, caller, true
}

func storedRole(stored registry.StoredRole) Role {
	return Role{Name: stored.Name, BuiltIn: stored.Marker != "", Marker: string(stored.Marker)}
}

func areas(stored registry.StoredRole) []areaBody {
	out := []areaBody{}
	for _, area := range stored.Areas() {
		body := areaBody{ID: string(area.Area), LabelKey: registry.AreaLabelKey(area.Area), Items: []itemBody{}}
		for _, access := range area.Items {
			body.Items = append(body.Items, itemBody{
				ID: access.Item.ID, LabelKey: access.Item.LabelKey(), Level: string(access.Level),
			})
		}
		out = append(out, body)
	}
	return out
}

func code(refusal *registry.Refusal) string {
	if refusal == nil {
		return ""
	}
	return refusal.Code
}

/* ── GET /admin/access/role-definitions ────────────────────────────────── */

// List answers every administration role.
func (h *RoleDefinitionsHandler) List(w http.ResponseWriter, r *http.Request) {
	roles, seedRecord, caller, ok := h.read(w, r)
	if !ok {
		return
	}
	held := registry.SetOf(caller.Permissions...)
	assigner := registry.Assigner{Administration: held, SuperAdmin: held[registry.SuperAdminPermission]}
	body := roleDefinitionsBody{Roles: make([]roleDefinitionBody, 0, len(roles))}
	for _, stored := range roles {
		assign := assigner.AssignRefusal(stored, seedRecord)
		body.Roles = append(body.Roles, roleDefinitionBody{
			roleBody:        role(storedRole(stored)),
			Status:          string(stored.Status(seedRecord)),
			Locked:          stored.Locked(),
			PermissionCount: len(stored.Permissions),
			Areas:           areas(stored),
			HeldByCaller:    slices.Contains(caller.Roles, stored.Name),
			Assignable:      assign == nil,
			RefusalCode:     code(assign),
			SelfRefusalCode: code(assigner.SelfRefusal(stored, seedRecord)),
		})
	}
	writeJSON(w, body)
}

/* ── GET /admin/access/role-definitions/compare ────────────────────────── */

// Compare answers two roles side by side.
func (h *RoleDefinitionsHandler) Compare(w http.ResponseWriter, r *http.Request) {
	nameA := strings.TrimSpace(r.URL.Query().Get("a"))
	nameB := strings.TrimSpace(r.URL.Query().Get("b"))
	if nameA == "" || nameB == "" {
		apierr.WriteStatus(w, http.StatusBadRequest, "name two roles, a and b")
		return
	}
	roles, seedRecord, _, ok := h.read(w, r)
	if !ok {
		return
	}
	find := func(name string) (registry.StoredRole, bool) {
		i := slices.IndexFunc(roles, func(stored registry.StoredRole) bool { return stored.Name == name })
		if i < 0 {
			return registry.StoredRole{}, false
		}
		return roles[i], true
	}
	a, okA := find(nameA)
	b, okB := find(nameB)
	if !okA || !okB {
		apierr.WriteStatus(w, http.StatusNotFound, "no such administration role")
		return
	}
	summary := func(stored registry.StoredRole) roleSummaryBody {
		return roleSummaryBody{
			roleBody: role(storedRole(stored)), Status: string(stored.Status(seedRecord)),
			PermissionCount: len(stored.Permissions),
		}
	}
	body := roleComparisonBody{A: summary(a), B: summary(b), Items: []comparedItemBody{},
		Permissions: comparePermissions(a.Permissions, b.Permissions)}
	setA, setB := registry.SetOf(a.Permissions...), registry.SetOf(b.Permissions...)
	for _, item := range registry.ConsoleItems() {
		if !item.Available {
			continue
		}
		levelA, openA := item.Level(setA)
		levelB, openB := item.Level(setB)
		if !openA && !openB {
			continue
		}
		body.Items = append(body.Items, comparedItemBody{
			ID: item.ID, Area: string(item.Area), LabelKey: item.LabelKey(),
			LevelA: string(levelA), LevelB: string(levelB),
		})
	}
	writeJSON(w, body)
}

func comparePermissions(a, b []string) comparedPermissionSet {
	setA, setB := registry.SetOf(a...), registry.SetOf(b...)
	out := comparedPermissionSet{Both: []string{}, OnlyA: []string{}, OnlyB: []string{}}
	for _, name := range setA.Names() {
		if setB[name] {
			out.Both = append(out.Both, name)
		} else {
			out.OnlyA = append(out.OnlyA, name)
		}
	}
	for _, name := range setB.Names() {
		if !setA[name] {
			out.OnlyB = append(out.OnlyB, name)
		}
	}
	return out
}

func (h *RoleDefinitionsHandler) fail(w http.ResponseWriter, r *http.Request, message string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	slog.ErrorContext(r.Context(), "admin access: "+message, "error", err, "path", r.URL.Path)
	apierr.WriteStatus(w, http.StatusInternalServerError, message)
}
