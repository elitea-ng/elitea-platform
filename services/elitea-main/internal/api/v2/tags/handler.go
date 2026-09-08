package tags

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Tag struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Data any    `json:"data"`
}

// EntityCoverage narrows a tag list to the tags one KIND of entity carries.
//
// A project's `tags` table is shared by every entity that can be tagged, so
// "the tags in this project" and "the tags I can filter agents by" are two
// different sets. The tag rail on the agents list wants the second one; the
// pipelines rail wants a third; and an unfiltered list answers all of them at
// once, which is why the legacy platform gave the read this argument.
type EntityCoverage string

const (
	// CoverageAll is every tag row the project holds, attached or not. It is
	// what an absent (or `all`) `entity_coverage` asks for, and it is the
	// only value that answers a tag nothing carries yet — the row a tag
	// CREATE has just made.
	CoverageAll EntityCoverage = "all"
	// CoverageApplication is the tags carried by agents: applications with no
	// pipeline version. Legacy draws the line at the APPLICATION and not at
	// the version (`not_(Entity.versions.any(agent_type == 'pipeline'))`),
	// so an application with one pipeline version is a pipeline, whole.
	CoverageApplication EntityCoverage = "application"
	// CoveragePipeline is the tags carried by pipelines: applications with at
	// least one pipeline version.
	CoveragePipeline EntityCoverage = "pipeline"
	// CoverageSkill is the tags carried by skill versions.
	CoverageSkill EntityCoverage = "skill"
)

// coverageFromQuery reads `entity_coverage` off the request.
//
// An unknown value is REFUSED rather than widened. The legacy implementation
// answers `{"rows": [], "total": 0}` for a value it does not recognise, which
// is the silent-empty shape a caller reads as "this project has no tags":
// a misspelt filter and an empty project are the same answer. A 400 naming
// the four accepted values says which of the two happened.
func coverageFromQuery(r *http.Request) (EntityCoverage, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("entity_coverage"))
	switch EntityCoverage(raw) {
	case "", CoverageAll:
		return CoverageAll, nil
	case CoverageApplication, CoveragePipeline, CoverageSkill:
		return EntityCoverage(raw), nil
	default:
		return "", apierr.BadRequest(
			"entity_coverage must be one of application, pipeline, skill, all")
	}
}

type Repository interface {
	List(ctx context.Context, projectID string, coverage EntityCoverage) ([]Tag, error)
	// Create stores one tag and answers the STORED row, id included. It is
	// idempotent on the name: `tags.name` is unique per tenant schema, so a
	// second create of the same name answers the row that already exists
	// rather than a second row or a conflict.
	Create(ctx context.Context, projectID string, tag Tag) (Tag, error)
	// Delete removes the tag and every association pointing at it, and
	// answers a 404 when the project holds no such tag.
	Delete(ctx context.Context, projectID, tagID string) error
}

type Handler struct {
	repo Repository
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	return r
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	coverage, err := coverageFromQuery(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	tags, err := h.repo.List(r.Context(), projectID, coverage)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"rows": tags, "total": len(tags)})
}

// Create stores a tag in the project.
//
// It used to decode the body and echo it back with a 201 and an `id` of 0,
// touching no table: the only way a tag row ever appeared was as a side
// effect of a version save. A client that created a tag here and then looked
// for it in the list could not find it, and the id it was handed addressed
// nothing.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var tag Tag
	if err := json.NewDecoder(r.Body).Decode(&tag); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	tag.Name = strings.TrimSpace(tag.Name)
	// `tags.name` is NOT NULL and is the key the whole feature is addressed
	// by, so a nameless tag is refused here rather than stored as "".
	if tag.Name == "" {
		apierr.Write(w, apierr.BadRequest("tag name is required"))
		return
	}

	stored, err := h.repo.Create(r.Context(), projectID, tag)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(stored)
}

// Delete removes one tag from the project.
//
// It used to write 204 and return, so a caller could not remove a tag at all:
// the row stayed in every list and in every agent that carried it, behind a
// success. The delete now removes the associations as well as the row, and a
// tag the project does not hold answers 404 rather than the same 204 a real
// delete does.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	tagID := chi.URLParam(r, "tagID")

	if err := h.repo.Delete(r.Context(), projectID, tagID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
