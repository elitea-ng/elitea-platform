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
	"strconv"

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
	return settingsOverrideGate{resolver: h.permissionResolver, storedType: h.overrideStoredType}.fallback(pattern)
}

// settingsOverrideGate is the central branch of one implementation of the
// project configurations routes: the compatibility Handler here, and the
// reviewed read and mutation routes (read.go, mutation.go) that cmd/elitea-main
// mounts on the same mode-less patterns and that serve them where they are
// composed (#3024). storedType answers the stored type of the request's
// `{configID}` row, "" when there is no such row.
type settingsOverrideGate struct {
	resolver   auth.PermissionResolver
	storedType func(*http.Request) (string, error)
}

func (g settingsOverrideGate) fallback(pattern string) middleware.CentralFallback {
	return func(r *http.Request) (*http.Request, bool, error) {
		permissions := adminaccess.OverrideRoutePermissions(r.Method, pattern)
		if len(permissions) == 0 {
			return r, false, nil
		}
		// The central permission first: a caller without it is refused
		// before anything of the project is read.
		userID, held, err := middleware.ResolveCentralPermission(r.Context(), g.resolver, permissions...)
		if err != nil || !held {
			return r, false, err
		}
		next, types, ok, err := g.types(r)
		if err != nil || !ok || !adminaccess.OverrideAllows(r.Method, pattern, types...) {
			return next, false, err
		}
		// The handler sees the caller as the member branch leaves it: the
		// resolved numeric id on the user (author_id, the audit actor), and
		// the mark that it came in centrally, so the write itself is held to
		// the guarded types whatever the body says (projectSettingsWriteForbidden).
		ctx := next.Context()
		if user, hasUser := auth.UserFromContext(ctx); hasUser {
			user.UserID = strconv.FormatInt(userID, 10)
			ctx = auth.ContextWithUser(ctx, user)
		}
		ctx = context.WithValue(ctx, centralSettingsAdmittedKey{}, true)
		adminaccess.MarkCentralOverride(ctx)
		return next.WithContext(ctx), true, nil
	}
}

type centralSettingsAdmittedKey struct{}

// admittedCentrally reports a request the route gate admitted through the
// central override, not as a member.
func admittedCentrally(ctx context.Context) bool {
	admitted, _ := ctx.Value(centralSettingsAdmittedKey{}).(bool)
	return admitted
}

// types reads the configuration types a request reaches, and the request to
// continue with (its body restored). ok is false when the request reaches no
// type the override can decide on: an unknown row, a create with no type, a
// list with no `type` filter, or a body over the bound.
func (g settingsOverrideGate) types(r *http.Request) (*http.Request, []string, bool, error) {
	stored := func() (string, error) {
		if g.storedType == nil {
			return "", nil
		}
		return g.storedType(r)
	}
	switch r.Method {
	case http.MethodGet:
		if chi.URLParam(r, "configID") == "" {
			types := parseConfigurationListQuery(r.URL.Query()).types
			return r, types, len(types) > 0, nil
		}
		storedType, err := stored()
		return r, []string{storedType}, err == nil && storedType != "", err
	case http.MethodDelete:
		storedType, err := stored()
		return r, []string{storedType}, err == nil && storedType != "", err
	case http.MethodPost:
		next, requested, ok := overrideBodyType(r)
		return next, []string{requested}, ok && requested != "", nil
	case http.MethodPut:
		storedType, err := stored()
		if err != nil || storedType == "" {
			return r, nil, false, err
		}
		next, requested, ok := overrideBodyType(r)
		if !ok {
			return r, nil, false, nil
		}
		// An update that names no type keeps the stored one.
		if requested == "" {
			return next, []string{storedType}, true, nil
		}
		return next, []string{storedType, requested}, true, nil
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
	// The handlers read the body as a map and the exact key `type`
	// (strVal), so this does too: a struct decode matches keys without
	// regard to case, and {"type":"github","TYPE":"browser"} would decide
	// on a type the handler never writes.
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return next, "", false
	}
	value, present := body["type"]
	if !present || value == nil {
		return next, "", true
	}
	typeName, isString := value.(string)
	if !isString {
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
	return centralSettingsWrite(ctx, h.permissionResolver, typeNames)
}

// centralSettingsWrite is (*Handler).centralSettingsWrite for any
// implementation of the routes.
func centralSettingsWrite(ctx context.Context, resolver auth.PermissionResolver, typeNames []string) bool {
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
	held, err := middleware.HoldsCentralPermission(ctx, resolver, adminaccess.ProjectsSettingsManage)
	if err != nil || !held {
		return false
	}
	adminaccess.MarkCentralOverride(ctx)
	return true
}
