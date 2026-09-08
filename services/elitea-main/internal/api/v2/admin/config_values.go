package admin

// The admin Configuration page's read/write surface — unit A14, issue #200.
//
//	GET  /admin/plugin_config_values/administration/{section}  — current values
//	PUT  /admin/plugin_config_values/administration/{section}  — save them
//	GET  /admin/plugin_config_values/prompt_lib/resources      — the one public section
//
// ## What this page configured in pylon, and why almost none of it survives
//
// The reference Configuration page is not a form over a settings table. pylon
// has no settings table. Every pylon announces its loaded plugins every 15
// seconds over the Arbiter bus — name, parsed config, raw config YAML and an
// `admin_schema.json` — and the admin plugin keeps the last announcement of each
// in an IN-PROCESS dict with a 60-second freshness cut-off
// (legacy/plugins/bootstrap/tools/event.py:29-103,
// legacy/plugins/admin/events/runtime_remote.py:28-50). The GET aggregates that
// dict. The PUT re-serialises the whole of a plugin's YAML with the changed
// paths patched in and fires `bootstrap_runtime_update` fire-and-forget; a
// handler on the target pylon upserts the bytes into THAT pylon's own
// `plugin_config` table and calls `descriptor.load_config()`
// (legacy/plugins/admin/api/v2/plugin_config_values.py:275-305,
// legacy/pylon/pylon/core/providers/internal/db_config.py:65-88).
//
// That is Pylon plugin loading over Arbiter transport, which AGENTS.md's
// architecture boundaries name explicitly as things the target architecture does
// NOT preserve. There is no dict of announcements here, no plugin descriptor to
// reconfigure, and no plan for either.
//
// So the sections divide, and the division is declared BY THE SERVER
// (`unavailableReason` in config_schemas.go) rather than decided in the page:
//
//   - `resources` — the Help Center cards every user sees. Owned here, in
//     `centry.platform_config`, and READ BACK by the Help Center: the public
//     `prompt_lib` route below is the same one pylon exposes for exactly this
//     section (`_PUBLIC_SECTIONS = {"resources"}`), and apps/elitea-web's
//     `pages/help-center` now calls it. This is the whole of the page's live
//     write surface, and it is live end to end.
//   - everything else — Pylon plugin configuration with no store and no consumer
//     in this platform. Those sections answer 501 WITH A REASON on both verbs,
//     and the page renders the reason instead of a form. A form over values
//     nothing reads is the worst version of this defect: the operator believes
//     the setting took effect.
//
// The three shapes this replaced were all present at once: `PluginConfigValues`
// returned the schema's DEFAULTS as if they were current values (a read that
// answers a different question — it reports what the platform would do if
// nothing were configured, which is indistinguishable from a fresh install);
// `PluginConfigValuesSave` was `writeJSON(200, {"values":{},"requires_restart":[]})`
// with the request body never read at all; and `ResourcesConfig` — the route the
// Help Center is meant to call — returned chat and upload limits
// (`max_file_size`, `max_context_length`, …) under no `values` wrapper, so even
// a client that called it got nothing it could use. Issue #26 records the
// symptom: every Help Center card renders "No links configured".
//
// ## The security boundary of this page
//
// A configuration surface is where a client gets to choose values the SERVER
// later acts on, so two families are refused rather than accepted-and-ignored:
//
//  1. **Link URLs.** A resource card's links are rendered as anchors to every
//     authenticated user on the platform. An unchecked URL there is stored XSS
//     via `javascript:`, and a stored `data:` document is the same thing with
//     extra steps. `validateLinks` accepts http and https only. This is the one
//     writable section, so it is the one place a hostile value could actually
//     land.
//  2. **Credentials.** Several fields in the (unavailable) `auth` section are
//     `format: password` — an OIDC client secret, a Postgres URL with a password
//     in it. The retired `litellm` section carried more of them (a proxy master
//     key, a database URL); `llm_proxy` replaced it and declares no fields at
//     all, which removes the hazard for that section rather than managing it.
//     Credentials belong in the vault
//     (`centry.secrets_*`, see internal/api/v2/secrets), never in a plaintext
//     settings row readable by anyone who can read this table. Even though those
//     sections are already refused as unavailable, `rejectCredentialFields`
//     checks the field spec independently, so a section that becomes writable
//     later cannot quietly acquire a plaintext-secret column.
//
// Both refuse with 400 and NAME the field. Silently dropping a field a caller
// believes it set is the failure mode this unit exists to remove.
//
// ## Authorisation
//
// The administration routes are gated in internal/api/router.go on
// `runtime.plugins`, which is the permission every pylon handler in this set
// declares (`@auth.decorators.check_api(["runtime.plugins"])`). Sections
// carrying `required_permission` are checked here as well, because the
// permission depends on the SECTION and route middleware cannot see it.
//
// The `prompt_lib` read is deliberately ungated beyond authentication, matching
// pylon's `PromptLibAPI`: the Help Center is a page for every user, and its card
// titles and documentation links are not sensitive. It is restricted to the
// `resources` section by having its own route rather than by a parameter.
//
// ## Mode is stated, never sniffed
//
// `administration` is a STATIC path segment so chi's trie prefers it over the
// `{mode}` pair. A static segment binds no URL parameter, so a handler reading
// `chi.URLParam(r, "mode")` would see `""` on exactly the requests that are
// administration requests. These handlers take the mode as a Go value.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// resourcesSectionID is the only section this platform stores and reads back.
const resourcesSectionID = "resources"

// maxConfigValueBytes bounds one stored value. The links editor is free-text and
// the row is served to every user through the public route below; without a
// bound, one paste turns the Help Center into a multi-megabyte response.
const maxConfigValueBytes = 64 * 1024

// configValuesResponse is pylon's GET shape. `fields_meta` is present because
// the reference client reads it; the per-pylon members pylon puts there
// (`plugin`, `pylon_id`) are absent by design — there are no pylons — and the
// page does not invent them.
type configValuesResponse struct {
	Values     map[string]any             `json:"values"`
	FieldsMeta map[string]configFieldMeta `json:"fields_meta,omitempty"`
	Saved      bool                       `json:"saved,omitempty"`
	Restart    []map[string]any           `json:"requires_restart,omitempty"`
}

type configFieldMeta struct {
	Path            string `json:"path"`
	RequiresRestart bool   `json:"requires_restart"`
}

// AdministrationPluginConfigValues serves
// `GET /admin/plugin_config_values/administration/{plugin}`.
//
// `{plugin}` is pylon's name for what is really the SECTION id; the reference
// client sends the section id there and this preserves that.
func (h *Handler) AdministrationPluginConfigValues(w http.ResponseWriter, r *http.Request) {
	section, ok := h.resolveWritableSection(w, r)
	if !ok {
		return
	}

	stored, err := h.loadSectionValues(r, section.id)
	if err != nil {
		// Never an empty 200: "this section has never been configured" and "the
		// configuration store is unreachable" are different facts and the page
		// must be able to tell them apart.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the platform configuration store",
		})
		return
	}

	writeJSON(w, http.StatusOK, configValuesResponse{
		Values:     mergeSectionValues(section, stored),
		FieldsMeta: sectionFieldsMeta(section),
	})
}

// PromptLibResourcesValues serves
// `GET /admin/plugin_config_values/prompt_lib/resources` — the Help Center's own
// read, for any authenticated user.
//
// pylon exposes exactly one section this way (`_PUBLIC_SECTIONS = {"resources"}`)
// and this route is restricted the same way, by being a route rather than a
// parameter: there is no section id a caller could substitute.
func (h *Handler) PromptLibResourcesValues(w http.ResponseWriter, r *http.Request) {
	section, ok := findConfigSection(resourcesSectionID)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "resources section missing"})
		return
	}
	stored, err := h.loadSectionValues(r, resourcesSectionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the platform configuration store",
		})
		return
	}
	// No `fields_meta`: this response is consumed by the product UI, not the
	// admin form, and `requires_restart` is an authoring concern.
	writeJSON(w, http.StatusOK, map[string]any{"values": mergeSectionValues(section, stored)})
}

// configSaveBody is the accepted write. pylon's shape, unchanged.
type configSaveBody struct {
	Values map[string]any `json:"values"`
}

// AdministrationPluginConfigValuesSave serves
// `PUT /admin/plugin_config_values/administration/{plugin}`.
func (h *Handler) AdministrationPluginConfigValuesSave(w http.ResponseWriter, r *http.Request) {
	section, ok := h.resolveWritableSection(w, r)
	if !ok {
		return
	}

	var body configSaveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if body.Values == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "values is required"})
		return
	}

	if reason := validateSectionValues(section, body.Values); reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}
	// The branding section carries field rules the schema cannot express
	// (a hue, an asset path); they apply through this door as well as the
	// typed one, so neither editor can store what the other refuses.
	if section.id == platformconfig.SectionBranding {
		if reason := validateBrandingValues(body.Values); reason != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
			return
		}
	}

	principal, _ := auth.UserFromContext(r.Context())
	author := principal.Email
	if author == "" {
		author = principal.ID
	}

	if err := h.storeSectionValues(r, section.id, body.Values, author); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not write the platform configuration store",
		})
		return
	}
	if section.id == platformconfig.SectionBranding {
		h.invalidateBranding()
		if stored, err := h.loadSectionValues(r, section.id); err == nil {
			h.collectBrandingAssets(r.Context(), stored)
		}
	}

	// Re-READ rather than echo. An echo would make a write that silently failed
	// to persist indistinguishable from one that landed — the exact defect
	// #130/#180 shipped twice.
	stored, err := h.loadSectionValues(r, section.id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the platform configuration store",
		})
		return
	}

	writeJSON(w, http.StatusOK, configValuesResponse{
		Saved:  true,
		Values: mergeSectionValues(section, stored),
		// Always empty, and honestly so. `requires_restart` in pylon names the
		// PYLONS whose plugins must reload; the only writable section here has
		// no field carrying the flag, and this service has no reload signal to
		// offer even if it did (`plugin_config_restart` answers 501). The page
		// therefore renders no restart bar rather than a button that no-ops.
		Restart: []map[string]any{},
	})
}

// ---------------------------------------------------------------------------
// section resolution
// ---------------------------------------------------------------------------

// configSection is one entry of configSections(), resolved once.
type configSection struct {
	id     string
	fields []map[string]any
	raw    map[string]any
}

func findConfigSection(id string) (configSection, bool) {
	for _, raw := range configSections() {
		gotID, _ := raw["id"].(string)
		if gotID != id {
			continue
		}
		fields, _ := raw["fields"].([]map[string]any)
		return configSection{id: gotID, fields: fields, raw: raw}, true
	}
	return configSection{}, false
}

// resolveWritableSection answers 404 for a section that does not exist, 501 with
// the section's own declared reason for one this platform cannot serve, and 403
// when the section declares a `required_permission` the caller lacks.
//
// The 501 is the correction that matters. Before this unit both verbs answered
// 200 for every section — the GET with schema defaults, the PUT with an empty
// object — so "this deployment does not configure that" and "that is configured
// to its defaults" rendered identically, and a save into a void reported success.
func (h *Handler) resolveWritableSection(w http.ResponseWriter, r *http.Request) (configSection, bool) {
	id := chi.URLParam(r, "plugin")
	section, ok := findConfigSection(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown configuration section"})
		return configSection{}, false
	}
	// The PERMISSION is checked before the availability is disclosed. Which
	// sections a deployment can and cannot serve is itself information about the
	// deployment, and the caller who may not read a section's values may not read
	// its disposition either.
	if perm, _ := section.raw["required_permission"].(string); perm != "" {
		if !h.hasAdministrationPermission(r, perm) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "access_denied"})
			return configSection{}, false
		}
	}
	if reason, _ := section.raw["unavailable_reason"].(string); reason != "" {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": reason})
		return configSection{}, false
	}
	return section, true
}

// hasAdministrationPermission fails CLOSED when no resolver is wired: a section
// that declares a permission is a section whose contents are privileged, and
// answering it unchecked because a dependency is missing is how implicit-admin
// bugs get shipped.
func (h *Handler) hasAdministrationPermission(r *http.Request, permission string) bool {
	if h.resolver == nil {
		return false
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return false
	}
	resolution, err := h.resolver.ResolvePermissions(
		r.Context(), principal, auth.PermissionModeAdministration, "",
	)
	if err != nil {
		return false
	}
	for _, granted := range resolution.Permissions {
		if granted == permission {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// values
// ---------------------------------------------------------------------------

// mergeSectionValues overlays what is STORED onto what the schema DECLARES.
//
// The overlay direction is the point. Every declared key is present in the
// response whether or not it has ever been written, so the form renders
// completely on a fresh install; and a stored key that the schema no longer
// declares is dropped rather than surfaced, so removing a field from the schema
// removes it from the product rather than leaving an orphan the page cannot edit.
func mergeSectionValues(section configSection, stored map[string]any) map[string]any {
	values := make(map[string]any, len(section.fields))
	for _, field := range section.fields {
		key, _ := field["key"].(string)
		if key == "" {
			continue
		}
		if def, ok := field["default"]; ok {
			values[key] = def
		} else {
			values[key] = nil
		}
		if got, ok := stored[key]; ok {
			values[key] = got
		}
	}
	return values
}

func sectionFieldsMeta(section configSection) map[string]configFieldMeta {
	meta := make(map[string]configFieldMeta, len(section.fields))
	for _, field := range section.fields {
		key, _ := field["key"].(string)
		if key == "" {
			continue
		}
		path, _ := field["path"].(string)
		requiresRestart, _ := field["requires_restart"].(bool)
		meta[key] = configFieldMeta{Path: path, RequiresRestart: requiresRestart}
	}
	return meta
}

func (h *Handler) loadSectionValues(r *http.Request, section string) (map[string]any, error) {
	if h.pool == nil {
		return nil, fmt.Errorf("no database pool")
	}
	rows, err := h.pool.Query(r.Context(),
		`SELECT key, value FROM centry.platform_config WHERE section = $1`, section)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := make(map[string]any)
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		values[key] = decoded
	}
	return values, rows.Err()
}

// storeSectionValues upserts every supplied key in ONE transaction.
//
// All-or-nothing matters more here than it looks: the page saves a whole section
// at once, so a partial apply would leave a card enabled with the links of the
// card it replaced, and the operator would be looking at a success toast.
func (h *Handler) storeSectionValues(
	r *http.Request, section string, values map[string]any, author string,
) error {
	if h.pool == nil {
		return fmt.Errorf("no database pool")
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Sorted so the statement order is deterministic — two admins saving the
	// same section concurrently take row locks in the same order and cannot
	// deadlock against each other.
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		encoded, err := json.Marshal(values[key])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
            VALUES ($1, $2, $3::jsonb, now(), $4)
            ON CONFLICT (section, key)
            DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by
        `, section, key, string(encoded), author); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// validateSectionValues returns the empty string when the body may be stored, or
// the sentence the operator is shown. Every refusal names the offending key: a
// configuration form that says only "failed to save" leaves the operator to
// guess which of forty fields the server disliked.
func validateSectionValues(section configSection, values map[string]any) string {
	byKey := make(map[string]map[string]any, len(section.fields))
	for _, field := range section.fields {
		if key, _ := field["key"].(string); key != "" {
			byKey[key] = field
		}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		field, known := byKey[key]
		if !known {
			// Refused, not ignored. A caller that believes it set something the
			// schema does not declare has a wrong model of the system, and a 200
			// confirms it.
			return fmt.Sprintf("unknown configuration key for section %q: %q", section.id, key)
		}
		if reason := rejectUnavailableField(key, field); reason != "" {
			return reason
		}
		if reason := rejectCredentialField(key, field); reason != "" {
			return reason
		}
		if reason := validateFieldValue(key, field, values[key]); reason != "" {
			return reason
		}
	}
	return ""
}

// rejectUnavailableField is the FIELD-level twin of the section-level 501.
//
// A section is the wrong granularity when one control in an otherwise working
// section has nothing behind it. `agent_publishing` enforces its block switch
// and its whitelist for real and feeds its categories to the Agents Hub; only
// `publish_validation_rules` is inert, because publish validation here is
// deterministic and has no evaluator for custom criteria to reach. Withholding
// the whole section to disclose that one field would have taken away three
// working controls; accepting the field would have stored a prompt nothing runs.
//
// The refusal is a 400 rather than the section's 501 deliberately: the SECTION
// is implemented, so "not implemented" would be the wrong thing to say about the
// request. What is wrong is the field, and the message names it.
func rejectUnavailableField(key string, field map[string]any) string {
	if reason, _ := field["unavailable_reason"].(string); reason != "" {
		return fmt.Sprintf("%q cannot be set on this platform: %s", key, reason)
	}
	return ""
}

// rejectCredentialField refuses any field the schema marks `format: password`.
//
// Those are an OIDC client secret, a LiteLLM master key and a Postgres URL with
// a password in it. This service already has a vault for exactly that
// (`centry.secrets_key`/`secrets_data`, unit A14's Secrets page); a plaintext
// JSONB column readable by every holder of `runtime.plugins` is not it. The
// check is on the FIELD SPEC rather than on a section list so that a section
// which becomes writable later cannot quietly acquire a plaintext-secret column.
func rejectCredentialField(key string, field map[string]any) string {
	if format, _ := field["format"].(string); format == "password" {
		return fmt.Sprintf(
			"%q is a credential and is refused here: secrets belong in the vault, not in a plaintext platform-configuration row",
			key,
		)
	}
	return ""
}

// forbiddenSplashMarkup names what a `format: "html"` field may not contain.
//
// The list is the executable and document-level half of the client sanitiser's
// forbid-list (apps/elitea-web shared/ui/lib/sanitizeMarkdownHtml.ts), plus the
// two attribute forms that carry script without a tag: an `on…=` handler and a
// `javascript:` URL. Everything here would either run code in the browser of
// every user shown the splash, or reach outside the page the splash is rendered
// into.
//
// WHY BOTH ENDS CHECK. This is not the sanitiser. A rejection here is an
// operator's mistake reported at the moment they can fix it, and it keeps
// executable markup out of a row that outlives every renderer that reads it. A
// renderer still sanitises, because a row written before this check existed —
// or by a future writer that forgets it — must not become a script tag on
// screen. Neither check makes the other redundant.
var forbiddenSplashMarkup = []string{
	"<script", "</script",
	"<style", "</style",
	"<iframe", "<object", "<embed", "<link", "<meta", "<base", "<form",
	"<svg", "<math", "<noscript",
	"javascript:",
	"onerror=", "onload=", "onclick=", "onmouseover=", "onfocus=", "onanimationstart=",
}

// validateSplashHTML refuses executable markup in a `format: "html"` value.
//
// The check is a case-insensitive substring scan and NOT an HTML parse, on
// purpose. A parser here would have to agree exactly with the browser's about
// what a malformed document means, and where they disagreed the browser would
// win — which is the whole class of bypass this kind of check keeps falling to.
// A substring scan over-refuses instead: it will reject the literal text
// "<script" in prose. That is the safe direction for a field whose output is
// injected into every user's page, and an operator who wants to WRITE about a
// script tag has the markdown message field beside this one.
func validateSplashHTML(key, text string) string {
	lowered := strings.ToLower(text)
	for _, forbidden := range forbiddenSplashMarkup {
		if strings.Contains(lowered, forbidden) {
			return fmt.Sprintf(
				"%q must not contain %q: the splash body is rendered in the browser of every user "+
					"the maintenance window refuses, so it carries no script, style, frame or "+
					"document-level markup",
				key, forbidden,
			)
		}
	}
	return ""
}

func validateFieldValue(key string, field map[string]any, value any) string {
	declared, _ := field["type"].(string)

	if strings.HasSuffix(key, "_links") {
		return validateLinks(key, value)
	}

	switch declared {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Sprintf("%q must be a boolean", key)
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Sprintf("%q must be a string", key)
		}
		if len(text) > maxConfigValueBytes {
			return fmt.Sprintf("%q is too long", key)
		}
		if format, _ := field["format"].(string); format == "html" {
			if reason := validateSplashHTML(key, text); reason != "" {
				return reason
			}
		}
		if allowed, ok := field["enum"].([]string); ok && len(allowed) > 0 {
			for _, candidate := range allowed {
				if candidate == text {
					return ""
				}
			}
			return fmt.Sprintf("%q must be one of: %s", key, strings.Join(allowed, ", "))
		}
	case "integer", "number":
		if _, ok := value.(float64); !ok {
			return fmt.Sprintf("%q must be a number", key)
		}
	case "array":
		entries, ok := value.([]any)
		if !ok {
			return fmt.Sprintf("%q must be an array", key)
		}
		return validateArrayItems(key, field, entries)
	case "object":
		entries, ok := value.(map[string]any)
		if !ok {
			return fmt.Sprintf("%q must be an object", key)
		}
		return validateObjectEntries(key, field, entries)
	}
	return ""
}

// Bounds on a map-valued field. `blocked_tools` and `sensitive_tools` are the
// only two, and both are read on paths that run per request and per agent
// execution — the toolkit catalogue and the agent tool freeze. An unbounded map
// is therefore not merely untidy: it is a cost every caller pays forever, paid
// once by whoever pasted it.
//
// The numbers are generous against the real shape of the data. The pinned SDK
// registry has 52 toolkit types, and the largest of them declares well under a
// hundred tools.
const (
	maxConfigObjectKeys      = 256
	maxConfigObjectListItems = 512
)

// validateObjectEntries checks a map-valued field against the element type its
// `additionalProperties` declares.
//
// This is `validateArrayItems`' argument one level up, and it applies with more
// force here. Both map fields are read back by code that type-asserts as it
// walks — `Values.StringLists` skips a value that is not an array and skips an
// element that is not a string — so `{"github": "create_issue"}` (a string where
// a list belongs) would be accepted, persisted, echoed by the GET, rendered in
// the form, and silently ignored by every consumer. The operator would have
// every reason to believe they had blocked that tool.
//
// The KEY is left unconstrained on purpose: it is a toolkit identifier, and the
// matching layer canonicalises it and drops what canonicalises to nothing
// (internal/domain/guardrails). Refusing an unknown toolkit name here would also
// refuse a toolkit type that a later SDK revision adds, and refusing `"*"` would
// refuse the wildcard the sensitive-tool map legitimately uses.
func validateObjectEntries(key string, field map[string]any, entries map[string]any) string {
	if len(entries) > maxConfigObjectKeys {
		return fmt.Sprintf("%q has too many entries (limit %d)", key, maxConfigObjectKeys)
	}

	additional, _ := field["additionalProperties"].(map[string]any)
	valueType, _ := additional["type"].(string)
	if valueType == "" {
		return ""
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		switch valueType {
		case "array":
			items, ok := entries[name].([]any)
			if !ok {
				return fmt.Sprintf("%q[%q] must be an array", key, name)
			}
			if len(items) > maxConfigObjectListItems {
				return fmt.Sprintf("%q[%q] has too many entries (limit %d)", key, name, maxConfigObjectListItems)
			}
			if reason := validateArrayItems(fmt.Sprintf("%s[%q]", key, name), additional, items); reason != "" {
				return reason
			}
		case "string":
			if _, ok := entries[name].(string); !ok {
				return fmt.Sprintf("%q[%q] must be a string", key, name)
			}
		}
	}
	return ""
}

// validateArrayItems checks an array against the element type its field
// declares, when it declares one.
//
// This matters more than a generic "must be an array" because both of the arrays
// this page writes are read back by code that TYPE-ASSERTS the elements and
// skips what does not match: `AgentCategories` takes `e.(string)` and
// `publishBlockedFor` takes `float64`. Storing `{"agent_categories": [{"n":1}]}`
// would therefore be accepted, persisted, echoed back by the GET, shown in the
// form — and silently ignored by the only consumer. That is the "saves into a
// void" failure at one level down, and the operator would have every reason to
// believe the category existed.
//
// A field that declares no `items` type (none does today, but the Configuration
// page's `blocked_toolkits` would if it were writable) is left unconstrained
// rather than guessed at.
func validateArrayItems(key string, field map[string]any, entries []any) string {
	items, _ := field["items"].(map[string]any)
	itemType, _ := items["type"].(string)

	for index, entry := range entries {
		switch itemType {
		case "string":
			if _, ok := entry.(string); !ok {
				return fmt.Sprintf("%q[%d] must be a string", key, index)
			}
		case "integer":
			// JSON numbers decode as float64. An integer field that was sent
			// 1.5 is a different mistake from one sent "1", and both are
			// refused, but the message is the same: this is a project id.
			number, ok := entry.(float64)
			if !ok || number != float64(int64(number)) {
				return fmt.Sprintf("%q[%d] must be an integer", key, index)
			}
		}
	}
	return ""
}

// validateLinks is the security boundary of the one writable section.
//
// These entries are rendered as anchors on the Help Center, to every
// authenticated user on the platform. A `javascript:` href there is stored XSS
// with an administrator's blessing, and `data:text/html,…` is the same attack
// wearing a different scheme. Only http and https are accepted, and the refusal
// names the scheme so an operator who pasted a `mailto:` learns why.
//
// Note what is deliberately NOT checked: the HOST. These links are followed by
// the user's browser, not fetched by the server, so an internal hostname here is
// a broken link rather than SSRF — and blocking private ranges would break the
// on-premises deployments whose documentation genuinely lives on an intranet.
func validateLinks(key string, value any) string {
	entries, ok := value.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array of links", key)
	}
	if len(entries) > 64 {
		return fmt.Sprintf("%q has too many links", key)
	}
	for index, entry := range entries {
		link, ok := entry.(map[string]any)
		if !ok {
			return fmt.Sprintf("%q[%d] must be an object with title and url", key, index)
		}
		title, _ := link["title"].(string)
		if len(title) > 512 {
			return fmt.Sprintf("%q[%d] title is too long", key, index)
		}
		rawURL, _ := link["url"].(string)
		if strings.TrimSpace(rawURL) == "" {
			continue
		}
		if len(rawURL) > 2048 {
			return fmt.Sprintf("%q[%d] url is too long", key, index)
		}
		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			return fmt.Sprintf("%q[%d] url could not be parsed", key, index)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Sprintf(
				"%q[%d] url must use http or https; %q links are refused because they execute in every user's browser",
				key, index, scheme,
			)
		}
	}
	return ""
}
