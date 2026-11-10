// Package agentworkforce serves the agent workforce (ADR-0050 decisions 5b,
// 5g-5j, 5l and 5m, #1616): an agent's scorecard, workforce state and
// impact preview; dismissal and demotion proposals and their decision;
// reinstatement; the owner's comment and appeal and its review.
//
// WHO MAY DO WHAT.
//
//   - read (scorecard, state, impact, proposals): models.applications
//     .applications.list (route).
//   - propose: an agent manager (agents.workforce.manage) or the agent's
//     owner.
//   - approve, reject: an agent manager, or the owner for their own agent.
//   - ask the owner, reinstate, review an appeal: an agent manager; the
//     appeal's reviewer is not the manager who decided, nor the owner.
//   - owner comment, appeal: the agent's owner.
//
// These are decided in the handler and the store (the route gate is the
// read permission), so the owner path works without the manager grant.
package agentworkforce

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The permissions the routes and the handler check.
const (
	ListPermission   = "models.applications.applications.list"
	ManagePermission = agentworkforce.ManagePermission
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	AgentPath        = "/agent_workforce/prompt_lib/{projectID}/agents/{applicationID}"
	ScorecardsPath   = "/agent_workforce/prompt_lib/{projectID}/agents/{applicationID}/scorecards"
	ImpactPath       = "/agent_workforce/prompt_lib/{projectID}/agents/{applicationID}/impact"
	ReinstatePath    = "/agent_workforce/prompt_lib/{projectID}/agents/{applicationID}/reinstate"
	ProposalsPath    = "/agent_workforce/prompt_lib/{projectID}/proposals"
	ProposalPath     = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}"
	ApprovePath      = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/approve"
	RejectPath       = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/reject"
	AskOwnerPath     = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/ask_owner"
	CommentPath      = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/comment"
	AppealPath       = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/appeal"
	AppealDecidePath = "/agent_workforce/prompt_lib/{projectID}/proposals/{proposalID}/appeal/decide"
	maxBody          = 32 << 10
)

// Store is *repos.AgentWorkforce.
type Store interface {
	Agent(ctx context.Context, projectID, applicationID int64) (repos.WorkforceAgent, error)
	Scorecards(ctx context.Context, projectID, applicationID int64, days int) ([]agentworkforce.Scorecard, error)
	Impact(ctx context.Context, projectID, applicationID int64) (repos.WorkforceImpact, error)
	ListProposals(ctx context.Context, projectID int64, filter repos.WorkforceFilter) ([]repos.WorkforceProposal, int, error)
	GetProposal(ctx context.Context, projectID, proposalID int64) (repos.WorkforceProposal, error)
	ProposeByPerson(ctx context.Context, request repos.NewWorkforceProposal) (repos.WorkforceProposal, error)
	Approve(ctx context.Context, decision repos.Decision) (repos.WorkforceProposal, error)
	Reject(ctx context.Context, decision repos.Decision) (repos.WorkforceProposal, error)
	AskOwner(ctx context.Context, decision repos.Decision) (repos.WorkforceProposal, error)
	OwnerComment(ctx context.Context, projectID, proposalID, userID int64, comment string) (repos.WorkforceProposal, error)
	Appeal(ctx context.Context, projectID, proposalID, userID int64, reason string) (repos.WorkforceProposal, error)
	DecideAppeal(ctx context.Context, decision repos.AppealDecision) (repos.WorkforceProposal, error)
	Reinstate(ctx context.Context, projectID, applicationID int64, actor repos.WorkforceActor, reason string) (repos.WorkforceAgent, error)
}

var _ Store = (*repos.AgentWorkforce)(nil)

// PermissionCheck reports whether the caller holds a permission in a
// project.
type PermissionCheck func(r *http.Request, projectID, permission string) (bool, error)

// Handler serves the routes.
type Handler struct {
	store Store
	holds PermissionCheck
}

// NewHandler builds the handler. A nil store answers 503.
func NewHandler(store Store, holds PermissionCheck) *Handler {
	if typed, ok := store.(*repos.AgentWorkforce); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, holds: holds}
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return false
	}
	return true
}

// actor is the signed-in person and whether they are an agent manager in
// the project.
func (h *Handler) actor(w http.ResponseWriter, r *http.Request, projectID int64) (repos.WorkforceActor, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return repos.WorkforceActor{}, false
	}
	actor := repos.WorkforceActor{UserID: id}
	if h.holds != nil {
		held, err := h.holds(r, strconv.FormatInt(projectID, 10), ManagePermission)
		if err != nil {
			writeError(w, r, err)
			return repos.WorkforceActor{}, false
		}
		actor.Manager = held
	}
	return actor, true
}

// forbidden is the refusal of a read or a write the caller is not entitled
// to: ADR-0050 5h lets agent managers and the owner (and HR agents, over
// gRPC) read a scorecard, and the proposals carry rationales, owner
// comments and appeal reasons of the same sensitivity.
var forbidden = agentworkforce.Refuse(agentworkforce.ForbiddenCode,
	"Only an agent manager or the agent's owner can read its scorecards and proposals.")

// mayReadAgent: the caller is an agent manager or the agent's owner. It
// answers false (and writes the refusal) otherwise.
func (h *Handler) mayReadAgent(w http.ResponseWriter, r *http.Request, projectID, applicationID int64) bool {
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return false
	}
	if actor.Manager {
		return true
	}
	agent, err := h.store.Agent(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(w, r, err)
		return false
	}
	if !agent.IsOwner(actor.UserID) {
		writeError(w, r, forbidden)
		return false
	}
	return true
}

// AgentView is the workforce state of one agent with its latest scorecard.
type AgentView struct {
	repos.WorkforceAgent
	Scorecard *agentworkforce.Scorecard `json:"scorecard"`
	// CanManage: the caller is an agent manager in the project.
	CanManage bool `json:"can_manage"`
	// IsOwner: the caller owns the agent.
	IsOwner bool `json:"is_owner"`
}

// GetAgent answers the agent's workforce state and latest scorecard.
func (h *Handler) GetAgent(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, applicationID, ok := agentPath(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	agent, err := h.store.Agent(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := AgentView{WorkforceAgent: agent, CanManage: actor.Manager, IsOwner: agent.IsOwner(actor.UserID)}
	if !view.CanManage && !view.IsOwner {
		// The ladder state is visible to anyone who can list the agent (a
		// retired agent is badged everywhere); the scorecard is not (5h).
		writeJSON(w, http.StatusOK, view)
		return
	}
	cards, err := h.store.Scorecards(r.Context(), projectID, applicationID, 1)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(cards) > 0 {
		view.Scorecard = &cards[0]
	}
	writeJSON(w, http.StatusOK, view)
}

// ListScorecards answers the agent's daily scorecards, newest first
// (days, default 30, at most 400).
func (h *Handler) ListScorecards(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, applicationID, ok := agentPath(w, r)
	if !ok {
		return
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 400 {
			apierr.Write(w, apierr.BadRequest("days is 1 to 400"))
			return
		}
		days = value
	}
	if !h.mayReadAgent(w, r, projectID, applicationID) {
		return
	}
	cards, err := h.store.Scorecards(r.Context(), projectID, applicationID, days)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": cards, "total": len(cards)})
}

// GetImpact answers what retiring or suspending the agent would touch.
func (h *Handler) GetImpact(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, applicationID, ok := agentPath(w, r)
	if !ok {
		return
	}
	if !h.mayReadAgent(w, r, projectID, applicationID) {
		return
	}
	impact, err := h.store.Impact(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

// List answers the project's proposals: open ones unless status names
// approved, rejected, asked_owner, proposed or all; application_id narrows.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := repos.WorkforceFilter{Status: query.Get("status")}
	if filter.Status != "" && !oneOf(filter.Status, "proposed", "asked_owner", "approved", "rejected", "all") {
		apierr.Write(w, apierr.BadRequest("status is proposed, asked_owner, approved, rejected or all"))
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
	if raw := query.Get("application_id"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			apierr.Write(w, apierr.BadRequest("application_id is not valid"))
			return
		}
		filter.ApplicationID = value
	}
	// The project-wide inbox is the agent manager's; the owner reads the
	// proposals of their own agent (application_id).
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	if !actor.Manager {
		if filter.ApplicationID == 0 {
			writeError(w, r, forbidden)
			return
		}
		if !h.mayReadAgent(w, r, projectID, filter.ApplicationID) {
			return
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
	if !h.mayReadAgent(w, r, projectID, proposal.ApplicationID) {
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

type proposeBody struct {
	ApplicationID int64                    `json:"application_id"`
	Stage         string                   `json:"stage"`
	Rationale     string                   `json:"rationale"`
	Narrowing     agentworkforce.Narrowing `json:"narrowing"`
}

// Propose stores a person's proposal.
func (h *Handler) Propose(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	var body proposeBody
	if !decode(w, r, &body) {
		return
	}
	if body.ApplicationID <= 0 || body.ApplicationID > 1<<31-1 {
		apierr.Write(w, apierr.BadRequest("application_id is required"))
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	proposal, err := h.store.ProposeByPerson(r.Context(), repos.NewWorkforceProposal{
		ProjectID: projectID, ApplicationID: body.ApplicationID, Actor: actor,
		Input: agentworkforce.ProposalInput{Stage: agentworkforce.Stage(body.Stage), Rationale: body.Rationale, Narrowing: body.Narrowing},
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, proposal)
}

type decisionBody struct {
	Note string `json:"note"`
	// ConfirmNested confirms retiring an agent that other agents nest.
	ConfirmNested bool `json:"confirm_nested"`
}

func (h *Handler) decision(w http.ResponseWriter, r *http.Request) (repos.Decision, bool) {
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return repos.Decision{}, false
	}
	var body decisionBody
	if !decodeOptional(w, r, &body) {
		return repos.Decision{}, false
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return repos.Decision{}, false
	}
	return repos.Decision{ProjectID: projectID, ProposalID: proposalID, Actor: actor, Note: body.Note, ConfirmNested: body.ConfirmNested}, true
}

// Approve applies the proposal's stage.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.storeApprove)
}

// Reject keeps the agent.
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.storeReject)
}

// AskOwner routes a proposal to the agent's owner for comment.
func (h *Handler) AskOwner(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.storeAskOwner)
}

func (h *Handler) storeApprove(ctx context.Context, d repos.Decision) (repos.WorkforceProposal, error) {
	return h.store.Approve(ctx, d)
}

func (h *Handler) storeReject(ctx context.Context, d repos.Decision) (repos.WorkforceProposal, error) {
	return h.store.Reject(ctx, d)
}

func (h *Handler) storeAskOwner(ctx context.Context, d repos.Decision) (repos.WorkforceProposal, error) {
	return h.store.AskOwner(ctx, d)
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request, do func(context.Context, repos.Decision) (repos.WorkforceProposal, error)) {
	if !h.ready(w) {
		return
	}
	decision, ok := h.decision(w, r)
	if !ok {
		return
	}
	proposal, err := do(r.Context(), decision)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

type textBody struct {
	Text string `json:"text"`
}

// Comment records the owner's comment on a proposal routed to them.
func (h *Handler) Comment(w http.ResponseWriter, r *http.Request) {
	h.ownerText(w, r, func(ctx context.Context, projectID, proposalID, userID int64, text string) (repos.WorkforceProposal, error) {
		return h.store.OwnerComment(ctx, projectID, proposalID, userID, text)
	})
}

// Appeal is the owner's appeal of an approved decision.
func (h *Handler) Appeal(w http.ResponseWriter, r *http.Request) {
	h.ownerText(w, r, func(ctx context.Context, projectID, proposalID, userID int64, text string) (repos.WorkforceProposal, error) {
		return h.store.Appeal(ctx, projectID, proposalID, userID, text)
	})
}

func (h *Handler) ownerText(w http.ResponseWriter, r *http.Request, do func(context.Context, int64, int64, int64, string) (repos.WorkforceProposal, error)) {
	if !h.ready(w) {
		return
	}
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return
	}
	var body textBody
	if !decode(w, r, &body) {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	proposal, err := do(r.Context(), projectID, proposalID, actor.UserID, body.Text)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

type appealDecisionBody struct {
	Grant bool   `json:"grant"`
	Note  string `json:"note"`
}

// DecideAppeal grants or upholds an open appeal.
func (h *Handler) DecideAppeal(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, proposalID, ok := proposalPath(w, r)
	if !ok {
		return
	}
	var body appealDecisionBody
	if !decode(w, r, &body) {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	proposal, err := h.store.DecideAppeal(r.Context(), repos.AppealDecision{
		ProjectID: projectID, ProposalID: proposalID, Actor: actor, Grant: body.Grant, Note: body.Note,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

// Reinstate brings a retired agent back within 30 days.
func (h *Handler) Reinstate(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, applicationID, ok := agentPath(w, r)
	if !ok {
		return
	}
	var body decisionBody
	if !decodeOptional(w, r, &body) {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	agent, err := h.store.Reinstate(r.Context(), projectID, applicationID, actor, body.Note)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	if refusal, ok := agentworkforce.AsRefusal(err); ok {
		writeJSON(w, refusal.Status(), refusal.Body())
		return
	}
	slog.ErrorContext(r.Context(), "agent workforce", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the agent workforce")
}

func agentPath(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return 0, 0, false
	}
	applicationID, ok := pathID(w, r, "applicationID")
	return projectID, applicationID, ok
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

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		apierr.Write(w, apierr.BadRequest("the request body is not readable or too large"))
		return nil, false
	}
	return raw, true
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	return decodeRaw(w, raw, target)
}

// decodeOptional accepts an empty body.
func decodeOptional(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return true
	}
	return decodeRaw(w, raw, target)
}

func decodeRaw(w http.ResponseWriter, raw []byte, target any) bool {
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
