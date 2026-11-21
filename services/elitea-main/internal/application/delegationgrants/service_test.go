package delegationgrants

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

type fakeStore struct {
	grants   map[int64]dg.Grant
	approved int
	governed bool
	// reapproved and requested count the re-approvals of stale grants, direct
	// and through a change request.
	reapproved, requested int
}

func (f *fakeStore) ProposeGrant(_ context.Context, projectID int64, edge dg.Edge, version int64, set dg.GrantSet, origin dg.Origin, governed bool, by int64) (dg.Grant, error) {
	g := dg.Grant{ID: int64(len(f.grants) + 1), ProjectID: projectID, Edge: edge, CalleeVersionID: version, RequestedSet: set, GrantSet: set,
		State: dg.StatePending, Governed: governed, Origin: origin, ProposedBy: &by}
	f.grants[g.ID] = g
	f.governed = governed
	return g, nil
}
func (f *fakeStore) Approve(_ context.Context, _, id, approver int64, choose []dg.Item, access dg.Access) (dg.Grant, error) {
	g := f.grants[id]
	approved, omitted, err := dg.Approvable(g.RequestedSet, choose, access)
	if err != nil {
		return dg.Grant{}, err
	}
	g.GrantSet, g.Omitted, g.State, g.ApprovedBy = approved, omitted, dg.StateApproved, &approver
	f.grants[id] = g
	f.approved++
	return g, nil
}
func (f *fakeStore) Revoke(_ context.Context, _, id, _ int64) (dg.Grant, error) {
	g, ok := f.grants[id]
	if !ok {
		return dg.Grant{}, dg.ErrNotFound
	}
	g.State = dg.StateRevoked
	f.grants[id] = g
	return g, nil
}
func (f *fakeStore) Get(_ context.Context, _, id int64) (dg.Grant, error) {
	g, ok := f.grants[id]
	if !ok {
		return dg.Grant{}, dg.ErrNotFound
	}
	return g, nil
}
func (f *fakeStore) ListForCaller(_ context.Context, _, caller int64, _ bool) ([]dg.Grant, error) {
	var out []dg.Grant
	for _, g := range f.grants {
		if g.Edge.CallerEntityID == caller {
			out = append(out, g)
		}
	}
	return out, nil
}
func (f *fakeStore) ActiveForEdge(context.Context, int64, dg.Edge) (dg.Grant, bool, error) {
	return dg.Grant{}, false, nil
}
func (f *fakeStore) ReapproveStale(_ context.Context, _, id, approver int64, _ []dg.Item, _ dg.Access) (dg.Grant, error) {
	g := f.grants[id]
	g.State, g.ApprovedBy, g.Drift = dg.StateApproved, &approver, nil
	f.grants[id] = g
	f.reapproved++
	return g, nil
}
func (f *fakeStore) RequestReapproval(context.Context, int64, int64, int64) (bool, error) {
	f.requested++
	return true, nil
}

type fakeCallees map[int64]dg.Callee

func (f fakeCallees) Snapshot(_ context.Context, _, app, _ int64) (dg.Callee, error) {
	c, ok := f[app]
	if !ok {
		return dg.Callee{}, dg.ErrCalleeNotFound
	}
	return c, nil
}
func (f fakeCallees) ApplicationName(_ context.Context, _, app int64) (string, error) {
	c, ok := f[app]
	if !ok {
		return "", dg.ErrCalleeNotFound
	}
	return c.Name, nil
}

type fakeChanges struct{}

func (fakeChanges) DelegationChangeForGrant(context.Context, int64, int64) (agentlifecycle.ChangeRequest, bool, error) {
	return agentlifecycle.ChangeRequest{ID: 77, State: agentlifecycle.ChangeSubmitted}, true, nil
}

type fakeAccess struct{ toolkits, credentials bool }

func (a fakeAccess) HoldsToolkit(string) bool            { return a.toolkits }
func (a fakeAccess) HoldsCredential(string, string) bool { return a.credentials }

type fakeResolver struct{ access dg.Access }

func (r fakeResolver) AccessFor(context.Context, int64, int64) (dg.Access, error) {
	return r.access, nil
}

func gitCallee() dg.Callee {
	return dg.Callee{ApplicationID: 1, VersionID: 5, Name: "Agent1", Set: dg.GrantSet{
		Toolkits: []dg.ToolkitGrant{{Key: "toolkit:11", Credential: "ref-git", Tools: dg.Tools{Mode: dg.ToolsOnly, Names: []string{"read"}}}},
	}}
}

func newService(t *testing.T, governed bool, access dg.Access) (*Service, *fakeStore) {
	t.Helper()
	store := &fakeStore{grants: map[int64]dg.Grant{}}
	service, err := New(store, fakeCallees{1: gitCallee(), 2: {ApplicationID: 2, Name: "Agent2"}}, fakeChanges{},
		fakeResolver{access}, func(context.Context) bool { return governed })
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestProposeApproveAndRevoke(t *testing.T) {
	service, store := newService(t, false, fakeAccess{toolkits: true, credentials: true})
	ctx := context.Background()
	v, err := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 7})
	if err != nil || v.Grant.State != dg.StatePending || v.Grant.Governed || v.ChangeRequestID != 0 || len(v.RequestedItems) != 3 {
		t.Fatalf("propose: %+v %v", v, err)
	}
	// Another agent's grant is not found.
	if _, err := service.Approve(ctx, 1, 99, v.Grant.ID, 7, nil); !errors.Is(err, dg.ErrNotFound) {
		t.Fatalf("wrong caller: %v", err)
	}
	approved, err := service.Approve(ctx, 1, 2, v.Grant.ID, 7, nil)
	if err != nil || approved.Grant.State != dg.StateApproved || len(approved.Items) != 3 {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	revoked, err := service.Revoke(ctx, 1, 2, v.Grant.ID, 7)
	if err != nil || revoked.Grant.State != dg.StateRevoked || store.approved != 1 {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
}

func TestProposeRefusals(t *testing.T) {
	service, _ := newService(t, false, fakeAccess{})
	ctx := context.Background()
	for name, req := range map[string]ProposeRequest{
		"an unknown edge kind": {ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: "skill", Actor: 7},
		"a missing caller":     {ProjectID: 1, CallerID: 9, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 7},
		"a missing callee":     {ProjectID: 1, CallerID: 2, CalleeID: 9, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 7},
	} {
		if _, err := service.Propose(ctx, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAnEmptyCalleeNeedsNoGrantAndANonPendingGrantIsAStateConflict(t *testing.T) {
	service, store := newService(t, true, fakeAccess{toolkits: true, credentials: true})
	ctx := context.Background()
	if _, err := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 1, CalleeID: 2, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 7}); !errors.Is(err, dg.ErrInvalid) {
		t.Fatalf("a callee that holds nothing: %v", err)
	}
	v, _ := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeAgentTool, Actor: 7})
	g := store.grants[v.Grant.ID]
	g.State = dg.StateRevoked
	store.grants[v.Grant.ID] = g
	// A revoked governed grant is a state conflict, not a pointer to its change request.
	if _, err := service.Approve(ctx, 1, 2, v.Grant.ID, 8, nil); !errors.Is(err, dg.ErrWrongState) {
		t.Fatalf("approve on a revoked governed grant: %v", err)
	}
}

func TestGovernedGrantIsApprovedThroughItsChangeRequest(t *testing.T) {
	service, store := newService(t, true, fakeAccess{toolkits: true, credentials: true})
	ctx := context.Background()
	v, err := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeAgentTool, Actor: 7})
	if err != nil || !v.Grant.Governed || v.ChangeRequestID != 77 {
		t.Fatalf("governed propose: %+v %v", v, err)
	}
	if _, err := service.Approve(ctx, 1, 2, v.Grant.ID, 8, nil); !errors.Is(err, dg.ErrGoverned) || store.approved != 0 {
		t.Fatalf("the direct route must refuse a governed grant: %v", err)
	}
}

func staleGrant(store *fakeStore, id int64) {
	g := store.grants[id]
	g.State = dg.StateStale
	g.Drift = &dg.Drift{CalleeVersionID: 5, Digest: "d"}
	store.grants[id] = g
}

func TestApprovingAStaleGrantReapprovesIt(t *testing.T) {
	service, store := newService(t, false, fakeAccess{toolkits: true, credentials: true})
	ctx := context.Background()
	v, _ := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeAgentTool, Actor: 7})
	staleGrant(store, v.Grant.ID)
	got, err := service.Approve(ctx, 1, 2, v.Grant.ID, 7, nil)
	if err != nil || got.Grant.State != dg.StateApproved || store.reapproved != 1 || store.approved != 0 {
		t.Fatalf("re-approve: %+v %v (reapproved %d)", got, err, store.reapproved)
	}
}

func TestAStaleGovernedGrantIsReapprovedThroughAChangeRequest(t *testing.T) {
	service, store := newService(t, true, fakeAccess{toolkits: true, credentials: true})
	ctx := context.Background()
	v, _ := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeAgentTool, Actor: 7})
	staleGrant(store, v.Grant.ID)
	if _, err := service.Approve(ctx, 1, 2, v.Grant.ID, 8, nil); !errors.Is(err, dg.ErrGoverned) || store.requested != 1 || store.reapproved != 0 {
		t.Fatalf("a governed re-approval goes through a change request: %v (requested %d)", err, store.requested)
	}
}

func TestListRunsTheDriftCheckFirst(t *testing.T) {
	service, _ := newService(t, false, fakeAccess{toolkits: true})
	var checked []int64
	service.WithDriftCheck(func(_ context.Context, _, caller int64) { checked = append(checked, caller) })
	if _, err := service.List(context.Background(), 1, 2, false); err != nil || len(checked) != 1 || checked[0] != 2 {
		t.Fatalf("list must check the caller's edges first: %v %v", checked, err)
	}
}

func TestApprovalLeavesOutWhatTheApproverDoesNotHold(t *testing.T) {
	service, _ := newService(t, false, fakeAccess{toolkits: true}) // no credential
	ctx := context.Background()
	v, _ := service.Propose(ctx, ProposeRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 7})
	// Without the credential the toolkit leaves too: nothing is left to approve.
	if _, err := service.Approve(ctx, 1, 2, v.Grant.ID, 8, nil); !errors.Is(err, dg.ErrNothingToApprove) {
		t.Fatalf("approver without the credential: %v", err)
	}
}

func TestPreviewSplitsByAccessAndWalksTheClosure(t *testing.T) {
	service, store := newService(t, false, fakeAccess{toolkits: true})
	// Agent1 already has an approved grant to Agent2: the closure of Agent1 reaches it.
	store.grants[5] = dg.Grant{ID: 5, ProjectID: 1, Edge: dg.Edge{CallerEntityID: 1, CalleeEntityID: 2, Kind: dg.EdgeAgentTool},
		State: dg.StateApproved, GrantSet: dg.GrantSet{EgressHosts: []string{"x.test"}}}
	p, err := service.Preview(context.Background(), PreviewRequest{ProjectID: 1, CallerID: 2, CalleeID: 1, CalleeVersionID: 5, EdgeKind: dg.EdgeSubAgent, Actor: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 3 || len(p.Withheld) != 3 || len(p.Approvable) != 0 { // no credential: the toolkit cannot run without it, so all three stay out
		t.Fatalf("preview: items %v approvable %v withheld %v", p.Items, p.Approvable, p.Withheld)
	}
	if len(p.Closure) != 2 || p.Closure[1].ApplicationID != 2 || p.Closure[1].Depth != 1 {
		t.Fatalf("closure: %+v", p.Closure)
	}
}
