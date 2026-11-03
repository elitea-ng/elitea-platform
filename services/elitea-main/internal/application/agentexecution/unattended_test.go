package agentexecution

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type grantSettingsStub struct {
	settings platformconfig.UnattendedSettings
}

func (s grantSettingsStub) Get(context.Context) (platformconfig.UnattendedSettings, error) {
	return s.settings, nil
}

type grantApprovalStub struct {
	settings platformconfig.ApprovalPolicySettings
}

func (s grantApprovalStub) Get(context.Context) (platformconfig.ApprovalPolicySettings, error) {
	return s.settings, nil
}

type grantCapsStub struct{ caps platformconfig.RunCaps }

func (s grantCapsStub) Get(context.Context) (platformconfig.RunCaps, error) { return s.caps, nil }

type grantTierStub struct{ tier string }

func (s grantTierStub) AgentRiskTier(context.Context, int64, int64) (string, error) {
	return s.tier, nil
}

const grantTestTools = `[
 {"type": "jira", "toolkit_name": "jira", "settings": {"selected_tools": ["create_issue", "read_issue"],
  "pinned_tools": [{"name": "create_issue", "category": "write"}]}},
 {"type": "mcp", "toolkit_name": "bank", "settings": {"pinned_tools": [{"name": "pay", "category": "payment"}]}},
 {"type": "mcp", "toolkit_name": "mail", "settings": {"pinned_tools": [{"name": "send", "category": "external_send"}]}},
 {"type": "application", "toolkit_name": "child", "settings": {}}
]`

func grantTestService(tier string) *CurrentApplicationStartService {
	return &CurrentApplicationStartService{unattended: &UnattendedGrantPolicy{
		Settings: grantSettingsStub{platformconfig.DefaultUnattendedSettings()},
		Approval: grantApprovalStub{platformconfig.ApprovalPolicySettings{Policy: approvalpolicy.Policy{
			RequireHITL: []agentlifecycle.Tier{agentlifecycle.TierHigh},
		}}},
		RunCaps: grantCapsStub{platformconfig.RunCaps{Tiers: map[agentlifecycle.Tier]platformconfig.TierRunCaps{
			agentlifecycle.TierLow: {MaxSpend: 2}, agentlifecycle.TierHigh: {MaxSpend: 2},
		}}},
		Tiers: grantTierStub{tier},
	}}
}

func grantTestTarget(request *unattended.Request) unattendedTarget {
	return unattendedTarget{
		request: request, capabilityID: executiondomain.AgentAdhocCapability,
		input:     &runtimev1.AgentExecutionInputV1{Tools: []byte(grantTestTools)},
		projectID: 1, actorUserID: 7, applicationID: 3,
		conversationUUID: "3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d", grantKey: "question-1",
		responseMessageID: "4d6f8b0c-2e3a-4b7c-9d1e-3f5a7b9c1d2e", questionID: "question-1",
		start: &unattended.CloudStart{Contract: CloudStartContractAdhoc, ParticipantID: 5},
	}
}

func TestAdmitUnattendedRefusals(t *testing.T) {
	tool := func(toolkit, name string) []unattended.Tool {
		return []unattended.Tool{{ToolkitName: toolkit, ToolName: name}}
	}
	cases := []struct {
		name    string
		service *CurrentApplicationStartService
		request unattended.Request
		origin  executiondomain.TriggerOrigin
		reason  string
	}{
		{"no policy", &CurrentApplicationStartService{}, unattended.Request{}, "", unattended.RefusedDisabled},
		{"desktop part", grantTestService("low"), unattended.Request{HookAsks: true}, "", unattended.RefusedInvalid},
		{"chain root on a cloud start", grantTestService("low"),
			unattended.Request{ChainRootExecutionID: "0123456789abcdef0123456789abcdef"}, "", unattended.RefusedInvalid},
		{"schedule names a tool", grantTestService("low"), unattended.Request{SensitiveTools: tool("jira", "create_issue")},
			executiondomain.TriggerOriginSchedule, unattended.RefusedOrigin},
		{"webhook names a tool", grantTestService("low"), unattended.Request{SensitiveTools: tool("jira", "create_issue")},
			executiondomain.TriggerOriginWebhook, unattended.RefusedOrigin},
		{"unknown toolkit", grantTestService("low"), unattended.Request{SensitiveTools: tool("github", "push")}, "",
			unattended.RefusedToolNotResolved},
		{"unselected tool", grantTestService("low"), unattended.Request{SensitiveTools: tool("jira", "delete_issue")}, "",
			unattended.RefusedToolNotResolved},
		{"sub-agent", grantTestService("low"), unattended.Request{SensitiveTools: tool("child", "run")}, "",
			unattended.RefusedToolNotResolved},
		{"withheld payment", grantTestService("low"), unattended.Request{SensitiveTools: tool("bank", "pay")}, "",
			unattended.RefusedCategoryWithheld},
		{"tier requires hitl", grantTestService("high"), unattended.Request{SensitiveTools: tool("mail", "send")}, "",
			unattended.RefusedTierRequiresHITL},
		{"over the maximum", grantTestService("low"), unattended.Request{
			Chain: unattended.ChainRequest{MaxContinuations: new(int64(201))}}, "", unattended.RefusedOverMaximum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			target := grantTestTarget(&request)
			target.triggerOrigin = tc.origin
			_, _, err := tc.service.admitUnattended(t.Context(), target)
			var refusal *unattended.RefusalError
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("admit = %v, want reason %q", err, tc.reason)
			}
		})
	}
}

func TestAdmitUnattendedGrantIsDeterministic(t *testing.T) {
	service := grantTestService("low")
	request := unattended.Request{
		SensitiveTools:    []unattended.Tool{{ToolkitName: "jira", ToolName: "create_issue"}, {ToolkitName: "mail", ToolName: "send"}},
		ContinueStepLimit: true,
	}
	first, admission, err := service.admitUnattended(t.Context(), grantTestTarget(&request))
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := service.admitUnattended(t.Context(), grantTestTarget(&request))
	if err != nil || first.GetGrantId() != again.GetGrantId() {
		t.Fatalf("a retried start has another grant id: %v, %v", first.GetGrantId(), again.GetGrantId())
	}
	if first.GetGrantId() != CloudGrantID("3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d", "question-1") ||
		first.GetChain().GetDeadlineUnixSeconds() != 0 || first.GetContinuationIndex() != 0 ||
		first.GetOwnerUserId() != 7 || len(first.GetSensitiveTools()) != 2 || !first.GetContinueStepLimit() {
		t.Fatalf("grant = %+v", first)
	}
	// The default spend is the tier's per-run cap times the multiplier.
	if admission.Start == nil || admission.Start.Limits.MaxSpendUSD != 20 ||
		admission.Start.Limits.MaxContinuations != platformconfig.DefaultChainContinuations ||
		admission.Start.Limits.Deadline != 12*time.Hour || admission.Start.Grant.Start == nil {
		t.Fatalf("admission = %+v", admission.Start)
	}
	if !ValidUnattendedGrant(first) {
		t.Fatal("the admitted grant is not valid")
	}
}

func TestUnattendedContinuationGrant(t *testing.T) {
	chain := unattended.Chain{
		RootExecutionID: "0123456789abcdef0123456789abcdef", ExecutionID: "fedcba9876543210fedcba9876543210",
		ContinuationIndex: 2, ProjectID: 1, OwnerUserID: 7, ConversationUUID: "3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d",
		Origin: unattended.OriginCloud, GrantID: "5e7a9c1d-3f4b-4c8d-8e2f-4a6b8c0d2e3f",
		Grant:      unattended.Grant{GrantID: "5e7a9c1d-3f4b-4c8d-8e2f-4a6b8c0d2e3f", ContinueStepLimit: true},
		Limits:     unattended.Limits{MaxContinuations: 5},
		DeadlineAt: time.Unix(1_900_000_000, 0),
	}
	target := grantTestTarget(nil)
	target.continuation = &UnattendedContinuation{Chain: chain, Kind: unattended.LinkStepLimit}
	grant, admission, err := grantTestService("low").admitUnattended(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if grant.GetContinuationIndex() != 3 || grant.GetChainRootExecutionId() != chain.RootExecutionID ||
		grant.GetChain().GetDeadlineUnixSeconds() != 1_900_000_000 || !ValidUnattendedGrant(grant) {
		t.Fatalf("continuation grant = %+v", grant)
	}
	if len(admission.MarkMessageIDs) != 2 || admission.Continue == nil || admission.Continue.Kind != unattended.LinkStepLimit {
		t.Fatalf("admission = %+v", admission)
	}
	other := target
	other.actorUserID = 8
	if _, _, err := grantTestService("low").admitUnattended(t.Context(), other); err == nil {
		t.Fatal("another person's continuation was admitted")
	}
	// approvals.unattended_grants is read again on every continuation: off
	// refuses it as disabled (the chain hook then ends the chain cancelled).
	off := grantTestService("low")
	disabled := platformconfig.DefaultUnattendedSettings()
	disabled.Enabled = false
	off.unattended.Settings = grantSettingsStub{disabled}
	var refusal *unattended.RefusalError
	if _, _, err := off.admitUnattended(t.Context(), target); !errors.As(err, &refusal) ||
		refusal.Reason != unattended.RefusedDisabled {
		t.Fatalf("continuation with grants off = %v", err)
	}
	if _, _, err := (&CurrentApplicationStartService{}).admitUnattended(t.Context(), target); !errors.As(err, &refusal) ||
		refusal.Reason != unattended.RefusedDisabled {
		t.Fatalf("continuation without a grant policy = %v", err)
	}
	ended := chain
	ended.EndState = unattended.EndCompleted
	target.continuation = &UnattendedContinuation{Chain: ended, Kind: unattended.LinkStepLimit}
	if _, _, err := grantTestService("low").admitUnattended(t.Context(), target); !errors.Is(err, unattended.ErrChainEnded) {
		t.Fatalf("ended chain = %v", err)
	}
}

// ADR-0045: the cloud workspace's git_push and open_pull_request are not in
// the frozen toolkit set. A grant names them as workspace/<tool>, and only
// for a thread handed off to the cloud executor.
func TestAdmitUnattendedWorkspaceTools(t *testing.T) {
	request := &unattended.Request{SensitiveTools: []unattended.Tool{
		{ToolkitName: "workspace", ToolName: "git_push"},
		{ToolkitName: "workspace", ToolName: "open_pull_request"},
	}}
	target := grantTestTarget(request)
	target.cloudWorkspace = true
	grant, admission, err := grantTestService("low").admitUnattended(t.Context(), target)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if admission == nil || len(grant.GetSensitiveTools()) != 2 ||
		grant.GetSensitiveTools()[0].GetToolkitName() != "workspace" {
		t.Fatalf("grant = %v", grant)
	}

	target = grantTestTarget(request)
	_, _, err = grantTestService("low").admitUnattended(t.Context(), target)
	var refused *unattended.RefusalError
	if !errors.As(err, &refused) || refused.Reason != unattended.RefusedToolNotResolved ||
		refused.Tool != "workspace/git_push" {
		t.Fatalf("outside a cloud workspace thread: err = %v", err)
	}

	target = grantTestTarget(&unattended.Request{SensitiveTools: []unattended.Tool{
		{ToolkitName: "workspace", ToolName: "run_command"},
	}})
	target.cloudWorkspace = true
	_, _, err = grantTestService("low").admitUnattended(t.Context(), target)
	if !errors.As(err, &refused) || refused.Reason != unattended.RefusedToolNotResolved {
		t.Fatalf("another workspace tool: err = %v", err)
	}
}
