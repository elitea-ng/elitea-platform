package admin

// The admin surface of the TOOLKIT TYPE catalogue — `Admin › Toolkits`.
//
// # What this is for
//
// The served type catalogue decides which tiles a user sees in the "+ Toolkit"
// chooser. Until this surface existed an operator could influence that in
// exactly two ways: the guardrails DENY-list, which is deployment-wide, carries
// no provenance and cannot say why a type is off; or a code change.
//
// This is the native control. It lists every type this deployment can serve,
// and it lets an administrator ENABLE it, DISABLE it, or RESTRICT it to named
// projects — always with a written reason and a recorded decider. Shared
// migration 0114 is the store.
//
// # What it deliberately does NOT do
//
// IT DOES NOT REGISTER A NEW TYPE. There are already two surfaces for that and
// a third registration path would be a third thing to keep in step:
//
//   - a pre-built MCP server becomes an `mcp_<key>` type, and its catalogue is
//     `Admin › Configuration` → the MCP servers editor (mcp_prebuilt.go);
//   - an external provider's toolkits arrive through provider-hub admission,
//     and its surface is `Admin › Service Descriptors`.
//
// The page links to both rather than growing its own add form. What this
// surface adds is the DECISION about a type that already exists.
//
// # The permission is a NEW string
//
// `toolkit_catalogue.type.manage`, granted by 0114 to the administration-mode
// super_admin, admin and system. Not a reuse of `runtime.plugins`: that grant
// already reaches the guardrails deny-list editor, which stops a type WORKING,
// while this decides what the product OFFERS and to which projects. The two
// must be separately grantable, and a reuse cannot be undone later without
// breaking every deployment that relied on it.
//
// # Absence is not unavailability
//
// A deployment with no policy rows serves the FULL catalogue, and this listing
// shows every type with `availability: "default"`. An unwired store answers 503
// with a sentence, never an empty list: "this platform offers no toolkit type"
// is never true, and rendering it would tell the operator their platform is
// broken.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// ToolkitTypePolicyStore is the seam this surface writes through.
//
// An interface so the package does not depend on a concrete pool for its unit
// tests, and so a test can assert that a refused write stores nothing.
type ToolkitTypePolicyStore interface {
	ListPolicies(ctx context.Context) ([]toolkitcatalogue.Policy, error)
	ListGrants(ctx context.Context) ([]toolkitcatalogue.Grant, error)
	SavePolicy(ctx context.Context, policy toolkitcatalogue.Policy) (toolkitcatalogue.Policy, error)
	DeletePolicy(ctx context.Context, toolkitType string) error
	SaveGrant(ctx context.Context, grant toolkitcatalogue.Grant) (toolkitcatalogue.Grant, error)
	DeleteGrant(ctx context.Context, toolkitType string, projectID int64) error
}

// WithToolkitTypePolicy supplies the policy store.
//
// Without it every route here answers 503 and writes nothing. It does not
// degrade to an in-memory decision: a control that accepts a decision and
// forgets it is worse than one that says it is unavailable, because the
// operator believes the type is off.
func WithToolkitTypePolicy(store ToolkitTypePolicyStore) Option {
	return func(h *Handler) {
		if store == nil {
			return
		}
		h.toolkitTypePolicy = store
	}
}

// ToolkitTypeManagePermission gates every route in this file, resolved in the
// `administration` mode. Shared migration 0114 grants it.
//
// Exported so internal/api/router.go names the same constant the handler
// documents, the way RolesCreatePermission and the provider-hub strings already
// do. A literal repeated at the route would be a second copy to drift, and
// router_permission_grant_gate_test.go resolves the constant through the import
// graph, so the gate reads exactly what ships.
const ToolkitTypeManagePermission = "toolkit_catalogue.type.manage"

// toolkitTypeCapabilities is the worker-support verdict source. It is a value
// with no state, so it needs no injection and no nil guard.
var toolkitTypeCapabilities = toolkitcatalogue.PinnedWorkerCapabilities{}

// availabilityDefault is the wire word for "no row": the type is served because
// nothing decided otherwise. It is NOT stored — writing it deletes the row.
const availabilityDefault = "default"

// maxBulkTypes bounds one bulk apply. The catalogue has fewer than a hundred
// types, so this cannot fire from the page; it is here so a scripted caller
// cannot turn one request into an unbounded transaction.
const maxBulkTypes = 512

/* ── wire shapes ────────────────────────────────────────────────────────── */

type toolkitTypeGrantView struct {
	ProjectID    int64  `json:"project_id"`
	Availability string `json:"availability"`
	Reason       string `json:"reason"`
	GrantedBy    string `json:"granted_by"`
	GrantedAt    string `json:"granted_at"`
}

type toolkitTypeCapabilityView struct {
	Verdict string `json:"verdict"`
	Python  bool   `json:"python"`
	Rust    bool   `json:"rust"`
	Reason  string `json:"reason"`
}

type toolkitTypeView struct {
	Type     string `json:"type"`
	Label    string `json:"label"`
	Category string `json:"category"`
	// Availability is the STORED decision, or "default" when there is none.
	Availability string `json:"availability"`
	// Reason, DecidedBy and DecidedAt are empty for a type at its default. They
	// are never filled with an invented sentence: "nothing was decided" is
	// carried by Availability, and a manufactured reason would read as an
	// operator's words.
	Reason    string `json:"reason"`
	DecidedBy string `json:"decided_by"`
	DecidedAt string `json:"decided_at"`
	// Available is what a project with NO grant row sees. A restricted type is
	// false here and still reachable through its grants, which is why the two
	// fields are both present.
	Available bool   `json:"available"`
	Source    string `json:"source"`
	// Registered reports whether this deployment's toolkit registry still
	// enumerates the type. A policy row for a type the registry dropped is
	// shown, marked, and can be reverted — hiding it would leave a decision the
	// operator cannot reach.
	Registered    bool                      `json:"registered"`
	Capability    toolkitTypeCapabilityView `json:"capability"`
	ProjectGrants []toolkitTypeGrantView    `json:"project_grants"`
}

type toolkitTypeDecisionBody struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason"`
}

type toolkitTypeBulkBody struct {
	Types        []string `json:"types"`
	Availability string   `json:"availability"`
	Reason       string   `json:"reason"`
}

/* ── readiness ──────────────────────────────────────────────────────────── */

func (h *Handler) toolkitTypePolicyReady(w http.ResponseWriter) bool {
	if h != nil && h.toolkitTypePolicy != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "the toolkit type policy store is not configured on this deployment",
	})
	return false
}

/* ── list ───────────────────────────────────────────────────────────────── */

// ToolkitTypeList answers `GET /admin/toolkit_types/administration`.
//
// It joins three sources: the deployment's toolkit registry (which types exist),
// the two policy tables (what was decided), and the pinned worker capability
// facts (what the admitted workers can build). One request, so the page cannot
// render a decision beside a type list read at a different moment.
func (h *Handler) ToolkitTypeList(w http.ResponseWriter, r *http.Request) {
	if !h.toolkitTypePolicyReady(w) {
		return
	}

	policies, err := h.toolkitTypePolicy.ListPolicies(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the toolkit type policy could not be read"})
		return
	}
	grants, err := h.toolkitTypePolicy.ListGrants(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the toolkit type project grants could not be read"})
		return
	}

	registered, registryAvailable := h.toolkitRegistryTypes()
	views := buildToolkitTypeViews(registered, registryAvailable, policies, grants)

	categories := make([]string, 0, len(toolkitcatalogue.Categories()))
	for _, category := range toolkitcatalogue.Categories() {
		categories = append(categories, string(category))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"types":      views,
		"categories": categories,
		"total":      len(views),
		// The page renders this rather than an empty grid when the registry is
		// unwired: zero toolkit types is not a state this platform can be in,
		// so an empty list would be a false statement about the deployment.
		"registry_available": registryAvailable,
	})
}

// buildToolkitTypeViews merges the three sources.
//
// A type with a policy row that the registry no longer enumerates is INCLUDED,
// marked `registered: false`. A decision the operator cannot see is a decision
// they cannot reverse, and the row still filters the served catalogue.
func buildToolkitTypeViews(
	registered []string,
	registryAvailable bool,
	policies []toolkitcatalogue.Policy,
	grants []toolkitcatalogue.Grant,
) []toolkitTypeView {
	policyByType := make(map[string]toolkitcatalogue.Policy, len(policies))
	for _, policy := range policies {
		policyByType[policy.ToolkitType] = policy
	}
	grantsByType := make(map[string][]toolkitcatalogue.Grant, len(grants))
	for _, grant := range grants {
		grantsByType[grant.ToolkitType] = append(grantsByType[grant.ToolkitType], grant)
	}

	known := make(map[string]bool, len(registered)+len(policies))
	order := make([]string, 0, len(registered)+len(policies))
	if registryAvailable {
		for _, toolkitType := range registered {
			if known[toolkitType] {
				continue
			}
			known[toolkitType] = true
			order = append(order, toolkitType)
		}
	}
	for _, policy := range policies {
		if known[policy.ToolkitType] {
			continue
		}
		known[policy.ToolkitType] = true
		order = append(order, policy.ToolkitType)
	}
	sort.Strings(order)

	views := make([]toolkitTypeView, 0, len(order))
	for _, toolkitType := range order {
		views = append(views, toolkitTypeViewFor(
			toolkitType,
			registryAvailable && slices.Contains(registered, toolkitType),
			policyByType[toolkitType],
			grantsByType[toolkitType],
		))
	}
	return views
}

func toolkitTypeViewFor(
	toolkitType string,
	registered bool,
	policy toolkitcatalogue.Policy,
	grants []toolkitcatalogue.Grant,
) toolkitTypeView {
	metadata := toolkitcatalogue.Lookup(toolkitType)
	capability := toolkitTypeCapabilities.ToolkitCapability(toolkitType)

	view := toolkitTypeView{
		Type:         toolkitType,
		Label:        metadata.Label,
		Category:     string(metadata.Category),
		Availability: availabilityDefault,
		Available:    true,
		Source:       string(toolkitcatalogue.SourceDefault),
		Registered:   registered,
		Capability: toolkitTypeCapabilityView{
			Verdict: string(capability.Verdict),
			Python:  capability.Python,
			Rust:    capability.Rust,
			Reason:  capability.Reason,
		},
		ProjectGrants: grantViews(grants),
	}

	if policy.ToolkitType == "" {
		return view
	}
	view.Availability = string(policy.Availability)
	view.Reason = policy.Reason
	view.DecidedBy = policy.DecidedBy
	view.DecidedAt = policy.DecidedAt.UTC().Format(time.RFC3339)
	view.Source = string(toolkitcatalogue.SourceDeployment)
	view.Available = policy.Availability == toolkitcatalogue.AvailabilityEnabled
	return view
}

func grantViews(grants []toolkitcatalogue.Grant) []toolkitTypeGrantView {
	views := make([]toolkitTypeGrantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, toolkitTypeGrantView{
			ProjectID:    grant.ProjectID,
			Availability: string(grant.Availability),
			Reason:       grant.Reason,
			GrantedBy:    grant.GrantedBy,
			GrantedAt:    grant.GrantedAt.UTC().Format(time.RFC3339),
		})
	}
	return views
}

/* ── set one policy ─────────────────────────────────────────────────────── */

// ToolkitTypeSave answers `PUT /admin/toolkit_types/administration/{type}`.
//
// `availability: "default"` DELETES the row and its project grants, which is
// the revert. It is spelled as a value of the same field rather than as a
// separate DELETE route because the page offers four choices in one control,
// and two transports for one control is how a client ends up implementing three
// of them.
func (h *Handler) ToolkitTypeSave(w http.ResponseWriter, r *http.Request) {
	if !h.toolkitTypePolicyReady(w) {
		return
	}
	toolkitType := chi.URLParam(r, "type")

	var body toolkitTypeDecisionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	if strings.TrimSpace(body.Availability) == availabilityDefault {
		if err := h.toolkitTypePolicy.DeletePolicy(r.Context(), toolkitType); err != nil {
			writeToolkitTypeError(w, err, "the toolkit type policy could not be written")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":         strings.TrimSpace(toolkitType),
			"availability": availabilityDefault,
		})
		return
	}

	saved, err := h.toolkitTypePolicy.SavePolicy(r.Context(), toolkitcatalogue.Policy{
		ToolkitType:  toolkitType,
		Availability: toolkitcatalogue.Availability(strings.TrimSpace(body.Availability)),
		Reason:       body.Reason,
		DecidedBy:    requestActor(r),
	})
	if err != nil {
		writeToolkitTypeError(w, err, "the toolkit type policy could not be written")
		return
	}
	writeJSON(w, http.StatusOK, toolkitTypeViewFor(saved.ToolkitType, true, saved, nil))
}

/* ── grant and revoke one project ───────────────────────────────────────── */

// ToolkitTypeGrantSave answers
// `PUT /admin/toolkit_types/administration/{type}/projects/{projectID}`.
func (h *Handler) ToolkitTypeGrantSave(w http.ResponseWriter, r *http.Request) {
	if !h.toolkitTypePolicyReady(w) {
		return
	}
	projectID, ok := toolkitTypeProjectID(w, r)
	if !ok {
		return
	}

	var body toolkitTypeDecisionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	saved, err := h.toolkitTypePolicy.SaveGrant(r.Context(), toolkitcatalogue.Grant{
		ToolkitType:  chi.URLParam(r, "type"),
		ProjectID:    projectID,
		Availability: toolkitcatalogue.Availability(strings.TrimSpace(body.Availability)),
		Reason:       body.Reason,
		GrantedBy:    requestActor(r),
	})
	if errors.Is(err, toolkitcatalogue.ErrPolicyNotFound) {
		// 409, not 404. The project and the type both exist; what is missing is
		// the deployment decision this grant would be an exception to. A 404
		// would send the operator looking for a type that is right in front of
		// them.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "decide about this toolkit type first: a project grant is an exception to a " +
				"deployment decision, and there is no decision to except",
		})
		return
	}
	if err != nil {
		writeToolkitTypeError(w, err, "the toolkit type project grant could not be written")
		return
	}
	writeJSON(w, http.StatusOK, grantViews([]toolkitcatalogue.Grant{saved})[0])
}

// ToolkitTypeGrantDelete answers
// `DELETE /admin/toolkit_types/administration/{type}/projects/{projectID}`.
func (h *Handler) ToolkitTypeGrantDelete(w http.ResponseWriter, r *http.Request) {
	if !h.toolkitTypePolicyReady(w) {
		return
	}
	projectID, ok := toolkitTypeProjectID(w, r)
	if !ok {
		return
	}
	err := h.toolkitTypePolicy.DeleteGrant(r.Context(), chi.URLParam(r, "type"), projectID)
	if errors.Is(err, toolkitcatalogue.ErrPolicyNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such project grant"})
		return
	}
	if err != nil {
		writeToolkitTypeError(w, err, "the toolkit type project grant could not be written")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": strings.TrimSpace(chi.URLParam(r, "type")), "project_id": projectID, "deleted": true,
	})
}

/* ── bulk apply ─────────────────────────────────────────────────────────── */

// ToolkitTypeBulk answers `POST /admin/toolkit_types/administration/bulk`.
//
// One decision, one reason, many types — the "disable every type this worker
// image cannot build" act, which is fifty dialogs otherwise.
//
// IT IS NOT ATOMIC, and the response says which types landed. A partial result
// is reported rather than hidden: the alternative is a transaction that rolls
// back forty-nine correct decisions because the fiftieth type name had a typo,
// and an operator who then cannot tell which of the two happened.
func (h *Handler) ToolkitTypeBulk(w http.ResponseWriter, r *http.Request) {
	if !h.toolkitTypePolicyReady(w) {
		return
	}

	var body toolkitTypeBulkBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if len(body.Types) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one toolkit type is required"})
		return
	}
	if len(body.Types) > maxBulkTypes {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "too many toolkit types in one request",
		})
		return
	}

	revert := strings.TrimSpace(body.Availability) == availabilityDefault
	applied := make([]string, 0, len(body.Types))
	failures := make([]map[string]any, 0)

	for _, toolkitType := range body.Types {
		var err error
		if revert {
			err = h.toolkitTypePolicy.DeletePolicy(r.Context(), toolkitType)
		} else {
			_, err = h.toolkitTypePolicy.SavePolicy(r.Context(), toolkitcatalogue.Policy{
				ToolkitType:  toolkitType,
				Availability: toolkitcatalogue.Availability(strings.TrimSpace(body.Availability)),
				Reason:       body.Reason,
				DecidedBy:    requestActor(r),
			})
		}
		if err != nil {
			failures = append(failures, map[string]any{
				"type": strings.TrimSpace(toolkitType), "error": toolkitTypeErrorMessage(err),
			})
			continue
		}
		applied = append(applied, strings.TrimSpace(toolkitType))
	}

	status := http.StatusOK
	if len(applied) == 0 {
		// Nothing landed. A 200 with an empty `applied` list reads as success
		// on a page that shows a spinner and then no change, which is the
		// "looks fine, is wrong" outcome this surface exists to remove.
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"applied": applied, "failed": failures, "total": len(applied),
	})
}

/* ── shared helpers ─────────────────────────────────────────────────────── */

// toolkitTypeProjectID reads and validates the path parameter.
func toolkitTypeProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projectID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "projectID")), 10, 64)
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return 0, false
	}
	return projectID, true
}

// requestActor names who decided.
//
// The e-mail when the principal carries one, the id otherwise. Never empty: the
// column refuses an empty decider, and an unattributed decision is one nobody
// can ask about. This mirrors branding.go's author resolution so the two admin
// surfaces attribute a change the same way.
func requestActor(r *http.Request) string {
	principal, _ := auth.UserFromContext(r.Context())
	if principal.Email != "" {
		return principal.Email
	}
	if principal.ID != "" {
		return principal.ID
	}
	return "unknown administrator"
}

// writeToolkitTypeError separates a refused BODY from a failed STORE.
//
// A validation failure is 400 with the store's own sentence, which names the
// field. Anything else is 503 with a fixed message: a pgx error names the
// database user, host and constraint, and that must not cross the boundary.
func writeToolkitTypeError(w http.ResponseWriter, err error, message string) {
	var validation toolkitcatalogue.PolicyValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": validation.Message})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": message})
}

func toolkitTypeErrorMessage(err error) string {
	var validation toolkitcatalogue.PolicyValidationError
	if errors.As(err, &validation) {
		return validation.Message
	}
	return "the toolkit type policy could not be written"
}
