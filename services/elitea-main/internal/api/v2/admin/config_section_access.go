package admin

// Who may open a Configuration section (ADR-0059 decision 2, story R0.5).
//
// # Two paths
//
// Before this story a section was gated twice: the route on `runtime.plugins`
// and the section, in the handler, on its own `required_permission`. A caller
// needed both, so no narrow role could edit one section. Now a request passes
// on one of two paths:
//
//   - the NARROW path: the section's narrow read permission (a GET) or write
//     permission (a PUT) alone. A write permission also reads: a section
//     its holder may save is a section its holder may see;
//   - the LEGACY path, unchanged: `runtime.plugins` AND the section's
//     `required_permission`, for both verbs.
//
// A section declares its console area and its pair in an `access` value next
// to `required_permission`. The agent governance sections declare
// `agents.governance.view` and `.manage` (R0.15). `required_permission` stays
// in the JSON the web reads until R3.7 stops using it.
//
// # The route gate is computed from the declarations
//
// The three Configuration routes admit `runtime.plugins` or ANY narrow name a
// section declares (configurationRoutePermissions), and the handler then
// decides per section. The list is read from the sections, not kept by hand,
// so a new section's permission cannot be forgotten at the route. That is
// also why the gate is a method here rather than a middleware call in
// router.go with literal arguments: a computed argument list is one the
// source-reading grant test cannot resolve. The names it admits are held to
// the registry by TestEverySectionDeclaresAnAreaAndARegisteredPair.
//
// # One resolution per question
//
// The legacy section check (hasAdministrationPermission), the
// `runtime.plugins` half of the legacy path (holdsRuntimePlugins) and the
// narrow check (holdsNarrowSectionPermission) each resolve the caller's set
// themselves. internal/api/adminsurface learns what a check accepts by
// probing each resolution in turn, so each question has to be its own
// resolution, asked from its own function, for the surface to name the
// section's legacy permission as the one the legacy check accepts.

import (
	"net/http"
	"slices"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// sectionAccessKey is the key of a section's access declaration.
const sectionAccessKey = "access"

// sectionAccess is what a Configuration section declares about its narrow
// path. It is served in the schema list as `access`.
type sectionAccess struct {
	// Area is the console area of decision 6 the section belongs to.
	Area adminaccess.Area `json:"area"`
	// Read and Write are the narrow pair. A section that keeps an existing
	// single permission names it for both halves.
	Read  string `json:"read_permission,omitempty"`
	Write string `json:"write_permission,omitempty"`
	// LegacyOnly is true for a section with no narrow path yet; Read and
	// Write are then empty.
	LegacyOnly bool `json:"legacy_only,omitempty"`
}

// narrowAccess declares a read and a write permission.
func narrowAccess(area adminaccess.Area, read, write string) sectionAccess {
	return sectionAccess{Area: area, Read: read, Write: write}
}

// singleAccess declares a section that keeps its existing single permission
// for both halves: no `view` half is added for it.
func singleAccess(area adminaccess.Area, permission string) sectionAccess {
	return narrowAccess(area, permission, permission)
}

// platformConfigAccess declares a section with no narrower permission:
// platform.config.view and platform.config.manage.
func platformConfigAccess(area adminaccess.Area) sectionAccess {
	return narrowAccess(area, adminaccess.PlatformConfigView, adminaccess.PlatformConfigManage)
}

// legacyOnlyAccess declares a section with no narrow path: only
// runtime.plugins and its required_permission open it.
func legacyOnlyAccess(area adminaccess.Area) sectionAccess {
	return sectionAccess{Area: area, LegacyOnly: true}
}

// agentGovernanceAccess declares an agent governance section: the pair of
// ADR-0059 R0.15, agents.governance.view and agents.governance.manage, beside
// the legacy path.
func agentGovernanceAccess() sectionAccess {
	return narrowAccess(adminaccess.AreaAgentsGovernance,
		adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage)
}

// The pairs the sections share.
var (
	guardrailSectionAccess = narrowAccess(adminaccess.AreaModelsSpend,
		adminaccess.LLMPolicyView, adminaccess.LLMGuardrailsManage)
	llmPolicySectionAccess = narrowAccess(adminaccess.AreaModelsSpend,
		adminaccess.LLMPolicyView, adminaccess.LLMPolicyManage)
	identitySectionAccess = narrowAccess(adminaccess.AreaPeopleAccess,
		adminaccess.IdentitySSOView, adminaccess.IdentitySSOSettingsManage)
	nativeClientsSectionAccess = singleAccess(adminaccess.AreaPlatform, adminaccess.ConfigurationNativeClients)
)

// narrowPermissions answers the narrow names that admit a verb: the write
// permission for a write, either half for a read. Nil for a legacy-only
// section, and nil for an incomplete declaration (no area, or half a pair, or
// none: a section that forgot its `access`), so an omission fails closed to
// the legacy path rather than admitting an empty name. The registry test
// fails on such a section.
func (a sectionAccess) narrowPermissions(write bool) []string {
	if a.LegacyOnly || a.Area == "" || a.Read == "" || a.Write == "" {
		return nil
	}
	if write {
		return []string{a.Write}
	}
	if a.Read == a.Write {
		return []string{a.Read}
	}
	return []string{a.Read, a.Write}
}

// configurationSections answers every section, resolved.
func configurationSections() []configSection {
	raws := configSections()
	sections := make([]configSection, 0, len(raws))
	for _, raw := range raws {
		sections = append(sections, newConfigSection(raw))
	}
	return sections
}

func newConfigSection(raw map[string]any) configSection {
	id, _ := raw["id"].(string)
	fields, _ := raw["fields"].([]map[string]any)
	legacy, _ := raw["required_permission"].(string)
	access, _ := raw[sectionAccessKey].(sectionAccess)
	return configSection{id: id, fields: fields, raw: raw, legacyPermission: legacy, access: access}
}

// configurationRoutePermissions answers the names the three Configuration
// routes admit: runtime.plugins and every narrow name a section declares.
func configurationRoutePermissions() []string {
	names := []string{adminaccess.RuntimePlugins}
	for _, section := range configurationSections() {
		for _, name := range section.access.narrowPermissions(false) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// readableBy reports whether a caller holding held may read the section:
// on the legacy path or on the narrow one.
func (s configSection) readableBy(held []string) bool {
	return s.openTo(held, false)
}

// writableBy reports whether a caller holding held may save the section: on
// the legacy path, which opens both verbs, or on the narrow write permission.
func (s configSection) writableBy(held []string) bool {
	return s.openTo(held, true)
}

func (s configSection) openTo(held []string, write bool) bool {
	if slices.Contains(held, adminaccess.RuntimePlugins) &&
		(s.legacyPermission == "" || slices.Contains(held, s.legacyPermission)) {
		return true
	}
	return slices.ContainsFunc(s.access.narrowPermissions(write), func(name string) bool {
		return slices.Contains(held, name)
	})
}

// Values of a listed section's `level` in the schema list (R3.7, issue
// 2538): what the caller may do with it. The page disables a `read`
// section's save; the PUT refuses it on its own account either way.
const (
	sectionLevelKey   = "level"
	sectionLevelRead  = "read"
	sectionLevelWrite = "write"
)

// levelFor answers the section's `level` for a caller holding held.
func (s configSection) levelFor(held []string) string {
	if s.writableBy(held) {
		return sectionLevelWrite
	}
	return sectionLevelRead
}

// RequireConfigurationAccess is the route gate of the three Configuration
// routes: runtime.plugins or any narrow section name. It fails closed when no
// resolver is wired.
func (h *Handler) RequireConfigurationAccess(next http.Handler) http.Handler {
	admitted := configurationRoutePermissions()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held, ok := h.administrationSet(r)
		if !ok || !slices.ContainsFunc(admitted, func(name string) bool { return slices.Contains(held, name) }) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "access_denied"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeSection decides one request for one section: the legacy path
// first, then the narrow one.
func (h *Handler) authorizeSection(r *http.Request, section configSection, write bool) bool {
	if h.legacySectionAccess(r, section) {
		return true
	}
	return h.holdsNarrowSectionPermission(r, section.access.narrowPermissions(write))
}

// legacySectionAccess is the legacy path: the section's required_permission
// and runtime.plugins.
func (h *Handler) legacySectionAccess(r *http.Request, section configSection) bool {
	if section.legacyPermission != "" && !h.hasAdministrationPermission(r, section.legacyPermission) {
		return false
	}
	if section.legacyPermission == adminaccess.RuntimePlugins {
		return true
	}
	return h.holdsRuntimePlugins(r)
}

// holdsRuntimePlugins is the runtime.plugins half of the legacy path, which
// the route gate no longer guarantees.
func (h *Handler) holdsRuntimePlugins(r *http.Request) bool {
	if h.resolver == nil {
		return false
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return false
	}
	resolution, err := h.resolver.ResolvePermissions(r.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		return false
	}
	return slices.Contains(resolution.Permissions, adminaccess.RuntimePlugins)
}

// holdsNarrowSectionPermission is the narrow path: any of names alone. No
// name (a legacy-only section) is a refusal.
func (h *Handler) holdsNarrowSectionPermission(r *http.Request, names []string) bool {
	if len(names) == 0 || h.resolver == nil {
		return false
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return false
	}
	resolution, err := h.resolver.ResolvePermissions(r.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		return false
	}
	return slices.ContainsFunc(names, func(name string) bool { return slices.Contains(resolution.Permissions, name) })
}

// administrationSet answers the caller's administration set, for the route
// gate, the schema list and the suggestions. It fails closed: no resolver,
// no principal or a failed resolution is no set.
func (h *Handler) administrationSet(r *http.Request) ([]string, bool) {
	if h.resolver == nil {
		return nil, false
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return nil, false
	}
	resolution, err := h.resolver.ResolvePermissions(r.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		return nil, false
	}
	return resolution.Permissions, true
}

// readableSections answers the sections the caller may read, in order, and
// the caller's set they were decided on.
func (h *Handler) readableSections(r *http.Request) ([]configSection, []string) {
	held, ok := h.administrationSet(r)
	if !ok {
		return nil, nil
	}
	var readable []configSection
	for _, section := range configurationSections() {
		if section.readableBy(held) {
			readable = append(readable, section)
		}
	}
	return readable, held
}

// suggestionSourceSections indexes, once, the sections that declare each
// suggestion source. The declarations are static, so the index is built from
// the first call of configSections and a suggestion request does not rebuild
// every section to answer a name lookup. The index is read only: its raw maps
// are never served.
var suggestionSourceSections = sync.OnceValue(func() map[string][]configSection {
	index := map[string][]configSection{}
	for _, section := range configurationSections() {
		for _, field := range section.fields {
			for _, key := range []string{"enum_source", "enum_source_keys", "enum_source_values"} {
				source, _ := field[key].(string)
				if source == "" || slices.ContainsFunc(index[source], func(s configSection) bool { return s.id == section.id }) {
					continue
				}
				index[source] = append(index[source], section)
			}
		}
	}
	return index
})

// mayReadSuggestionSource reports whether the caller may read a section that
// declares source. A source no section declares is refused before the
// caller's set is read: no permission could change that answer.
func (h *Handler) mayReadSuggestionSource(r *http.Request, source string) bool {
	declaring := suggestionSourceSections()[source]
	if len(declaring) == 0 {
		return false
	}
	held, ok := h.administrationSet(r)
	return ok && slices.ContainsFunc(declaring, func(s configSection) bool { return s.readableBy(held) })
}
