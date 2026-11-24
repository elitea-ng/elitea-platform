package configurations

// The central branch of the guarded project settings (ADR-0059 R0.6b).
//
// The five guarded settings (project_settings_access.go) are configuration
// rows, written through the generic project configurations routes. A global
// administrator who is not a member of a project could not change them: the
// route gates resolve the default mode in the project. The central override
// lets projects.settings.view read, and projects.settings.manage write, those
// five types of ANY project, and nothing else: the pairs are the reviewed
// allowlist of internal/adminaccess/central_override.go.
//
// Both checks get the branch:
//
//   - the route gate (requireOrSettingsOverride): a caller who does not hold
//     the route's permission in the project passes only for an allowlisted
//     (route, configuration type) pair, decided BEFORE the body is trusted.
//     The type comes from the `type` query (the list), the stored row (the
//     detail, the update, the delete) and the body (the create, and the type
//     an update asks for). An update that changes the type passes only when
//     both are on the list. A credential, model or toolkit type is on no
//     pair, so those rows stay refused.
//   - projectSettingsWriteForbidden: a caller without
//     models.project_settings.edit in the project writes a guarded type only
//     when every type of the write is on the list and it holds
//     projects.settings.manage centrally.
//
// RequireProjectAccess is not widened, and no other project route gains the
// branch. A member who holds the project permissions keeps the existing path
// and sets no central_override; a write admitted by the branch marks its
// audit record central_override.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// configurationsMount is where router.go mounts Routes(); the allowlist names
// the full route patterns.
const configurationsMount = "/api/v2/configurations"

// requireOrSettingsOverride is h.require with the central branch, for a route
// whose pattern (relative to the mount) is on the allowlist.
func (h *Handler) requireOrSettingsOverride(pattern, permission string) func(http.Handler) http.Handler {
	return middleware.RequireResolvedPermissionsOrCentral(
		h.permissionResolver,
		auth.PermissionModeDefault,
		h.settingsOverride(configurationsMount+pattern),
		permission,
	)
}

// settingsOverride is the fallback of the route gate on one route pattern.
func (h *Handler) settingsOverride(pattern string) middleware.CentralFallback {
	return func(r *http.Request) (*http.Request, bool, error) {
		permissions := adminaccess.OverrideRoutePermissions(r.Method, pattern)
		if len(permissions) == 0 {
			return r, false, nil
		}
		// The central permission first: a caller without it is refused
		// before anything of the project is read.
		held, err := middleware.HoldsCentralPermission(r.Context(), h.permissionResolver, permissions...)
		if err != nil || !held {
			return r, false, err
		}
		next, types, ok, err := h.overrideTypes(r)
		if err != nil || !ok || !adminaccess.OverrideAllows(r.Method, pattern, types...) {
			return next, false, err
		}
		adminaccess.MarkCentralOverride(next.Context())
		return next, true, nil
	}
}

// overrideTypes reads the configuration types a request reaches, and the
// request to continue with (its body restored). ok is false when the request
// reaches no type the override can decide on: an unknown row, a create with
// no type, a list with no `type` filter, or a body over the bound.
func (h *Handler) overrideTypes(r *http.Request) (*http.Request, []string, bool, error) {
	switch r.Method {
	case http.MethodGet:
		if chi.URLParam(r, "configID") == "" {
			types := parseConfigurationListQuery(r.URL.Query()).types
			return r, types, len(types) > 0, nil
		}
		stored, err := h.overrideStoredType(r)
		return r, []string{stored}, err == nil && stored != "", err
	case http.MethodDelete:
		stored, err := h.overrideStoredType(r)
		return r, []string{stored}, err == nil && stored != "", err
	case http.MethodPost:
		next, requested, ok := overrideBodyType(r)
		return next, []string{requested}, ok && requested != "", nil
	case http.MethodPut:
		stored, err := h.overrideStoredType(r)
		if err != nil || stored == "" {
			return r, nil, false, err
		}
		next, requested, ok := overrideBodyType(r)
		if !ok {
			return r, nil, false, nil
		}
		// An update that names no type keeps the stored one.
		if requested == "" {
			return next, []string{stored}, true, nil
		}
		return next, []string{stored, requested}, true, nil
	}
	return r, nil, false, nil
}

// overrideStoredType is the stored type of the `{configID}` row, "" when there
// is no such row (or no store).
func (h *Handler) overrideStoredType(r *http.Request) (string, error) {
	if h.pool == nil {
		return "", nil
	}
	schema, err := tenantschema.Quote(chi.URLParam(r, "projectID"))
	if err != nil {
		return "", nil
	}
	return h.storedConfigurationType(r.Context(), schema, chi.URLParam(r, "configID"))
}

// overrideBodyType reads the body's `type`, within the handler's own bound,
// and restores the body for the handler. ok is false for a body the handler
// would refuse anyway (over the bound, or not a JSON object).
func overrideBodyType(r *http.Request) (*http.Request, string, bool) {
	if r.Body == nil {
		return r, "", false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConfigurationRequestBytes+1))
	_ = r.Body.Close()
	next := r.Clone(r.Context())
	next.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) > maxConfigurationRequestBytes {
		return next, "", false
	}
	var body struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return next, "", false
	}
	if len(body.Type) == 0 || string(body.Type) == "null" {
		return next, "", true
	}
	var typeName string
	if err := json.Unmarshal(body.Type, &typeName); err != nil {
		// A non-string type is refused by the handler; it is no type here.
		return next, "", false
	}
	return next, typeName, true
}

// centralSettingsWrite reports whether a caller without
// models.project_settings.edit in the project may still write these types
// through the central override: every named type is a guarded setting (the
// types of the allowlist's write pairs) and the caller holds
// projects.settings.manage centrally. It marks the request when it may.
func (h *Handler) centralSettingsWrite(ctx context.Context, typeNames []string) bool {
	named := 0
	for _, name := range typeNames {
		if name == "" {
			continue
		}
		if !adminaccess.IsGuardedSettingType(name) {
			return false
		}
		named++
	}
	if named == 0 {
		return false
	}
	held, err := middleware.HoldsCentralPermission(ctx, h.permissionResolver, adminaccess.ProjectsSettingsManage)
	if err != nil || !held {
		return false
	}
	adminaccess.MarkCentralOverride(ctx)
	return true
}
