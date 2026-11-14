// Package entityfolders serves the project folders of the Agents, Pipelines,
// Skills, Toolkits and MCPs lists (issue 2048).
//
// One model for all five lists, keyed by Kind. Unlike the chat folders
// (internal/api/v2/folders), which belong to one user, these folders are
// project-scoped and shared with every member, like the entities they group.
// The router gates every route with the SAME permission the list itself uses
// (list to read, create to make a folder, update to rename, reorder, move an
// entity or delete a folder), so no new permission exists.
package entityfolders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/entitylimits"
)

// Kind names the list a folder belongs to.
type Kind string

const (
	KindAgent    Kind = "agent"
	KindPipeline Kind = "pipeline"
	KindSkill    Kind = "skill"
	KindToolkit  Kind = "toolkit"
	KindMCP      Kind = "mcp"
)

// Kinds is every kind a folder can hold, in the order the lists appear.
var Kinds = []Kind{KindAgent, KindPipeline, KindSkill, KindToolkit, KindMCP}

// Folder is one project folder with the ids of the entities inside it.
type Folder struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name"`
	Position  int       `json:"position"`
	ItemIDs   []int     `json:"item_ids"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Repository is the storage the handler needs. Every method is scoped to one
// project and one kind.
type Repository interface {
	List(ctx context.Context, projectID string, kind Kind) ([]Folder, error)
	Create(ctx context.Context, projectID string, kind Kind, name string, position *int) (Folder, error)
	Update(ctx context.Context, projectID string, kind Kind, folderID string, name *string, position *int) (Folder, error)
	// Delete removes the folder and returns its entities to the root. It never
	// deletes an entity.
	Delete(ctx context.Context, projectID string, kind Kind, folderID string) error
	// Rebalance respaces the kind's folders by folders.PositionGap, keeping the
	// display order, for a drop between two folders with no integer between.
	Rebalance(ctx context.Context, projectID string, kind Kind) ([]Folder, error)
	// Move puts the entity into the folder, or at the root when folderID is nil.
	Move(ctx context.Context, projectID string, kind Kind, entityID int, folderID *string) error
}

type Handler struct{ repo Repository }

func NewHandler(repo Repository) *Handler { return &Handler{repo: repo} }

// KindOf reads and validates the `{kind}` path segment. An unknown kind is a
// 404: the path does not exist.
func KindOf(r *http.Request) (Kind, error) {
	raw := Kind(chi.URLParam(r, "kind"))
	for _, k := range Kinds {
		if k == raw {
			return k, nil
		}
	}
	return "", apierr.NotFound("unknown folder list")
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	kind, err := KindOf(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	items, err := h.repo.List(r.Context(), chi.URLParam(r, "projectID"), kind)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type createBody struct {
	Name     string `json:"name"`
	Position *int   `json:"position"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	kind, err := KindOf(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var body createBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	name, err := cleanName(body.Name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	created, err := h.repo.Create(r.Context(), chi.URLParam(r, "projectID"), kind, name, body.Position)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// Update renames and/or reorders. `name` absent leaves the name alone, and with
// neither `position` nor a neighbour the order is left alone, so a rename never
// moves the folder. Neighbour ids are authoritative over `position`, as for the
// chat folders (see folders.PositionBetween).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	kind, err := KindOf(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	projectID, folderID := chi.URLParam(r, "projectID"), chi.URLParam(r, "folderID")
	raw, err := readObject(r.Body)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	name, position, err := h.resolveUpdate(r.Context(), projectID, kind, raw)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	updated, err := h.repo.Update(r.Context(), projectID, kind, folderID, name, position)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) resolveUpdate(ctx context.Context, projectID string, kind Kind, raw map[string]json.RawMessage) (*string, *int, error) {
	var name *string
	if v, ok := raw["name"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, nil, apierr.BadRequest("name must be a string")
		}
		clean, err := cleanName(s)
		if err != nil {
			return nil, nil, err
		}
		name = &clean
	}
	var position *int
	if v, ok := raw["position"]; ok && string(v) != "null" {
		var p int
		if err := json.Unmarshal(v, &p); err != nil {
			return nil, nil, apierr.BadRequest("position must be an integer")
		}
		position = &p
	}
	above, hasAbove := optionalID(raw["neighbor_above_id"])
	below, hasBelow := optionalID(raw["neighbor_below_id"])
	if !hasAbove && !hasBelow {
		return name, position, nil
	}
	existing, err := h.repo.List(ctx, projectID, kind)
	if err != nil {
		return nil, nil, err
	}
	pos, rebalance := folders.PositionBetween(toPositions(existing), above, below)
	if rebalance {
		if existing, err = h.repo.Rebalance(ctx, projectID, kind); err != nil {
			return nil, nil, err
		}
		pos, _ = folders.PositionBetween(toPositions(existing), above, below)
	}
	return name, &pos, nil
}

func toPositions(in []Folder) []folders.Folder {
	out := make([]folders.Folder, len(in))
	for i := range in {
		p := in[i].Position
		out[i] = folders.Folder{ID: in[i].ID, Position: &p}
	}
	return out
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	kind, err := KindOf(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := h.repo.Delete(r.Context(), chi.URLParam(r, "projectID"), kind, chi.URLParam(r, "folderID")); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Move answers PUT .../items/{entityID} with {"folder_id": "<id>" | null}. A
// null (or absent) folder_id puts the entity back at the root.
func (h *Handler) Move(w http.ResponseWriter, r *http.Request) {
	kind, err := KindOf(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	entityID, err := strconv.Atoi(chi.URLParam(r, "entityID"))
	if err != nil || entityID <= 0 {
		apierr.Write(w, apierr.BadRequest("entity id must be a positive integer"))
		return
	}
	raw, err := readObject(r.Body)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var folderID *string
	if id, ok := optionalID(raw["folder_id"]); ok {
		folderID = &id
	}
	if err := h.repo.Move(r.Context(), chi.URLParam(r, "projectID"), kind, entityID, folderID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func cleanName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apierr.BadRequest("name is required")
	}
	if err := entitylimits.CheckName("name", name); err != nil {
		return "", err
	}
	return name, nil
}

func readObject(body io.Reader) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, apierr.BadRequest("invalid request body")
	}
	var raw map[string]json.RawMessage
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil, apierr.BadRequest("invalid request body")
	}
	return raw, nil
}

// optionalID reads an id the client may send as a string, a number or null.
func optionalID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, s != ""
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), true
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
