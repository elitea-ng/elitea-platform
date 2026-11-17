package delegationdrift

import (
	"context"
	"testing"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

func tools(names ...string) dg.GrantSet {
	return dg.GrantSet{Toolkits: []dg.ToolkitGrant{{Key: "toolkit:1", Tools: dg.Tools{Mode: dg.ToolsOnly, Names: names}}}}
}

type fakeStore struct {
	tracked []dg.Tracked
	applied []dg.Verdict
	closure []dg.Closure
	below   map[int64][]dg.Grant
}

func (f *fakeStore) ListTracked(context.Context, int64, []int64, int64) ([]dg.Tracked, error) {
	return f.tracked, nil
}

func (f *fakeStore) ApplyDrift(_ context.Context, _ int64, id int64, _ dg.State, v dg.Verdict, closure dg.Closure) (dg.DriftApplied, error) {
	f.applied = append(f.applied, v)
	f.closure = append(f.closure, closure)
	return dg.DriftApplied{Grant: dg.Grant{ID: id}, Changed: true, NotifiedStale: 2}, nil
}

func (f *fakeStore) ConsumableFromCallers(_ context.Context, _ int64, callers []int64) ([]dg.Grant, error) {
	var out []dg.Grant
	for _, caller := range callers {
		out = append(out, f.below[caller]...)
	}
	return out, nil
}

type fakeCallees struct {
	sets     map[int64]dg.GrantSet
	attached []int64
}

func (f fakeCallees) Snapshot(_ context.Context, _, app, version int64) (dg.Callee, error) {
	set, ok := f.sets[version]
	if !ok {
		return dg.Callee{}, dg.ErrCalleeNotFound
	}
	return dg.Callee{ApplicationID: app, VersionID: version, Set: set}, nil
}

func (f fakeCallees) AttachedVersions(context.Context, int64, int64, int64) ([]int64, error) {
	return f.attached, nil
}

func approved(requested dg.GrantSet) dg.Tracked {
	return dg.Tracked{
		Grant: dg.Grant{
			ID: 1, State: dg.StateApproved, CalleeVersionID: 5, RequestedSet: requested.Normalize(), GrantSet: requested.Normalize(),
			Edge: dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeAgentTool},
		},
		Closure: dg.Closure{},
	}
}

func run(t *testing.T, store *fakeStore, callees fakeCallees) Result {
	t.Helper()
	detector, err := New(store, callees)
	if err != nil {
		t.Fatal(err)
	}
	result, err := detector.CheckProject(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAWiderCalleeVersionMarksTheGrantStale(t *testing.T) {
	store := &fakeStore{tracked: []dg.Tracked{approved(tools("read"))}}
	result := run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{7: tools("read", "write")}, attached: []int64{7}})
	if result.Stale != 1 || result.Notified != 2 || len(store.applied) != 1 || store.applied[0].Action != dg.DriftMarkStale {
		t.Fatalf("result %+v applied %+v", result, store.applied)
	}
	if store.applied[0].Drift.CalleeVersionID != 7 {
		t.Fatalf("the drift names the version it was read from: %+v", store.applied[0])
	}
}

func TestTheSameOrASmallerSetNeedsNothing(t *testing.T) {
	for name, current := range map[string]dg.GrantSet{"same": tools("read"), "smaller": tools()} {
		store := &fakeStore{tracked: []dg.Tracked{approved(tools("read"))}}
		result := run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{7: current}, attached: []int64{7}})
		if result.Stale != 0 || len(store.applied) != 0 {
			t.Fatalf("%s: result %+v applied %+v", name, result, store.applied)
		}
	}
}

func TestTheWidestAttachedVersionDecides(t *testing.T) {
	store := &fakeStore{tracked: []dg.Tracked{approved(tools("read"))}}
	run(t, store, fakeCallees{
		sets:     map[int64]dg.GrantSet{7: tools("read"), 8: tools("read", "delete")},
		attached: []int64{7, 8},
	})
	if len(store.applied) != 1 || store.applied[0].Action != dg.DriftMarkStale || store.applied[0].Drift.CalleeVersionID != 8 {
		t.Fatalf("applied %+v", store.applied)
	}
}

func TestAnEdgeWithNoToolRowReadsTheGrantsVersion(t *testing.T) {
	store := &fakeStore{tracked: []dg.Tracked{approved(tools("read"))}}
	run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{5: tools("read", "write")}})
	if len(store.applied) != 1 || store.applied[0].Drift.CalleeVersionID != 5 {
		t.Fatalf("applied %+v", store.applied)
	}
}

func TestAStaleGrantComesBackWhenTheCalleeIsWithinIt(t *testing.T) {
	tracked := approved(tools("read"))
	tracked.Grant.State = dg.StateStale
	tracked.Drift = &dg.Drift{Digest: "x"}
	store := &fakeStore{tracked: []dg.Tracked{tracked}}
	result := run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{7: tools("read")}, attached: []int64{7}})
	if result.Restored != 1 || store.applied[0].Action != dg.DriftRestore {
		t.Fatalf("result %+v applied %+v", result, store.applied)
	}
}

func TestClosureGrowthIsRecordedWithoutAStateChange(t *testing.T) {
	tracked := approved(tools("read"))
	tracked.Closure = dg.Closure{3: "a"}
	store := &fakeStore{tracked: []dg.Tracked{tracked}, below: map[int64][]dg.Grant{
		1: {{Edge: dg.Edge{CallerEntityID: 1, CalleeEntityID: 3}, Digest: "a"}, {Edge: dg.Edge{CallerEntityID: 1, CalleeEntityID: 4}, Digest: "b"}},
	}}
	result := run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{7: tools("read")}, attached: []int64{7}})
	if result.Stale != 0 || len(store.applied) != 1 || store.applied[0].Action != dg.DriftNone || len(store.closure[0]) != 2 {
		t.Fatalf("result %+v applied %+v closure %+v", result, store.applied, store.closure)
	}
}

func TestAnUnchangedEdgeWritesNothing(t *testing.T) {
	tracked := approved(tools("read"))
	store := &fakeStore{tracked: []dg.Tracked{tracked}}
	run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{7: tools("read")}, attached: []int64{7}})
	if len(store.applied) != 0 {
		t.Fatalf("a check that finds nothing must not write: %+v", store.applied)
	}
}

func TestADeletedCalleeVersionIsSkipped(t *testing.T) {
	store := &fakeStore{tracked: []dg.Tracked{approved(tools("read"))}}
	result := run(t, store, fakeCallees{sets: map[int64]dg.GrantSet{}, attached: []int64{7}})
	if result.Stale != 0 || len(store.applied) != 0 {
		t.Fatalf("result %+v applied %+v", result, store.applied)
	}
}
