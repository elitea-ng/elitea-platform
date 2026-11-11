package agentexecution

import (
	"context"
	"encoding/json"
	"testing"
)

// idleGovernedSubmit is governedSubmit as an idle compaction of the same
// agent version (issue 1776).
func idleGovernedSubmit(t *testing.T, toolkitIDs ...int64) SubmitRequest {
	t.Helper()
	request := governedSubmit(t, toolkitIDs...)
	turn := request.CurrentTurn
	request.CurrentTurn = nil
	request.CurrentIdleCompactionTurn = &CurrentIdleCompactionTurn{
		ProjectID: turn.ProjectID, ActorUserID: turn.ActorUserID, ConversationUUID: turn.ConversationUUID,
		ResponseMessageID: turn.ResponseMessageID, ParticipantID: turn.TargetParticipantID,
		ApplicationID: turn.ApplicationID, ApplicationVersionID: turn.ApplicationVersionID,
	}
	return request
}

// #1920: an idle compaction binds the agent identity but resolves no toolkit
// credential, so a toolkit whose reference would refuse a real run (title
// only, here) does not drop the summary.
func TestGovernedFreezeOfAnIdleCompactionResolvesNoToolkit(t *testing.T) {
	stored := fakeGovernedToolkits{21: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false})}
	credentials, resolver, _ := governedWorld(t, stored, 21)

	if _, _, err := credentials.Freeze(context.Background(), governedSubmit(t, 21)); err == nil {
		t.Fatal("the same toolkit did not refuse a real run")
	}
	resolver.governed = nil

	request := idleGovernedSubmit(t, 21)
	input, freeze, err := credentials.Freeze(context.Background(), request)
	if err != nil {
		t.Fatalf("idle compaction freeze: %v", err)
	}
	if input != request.Input {
		t.Fatal("the idle compaction's input was rewritten")
	}
	if freeze == nil || freeze.AgentBindingID != frozenBinding || freeze.ApplicationVersionID != 4 ||
		!freeze.DefinitionChecked || len(freeze.References) != 0 {
		t.Fatalf("freeze %+v", freeze)
	}
	if len(resolver.governed) != 0 {
		t.Fatal("a toolkit credential was resolved for an idle compaction")
	}
}

// #1920: an idle compaction of a version edited since its source ran binds
// no identity and writes no definition-mismatch audit event.
func TestGovernedFreezeOfAMismatchingIdleCompactionAuditsNothing(t *testing.T) {
	credentials, _, bindings, _, definitions := governedWorldWithDefinitions(t, fakeGovernedToolkits{}, 21)
	bindings.digest = [32]byte{0x99}
	_, freeze, err := credentials.Freeze(context.Background(), idleGovernedSubmit(t))
	if err != nil {
		t.Fatal(err)
	}
	if freeze == nil || freeze.AgentBindingID != "" || len(definitions.mismatches) != 0 {
		t.Fatalf("freeze %+v mismatches %v", freeze, definitions.mismatches)
	}
}

// #1919: start.go freezes application.version_id (start_test), and the
// governed freeze, the one path that re-encodes a frozen application,
// keeps it, so an idle compaction can read the version its source ran.
func TestGovernedFreezeKeepsTheFrozenVersionID(t *testing.T) {
	stored := fakeGovernedToolkits{21: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference})}
	credentials, _, _ := governedWorld(t, stored, 21)
	request := governedSubmit(t, 21)
	var application map[string]json.RawMessage
	if err := json.Unmarshal(request.Input.GetApplication(), &application); err != nil {
		t.Fatal(err)
	}
	application["version_id"] = json.RawMessage("4")
	encoded, err := json.Marshal(application)
	if err != nil {
		t.Fatal(err)
	}
	request.Input.Application = encoded

	input, _, err := credentials.Freeze(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if input == request.Input {
		t.Fatal("the reference was not written")
	}
	var frozen struct {
		ID        int64 `json:"id"`
		VersionID int64 `json:"version_id"`
	}
	if err := json.Unmarshal(input.GetApplication(), &frozen); err != nil || frozen.ID != 3 || frozen.VersionID != 4 {
		t.Fatalf("frozen application %s (%v)", input.GetApplication(), err)
	}
}
