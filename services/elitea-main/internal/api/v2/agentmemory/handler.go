// Package agentmemory serves the Memory routes of an agent (ADR-0050
// decision 1b, #1607): list and read entries, edit, pin, archive, approve or
// reject, delete, clear, and a person's "remember things about me" switch.
//
// WHO MAY DO WHAT. The router admits every route on
// models.applications.memory.list (a project member who can use the agent).
// The handler then decides:
//
//   - An entry about another person (subject_user_id) is visible only to
//     that person and to a manager. A hidden entry is 404, not 403, so its
//     existence is not disclosed.
//   - A manager is an agent owner (agent_registry business or technical
//     owner) or a holder of models.applications.memory.update. A manager
//     edits, pins and archives; deleting and clearing also take
//     models.applications.memory.delete (or ownership); approving or
//     rejecting a pending entry takes models.applications.memory.approve (or
//     ownership).
//   - Anyone may correct or delete an entry about themself, and switch the
//     agent's memory of them off.
package agentmemory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The permissions (shared 0229).
const (
	ListPermission    = "models.applications.memory.list"
	UpdatePermission  = "models.applications.memory.update"
	DeletePermission  = "models.applications.memory.delete"
	ApprovePermission = "models.applications.memory.approve"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	EntriesPath = "/agent_memory/prompt_lib/{projectID}/{applicationID}"
	EntryPath   = "/agent_memory/prompt_lib/{projectID}/{applicationID}/{entryID}"
	OptOutPath  = "/agent_memory_opt_out/prompt_lib/{projectID}/{applicationID}"
	maxBody     = 8 << 10
)

// Store is *repos.AgentMemory.
type Store interface {
	ListMemory(ctx context.Context, projectID, applicationID int64, viewer repos.AgentMemoryViewer, filter repos.AgentMemoryFilter) ([]repos.AgentMemoryEntry, int, error)
	GetMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer repos.AgentMemoryViewer) (repos.AgentMemoryEntry, error)
	UpdateMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer repos.AgentMemoryViewer, approver bool, update repos.AgentMemoryUpdate) (repos.AgentMemoryEntry, error)
	DeleteMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer repos.AgentMemoryViewer) error
	ClearMemory(ctx context.Context, projectID, applicationID, actorID int64) (int64, error)
	SetSubjectOptOut(ctx context.Context, projectID, applicationID, userID int64, optOut bool) error
	SubjectOptedOut(ctx context.Context, projectID, applicationID, userID int64) (bool, error)
	IsAgentOwner(ctx context.Context, projectID, applicationID, userID int64) (bool, error)
	PeopleNames(ctx context.Context, ids []int64) ([]repos.AgentMemoryPerson, error)
}

var _ Store = (*repos.AgentMemory)(nil)

// PermissionCheck reports whether the caller holds one permission in the
// path's project.
type PermissionCheck func(r *http.Request, permission string) (bool, error)

// Handler serves the routes.
type Handler struct {
	store Store
	holds PermissionCheck
}

// NewHandler builds the handler. A nil store answers 503; a nil check holds
// nothing beyond the route's own permission.
func NewHandler(store Store, holds PermissionCheck) *Handler {
	if typed, ok := store.(*repos.AgentMemory); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, holds: holds}
}

// caller is who asks, and what they may do with this agent's memory.
type caller struct {
	projectID, applicationID int64
	userID                   int64
	owner                    bool
	update, del, approve     bool
}

func (c caller) viewer() repos.AgentMemoryViewer {
	return repos.AgentMemoryViewer{UserID: c.userID, Manager: c.owner || c.update}
}

func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return caller{}, false
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return caller{}, false
	}
	applicationID, ok := pathID(w, r, "applicationID")
	if !ok {
		return caller{}, false
	}
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return caller{}, false
	}
	c := caller{projectID: projectID, applicationID: applicationID, userID: userID}
	var err error
	if c.owner, err = h.store.IsAgentOwner(r.Context(), projectID, applicationID, userID); err != nil {
		return caller{}, unavailable(w, r, err)
	}
	for permission, held := range map[string]*bool{UpdatePermission: &c.update, DeletePermission: &c.del, ApprovePermission: &c.approve} {
		if h.holds == nil {
			continue
		}
		if *held, err = h.holds(r, permission); err != nil {
			return caller{}, unavailable(w, r, err)
		}
	}
	return c, true
}

func unavailable(w http.ResponseWriter, r *http.Request, err error) bool {
	slog.ErrorContext(r.Context(), "agent memory: resolve the caller", "error", err)
	apierr.WriteStatus(w, http.StatusServiceUnavailable, "could not resolve the caller's permissions")
	return false
}

// List answers the entries the caller may see.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := repos.AgentMemoryFilter{
		Kind: query.Get("kind"), Status: query.Get("status"), Query: strings.TrimSpace(query.Get("query")),
		Source: query.Get("source"),
	}
	if filter.Kind != "" && !oneOf(filter.Kind, "fact", "preference", "episode", "cursor") ||
		filter.Status != "" && !oneOf(filter.Status, "pending", "active", "archived") || len(filter.Query) > 256 ||
		filter.Source != "" && !oneOf(filter.Source, "agent", "person") {
		apierr.Write(w, apierr.BadRequest("kind, status, source or query is not valid"))
		return
	}
	var err error
	for name, target := range map[string]*time.Time{"created_after": &filter.CreatedAfter, "created_before": &filter.CreatedBefore} {
		if raw := query.Get(name); raw != "" {
			if *target, err = time.Parse(time.RFC3339, raw); err != nil {
				apierr.Write(w, apierr.BadRequest(name+" is not an RFC 3339 date-time"))
				return
			}
		}
	}
	for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		if raw := query.Get(name); raw != "" {
			if *target, err = strconv.Atoi(raw); err != nil || *target < 0 {
				apierr.Write(w, apierr.BadRequest(name+" is not valid"))
				return
			}
		}
	}
	if raw := query.Get("subject_user_id"); raw != "" {
		if filter.SubjectUserID, err = strconv.ParseInt(raw, 10, 32); err != nil || filter.SubjectUserID <= 0 {
			apierr.Write(w, apierr.BadRequest("subject_user_id is not valid"))
			return
		}
	}
	entries, total, err := h.store.ListMemory(r.Context(), c.projectID, c.applicationID, c.viewer(), filter)
	if err != nil {
		writeError(w, r, err)
		return
	}
	optedOut, err := h.store.SubjectOptedOut(r.Context(), c.projectID, c.applicationID, c.userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Only the people of rows the caller may see are named.
	people, err := h.store.PeopleNames(r.Context(), peopleOf(entries))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": entries, "total": total, "people": people, "can_manage": c.viewer().Manager,
		"can_delete": c.owner || c.del, "can_approve": c.owner || c.approve, "opted_out": optedOut,
	})
}

// Get answers one entry with its version history.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	entryID, ok := pathID(w, r, "entryID")
	if !ok {
		return
	}
	entry, err := h.store.GetMemory(r.Context(), c.projectID, c.applicationID, entryID, c.viewer())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// Update edits, pins, archives, approves or rejects one entry.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	entryID, ok := pathID(w, r, "entryID")
	if !ok {
		return
	}
	var body struct {
		Content *string `json:"content"`
		Kind    *string `json:"kind"`
		Pinned  *bool   `json:"pinned"`
		Status  *string `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Content == nil && body.Kind == nil && body.Pinned == nil && body.Status == nil {
		apierr.Write(w, apierr.BadRequest("nothing to change"))
		return
	}
	entry, err := h.store.UpdateMemory(r.Context(), c.projectID, c.applicationID, entryID, c.viewer(), c.owner || c.approve,
		repos.AgentMemoryUpdate{Content: body.Content, Kind: body.Kind, Pinned: body.Pinned, Status: body.Status})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// Delete deletes one entry.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	entryID, ok := pathID(w, r, "entryID")
	if !ok {
		return
	}
	viewer := repos.AgentMemoryViewer{UserID: c.userID, Manager: c.owner || c.del}
	if err := h.store.DeleteMemory(r.Context(), c.projectID, c.applicationID, entryID, viewer); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Clear deletes every entry of the agent.
func (h *Handler) Clear(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !c.owner && !c.del {
		writeError(w, r, repos.ErrAgentMemoryForbidden)
		return
	}
	cleared, err := h.store.ClearMemory(r.Context(), c.projectID, c.applicationID, c.userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}

// SetOptOut switches the agent's memory of the caller off or back on.
func (h *Handler) SetOptOut(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		OptedOut *bool `json:"opted_out"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.OptedOut == nil {
		apierr.Write(w, apierr.BadRequest("opted_out is required"))
		return
	}
	if err := h.store.SetSubjectOptOut(r.Context(), c.projectID, c.applicationID, c.userID, *body.OptedOut); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opted_out": *body.OptedOut})
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repos.ErrAgentMemoryNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "The memory entry does not exist.", "code": "not_found"})
	case errors.Is(err, repos.ErrAgentMemoryForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "You cannot make this change to the agent's memory.", "code": "memory_change_forbidden"})
	case errors.Is(err, repos.ErrAgentMemoryInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "memory_entry_invalid"})
	default:
		slog.ErrorContext(r.Context(), "agent memory", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the agent's memory")
	}
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || value <= 0 {
		apierr.Write(w, apierr.BadRequest("the path's "+name+" is not a valid id"))
		return 0, false
	}
	return value, true
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		apierr.Write(w, apierr.BadRequest("the request body is not readable or too large"))
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		apierr.Write(w, apierr.BadRequest("the request body is not valid JSON for this route"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// peopleOf answers the distinct subjects and writers of entries, sorted.
func peopleOf(entries []repos.AgentMemoryEntry) []int64 {
	ids := []int64{}
	for _, entry := range entries {
		for _, id := range []*int64{entry.SubjectUserID, entry.WrittenByUserID} {
			if id != nil && *id > 0 && !slices.Contains(ids, *id) {
				ids = append(ids, *id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
