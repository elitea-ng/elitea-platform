// Package delegationgrants is the use case behind the delegation grant API
// (ADR-0044 amendment 1, A1.2 rule 2): propose the grant of a composition
// edge, approve it (all of it, or the items the approver chooses and holds
// access to), revoke it, and preview what an attach would let the callee use.
//
// Outside the governed profile an editor of the caller approves directly.
// Under it the grant is approved through its ADR-0040 change request of kind
// delegation (maker-checker, at every tier), and the direct route refuses with
// the request to use.
package delegationgrants

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// MaxClosureDepth bounds the closure a preview walks (the platform's default
// delegation depth cap).
const MaxClosureDepth = 4

// Store is the grant storage.
type Store interface {
	ProposeGrant(ctx context.Context, projectID int64, edge dg.Edge, calleeVersionID int64, requested dg.GrantSet, origin dg.Origin, governed bool, proposedBy int64) (dg.Grant, error)
	Approve(ctx context.Context, projectID, id, approver int64, choose []dg.Item, access dg.Access) (dg.Grant, error)
	Revoke(ctx context.Context, projectID, id, actor int64) (dg.Grant, error)
	Get(ctx context.Context, projectID, id int64) (dg.Grant, error)
	ListForCaller(ctx context.Context, projectID, callerEntityID int64, includeHistory bool) ([]dg.Grant, error)
	ActiveForEdge(ctx context.Context, projectID int64, edge dg.Edge) (dg.Grant, bool, error)
}

// Callees reads what a version holds now.
type Callees interface {
	Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error)
	// ApplicationName is the name of an agent or pipeline of the project, or
	// dg.ErrCalleeNotFound.
	ApplicationName(ctx context.Context, projectID, applicationID int64) (string, error)
}

// Changes reads the change request of a governed grant.
type Changes interface {
	DelegationChangeForGrant(ctx context.Context, projectID, grantID int64) (agentlifecycle.ChangeRequest, bool, error)
}

// Access resolves what a person holds access to in a project.
type Access interface {
	AccessFor(ctx context.Context, projectID, userID int64) (dg.Access, error)
}

// Service is the delegation grant use case.
type Service struct {
	grants   Store
	callees  Callees
	changes  Changes
	access   Access
	governed func(context.Context) bool
}

// New builds the service. governed reports whether the governed profile is on.
func New(grants Store, callees Callees, changes Changes, access Access, governed func(context.Context) bool) (*Service, error) {
	if grants == nil || callees == nil || changes == nil || access == nil || governed == nil {
		return nil, errors.New("delegation grants: every dependency is required")
	}
	return &Service{grants: grants, callees: callees, changes: changes, access: access, governed: governed}, nil
}

// View is a grant with what a client needs to show it.
type View struct {
	Grant dg.Grant
	// Items lists what the grant covers, and RequestedItems what the callee
	// held when it was proposed (the part left out is Grant.Omitted).
	Items          []dg.Item
	RequestedItems []dg.Item
	// ChangeRequestID is the change request that approves a governed grant, 0
	// when the grant is not governed.
	ChangeRequestID int64
	ChangeState     agentlifecycle.ChangeState
}

func (s *Service) view(ctx context.Context, grant dg.Grant) (View, error) {
	v := View{Grant: grant, Items: grant.GrantSet.Items(), RequestedItems: grant.RequestedSet.Items()}
	if grant.Governed {
		change, found, err := s.changes.DelegationChangeForGrant(ctx, grant.ProjectID, grant.ID)
		if err != nil {
			return View{}, err
		}
		if found {
			v.ChangeRequestID, v.ChangeState = change.ID, change.State
		}
	}
	return v, nil
}

// ProposeRequest asks for the grant of one edge.
type ProposeRequest struct {
	ProjectID       int64
	CallerID        int64
	CalleeID        int64
	CalleeVersionID int64
	EdgeKind        dg.EdgeKind
	Origin          dg.Origin
	Actor           int64
}

// Propose records the pending grant of an edge from what the callee version
// holds now. Under the governed profile it also records the change request.
func (s *Service) Propose(ctx context.Context, req ProposeRequest) (View, error) {
	if !req.EdgeKind.Valid() {
		return View{}, fmt.Errorf("%w: unknown edge kind %q", dg.ErrInvalid, req.EdgeKind)
	}
	if req.Origin == "" {
		req.Origin = dg.OriginAttach
	}
	if _, err := s.callees.ApplicationName(ctx, req.ProjectID, req.CallerID); err != nil {
		return View{}, err
	}
	callee, err := s.callees.Snapshot(ctx, req.ProjectID, req.CalleeID, req.CalleeVersionID)
	if err != nil {
		return View{}, err
	}
	// A callee that holds nothing needs no grant: it could never be approved
	// (nothing to list) and would hold the edge's slot against a real attach.
	if len(callee.Set.Toolkits) == 0 && len(callee.Set.EgressHosts) == 0 && len(callee.Set.DataClasses) == 0 {
		return View{}, fmt.Errorf("%w: the callee holds no toolkit, host or data class, so it needs no grant", dg.ErrInvalid)
	}
	grant, err := s.grants.ProposeGrant(ctx, req.ProjectID,
		dg.Edge{CallerEntityID: req.CallerID, CalleeEntityID: req.CalleeID, Kind: req.EdgeKind},
		req.CalleeVersionID, callee.Set, req.Origin, s.governed(ctx), req.Actor)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, grant)
}

// Get reads one grant of the caller.
func (s *Service) Get(ctx context.Context, projectID, callerID, grantID int64) (View, error) {
	grant, err := s.grants.Get(ctx, projectID, grantID)
	if err != nil {
		return View{}, err
	}
	if grant.Edge.CallerEntityID != callerID {
		return View{}, dg.ErrNotFound
	}
	return s.view(ctx, grant)
}

// List lists the grants of the caller's edges.
func (s *Service) List(ctx context.Context, projectID, callerID int64, history bool) ([]View, error) {
	grants, err := s.grants.ListForCaller(ctx, projectID, callerID, history)
	if err != nil {
		return nil, err
	}
	views := make([]View, 0, len(grants))
	for _, grant := range grants {
		v, err := s.view(ctx, grant)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, nil
}

// Approve approves a pending grant of the caller. choose lists the items the
// approver keeps (nil keeps all); the approver's own access limits it. A
// governed grant is refused with dg.ErrGoverned: its change request approves.
func (s *Service) Approve(ctx context.Context, projectID, callerID, grantID, approver int64, choose []dg.Item) (View, error) {
	grant, err := s.grants.Get(ctx, projectID, grantID)
	if err != nil {
		return View{}, err
	}
	if grant.Edge.CallerEntityID != callerID {
		return View{}, dg.ErrNotFound
	}
	// A grant that is not pending is a state conflict whatever its profile: the
	// client must not be sent to a change request that is already decided.
	if grant.State != dg.StatePending {
		return View{}, dg.ErrWrongState
	}
	if grant.Governed {
		return View{}, dg.ErrGoverned
	}
	access, err := s.access.AccessFor(ctx, projectID, approver)
	if err != nil {
		return View{}, err
	}
	approved, err := s.grants.Approve(ctx, projectID, grantID, approver, choose, access)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, approved)
}

// Revoke ends an active grant of the caller. A governed grant is revoked by
// rejecting or withdrawing its change request while that is open, and here
// once it is decided.
func (s *Service) Revoke(ctx context.Context, projectID, callerID, grantID, actor int64) (View, error) {
	grant, err := s.grants.Get(ctx, projectID, grantID)
	if err != nil {
		return View{}, err
	}
	if grant.Edge.CallerEntityID != callerID {
		return View{}, dg.ErrNotFound
	}
	revoked, err := s.grants.Revoke(ctx, projectID, grantID, actor)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, revoked)
}

// Preview is what an attach of the callee version would let the callee use,
// and what the person could approve of it.
type Preview struct {
	Callee dg.Callee
	// Items is everything the callee version holds.
	Items []dg.Item
	// Approvable and Withheld split Items by what the person holds access to:
	// they could approve the first, and the second would stay out of the grant.
	Approvable []dg.Item
	Withheld   []dg.Item
	// Closure is the callee and the callees it reaches through APPROVED grants,
	// with what each holds, as information: each hop still needs its own grant.
	Closure []ClosureNode
	// Existing is the edge's active grant, when it has one.
	Existing *View
	// Governed is the governed profile: an attach records a change request that
	// a different approver decides, so the person asking cannot approve it.
	Governed bool
}

// ClosureNode is one agent in the closure of an edge.
type ClosureNode struct {
	ApplicationID int64
	Name          string
	Depth         int
	// Items is what the approved grant into this node covers (the callee's own
	// items for the first node).
	Items []dg.Item
}

// PreviewRequest asks for the preview of one edge.
type PreviewRequest struct {
	ProjectID       int64
	CallerID        int64
	CalleeID        int64
	CalleeVersionID int64
	EdgeKind        dg.EdgeKind
	Actor           int64
}

// Preview answers "what will X be able to use" for the attach dialog.
func (s *Service) Preview(ctx context.Context, req PreviewRequest) (Preview, error) {
	if !req.EdgeKind.Valid() {
		return Preview{}, fmt.Errorf("%w: unknown edge kind %q", dg.ErrInvalid, req.EdgeKind)
	}
	callee, err := s.callees.Snapshot(ctx, req.ProjectID, req.CalleeID, req.CalleeVersionID)
	if err != nil {
		return Preview{}, err
	}
	access, err := s.access.AccessFor(ctx, req.ProjectID, req.Actor)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Callee: callee, Items: callee.Set.Items(), Approvable: []dg.Item{}, Withheld: []dg.Item{}, Governed: s.governed(ctx)}
	if approvable, _, err := dg.Approvable(callee.Set, nil, access); err == nil {
		held := make(map[dg.Item]bool)
		for _, item := range approvable.Items() {
			held[item] = true
		}
		for _, item := range out.Items {
			if held[item] {
				out.Approvable = append(out.Approvable, item)
			} else {
				out.Withheld = append(out.Withheld, item)
			}
		}
	} else if errors.Is(err, dg.ErrNothingToApprove) {
		out.Withheld = out.Items
	} else {
		return Preview{}, err
	}
	if existing, found, err := s.grants.ActiveForEdge(ctx, req.ProjectID,
		dg.Edge{CallerEntityID: req.CallerID, CalleeEntityID: req.CalleeID, Kind: req.EdgeKind}); err != nil {
		return Preview{}, err
	} else if found {
		v, err := s.view(ctx, existing)
		if err != nil {
			return Preview{}, err
		}
		out.Existing = &v
	}
	closure, err := s.closure(ctx, req.ProjectID, callee)
	if err != nil {
		return Preview{}, err
	}
	out.Closure = closure
	return out, nil
}

// closure walks the approved grants below the callee, breadth first, to
// MaxClosureDepth, once per agent.
func (s *Service) closure(ctx context.Context, projectID int64, callee dg.Callee) ([]ClosureNode, error) {
	nodes := []ClosureNode{{ApplicationID: callee.ApplicationID, Name: callee.Name, Depth: 0, Items: callee.Set.Items()}}
	seen := map[int64]bool{callee.ApplicationID: true}
	frontier := []int64{callee.ApplicationID}
	for depth := 1; depth <= MaxClosureDepth && len(frontier) > 0; depth++ {
		var next []int64
		for _, caller := range frontier {
			grants, err := s.grants.ListForCaller(ctx, projectID, caller, false)
			if err != nil {
				return nil, err
			}
			for _, grant := range grants {
				if !grant.State.Consumable() || seen[grant.Edge.CalleeEntityID] {
					continue
				}
				seen[grant.Edge.CalleeEntityID] = true
				next = append(next, grant.Edge.CalleeEntityID)
				name, err := s.callees.ApplicationName(ctx, projectID, grant.Edge.CalleeEntityID)
				if err != nil && !errors.Is(err, dg.ErrCalleeNotFound) {
					return nil, err
				}
				nodes = append(nodes, ClosureNode{
					ApplicationID: grant.Edge.CalleeEntityID, Name: name, Depth: depth, Items: grant.GrantSet.Items(),
				})
			}
		}
		frontier = next
	}
	return nodes, nil
}
