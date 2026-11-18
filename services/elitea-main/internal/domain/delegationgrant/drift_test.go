package delegationgrant

import "testing"

func gitOnly(tools ...string) GrantSet {
	return GrantSet{Toolkits: []ToolkitGrant{{Key: "toolkit:1", Tools: Tools{Mode: ToolsOnly, Names: tools}}}}
}

func grantOf(state State, requested, approved GrantSet) Grant {
	return Grant{State: state, RequestedSet: requested.Normalize(), GrantSet: approved.Normalize()}
}

func TestAssessContainmentNotVersionEquality(t *testing.T) {
	approved := grantOf(StateApproved, gitOnly("read", "write"), gitOnly("read", "write"))

	// The same set, and a smaller one, need nothing.
	if v := Assess(approved, gitOnly("read", "write"), 9, nil); v.Action != DriftNone {
		t.Fatalf("the same set: %+v", v)
	}
	if v := Assess(approved, gitOnly("read"), 9, nil); v.Action != DriftNone {
		t.Fatalf("a smaller set: %+v", v)
	}
	// A wider set marks the grant stale and records what changed.
	v := Assess(approved, gitOnly("read", "write", "delete"), 9, nil)
	if v.Action != DriftMarkStale || v.Drift.CalleeVersionID != 9 || len(v.Drift.Added) != 1 || v.Drift.Added[0].Name != "delete" {
		t.Fatalf("a wider set: %+v", v)
	}
	if v.Drift.Digest != gitOnly("read", "write", "delete").Digest() {
		t.Fatalf("the drift keys on the digest of the current set: %+v", v)
	}
	// A new toolkit is drift too.
	two := GrantSet{Toolkits: append(gitOnly("read", "write").Toolkits, ToolkitGrant{Key: "toolkit:2", Tools: Tools{Mode: ToolsAllExcept}})}
	if v := Assess(approved, two, 9, nil); v.Action != DriftMarkStale || v.Drift.Added[0] != (Item{Kind: ItemToolkit, Name: "toolkit:2"}) {
		t.Fatalf("a new toolkit: %+v", v)
	}
}

func TestAssessAPartialApprovalIsNotDrift(t *testing.T) {
	// The approver left "write" out: the callee still holding it has not drifted.
	partial := grantOf(StateApproved, gitOnly("read", "write"), gitOnly("read"))
	if v := Assess(partial, gitOnly("read", "write"), 9, nil); v.Action != DriftNone {
		t.Fatalf("an omitted item is not drift: %+v", v)
	}
}

func TestAssessStaleGrants(t *testing.T) {
	stale := grantOf(StateStale, gitOnly("read"), gitOnly("read"))
	// Back within the grant: restored.
	if v := Assess(stale, gitOnly("read"), 9, &Drift{Digest: "x"}); v.Action != DriftRestore {
		t.Fatalf("restored: %+v", v)
	}
	wider := gitOnly("read", "write")
	// The same drift again: nothing to do (and no second notification).
	if v := Assess(stale, wider, 9, &Drift{Digest: wider.Digest()}); v.Action != DriftNone || v.Drift.Digest == "" {
		t.Fatalf("the same drift: %+v", v)
	}
	// A different wider set: the record moves.
	if v := Assess(stale, gitOnly("read", "write", "delete"), 9, &Drift{Digest: wider.Digest()}); v.Action != DriftUpdate {
		t.Fatalf("a different drift: %+v", v)
	}
}

func TestAssessLeavesOtherStatesAlone(t *testing.T) {
	for _, state := range []State{StatePending, StateSuperseded, StateRevoked} {
		if v := Assess(grantOf(state, gitOnly("read"), gitOnly("read")), gitOnly("read", "write"), 9, nil); v.Action != DriftNone {
			t.Fatalf("%s: %+v", state, v)
		}
	}
}

func TestClosureGrowthIsNotifyOnly(t *testing.T) {
	before := Closure{3: "a"}
	if (Closure{3: "a"}).Grew(before) {
		t.Fatal("the same closure did not grow")
	}
	if (Closure{}).Grew(before) {
		t.Fatal("a smaller closure did not grow")
	}
	if !(Closure{3: "a", 4: "b"}).Grew(before) {
		t.Fatal("a new callee is growth")
	}
	if !(Closure{3: "changed"}).Grew(before) {
		t.Fatal("a re-approved grant into a callee changes the closure")
	}
	if (Closure{3: "a", 4: "b"}).Grew(nil) {
		t.Fatal("a first reading is a baseline, not growth")
	}
	if (Closure{3: "a", 4: "b"}).Key() != (Closure{4: "b", 3: "a"}).Key() {
		t.Fatal("the key is order independent")
	}
}
