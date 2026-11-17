// Package delegationgrants serves the delegation grants of a project's
// composition edges (ADR-0044 amendment 1, A1.2 rule 2; delegation epic 2156
// story S2b).
//
// Attaching a sub-agent, an agent tool or a pipeline child X to a caller Y
// creates a pending grant for the edge Y to X. An editor of Y approves it, all
// of it or the items they choose and hold access to. Under the governed
// profile the grant is approved through an ADR-0040 change request of kind
// `delegation` on Y instead, and the direct approve route answers 409 with the
// request to use. All routes are scoped to the caller agent Y, the {applicationID}
// of the path: a grant of another agent answers 404.
//
// Reading takes the permission that reads an agent. Proposing, approving and
// revoking take the permission that edits one; the approver's own access to
// toolkits and credential references limits what an approval can list.
package delegationgrants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	appgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Service is what the handler needs (application/delegationgrants.Service).
type Service interface {
	List(ctx context.Context, projectID, callerID int64, history bool) ([]appgrants.View, error)
	Get(ctx context.Context, projectID, callerID, grantID int64) (appgrants.View, error)
	Propose(ctx context.Context, req appgrants.ProposeRequest) (appgrants.View, error)
	Approve(ctx context.Context, projectID, callerID, grantID, approver int64, choose []dg.Item) (appgrants.View, error)
	Revoke(ctx context.Context, projectID, callerID, grantID, actor int64) (appgrants.View, error)
	Preview(ctx context.Context, req appgrants.PreviewRequest) (appgrants.Preview, error)
}

// Handler serves the routes.
type Handler struct{ service Service }

// NewHandler builds the handler.
func NewHandler(service Service) *Handler { return &Handler{service: service} }

// GovernedCode is the code of the 409 that sends the caller to the grant's
// change request.
const GovernedCode = "DELEGATION_GOVERNED"

// Item is one thing a grant lists: a toolkit, a tool of a toolkit, a credential
// reference, a data class or an egress host.
type Item struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Toolkit string `json:"toolkit,omitempty"`
}

// Grant is one delegation grant.
type Grant struct {
	ID                  int64      `json:"id"`
	CallerApplicationID int64      `json:"caller_application_id"`
	CalleeApplicationID int64      `json:"callee_application_id"`
	EdgeKind            string     `json:"edge_kind"`
	CalleeVersionID     int64      `json:"callee_version_id"`
	State               string     `json:"state"`
	Legacy              bool       `json:"legacy"`
	Governed            bool       `json:"governed"`
	Origin              string     `json:"origin"`
	Digest              string     `json:"digest"`
	RequestedDigest     string     `json:"requested_digest"`
	Items               []Item     `json:"items"`
	RequestedItems      []Item     `json:"requested_items"`
	Omitted             []Item     `json:"omitted"`
	ProposedBy          *int64     `json:"proposed_by,omitempty"`
	ApprovedBy          *int64     `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	RevokedBy           *int64     `json:"revoked_by,omitempty"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	SupersededBy        *int64     `json:"superseded_by,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	// ChangeRequestID is the change request that approves a governed grant.
	ChangeRequestID *int64  `json:"change_request_id,omitempty"`
	ChangeState     *string `json:"change_state,omitempty"`
	// Drift is what the callee holds now that a stale grant does not cover.
	Drift *Drift `json:"drift,omitempty"`
}

// Drift is what a stale grant's callee holds beyond the grant (A1.2 rule 3).
type Drift struct {
	CalleeVersionID int64  `json:"callee_version_id"`
	Digest          string `json:"digest"`
	Added           []Item `json:"added"`
}

// GrantList is the grants of one caller agent.
type GrantList struct {
	Grants []Grant `json:"grants"`
}

// ClosureNode is one agent reached through approved grants.
type ClosureNode struct {
	ApplicationID int64  `json:"application_id"`
	Name          string `json:"name"`
	Depth         int    `json:"depth"`
	Items         []Item `json:"items"`
}

// Preview is what an attach would let the callee use.
type Preview struct {
	CalleeApplicationID int64         `json:"callee_application_id"`
	CalleeVersionID     int64         `json:"callee_version_id"`
	CalleeName          string        `json:"callee_name"`
	Digest              string        `json:"digest"`
	Items               []Item        `json:"items"`
	Approvable          []Item        `json:"approvable"`
	Withheld            []Item        `json:"withheld"`
	Closure             []ClosureNode `json:"closure"`
	Existing            *Grant        `json:"existing,omitempty"`
}

func items(in []dg.Item) []Item {
	out := make([]Item, 0, len(in))
	for _, item := range in {
		out = append(out, Item{Kind: item.Kind, Name: item.Name, Toolkit: item.Toolkit})
	}
	return out
}

func grant(v appgrants.View) Grant {
	g := v.Grant
	out := Grant{
		ID: g.ID, CallerApplicationID: g.Edge.CallerEntityID, CalleeApplicationID: g.Edge.CalleeEntityID,
		EdgeKind: string(g.Edge.Kind), CalleeVersionID: g.CalleeVersionID, State: string(g.State),
		Legacy: g.Legacy, Governed: g.Governed, Origin: string(g.Origin), Digest: g.Digest, RequestedDigest: g.RequestedDigest,
		Items: items(v.Items), RequestedItems: items(v.RequestedItems), Omitted: items(g.Omitted),
		ProposedBy: g.ProposedBy, ApprovedBy: g.ApprovedBy, ApprovedAt: g.ApprovedAt, RevokedBy: g.RevokedBy,
		RevokedAt: g.RevokedAt, SupersededBy: g.SupersededBy, CreatedAt: g.CreatedAt.UTC(), UpdatedAt: g.UpdatedAt.UTC(),
	}
	if g.Drift != nil && g.State == dg.StateStale {
		out.Drift = &Drift{CalleeVersionID: g.Drift.CalleeVersionID, Digest: g.Drift.Digest, Added: items(g.Drift.Added)}
	}
	if v.ChangeRequestID > 0 {
		id, state := v.ChangeRequestID, string(v.ChangeState)
		out.ChangeRequestID, out.ChangeState = &id, &state
	}
	return out
}

func ids(r *http.Request) (projectID, applicationID int64, err error) {
	projectID, err = strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID <= 0 {
		return 0, 0, apierr.BadRequest("project id must be a positive integer")
	}
	applicationID, err = strconv.ParseInt(chi.URLParam(r, "applicationID"), 10, 64)
	if err != nil || applicationID <= 0 || applicationID > 1<<31-1 {
		return 0, 0, apierr.BadRequest("application id must be a positive integer")
	}
	return projectID, applicationID, nil
}

func grantID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "grantID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apierr.BadRequest("grant id must be a positive integer")
	}
	return id, nil
}

func actor(r *http.Request) (int64, error) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		return 0, apierr.Unauthorized("this action needs a signed-in user")
	}
	return id, nil
}

// List serves GET: the grants of the agent's edges. `history=true` adds the
// superseded and revoked ones.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	views, err := h.service.List(r.Context(), projectID, applicationID, r.URL.Query().Get("history") == "true")
	if err != nil {
		writeError(w, err)
		return
	}
	list := GrantList{Grants: make([]Grant, 0, len(views))}
	for _, v := range views {
		list.Grants = append(list.Grants, grant(v))
	}
	writeJSON(w, http.StatusOK, list)
}

// Get serves GET of one grant.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	id, err := grantID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	v, err := h.service.Get(r.Context(), projectID, applicationID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, grant(v))
}

type proposeBody struct {
	CalleeApplicationID int64  `json:"callee_application_id"`
	CalleeVersionID     int64  `json:"callee_version_id"`
	EdgeKind            string `json:"edge_kind"`
}

// Propose serves POST: the pending grant of an edge, from what the callee
// version holds now. Attaching creates it; this route lets an editor ask for it
// again (for example after a detach and re-attach). Under the governed profile
// it also records the grant's change request.
func (h *Handler) Propose(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	user, err := actor(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var body proposeBody
	if err := decode(r, &body); err != nil {
		apierr.Write(w, err)
		return
	}
	if body.CalleeApplicationID <= 0 || body.CalleeVersionID <= 0 {
		apierr.Write(w, apierr.BadRequest("callee_application_id and callee_version_id are required"))
		return
	}
	v, err := h.service.Propose(r.Context(), appgrants.ProposeRequest{
		ProjectID: projectID, CallerID: applicationID, CalleeID: body.CalleeApplicationID,
		CalleeVersionID: body.CalleeVersionID, EdgeKind: dg.EdgeKind(body.EdgeKind), Origin: dg.OriginAttach, Actor: user,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     "Proposed a delegation grant to agent " + strconv.FormatInt(body.CalleeApplicationID, 10),
		EntityType: "delegation_grant", EntityID: audit.ID(v.Grant.ID), ProjectID: audit.ID(projectID),
	})
	writeJSON(w, http.StatusCreated, grant(v))
}

type approveBody struct {
	// Items are the items the approver keeps. Absent or null keeps all of them;
	// an empty list keeps none and is refused.
	Items *[]Item `json:"items"`
}

// Approve serves POST .../approve: the editor of the caller approves a pending
// grant, all of it or the items they choose. They approve only what they hold
// access to; the rest is recorded as omitted and the callee runs without it.
// A governed grant is approved through its change request: 409.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	id, err := grantID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	user, err := actor(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var body approveBody
	if err := decode(r, &body); err != nil {
		apierr.Write(w, err)
		return
	}
	var choose []dg.Item
	if body.Items != nil {
		choose = make([]dg.Item, 0, len(*body.Items))
		for _, item := range *body.Items {
			choose = append(choose, dg.Item{Kind: item.Kind, Name: item.Name, Toolkit: item.Toolkit})
		}
	}
	v, err := h.service.Approve(r.Context(), projectID, applicationID, id, user, choose)
	if err != nil {
		if errors.Is(err, dg.ErrGoverned) {
			h.writeGoverned(w, r, projectID, applicationID, id)
			return
		}
		writeError(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     "Approved a delegation grant to agent " + strconv.FormatInt(v.Grant.Edge.CalleeEntityID, 10),
		EntityType: "delegation_grant", EntityID: audit.ID(id), ProjectID: audit.ID(projectID),
	})
	writeJSON(w, http.StatusOK, grant(v))
}

// writeGoverned answers 409 with the change request that approves the grant.
func (h *Handler) writeGoverned(w http.ResponseWriter, r *http.Request, projectID, applicationID, id int64) {
	body := map[string]any{
		"error": "this delegation is governed: approve it through its change request",
		"code":  GovernedCode,
	}
	if v, err := h.service.Get(r.Context(), projectID, applicationID, id); err == nil && v.ChangeRequestID > 0 {
		body["change_request_id"] = v.ChangeRequestID
	}
	writeJSON(w, http.StatusConflict, body)
}

// Revoke serves DELETE: the grant ends. The callee runs with its own grants
// again where the caller holds them, and is refused where it does not.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	id, err := grantID(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	user, err := actor(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	v, err := h.service.Revoke(r.Context(), projectID, applicationID, id, user)
	if err != nil {
		writeError(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     "Revoked a delegation grant to agent " + strconv.FormatInt(v.Grant.Edge.CalleeEntityID, 10),
		EntityType: "delegation_grant", EntityID: audit.ID(id), ProjectID: audit.ID(projectID),
	})
	writeJSON(w, http.StatusOK, grant(v))
}

// Preview serves GET: what an attach of the callee version would let it use,
// what the caller of this route could approve of it, and the closure.
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, err := ids(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	user, err := actor(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	query := r.URL.Query()
	callee, err1 := strconv.ParseInt(query.Get("callee_application_id"), 10, 64)
	version, err2 := strconv.ParseInt(query.Get("callee_version_id"), 10, 64)
	if err1 != nil || err2 != nil || callee <= 0 || version <= 0 {
		apierr.Write(w, apierr.BadRequest("callee_application_id and callee_version_id are required"))
		return
	}
	p, err := h.service.Preview(r.Context(), appgrants.PreviewRequest{
		ProjectID: projectID, CallerID: applicationID, CalleeID: callee, CalleeVersionID: version,
		EdgeKind: dg.EdgeKind(query.Get("edge_kind")), Actor: user,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := Preview{
		CalleeApplicationID: p.Callee.ApplicationID, CalleeVersionID: p.Callee.VersionID, CalleeName: p.Callee.Name,
		Digest: p.Callee.Set.Digest(), Items: items(p.Items), Approvable: items(p.Approvable), Withheld: items(p.Withheld),
		Closure: make([]ClosureNode, 0, len(p.Closure)),
	}
	for _, node := range p.Closure {
		out.Closure = append(out.Closure, ClosureNode{ApplicationID: node.ApplicationID, Name: node.Name, Depth: node.Depth, Items: items(node.Items)})
	}
	if p.Existing != nil {
		existing := grant(*p.Existing)
		out.Existing = &existing
	}
	writeJSON(w, http.StatusOK, out)
}

// decode reads an optional JSON body of at most 64 KiB with no unknown fields.
func decode(r *http.Request, target any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		return apierr.BadRequest("invalid request body")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.More() {
		return apierr.BadRequest("invalid request body")
	}
	return nil
}

// writeError maps a domain refusal to its status.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dg.ErrNotFound), errors.Is(err, dg.ErrCalleeNotFound):
		apierr.Write(w, apierr.NotFound("delegation grant or agent not found"))
	case errors.Is(err, dg.ErrActiveEdgeExists):
		apierr.Write(w, apierr.Conflict("the edge already has an active delegation grant"))
	case errors.Is(err, dg.ErrWrongState):
		apierr.Write(w, apierr.Conflict("the delegation grant is not in a state this applies to"))
	case errors.Is(err, dg.ErrGoverned):
		apierr.Write(w, apierr.Conflict("this delegation is governed: approve it through its change request"))
	case errors.Is(err, dg.ErrSelfApproval):
		apierr.Write(w, apierr.Forbidden("the approver must differ from the proposer"))
	case errors.Is(err, dg.ErrNothingToApprove):
		apierr.Write(w, apierr.BadRequest("nothing is left to approve: you hold access to none of the items, or none were chosen"))
	case errors.Is(err, dg.ErrInvalid), errors.Is(err, dg.ErrMalformedSet):
		apierr.Write(w, apierr.BadRequest(err.Error()))
	default:
		if refusal, ok := agentlifecycle.AsRefusal(err); ok {
			apierr.Write(w, apierr.Conflict(refusal.Error()))
			return
		}
		apierr.Write(w, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
