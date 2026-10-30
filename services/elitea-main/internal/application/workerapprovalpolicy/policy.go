// Package workerapprovalpolicy assembles the approval policy an agent
// execution command carries (ADR-0041 decision 5, #1498), and checks the
// pipeline hitl rule when a pipeline run starts.
//
// The worker decides at every tool call whether the call needs a human
// approval: when the platform's sensitive action tools name the tool, or
// when the tool's category is one the policy names for the run's tier. Main
// supplies what the worker cannot know: the administrator's tier by category
// matrix (platform configuration section `approval_policy`) and the root
// agent's risk tier (its elitea_lifecycle.agent_registry row). Nested callees
// use the root agent's tier.
package workerapprovalpolicy

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// SettingsReader reads the platform `approval_policy` section (in
// production a platformconfig.CachedSection the admin save drops).
type SettingsReader interface {
	Get(ctx context.Context) (platformconfig.ApprovalPolicySettings, error)
}

// TierReader reads the risk tier of one agent's registry row. No row (or no
// registry table) is the empty tier, read as low: the tier of a new agent.
type TierReader interface {
	AgentRiskTier(ctx context.Context, projectID, applicationID int64) (string, error)
}

// Resolver computes an agent execution command's approval policy. It is
// the commandbus producers' ApprovalPolicyResolver.
type Resolver struct {
	settings SettingsReader
	inputs   workerdelegation.InputReader
	tiers    TierReader
}

// NewResolver builds the resolver.
func NewResolver(settings SettingsReader, inputs workerdelegation.InputReader, tiers TierReader) (*Resolver, error) {
	if settings == nil || inputs == nil || tiers == nil {
		return nil, errors.New("worker approval policy: the settings, input and tier readers are required")
	}
	return &Resolver{settings: settings, inputs: inputs, tiers: tiers}, nil
}

// WorkerApprovalPolicy answers the command's policy, or nil when no
// administrator saved the `approval_policy` section, so such a command is
// unchanged byte for byte and a worker from an earlier release still accepts
// it (the chart rolls Main out first). Only an agent execution command
// carries one. An error is a source that could not be read; the dispatch is
// retried.
func (r *Resolver) WorkerApprovalPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerApprovalPolicyV1, error) {
	if command.GetAgentExecution() == nil {
		return nil, nil
	}
	settings, err := r.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker approval policy: read the platform setting: %w", err)
	}
	if !settings.Stored {
		return nil, nil
	}
	tier, err := r.rootTier(ctx, command)
	if err != nil {
		return nil, err
	}
	return Policy(settings, tier), nil
}

// Policy is the command field for settings and the root agent's tier: nil
// when the section was never saved.
func Policy(settings platformconfig.ApprovalPolicySettings, tier agentlifecycle.Tier) *runtimev1.WorkerApprovalPolicyV1 {
	if !settings.Stored {
		return nil
	}
	if !tier.Valid() {
		tier = agentlifecycle.TierLow
	}
	categories := []string{}
	for _, category := range settings.Policy.CategoriesFor(tier) {
		categories = append(categories, string(category))
	}
	return &runtimev1.WorkerApprovalPolicyV1{RiskTier: string(tier), ApprovalCategories: categories}
}

// rootTier reads the risk tier of the run's root agent: the saved agent the
// command's input names, in the command's project. An ad hoc conversation
// names none and is low.
func (r *Resolver) rootTier(ctx context.Context, command *runtimev1.WorkerCommandV1) (agentlifecycle.Tier, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if bundle == "" || entryID == "" {
		return agentlifecycle.TierLow, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return "", fmt.Errorf("worker approval policy: read the command's input: %w", err)
	}
	applicationID := workerdelegation.ApplicationID(contents[entryID])
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if applicationID <= 0 || err != nil || projectID <= 0 {
		return agentlifecycle.TierLow, nil
	}
	return ReadTier(ctx, r.tiers, projectID, applicationID)
}

// ReadTier reads one agent's tier. No row is low. A stored value that is
// not a tier is an error: reading it as low would lift the policy.
func ReadTier(ctx context.Context, tiers TierReader, projectID, applicationID int64) (agentlifecycle.Tier, error) {
	stored, err := tiers.AgentRiskTier(ctx, projectID, applicationID)
	if err != nil {
		return "", fmt.Errorf("worker approval policy: read the agent's risk tier: %w", err)
	}
	if stored == "" {
		return agentlifecycle.TierLow, nil
	}
	tier := agentlifecycle.Tier(stored)
	if !tier.Valid() {
		return "", fmt.Errorf("worker approval policy: the agent's risk tier %q is not a tier", stored)
	}
	return tier, nil
}
