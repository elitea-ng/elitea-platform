package agentworkforce

// The workforce policy routes (ADR-0050 decisions 5c and 5k, #1617): read
// and set the project's automatic hiring and dismissal policy, list what it
// did, undo or keep an automatic hire, object to a retire notice.
//
// WHO. Reading takes applications.list (the route). Setting the policy,
// undoing and keeping a hire take agents.workforce.manage (checked in the
// store, so the refusal is typed). Objecting takes the agent's ownership or
// agents.workforce.manage.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The policy route patterns, relative to the /api/v2/elitea_core group.
const (
	PolicyPath       = "/agent_workforce/prompt_lib/{projectID}/policy"
	ActionsPath      = "/agent_workforce/prompt_lib/{projectID}/automatic_actions"
	ActionUndoPath   = "/agent_workforce/prompt_lib/{projectID}/automatic_actions/{actionID}/undo"
	ActionKeepPath   = "/agent_workforce/prompt_lib/{projectID}/automatic_actions/{actionID}/keep"
	ActionObjectPath = "/agent_workforce/prompt_lib/{projectID}/automatic_actions/{actionID}/object"
)

// PolicyStore is the policy side of *repos.AgentWorkforce.
type PolicyStore interface {
	Policy(ctx context.Context, projectID int64) (repos.WorkforcePolicy, error)
	SetPolicy(ctx context.Context, projectID int64, actor repos.WorkforceActor, policy agentworkforce.Policy) (repos.WorkforcePolicy, error)
	ListAutomaticActions(ctx context.Context, projectID int64, filter repos.ActionFilter) ([]repos.AutomaticAction, int, error)
	UndoAutoHire(ctx context.Context, projectID, actionID int64, actor repos.WorkforceActor, note string, confirmNested bool) (repos.AutomaticAction, error)
	KeepAutoHire(ctx context.Context, projectID, actionID int64, actor repos.WorkforceActor, note string) (repos.AutomaticAction, error)
	ObjectToAction(ctx context.Context, projectID, actionID int64, actor repos.WorkforceActor, note string) (repos.AutomaticAction, error)
}

var _ PolicyStore = (*repos.AgentWorkforce)(nil)

func (h *Handler) policyStore(w http.ResponseWriter) (PolicyStore, bool) {
	if !h.ready(w) {
		return nil, false
	}
	store, ok := h.store.(PolicyStore)
	if !ok {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return nil, false
	}
	return store, true
}

// PolicyView is the policy with whether the caller may change it.
type PolicyView struct {
	repos.WorkforcePolicy
	CanManage bool `json:"can_manage"`
}

// GetPolicy answers the project's policy (the defaults, everything off,
// when none was saved).
func (h *Handler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := h.policyStore(w)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	policy, err := store.Policy(r.Context(), projectID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PolicyView{WorkforcePolicy: policy, CanManage: actor.Manager})
}

// SetPolicy saves the project's policy.
func (h *Handler) SetPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := h.policyStore(w)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	var body agentworkforce.Policy
	if !decode(w, r, &body) {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	policy, err := store.SetPolicy(r.Context(), projectID, actor, body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PolicyView{WorkforcePolicy: policy, CanManage: actor.Manager})
}

// ListActions answers the project's automatic actions, newest first.
func (h *Handler) ListActions(w http.ResponseWriter, r *http.Request) {
	store, ok := h.policyStore(w)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := repos.ActionFilter{Status: query.Get("status")}
	if filter.Status != "" && !oneOf(filter.Status, "open", agentworkforce.ActionPending, agentworkforce.ActionFlagged,
		agentworkforce.ActionApplied, agentworkforce.ActionHeld, agentworkforce.ActionObjected, agentworkforce.ActionUndone,
		agentworkforce.ActionKept, agentworkforce.ActionResolved, agentworkforce.ActionCancelled) {
		apierr.Write(w, apierr.BadRequest("status is not a known automatic action status"))
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
	rows, total, err := store.ListAutomaticActions(r.Context(), projectID, filter)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

// UndoAction undoes an automatic hire within its 14 days.
func (h *Handler) UndoAction(w http.ResponseWriter, r *http.Request) {
	h.actOn(w, r, func(ctx context.Context, store PolicyStore, projectID, actionID int64, actor repos.WorkforceActor, body decisionBody) (repos.AutomaticAction, error) {
		return store.UndoAutoHire(ctx, projectID, actionID, actor, body.Note, body.ConfirmNested)
	})
}

// KeepAction ends an automatic hire's probation.
func (h *Handler) KeepAction(w http.ResponseWriter, r *http.Request) {
	h.actOn(w, r, func(ctx context.Context, store PolicyStore, projectID, actionID int64, actor repos.WorkforceActor, body decisionBody) (repos.AutomaticAction, error) {
		return store.KeepAutoHire(ctx, projectID, actionID, actor, body.Note)
	})
}

// ObjectAction objects to a retire notice in its grace period.
func (h *Handler) ObjectAction(w http.ResponseWriter, r *http.Request) {
	h.actOn(w, r, func(ctx context.Context, store PolicyStore, projectID, actionID int64, actor repos.WorkforceActor, body decisionBody) (repos.AutomaticAction, error) {
		return store.ObjectToAction(ctx, projectID, actionID, actor, body.Note)
	})
}

func (h *Handler) actOn(w http.ResponseWriter, r *http.Request, do func(context.Context, PolicyStore, int64, int64, repos.WorkforceActor, decisionBody) (repos.AutomaticAction, error)) {
	store, ok := h.policyStore(w)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	actionID, ok := pathID(w, r, "actionID")
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
	action, err := do(r.Context(), store, projectID, actionID, actor, body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, action)
}
