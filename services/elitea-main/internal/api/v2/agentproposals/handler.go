// Package agentproposals serves the agent proposals inbox and Copy to project
// (ADR-0050 decisions 5a and 1, #1615).
//
// THE INBOX. An agent whose version sets capabilities.can_propose_agents
// proposes, with the propose_agent tool, that a temporary sub-agent become a
// saved agent (elitea_lifecycle.agent_proposals). These routes list the open
// proposals, read one for the review form (the draft, the suggested memory
// seeds, the tool categories and the spawn's evidence), dismiss one, and
// record the agent a person created from one.
//
// NOTHING IS SAVED BEFORE CREATE. The review form creates the agent through
// the ordinary create route (models.applications.applications.create),
// which writes the applications row and its registry row at tier low. Then
// it calls MarkCreated, which only links the new agent to the proposal and
// writes the seeds the person kept into the NEW agent's own memory, and
// SeedMemory for those seeds. The proposing agent's tier is never read.
//
// COPY TO PROJECT. A personal agent (one in the caller's personal project)
// is copied by the fork route into a team project as a new agent with its own
// rows; CopyMemory then copies the entries the person picked from the source
// agent's memory into the copy's. Entries about a person and cursors are
// never copied.
//
// WHO MAY DO WHAT.
//
//   - list, get: models.applications.applications.list (route).
//   - dismiss: models.applications.applications.create, or an owner of the
//     proposing agent.
//   - mark created: models.applications.applications.create (route).
//   - copy memory: models.applications.applications.create in the target
//     (route), and in the source the caller is an owner of the agent or holds
//     models.applications.memory.list; the source must be the caller's own
//     personal project.
package agentproposals

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The permissions the routes and the handler check.
const (
	ListPermission       = "models.applications.applications.list"
	CreatePermission     = agentproposal.CreatePermission
	MemoryListPermission = "models.applications.memory.list"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	ProposalsPath  = "/agent_proposals/prompt_lib/{projectID}"
	ProposalPath   = "/agent_proposals/prompt_lib/{projectID}/{proposalID}"
	DismissPath    = "/agent_proposals/prompt_lib/{projectID}/{proposalID}/dismiss"
	CreatedPath    = "/agent_proposals/prompt_lib/{projectID}/{proposalID}/created"
	CopyMemoryPath = "/agent_copy_memory/prompt_lib/{projectID}/{applicationID}"
	maxBody        = 64 << 10
	maxCopyEntries = 200
)

// Store is *repos.AgentProposals.
type Store interface {
	ListProposals(ctx context.Context, projectID int64, filter repos.AgentProposalFilter) ([]repos.AgentProposal, int, error)
	GetProposal(ctx context.Context, projectID, proposalID int64) (repos.AgentProposal, error)
	DismissProposal(ctx context.Context, projectID, proposalID, userID int64) error
	MarkProposalCreated(ctx context.Context, projectID, proposalID, userID, applicationID int64) error
	SeedAgentMemory(ctx context.Context, projectID, applicationID, userID int64, entries []repos.CopiedMemory, action, resource string) (int, error)
	MemoryEntriesForCopy(ctx context.Context, projectID, applicationID int64, ids []int64) ([]repos.CopiedMemory, error)
	IsForkOfBy(ctx context.Context, projectID, applicationID, userID, sourceProjectID, sourceApplicationID int64) (bool, error)
}

var _ Store = (*repos.AgentProposals)(nil)

// Owners answers whether a user owns an agent (agent_registry business or
// technical owner): *repos.AgentMemory.
type Owners interface {
	IsAgentOwner(ctx context.Context, projectID, applicationID, userID int64) (bool, error)
}

// PermissionCheck reports whether the caller holds a permission in a
// project.
type PermissionCheck func(r *http.Request, projectID, permission string) (bool, error)

// PersonalProjects answers a user's personal project id, 0 for none.
type PersonalProjects interface {
	PersonalProjectID(ctx context.Context, userID string) (int, error)
}

// Handler serves the routes.
type Handler struct {
	store    Store
	owners   Owners
	holds    PermissionCheck
	personal PersonalProjects
}

// NewHandler builds the handler. A nil store answers 503.
func NewHandler(store Store, owners Owners, holds PermissionCheck, personal PersonalProjects) *Handler {
	if typed, ok := store.(*repos.AgentProposals); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, owners: owners, holds: holds, personal: personal}
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return false
	}
	return true
}

func userID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return 0, false
	}
	return id, true
}

// List answers the project's proposals: open ones unless status names
// created, dismissed or all; conversation_uuid narrows them to one chat.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := repos.AgentProposalFilter{Status: query.Get("status"), ConversationUUID: strings.TrimSpace(query.Get("conversation_uuid"))}
	if filter.Status != "" && !oneOf(filter.Status, "proposed", "created", "dismissed", "all") {
		apierr.Write(w, apierr.BadRequest("status is proposed, created, dismissed or all"))
		return
	}
	if filter.ConversationUUID != "" && !uuidLike(filter.ConversationUUID) {
		apierr.Write(w, apierr.BadRequest("conversation_uuid is not a UUID"))
		return
	}
	for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || (name == "limit" && value > 100) {
				apierr.Write(w, apierr.BadRequest(name+" is not valid"))
				return
			}
			*target = value
		}
	}
	rows, total, err := h.store.ListProposals(r.Context(), projectID, filter)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

// Get answers one proposal.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return
	}
	proposal, err := h.store.GetProposal(r.Context(), projectID, proposalID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

// Dismiss dismisses an open proposal. Its spec is suppressed for 30 days.
func (h *Handler) Dismiss(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return
	}
	user, ok := userID(w, r)
	if !ok {
		return
	}
	proposal, err := h.store.GetProposal(r.Context(), projectID, proposalID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	allowed, err := h.mayDecide(r, projectID, proposal.ProposerApplicationID, user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only a person who can create agents here, or the proposing agent's owner, can dismiss this proposal.", "code": "proposal_decision_forbidden"})
		return
	}
	if err := h.store.DismissProposal(r.Context(), projectID, proposalID, user); err != nil {
		writeError(w, r, err)
		return
	}
	proposal, err = h.store.GetProposal(r.Context(), projectID, proposalID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (h *Handler) mayDecide(r *http.Request, projectID, proposer, user int64) (bool, error) {
	if h.holds != nil {
		held, err := h.holds(r, strconv.FormatInt(projectID, 10), CreatePermission)
		if err != nil || held {
			return held, err
		}
	}
	if h.owners == nil {
		return false, nil
	}
	return h.owners.IsAgentOwner(r.Context(), projectID, proposer, user)
}

// createdBody is MarkCreated's body.
type createdBody struct {
	ApplicationID int64 `json:"application_id"`
	// MemorySeeds are the indexes of the proposal's seeds the person kept.
	MemorySeeds []int `json:"memory_seeds"`
}

// MarkCreated records the agent the review form created from an open
// proposal through the create route, and writes the seeds the person kept
// into that agent's memory. The agent must be one the caller just created
// (repos.ErrNotYourNewAgent, 403 otherwise).
func (h *Handler) MarkCreated(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return
	}
	user, ok := userID(w, r)
	if !ok {
		return
	}
	var body createdBody
	if !decode(w, r, &body) {
		return
	}
	if body.ApplicationID <= 0 || len(body.MemorySeeds) > agentproposal.MaxSeeds {
		apierr.Write(w, apierr.BadRequest("application_id is required and memory_seeds names at most 20 seeds"))
		return
	}
	proposal, err := h.store.GetProposal(r.Context(), projectID, proposalID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	seeds := make([]repos.CopiedMemory, 0, len(body.MemorySeeds))
	seen := map[int]struct{}{}
	for _, index := range body.MemorySeeds {
		if index < 0 || index >= len(proposal.MemorySeeds) {
			apierr.Write(w, apierr.BadRequest("memory_seeds names a seed the proposal does not have"))
			return
		}
		if _, dup := seen[index]; dup {
			continue
		}
		seen[index] = struct{}{}
		seed := proposal.MemorySeeds[index]
		seeds = append(seeds, repos.CopiedMemory{Kind: seed.Kind, Content: seed.Content})
	}
	if err := h.store.MarkProposalCreated(r.Context(), projectID, proposalID, user, body.ApplicationID); err != nil {
		writeError(w, r, err)
		return
	}
	seeded, err := h.store.SeedAgentMemory(r.Context(), projectID, body.ApplicationID, user, seeds,
		"agent.proposal.seeded", strconv.FormatInt(proposalID, 10))
	if err != nil {
		// The agent and the link stand; the seeds are a convenience the
		// person can add in the Memory tab.
		slog.ErrorContext(r.Context(), "agent proposals: seed the new agent's memory", "error", err)
	}
	proposal, err = h.store.GetProposal(r.Context(), projectID, proposalID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal, "memory_seeded": seeded})
}

// copyBody is CopyMemory's body.
type copyBody struct {
	SourceProjectID     int64   `json:"source_project_id"`
	SourceApplicationID int64   `json:"source_application_id"`
	EntryIDs            []int64 `json:"entry_ids"`
}

// CopyMemory copies the entries the person picked from a personal agent's
// memory into the memory of its copy in the path's project (Copy to
// project). The copy is an agent of its own (the fork route made it): only
// the text moves; the new entries are the person's, active, about nobody.
func (h *Handler) CopyMemory(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	applicationID, ok := pathID(w, r, "applicationID")
	if !ok {
		return
	}
	user, ok := userID(w, r)
	if !ok {
		return
	}
	var body copyBody
	if !decode(w, r, &body) {
		return
	}
	if body.SourceProjectID <= 0 || body.SourceApplicationID <= 0 || len(body.EntryIDs) > maxCopyEntries {
		apierr.Write(w, apierr.BadRequest("source_project_id and source_application_id are required; at most 200 entry_ids"))
		return
	}
	if body.SourceProjectID == projectID {
		apierr.Write(w, apierr.BadRequest("the copy must be in another project"))
		return
	}
	allowed, err := h.maySourceMemory(r, body.SourceProjectID, body.SourceApplicationID, user)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Copy to project copies a personal agent of yours, with the memory you can read.", "code": "copy_forbidden"})
		return
	}
	// The memory lands only in the caller's own fork of the source: active,
	// person-written entries in any other agent of the project would bypass
	// the memory routes' ownership and permission checks.
	fork, err := h.store.IsForkOfBy(r.Context(), projectID, applicationID, user, body.SourceProjectID, body.SourceApplicationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !fork {
		writeError(w, r, repos.ErrNotYourNewAgent)
		return
	}
	entries, err := h.store.MemoryEntriesForCopy(r.Context(), body.SourceProjectID, body.SourceApplicationID, body.EntryIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	copied, err := h.store.SeedAgentMemory(r.Context(), projectID, applicationID, user, entries, "agent.copied_to_project",
		strconv.FormatInt(body.SourceProjectID, 10)+":"+strconv.FormatInt(body.SourceApplicationID, 10))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"copied": copied})
}

// maySourceMemory: the source is the caller's personal project, and the
// caller owns the agent or reads its memory there.
func (h *Handler) maySourceMemory(r *http.Request, sourceProjectID, sourceApplicationID, user int64) (bool, error) {
	if h.personal == nil {
		return false, nil
	}
	personal, err := h.personal.PersonalProjectID(r.Context(), strconv.FormatInt(user, 10))
	if err != nil {
		return false, err
	}
	if personal <= 0 || int64(personal) != sourceProjectID {
		return false, nil
	}
	if h.owners != nil {
		owner, err := h.owners.IsAgentOwner(r.Context(), sourceProjectID, sourceApplicationID, user)
		if err != nil || owner {
			return owner, err
		}
	}
	if h.holds == nil {
		return false, nil
	}
	return h.holds(r, strconv.FormatInt(sourceProjectID, 10), MemoryListPermission)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repos.ErrProposalNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "The proposal or agent does not exist.", "code": "not_found"})
	case errors.Is(err, repos.ErrNotYourNewAgent):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "The memory can only go into the agent you just created or copied.", "code": "not_your_new_agent"})
	case errors.Is(err, repos.ErrProposalDecided):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "The proposal was already decided.", "code": "proposal_decided"})
	default:
		slog.ErrorContext(r.Context(), "agent proposals", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the agent proposal")
	}
}

func proposalPath(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return 0, 0, false
	}
	proposalID, ok := pathID(w, r, "proposalID")
	return projectID, proposalID, ok
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || (name != "proposalID" && value > 1<<31-1) {
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
	if err := decoder.Decode(target); err != nil {
		apierr.Write(w, apierr.BadRequest("the request body is not valid"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func uuidLike(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, r := range value {
		switch index {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
