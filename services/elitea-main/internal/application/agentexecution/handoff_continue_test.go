package agentexecution

import (
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// ADR-0045 decision 8: a send's continue turn continues a DESKTOP chain as
// a `handoff` link; the grant's sensitive tools answer to this cloud turn's
// toolkits, and the chain's later cloud continuations start as this turn.
func TestHandoffContinuationGrant(t *testing.T) {
	chain := unattended.Chain{
		RootExecutionID: "0123456789abcdef0123456789abcdef", ExecutionID: "fedcba9876543210fedcba9876543210",
		ContinuationIndex: 4, ProjectID: 1, OwnerUserID: 7, ConversationUUID: "3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d",
		Origin: unattended.OriginDesktop, GrantID: "5e7a9c1d-3f4b-4c8d-8e2f-4a6b8c0d2e3f",
		Grant: unattended.Grant{GrantID: "5e7a9c1d-3f4b-4c8d-8e2f-4a6b8c0d2e3f", ContinueStepLimit: true,
			ContinueToolCallCap: true, HookAsks: true},
		Limits:     unattended.Limits{MaxContinuations: 10},
		DeadlineAt: time.Unix(1_900_000_000, 0),
	}
	target := grantTestTarget(nil)
	target.continuation = &UnattendedContinuation{Chain: chain, Kind: unattended.LinkHandoff}
	grant, admission, err := grantTestService("low").admitUnattended(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if grant.GetContinuationIndex() != 5 || grant.GetChainRootExecutionId() != chain.RootExecutionID {
		t.Fatalf("handoff grant = %+v", grant)
	}
	if admission.Continue == nil || admission.Continue.Kind != unattended.LinkHandoff ||
		admission.Continue.Start == nil || admission.Continue.Start.ParticipantID != target.start.ParticipantID ||
		admission.Continue.Start.Contract != target.start.Contract ||
		len(admission.MarkMessageIDs) != 2 {
		t.Fatalf("handoff admission = %+v", admission)
	}
	clone := admission.Clone()
	if clone.Continue.Start == admission.Continue.Start {
		t.Fatal("Clone shares the cloud start")
	}

	// Another kind still continues only a cloud chain.
	target.continuation = &UnattendedContinuation{Chain: chain, Kind: unattended.LinkStepLimit}
	var refusal *unattended.RefusalError
	if _, _, err := grantTestService("low").admitUnattended(t.Context(), target); !errors.As(err, &refusal) ||
		refusal.Reason != unattended.RefusedNotOwner {
		t.Fatalf("a step-limit link of a desktop chain = %v", err)
	}

	// The grant's sensitive tools answer to the cloud turn's toolkits.
	for name, test := range map[string]struct {
		tier   string
		tools  []unattended.Tool
		reason string
	}{
		"a selected tool":    {"low", []unattended.Tool{{ToolkitName: "jira", ToolName: "create_issue"}}, ""},
		"an unknown toolkit": {"low", []unattended.Tool{{ToolkitName: "github", ToolName: "push"}}, unattended.RefusedToolNotResolved},
		"a withheld payment": {"low", []unattended.Tool{{ToolkitName: "bank", ToolName: "pay"}}, unattended.RefusedCategoryWithheld},
		"a tier hitl rule":   {"high", []unattended.Tool{{ToolkitName: "mail", ToolName: "send"}}, unattended.RefusedTierRequiresHITL},
	} {
		withTools := chain
		withTools.Grant.SensitiveTools = test.tools
		target.continuation = &UnattendedContinuation{Chain: withTools, Kind: unattended.LinkHandoff}
		_, _, err := grantTestService(test.tier).admitUnattended(t.Context(), target)
		if test.reason == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.As(err, &refusal) || refusal.Reason != test.reason {
			t.Errorf("%s = %v, want %s", name, err, test.reason)
		}
	}

	// Grants off refuse the handoff link: the send stays HANDING_OFF.
	off := grantTestService("low")
	disabled := platformconfig.DefaultUnattendedSettings()
	disabled.Enabled = false
	off.unattended.Settings = grantSettingsStub{disabled}
	target.continuation = &UnattendedContinuation{Chain: chain, Kind: unattended.LinkHandoff}
	if _, _, err := off.admitUnattended(t.Context(), target); !errors.As(err, &refusal) ||
		refusal.Reason != unattended.RefusedDisabled {
		t.Fatalf("a handoff link with grants off = %v", err)
	}
}

func TestHandoffCompletionShape(t *testing.T) {
	valid := HandoffCompletion{HandoffID: "0b7e4c1a-2d3f-4a5b-9c6d-7e8f9a0b1c2d", Epoch: 3,
		HandoffRef: "refs/elitea/handoff/1", SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}
	if !valid.Valid() {
		t.Fatal("a well-formed completion is invalid")
	}
	for name, change := range map[string]func(*HandoffCompletion){
		"no epoch":    func(c *HandoffCompletion) { c.Epoch = 0 },
		"another ref": func(c *HandoffCompletion) { c.HandoffRef = "refs/elitea/handoff/2" },
		"short sha":   func(c *HandoffCompletion) { c.SHA = "4b825dc" },
		"not a uuid":  func(c *HandoffCompletion) { c.HandoffID = "send-1" },
	} {
		broken := valid
		change(&broken)
		if broken.Valid() {
			t.Errorf("%s: valid", name)
		}
	}
	first := HandoffContinueQuestionID(valid.HandoffID, 3)
	if first != HandoffContinueQuestionID(valid.HandoffID, 3) || first == HandoffContinueQuestionID(valid.HandoffID, 4) ||
		!validUUID(first) || !validUUID(HandoffContinueResponseID(first)) || HandoffContinueResponseID(first) == first {
		t.Fatalf("question %s is not stable per send and epoch", first)
	}
}
