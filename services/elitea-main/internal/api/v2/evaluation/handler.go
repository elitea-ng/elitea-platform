package evaluation

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Handler serves the four dimension-library routes.
type Handler struct {
	repo Repository
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

// listResponse is `{rows, total}`.
//
// THE ENVELOPE IS PART OF THE CONTRACT, AND GETTING IT WRONG IS SILENT. This
// API serves three list shapes — `{rows,total}`, `{items,total,page,…}` and a
// bare array — and a client reading the wrong key gets `undefined`, coerces it
// to `[]`, and renders an empty library behind a 200 with nothing in the
// console (issue #132; a handler in this repository answered `items` where its
// client read `rows` and the screen simply stayed empty).
//
// `{rows,total}` is the shape every other /elitea_core project listing uses
// (tags, toolkits, users, participants), so it is the one chosen here. The
// client does NOT hardcode it either: it goes through
// `shared/api/unwrap.ts`'s `unwrapList`, which accepts all three and is LOUD
// on a fourth. Both halves are pinned by tests — this package's
// TestListAnswersRowsEnvelope and the web app's own unwrap tests — so the pair
// cannot drift apart unnoticed.
type listResponse struct {
	Rows  []Dimension `json:"rows"`
	Total int         `json:"total"`
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	filter := ListFilter{IncludePlatform: true}
	if raw := r.URL.Query().Get("include_platform"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("include_platform must be a boolean"))
			return
		}
		filter.IncludePlatform = parsed
	}
	// An absent `agent_id` means "the project library alone". It does NOT mean
	// "every ad-hoc dimension in the project": an ad-hoc dimension authored on
	// another agent is SCOPED to that agent, and listing it here would fill
	// every agent's editor with every other agent's criteria.
	//
	// The scope is a filter, NOT an authorization boundary. Every evaluation
	// route is gated on a project permission only, and so is the agent itself
	// (`models.applications.application.details` / `.update`): any project
	// editor may open and change any agent in the project, so they may read,
	// edit, delete or promote that agent's dimensions too. Nothing here checks
	// the caller against the agent's author, because nothing else in the
	// project does either. If agent-level ownership is ever introduced, these
	// routes must check it on List with `agent_id`, Update (including the
	// agent_adhoc -> project promotion) and Delete.
	if raw := r.URL.Query().Get("agent_id"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("agent_id must be an integer"))
			return
		}
		filter.ApplicationID = &parsed
	}

	dimensions, err := h.repo.List(r.Context(), projectID, filter)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if dimensions == nil {
		dimensions = []Dimension{}
	}
	writeJSON(w, http.StatusOK, listResponse{Rows: dimensions, Total: len(dimensions)})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	dimension, ok := decodeDimension(w, r)
	if !ok {
		return
	}
	dimension.Normalize()
	if err := dimension.Validate(true); err != nil {
		apierr.Write(w, err)
		return
	}

	created, err := h.repo.Create(r.Context(), projectID, dimension)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	dimensionID := chi.URLParam(r, "dimensionID")

	dimension, ok := decodeDimension(w, r)
	if !ok {
		return
	}
	// TIER ON AN UPDATE. The body's `tier` is the scope the caller asks for,
	// and an ABSENT tier keeps the stored scope: Normalize would otherwise
	// default it to `project` and promote an agent-scoped dimension without
	// the caller asking. The tier is therefore taken out before Normalize and
	// Validate, checked on its own here, and decided against the STORED tier
	// by ResolveTierUpdate inside the repository's write. `application_id` is
	// never taken from an update body: a promotion clears it, and nothing
	// else changes it.
	requestedTier := strings.TrimSpace(dimension.Tier)
	dimension.Tier = ""
	dimension.ApplicationID = nil
	dimension.Normalize()
	if err := dimension.Validate(false); err != nil {
		apierr.Write(w, err)
		return
	}
	if requestedTier != "" && !knownTiers[requestedTier] {
		apierr.Write(w, apierr.BadRequest("tier must be one of project, agent_adhoc, platform"))
		return
	}
	if requestedTier == TierPlatform {
		apierr.Write(w, apierr.BadRequest("the platform tier is not authorable: platform dimensions are materialised from the platform catalogue, which this release does not serve"))
		return
	}
	dimension.Tier = requestedTier

	updated, err := h.repo.Update(r.Context(), projectID, dimensionID, dimension)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	dimensionID := chi.URLParam(r, "dimensionID")

	if err := h.repo.Delete(r.Context(), projectID, dimensionID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeDimension(w http.ResponseWriter, r *http.Request) (Dimension, bool) {
	var dimension Dimension
	// An unknown field is refused rather than dropped. The baseline's editor
	// posts `evidence_scope` alongside the dimension body in one place, and
	// that field belongs to a BINDING — a concept this slice does not have.
	// Accepting and discarding it would report success for a setting that was
	// never stored, which is the failure this whole slice is trying not to
	// repeat. decodeStrict does that, and bounds the body (413) first.
	if !decodeStrict(w, r, &dimension) {
		return Dimension{}, false
	}
	return dimension, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
