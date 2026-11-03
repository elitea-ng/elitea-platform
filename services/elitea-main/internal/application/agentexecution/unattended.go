package agentexecution

// Unattended cloud runs (ADR-0045 decisions 1, 2 and 4, #1435, #1436).
//
// A person who starts a run can give one grant (`unattended` on the start
// body). Main admits it here, before the request digest: the platform switch
// approvals.unattended_grants, each named sensitive tool against the run's
// own frozen toolkit set, the withheld categories and the tier hitl rule. A
// refused part refuses the whole start (422 unattended_grant_refused); a
// grant is never narrowed. The admitted grant rides the input as
// UnattendedGrantV1 (field 68) and the admission transaction writes the
// chain row (repos/run_chains.go).
//
// Main itself starts each continuation of the chain (unattendedchain): the
// same grant, chain_root_execution_id set, and the chain check in the
// admission transaction.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// UnattendedGrantSettings reads the grant settings (approvals section).
type UnattendedGrantSettings interface {
	Get(ctx context.Context) (platformconfig.UnattendedSettings, error)
}

// UnattendedApprovalPolicy reads the approval policy (its hitl tier rule).
type UnattendedApprovalPolicy interface {
	Get(ctx context.Context) (platformconfig.ApprovalPolicySettings, error)
}

// UnattendedRunCaps reads the per-tier run caps (the per-run spend cap).
type UnattendedRunCaps interface {
	Get(ctx context.Context) (platformconfig.RunCaps, error)
}

// UnattendedTierReader reads an agent's risk tier.
type UnattendedTierReader interface {
	AgentRiskTier(ctx context.Context, projectID, applicationID int64) (string, error)
}

// UnattendedGrantPolicy is what the grant admission reads.
type UnattendedGrantPolicy struct {
	Settings UnattendedGrantSettings
	Approval UnattendedApprovalPolicy
	RunCaps  UnattendedRunCaps
	Tiers    UnattendedTierReader
}

func (p *UnattendedGrantPolicy) complete() bool {
	return p != nil && p.Settings != nil && p.Approval != nil && p.RunCaps != nil && p.Tiers != nil
}

// WithUnattendedGrants lets the start service admit grants. Without it, a
// start that carries a grant is refused as disabled.
func (service *CurrentApplicationStartService) WithUnattendedGrants(policy *UnattendedGrantPolicy) *CurrentApplicationStartService {
	if service != nil && policy.complete() {
		service.unattended = policy
	}
	return service
}

// UnattendedContinuation is a chain continuation that Main starts: the
// chain of the run it continues, and why.
type UnattendedContinuation struct {
	// Chain is the link of the run that this one continues.
	Chain unattended.Chain
	// Kind is unattended.LinkStepLimit, unattended.LinkOutputLimit or
	// unattended.LinkGoalNotMet.
	Kind string
}

// RunBriefStarter writes a new cloud chain's run brief in the background
// (ADR-0045 decision 11). Begin never blocks the start.
type RunBriefStarter interface {
	Begin(ctx context.Context, start RunBriefStart)
}

// RunBriefStart is what the brief writer needs from a chain's first run.
type RunBriefStart struct {
	RootExecutionID string
	ProjectID       int64
	OwnerUserID     int64
	// Request is the person's first message.
	Request string
}

// WithRunBriefs lets the start service begin the run brief of each new
// cloud chain whose grant asks for a goal check.
func (service *CurrentApplicationStartService) WithRunBriefs(briefs RunBriefStarter) *CurrentApplicationStartService {
	if service != nil && briefs != nil {
		service.runBriefs = briefs
	}
	return service
}

// beginRunBrief starts the brief of a chain's first run after its admission.
// A replayed start begins it again: the writer does nothing for a brief that
// is not pending.
func (service *CurrentApplicationStartService) beginRunBrief(
	ctx context.Context, chain *UnattendedAdmission, executionID, request string,
) {
	if service.runBriefs == nil || chain == nil || chain.Start == nil ||
		chain.Start.BriefState != unattended.BriefPending || executionID == "" {
		return
	}
	service.runBriefs.Begin(ctx, RunBriefStart{
		RootExecutionID: executionID, ProjectID: chain.Start.ProjectID,
		OwnerUserID: chain.Start.OwnerUserID, Request: request,
	})
}

// UnattendedAdmission is what the admission transaction writes for a run
// with a grant: the chain's first run, or a continuation.
type UnattendedAdmission struct {
	// Start is the first run; its RootExecutionID is the new execution's id.
	Start *unattended.ChainStart
	// Continue is a continuation; its ExecutionID is the new execution's id.
	Continue *unattended.ChainContinue
	// MarkMessageIDs are the chat messages a continuation marks with
	// meta.continued_automatically (the new question of a step-limit
	// continuation, and the response).
	MarkMessageIDs []string
}

// Clone copies the admission.
func (a *UnattendedAdmission) Clone() *UnattendedAdmission {
	if a == nil {
		return nil
	}
	clone := *a
	if a.Start != nil {
		start := *a.Start
		start.Grant.SensitiveTools = append([]unattended.Tool(nil), a.Start.Grant.SensitiveTools...)
		clone.Start = &start
	}
	if a.Continue != nil {
		next := *a.Continue
		clone.Continue = &next
	}
	clone.MarkMessageIDs = append([]string(nil), a.MarkMessageIDs...)
	return &clone
}

// unattendedTarget is one start's view for the grant admission.
type unattendedTarget struct {
	request      *unattended.Request
	continuation *UnattendedContinuation
	capabilityID string
	input        *runtimev1.AgentExecutionInputV1
	projectID    int64
	actorUserID  int64
	// applicationID is 0 for a model chat.
	applicationID     int64
	conversationUUID  string
	grantKey          string
	responseMessageID string
	questionID        string
	triggerOrigin     executiondomain.TriggerOrigin
	// cloudWorkspace: the conversation is a thread handed off to the cloud
	// executor, so the turn has the workspace tools.
	cloudWorkspace bool
	start          *unattended.CloudStart
}

var unattendedGrantNamespace = uuid.MustParse("4f8f6a52-1c55-4d77-9a0e-7b7f0c3c2b61")

// CloudGrantID is the grant id Main derives for a cloud start that names
// none: stable for the start's idempotency key, so a retried start is the
// same request.
func CloudGrantID(conversationUUID, key string) string {
	return uuid.NewSHA1(unattendedGrantNamespace, []byte(conversationUUID+"\x00"+key)).String()
}

// admitUnattended admits a start's grant, or a continuation's chain. It
// answers nil, nil for a run with neither.
func (service *CurrentApplicationStartService) admitUnattended(
	ctx context.Context, target unattendedTarget,
) (*runtimev1.UnattendedGrantV1, *UnattendedAdmission, error) {
	switch {
	case target.request == nil && target.continuation == nil:
		return nil, nil, nil
	case target.request != nil && target.continuation != nil:
		return nil, nil, ErrInvalidCurrentAgentStart
	case target.continuation != nil:
		if err := service.unattendedGrantsEnabled(ctx); err != nil {
			return nil, nil, err
		}
		return unattendedContinuationGrant(target)
	}
	request := *target.request
	if request.ChainRootExecutionID != "" {
		// A cloud chain continues only through Main.
		return nil, nil, unattended.Refused("chain_root_execution_id", "", unattended.RefusedInvalid)
	}
	if request.GrantID == "" {
		request.GrantID = CloudGrantID(target.conversationUUID, target.grantKey)
	}
	if err := unattended.ValidateShape(request); err != nil {
		return nil, nil, err
	}
	if request.ContinueToolCallCap || request.HookAsks || request.RemoteConfirmations {
		// Desktop-only parts: a cloud run has no tool call cap, hook or
		// remote confirmation.
		return nil, nil, unattended.Refused("unattended", "", unattended.RefusedInvalid)
	}
	settings, err := service.unattendedSettings(ctx)
	if unattended.IsDisabled(err) && request.OnlyContinues() {
		// Grants are off: the default grant only continued, so the run
		// starts with no grant (ADR-0045 decision 10).
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(request.SensitiveTools) > 0 &&
		(target.triggerOrigin == executiondomain.TriggerOriginSchedule ||
			target.triggerOrigin == executiondomain.TriggerOriginWebhook) {
		// ADR-0041 decision 5 stays in force for schedules and webhooks.
		return nil, nil, unattended.Refused("sensitive_tools", "", unattended.RefusedOrigin)
	}
	tier := agentlifecycle.TierLow
	if target.applicationID > 0 {
		raw, err := service.unattended.Tiers.AgentRiskTier(ctx, target.projectID, target.applicationID)
		if err != nil {
			return nil, nil, fmt.Errorf("read the agent's risk tier: %w", err)
		}
		if parsed := agentlifecycle.Tier(raw); parsed.Valid() {
			tier = parsed
		}
	}
	if len(request.SensitiveTools) > 0 {
		policy, err := service.unattended.Approval.Get(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("read the approval policy: %w", err)
		}
		if err := admitSensitiveTools(target, request.SensitiveTools, settings, policy, tier); err != nil {
			return nil, nil, err
		}
	}
	caps, err := service.unattended.RunCaps.Get(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the run caps: %w", err)
	}
	limits, err := unattended.ResolveLimits(settings, request.Chain, caps.For(tier).MaxSpend)
	if err != nil {
		return nil, nil, err
	}
	grant := unattended.GrantOf(request, target.start)
	admission := &UnattendedAdmission{Start: &unattended.ChainStart{
		ProjectID: target.projectID, OwnerUserID: target.actorUserID,
		ConversationUUID: target.conversationUUID, Origin: unattended.OriginCloud,
		TriggerOrigin: string(target.triggerOrigin.Stored()), Grant: grant, Limits: limits,
		ResponseMessageID: target.responseMessageID,
		BriefState:        unattended.BriefStateAtStart(request.GoalCheck, settings.RunBrief),
	}}
	// The deadline is 0 on the first run: the chain row holds it, and a
	// clock read here would make a retried start a different request.
	return grantProto(grant, target.actorUserID, "", 0, limits, 0), admission, nil
}

// unattendedSettings reads the grant settings. It refuses as disabled when
// the start service has no grant policy or approvals.unattended_grants is
// off.
func (service *CurrentApplicationStartService) unattendedSettings(
	ctx context.Context,
) (platformconfig.UnattendedSettings, error) {
	if !service.unattended.complete() {
		return platformconfig.UnattendedSettings{}, unattended.Refused("unattended", "", unattended.RefusedDisabled)
	}
	settings, err := service.unattended.Settings.Get(ctx)
	if err != nil {
		return platformconfig.UnattendedSettings{}, fmt.Errorf("read the unattended grant settings: %w", err)
	}
	if !settings.Enabled {
		return platformconfig.UnattendedSettings{}, unattended.Refused("unattended", "", unattended.RefusedDisabled)
	}
	return settings, nil
}

// unattendedGrantsEnabled checks the platform switch again before Main
// starts a continuation. An administrator who turns grants off stops every
// running chain at its next continuation: the chain hook ends it as
// cancelled.
func (service *CurrentApplicationStartService) unattendedGrantsEnabled(ctx context.Context) error {
	_, err := service.unattendedSettings(ctx)
	return err
}

// unattendedContinuationGrant is the grant of a continuation that Main
// starts: the chain's own grant, owner and limits.
func unattendedContinuationGrant(target unattendedTarget) (*runtimev1.UnattendedGrantV1, *UnattendedAdmission, error) {
	chain := target.continuation.Chain
	if chain.OwnerUserID != target.actorUserID || chain.ProjectID != target.projectID ||
		chain.ConversationUUID != target.conversationUUID || chain.Origin != unattended.OriginCloud {
		return nil, nil, unattended.Refused("chain_root_execution_id", "", unattended.RefusedNotOwner)
	}
	if chain.EndState != "" {
		return nil, nil, unattended.ErrChainEnded
	}
	marks := []string{target.responseMessageID}
	if target.continuation.Kind != unattended.LinkOutputLimit && target.questionID != "" {
		marks = append([]string{target.questionID}, marks...)
	}
	admission := &UnattendedAdmission{
		Continue: &unattended.ChainContinue{
			RootExecutionID: chain.RootExecutionID, Kind: target.continuation.Kind,
			OwnerUserID: chain.OwnerUserID, GrantID: chain.GrantID, ProjectID: chain.ProjectID,
			ConversationUUID: chain.ConversationUUID, ResponseMessageID: target.responseMessageID,
		},
		MarkMessageIDs: marks,
	}
	// The index of the link this one follows plus one: stable on a replay.
	index := uint32(chain.ContinuationIndex) + 1 //nolint:gosec // a chain index is bounded by max_continuations
	return grantProto(chain.Grant, chain.OwnerUserID, chain.RootExecutionID, index, chain.Limits,
		chain.DeadlineAt.Unix()), admission, nil
}

func grantProto(
	grant unattended.Grant, owner int64, root string, index uint32, limits unattended.Limits, deadline int64,
) *runtimev1.UnattendedGrantV1 {
	tools := make([]*runtimev1.UnattendedGrantToolV1, 0, len(grant.SensitiveTools))
	for _, tool := range grant.SensitiveTools {
		tools = append(tools, &runtimev1.UnattendedGrantToolV1{ToolkitName: tool.ToolkitName, ToolName: tool.ToolName})
	}
	return &runtimev1.UnattendedGrantV1{
		GrantId: grant.GrantID, ChainRootExecutionId: root, OwnerUserId: owner,
		SensitiveTools: tools, ContinueStepLimit: grant.ContinueStepLimit,
		ContinueOutputLimit: grant.ContinueOutputLimit,
		Chain: &runtimev1.UnattendedChainLimitsV1{
			MaxContinuations: uint32(limits.MaxContinuations), //nolint:gosec // bounded by the platform maximum
			MaxSpendUsd:      limits.MaxSpendUSD, DeadlineUnixSeconds: deadline,
		},
		ContinuationIndex: index,
	}
}

// frozenGrantToolkit is the part of a frozen toolkit reference the grant
// admission reads.
type frozenGrantToolkit struct {
	Type        string `json:"type"`
	ToolkitName string `json:"toolkit_name"`
	Settings    struct {
		SelectedTools []any `json:"selected_tools"`
		PinnedTools   []struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		} `json:"pinned_tools"`
	} `json:"settings"`
}

// admitSensitiveTools checks each named tool against the run's own frozen
// toolkit set, the withheld categories and the tier hitl rule. The first
// failure refuses the start and names the tool.
func admitSensitiveTools(
	target unattendedTarget, tools []unattended.Tool, settings platformconfig.UnattendedSettings,
	policy platformconfig.ApprovalPolicySettings, tier agentlifecycle.Tier,
) error {
	raw, err := frozenToolkitsRaw(target.capabilityID, target.input)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAgentAdmission, err)
	}
	var toolkits []frozenGrantToolkit
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &toolkits); err != nil {
			return fmt.Errorf("%w: the frozen toolkit set: %v", ErrInvalidAgentAdmission, err)
		}
	}
	for _, tool := range tools {
		name := tool.ToolkitName + "/" + tool.ToolName
		toolkit, ok := resolvedGrantToolkit(toolkits, tool)
		if !ok && target.cloudWorkspace && workspaceGrantTool(tool) {
			toolkit, ok = frozenGrantToolkit{Type: workspaceGrantToolkit, ToolkitName: workspaceGrantToolkit}, true
		}
		if !ok {
			return unattended.Refused("sensitive_tools", name, unattended.RefusedToolNotResolved)
		}
		category := grantToolCategory(toolkit, tool.ToolName)
		if settings.Withholds(category) {
			return unattended.Refused("sensitive_tools", name, unattended.RefusedCategoryWithheld)
		}
		if policy.Policy.RequiresHITL(tier) && approvalpolicy.GatedByHITL(category) {
			return unattended.Refused("sensitive_tools", name, unattended.RefusedTierRequiresHITL)
		}
	}
	return nil
}

// workspaceGrantToolkit is the toolkit name of the cloud workspace's
// pull-request tools: their pauses, the worker's grant approval event and a
// grant all name them workspace/git_push and workspace/open_pull_request.
const workspaceGrantToolkit = "workspace"

// workspaceGrantTool reports one of the cloud workspace's pull-request
// tools. They are not in the frozen toolkit set: the worker adds them to a
// turn of a thread handed off to the cloud executor (ADR-0034 decision 10),
// and they pause only when the project's policy asks before each push and
// pull request. A grant names them only for such a thread.
func workspaceGrantTool(tool unattended.Tool) bool {
	return tool.ToolkitName == workspaceGrantToolkit &&
		(tool.ToolName == "git_push" || tool.ToolName == "open_pull_request")
}

// resolvedGrantToolkit finds the toolkit a named tool belongs to: the
// toolkit name must be one of the run's own toolkits (not a sub-agent), and
// a toolkit that selects its tools must select this one.
func resolvedGrantToolkit(toolkits []frozenGrantToolkit, tool unattended.Tool) (frozenGrantToolkit, bool) {
	for _, toolkit := range toolkits {
		if toolkit.ToolkitName != tool.ToolkitName || toolkit.Type == "application" {
			continue
		}
		if len(toolkit.Settings.SelectedTools) == 0 {
			return toolkit, true
		}
		for _, selected := range toolkit.Settings.SelectedTools {
			if name, ok := selected.(string); ok && name == tool.ToolName {
				return toolkit, true
			}
		}
		return frozenGrantToolkit{}, false
	}
	return frozenGrantToolkit{}, false
}

// grantToolCategory is the tool's category: an approved MCP server's pin,
// else the contract's declaration (toolcategory.Of; undeclared is write).
func grantToolCategory(toolkit frozenGrantToolkit, toolName string) toolcategory.Category {
	for _, pin := range toolkit.Settings.PinnedTools {
		if pin.Name == toolName {
			if category, ok := toolcategory.Parse(pin.Category); ok {
				return category
			}
			return toolcategory.Default
		}
	}
	return toolcategory.Of(toolkit.Type, toolName)
}

// continuationQuestionID is the question of a step-limit continuation:
// derived from the run it continues, so a replayed settle hook starts the
// same turn.
func continuationQuestionID(previousExecutionID string) string {
	return uuid.NewSHA1(unattendedGrantNamespace, []byte("step-limit\x00"+previousExecutionID)).String()
}

// GoalContinuationQuestionID is the question of a goal_not_met
// continuation, derived from the run that the goal check read.
func GoalContinuationQuestionID(previousExecutionID string) string {
	return uuid.NewSHA1(unattendedGrantNamespace, []byte("goal-not-met\x00"+previousExecutionID)).String()
}

// ContinuationQuestionID exposes continuationQuestionID to the chain hook.
func ContinuationQuestionID(previousExecutionID string) string {
	return continuationQuestionID(previousExecutionID)
}

// The cloud start contracts a step-limit continuation repeats.
const (
	CloudStartContractApplication = "agent.execute.application.v1"
	CloudStartContractAdhoc       = "agent.execute.adhoc.v1"
)

// markContinuedAutomatically adds continued_automatically to a question's
// meta: the transcript shows the continuation as host-authored.
func markContinuedAutomatically(meta json.RawMessage) json.RawMessage {
	object := map[string]any{}
	if len(meta) != 0 {
		if err := json.Unmarshal(meta, &object); err != nil || object == nil {
			object = map[string]any{}
		}
	}
	object["continued_automatically"] = true
	marked, err := json.Marshal(object)
	if err != nil {
		return meta
	}
	return marked
}

// ValidUnattendedGrant reports a well-formed grant: a grant id, an owner,
// chain limits, and at most unattended.MaxSensitiveTools distinct named
// tools.
func ValidUnattendedGrant(grant *runtimev1.UnattendedGrantV1) bool {
	if !unattended.ValidGrantID(grant.GetGrantId()) || grant.GetOwnerUserId() <= 0 || grant.GetChain() == nil ||
		(grant.GetChainRootExecutionId() != "" && !unattended.ValidExecutionID(grant.GetChainRootExecutionId())) ||
		(grant.GetChainRootExecutionId() == "") != (grant.GetContinuationIndex() == 0) ||
		len(grant.GetSensitiveTools()) > unattended.MaxSensitiveTools {
		return false
	}
	tools := make([]unattended.Tool, 0, len(grant.GetSensitiveTools()))
	for _, tool := range grant.GetSensitiveTools() {
		tools = append(tools, unattended.Tool{ToolkitName: tool.GetToolkitName(), ToolName: tool.GetToolName()})
	}
	return unattended.ValidateShape(unattended.Request{GrantID: grant.GetGrantId(), SensitiveTools: tools}) == nil
}
