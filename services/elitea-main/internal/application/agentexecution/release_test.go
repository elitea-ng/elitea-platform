package agentexecution

import (
	"context"
	"testing"
)

// A release run (ADR-0040 decision 2) binds the binding of the release's
// own version row and compares it with the release's digest: the
// participant's version (5 here, after a rollback to the release of
// version 4) is not read, and no live definition digest is computed.
func TestGovernedFreezeOfAReleaseRunBindsTheReleaseVersion(t *testing.T) {
	stored := fakeGovernedToolkits{21: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference})}
	credentials, _, _, _, definitions := governedWorldWithDefinitions(t, stored, 21)
	request := governedSubmit(t, 21)
	request.CurrentTurn.ApplicationVersionID = 5
	request.Release = &ReleaseRun{ID: 41, ApplicationVersionID: 4, DefinitionDigest: frozenDefinitionDigest}
	_, freeze, err := credentials.Freeze(context.Background(), request)
	if err != nil || freeze.AgentBindingID != frozenBinding || freeze.ApplicationVersionID != 4 ||
		!freeze.DefinitionChecked || freeze.DefinitionDigest != frozenDefinitionDigest || definitions.calls != 0 {
		t.Fatalf("release run: freeze %+v calls %d err=%v", freeze, definitions.calls, err)
	}
	// A release whose digest is not the one the binding pins binds no
	// agent identity, and the mismatch is audited.
	request.Release.DefinitionDigest = [32]byte{0x01}
	_, freeze, err = credentials.Freeze(context.Background(), request)
	if err != nil || freeze.AgentBindingID != "" || len(definitions.mismatches) != 1 {
		t.Fatalf("drifted release: freeze %+v err=%v", freeze, err)
	}
}

func TestReleaseRunHelpers(t *testing.T) {
	var none *ReleaseRun
	if releaseID(none) != 0 || none.Clone() != nil || bindingVersionID(none, 9) != 9 {
		t.Fatal("no release")
	}
	run := &ReleaseRun{ID: 3, ApplicationVersionID: 7}
	clone := run.Clone()
	clone.ID = 4
	if releaseID(run) != 3 || bindingVersionID(run, 9) != 7 || bindingVersionID(&ReleaseRun{ID: 3}, 9) != 9 {
		t.Fatal("release helpers")
	}
}
