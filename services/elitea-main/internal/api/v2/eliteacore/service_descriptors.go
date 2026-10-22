package eliteacore

// The admin SERVICE DESCRIPTORS surface — unit A14, issue #200. The tenth and
// last page of the admin port.
//
//	GET    /elitea_core/admin/administration              — the descriptor listing
//	POST   /elitea_core/register_descriptor/{projectID}   — register one
//	DELETE /elitea_core/register_descriptor/{projectID}   — remove one
//
// HISTORICAL, AND KEPT AS SUCH. Everything below describes why all three routes
// answered 501, and it is the account of a decision rather than of the current
// behaviour: migration 0107 gave this surface a store, so the listing answers
// from tables and both registration verbs write, and migration 0109 added the
// policy overlay behind the activate/deactivate routes in provider_activation.go.
// The 501 path still exists and is still reached — a deployment that has not
// applied those migrations has no admission plane, which is exactly the state
// the reason describes. Read on for what the subsystem is and why the shape it
// replaces was not portable; ServiceDescriptorsUnavailableReason is still the
// single sentence every refusal gives.
//
// ## What a "service descriptor" is, in the system this page came from
//
// It is not a description of this platform's services. It is one row of pylon's
// PROVIDER HUB: an external service provider that registers itself with Elitea
// and contributes toolkits to agents.
//
//   - An `ExternalServiceProviderDescriptor` (a JSON Schema in
//     legacy/plugins/elitea_core/data/, from which a Pydantic model is generated
//     at plugin load by methods/descriptor_model.py) carries a provider `name`, a
//     `service_location_url`, a `configuration` blob and a list of
//     `provided_toolkits`, each with its tools and argument schemas.
//   - `api/v2/register_descriptor.py` validates the posted descriptor, stores the
//     raw bytes in `elitea_core_descriptor` keyed by a JSON triple of
//     (project_id, provider_name, service_location_url), and then calls
//     `init_provider`.
//   - `methods/providers.py:init_provider` performs an HTTP health check against
//     the provider's URL and files the parsed descriptor into ONE OF TWO
//     IN-PROCESS DICTS on the plugin module — `present_providers` or
//     `unhealthy_providers`.
//   - `api/v2/admin.py` — the endpoint this page reads — walks exactly those two
//     dicts and emits `{project_id, provider_name, service_location_url, healthy}`
//     per entry. `healthy` is not a stored column: it is which dict the entry
//     landed in when this pylon process last started.
//   - `methods/provider_lookup.py:lookup_provider` reads `present_providers` to
//     pick a provider for a toolkit call, and `utils/internal_tools.py` uses it to
//     decide whether image generation is available.
//
// The concept is real and it carries real data. In the reference deployment's
// database `elitea_core_descriptor` holds 22 rows — DeepWiki and Inventory
// providers across nineteen projects, plus an ImageGen provider — so this is not
// a feature nobody used.
//
// ## Why it is nonetheless not servable here
//
// 1. There is NOTHING TO READ. This service has no descriptor table, no
//    migration that would create one, no repository, and no code path that
//    registers, health-checks or looks up a provider. The single mention of the
//    subsystem in the whole Go tree is a constant asserting its absence:
//    `internal/application/configurations/available.go` pins the dynamic
//    source `provider_hub_configurations` to `current_source_returns_empty`.
//
// 2. There would be NOTHING TO SERVE EVEN WITH THE TABLE. Two of the four
//    columns this page displays do not come from storage. `healthy` is
//    process-local health-probe state belonging to a Pylon plugin's module
//    object, and the row set itself is the union of two such dicts — not a
//    query. Copying the table across without the runtime that fills those dicts
//    would produce a listing whose health column was invented.
//
// 3. The target architecture DOES plan provider descriptors, and deliberately
//    not these. AGENTS.md's "Go owns provider descriptors, authorization, policy,
//    routing, health, and grants" is a statement of intent about ADR-0012 and the
//    Provider Service Protocol specification, both of which are still `In Review`
//    and sit in phase P3 of the migration plan ("Convert provider descriptors to
//    immutable manifests"). That contract replaces the mutable descriptor blob
//    this page edits with an admitted, immutable `AdmittedProviderRevisionV1`
//    reached through a review pipeline, and replaces the free-form
//    `service_location_url` with a normalised reviewed origin. A page built now
//    against the pylon shape would have to be rebuilt, and in the meantime would
//    be a registration surface for a runtime that cannot invoke what it registers.
//
// So the page renders this reason and every endpoint refuses. Which is a change
// on all three routes:
//
//   - `ServiceDescriptors` answered 200 with THREE HARDCODED ROWS naming
//     `elitea_core`, `auth` and `indexer` at version "2.0.0" with status
//     "active". None of those is a provider; they are Pylon plugin names, and the
//     shape is not even this endpoint's shape — the client reads `project_id`,
//     `provider_name`, `service_location_url` and `healthy`, none of which was
//     present. It took `_ *http.Request`, so it was also ungated: any
//     authenticated session got the same answer.
//   - `RegisterDescriptor` returned `{"ok": true}` for both verbs, decoding the
//     POST body into a variable it then dropped, and was MOUNTED ON NO ROUTE at
//     all. A no-op write handler with no caller is the shape that gets wired up
//     later by someone who assumes it works.
//
// A hardcoded row that looks like real data is worse than a 501, because nobody
// investigates it. An operator reading that listing would have concluded the
// platform had three healthy services registered.
//
// ## Authorisation
//
// Gated in internal/api/router.go on the permissions the pylon originals declare,
// resolved in `administration` mode:
//
//	admin.py               -> runtime.airun.serviceproviders
//	register_descriptor.py -> provider_hub.descriptor.register
//
// The gate runs BEFORE the refusal, so an unauthorised caller gets 403 and never
// learns whether the deployment serves this at all — which sub-systems a
// deployment runs is itself information about it. `window.admin_ui_config
// .permissions` is presentation state and is never consulted here; see
// apps/elitea-web/src/pages/admin/adminUiConfig.ts.
//
// ## Mode is stated, never sniffed
//
// pylon's `api_tools.APIBase` registers `admin.py` under `mode_handlers =
// {'administration': AdminAPI}` and no other mode, so `administration` is the
// only mode this path ever had. The route is registered as a STATIC
// `administration` segment rather than `{mode}`, which means `chi.URLParam(r,
// "mode")` is `""` on exactly the requests that are administration requests —
// the trap #207's test caught. These handlers take no mode from the URL, and any
// other mode 404s rather than being answered with something plausible.

import (
	"errors"
	"net/http"
	"strings"
)

// The permissions the pylon originals declare, resolved in `administration`
// mode. Constants rather than literals at the registration site because the
// router and the acceptance tests must not be able to disagree about them: a
// typo in either place alone produces a 403 that reads exactly like a
// deployment whose roles were never granted, and nothing would point at the
// spelling. `001_initial.sql` grants both to the administration-mode
// `super_admin` and `admin` roles.
const (
	// ServiceDescriptorListPermission gates the listing —
	// legacy/plugins/elitea_core/api/v2/admin.py:
	// `@auth.decorators.check_api(["runtime.airun.serviceproviders"])`.
	ServiceDescriptorListPermission = "runtime.airun.serviceproviders"

	// ServiceDescriptorRegisterPermission gates both registration verbs —
	// legacy/plugins/elitea_core/api/v2/register_descriptor.py declares
	// `provider_hub.descriptor.register` on POST and DELETE alike.
	ServiceDescriptorRegisterPermission = "provider_hub.descriptor.register"

	// ServiceDescriptorActivatePermission gates activate and deactivate. It has
	// no pylon original — pylon had no activation, because it had no admission
	// plane to activate anything in — so the string is CHOSEN, and granted by
	// migrations/shared/0109_provider_policy_overlay.sql to the
	// administration-mode `super_admin` and `admin` only.
	//
	// SEPARATE FROM `.register` ON PURPOSE. A facade's boot-time registrar
	// files a registration on every start, so `.register` is a permission a
	// deployment hands out freely; activation is the switch that lets agents
	// call the provider. One string for both would make the operator who may
	// record a descriptor automatically the operator who may put it in force —
	// and a holder of `.register` alone gets 403 on activate, which the
	// acceptance suite asserts rather than leaving to the router to remember.
	ServiceDescriptorActivatePermission = "provider_hub.descriptor.activate"
)

// ServiceDescriptorsUnavailableReason is the single sentence the server gives for
// every refusal on this surface, and the one the admin page renders.
//
// It is exported because `internal/api/v2/admin/config_schemas.go` declares the
// same reason on the Configuration page's `service_descriptors` section. One
// string, one place: an operator who reaches the subject from either surface must
// not be told two different things, and two copies drift the moment one is
// edited.
const ServiceDescriptorsUnavailableReason = "service descriptors registered EXTERNAL PROVIDERS with the legacy " +
	"runtime's provider hub: a posted descriptor was health-checked into an in-process registry that an agent's " +
	"toolkit call read to find its provider. This platform has no descriptor " +
	"store, no provider health probe and no provider lookup, so there is nothing to list and nothing a " +
	"registration would reach. The replacement is specified — immutable admitted provider manifests, ADR-0012 and " +
	"the Provider Service Protocol — but both are still In Review and scheduled for migration phase P3, and that " +
	"contract deliberately does not preserve this page's mutable descriptor shape."

// ServiceDescriptors serves `GET /elitea_core/admin/administration`.
//
// It answers 200 from storage now (migration 0107). Every column comes from a
// table; none is invented. See provider_admission.go for the three-state
// `healthy` and why it exists.
func (h *Handler) ServiceDescriptors(w http.ResponseWriter, r *http.Request) {
	// No database, or no migration 0107: there is no admission plane, which is
	// the state the recorded reason describes. An empty list would read as "no
	// providers are registered", and a 500 would tell an operator nothing
	// about the unapplied migration behind it.
	if h.pool == nil || !h.admissionPlanePresent(r.Context()) {
		writeServiceDescriptorsRefusal(w)
		return
	}
	rows, err := h.listServiceDescriptors(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the registered providers could not be read",
		})
		return
	}
	// THE POSTURE TRAVELS WITH THE ROWS, and it has to. `inactive` means two
	// completely different things depending on ELITEA_PROVIDER_ADMISSION: under
	// `record` the provider still serves every invoke, and under `enforce` it is
	// refused. A listing that shows the status without the posture shows an
	// operator a word whose meaning is held in an environment variable they
	// cannot see from the page — and the page would read identically in the two
	// deployments where the same row has opposite consequences.
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":              rows,
		"total":             len(rows),
		"admission_posture": admissionPostureName(),
	})
}

// RegisterDescriptor serves POST and DELETE on
// `/elitea_core/register_descriptor/{projectID}`.
//
// POST answers 202, not 200. The descriptor is RECORDED and is not in force:
// 200 would say the provider is admitted, and admitting one needs a policy
// overlay this deployment cannot issue. The body says which revision was
// created, which manifest digest it cites, that its status is inactive, and
// why — rather than the `{"ok": true}` the handler this replaces returned
// without storing anything.
//
// DELETE revokes. It never deletes a row: an admission that was once in force
// is a fact about what this deployment ran.
func (h *Handler) RegisterDescriptor(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil || !h.admissionPlanePresent(r.Context()) {
		writeServiceDescriptorsRefusal(w)
		return
	}
	projectID, err := projectIDFromPath(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.registerDescriptorRoute(w, r, projectID)
	case http.MethodDelete:
		h.revokeDescriptorRoute(w, r, projectID)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "this route serves POST and DELETE",
		})
	}
}

func (h *Handler) registerDescriptorRoute(w http.ResponseWriter, r *http.Request, projectID int64) {
	req, err := decodeRegisterRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	response, err := h.registerDescriptor(r.Context(), projectID, providerActor(r), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the descriptor could not be recorded",
		})
		return
	}
	// 202 ACCEPTED: recorded, not in force.
	writeJSON(w, http.StatusAccepted, response)
}

func (h *Handler) revokeDescriptorRoute(w http.ResponseWriter, r *http.Request, projectID int64) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider_name"))
	if provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "provider_name is required, so that a revoke names what it revokes",
		})
		return
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" {
		reason = "revoked through the administration surface"
	}

	err := h.revokeDescriptor(r.Context(), projectID, provider, providerActor(r), reason)
	switch {
	case errors.Is(err, ErrProviderNotRegistered):
		// 404 rather than a silent success: a revoke that matched nothing is
		// usually a misspelt provider, and reporting it as done sends the
		// operator away believing they turned something off.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such provider is registered for this project",
		})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the provider could not be revoked",
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "revoked", "reason": reason,
		})
	}
}

// writeServiceDescriptorsRefusal is the answer when the admission plane has no
// database. It keeps the recorded reason rather than inventing a new one.
func writeServiceDescriptorsRefusal(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": ServiceDescriptorsUnavailableReason,
	})
}
