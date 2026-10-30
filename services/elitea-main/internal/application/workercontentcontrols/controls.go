// Package workercontentcontrols assembles the content controls an agent
// execution command carries (ADR-0043 decisions 3 and 4, #1500;
// WorkerCommandV1.content_controls, field 71): the AI disclosure marker
// text and opt-outs, the platform preamble with its revision, and the "read
// untrusted, then act" switch for the root agent's tier.
//
// The worker applies the controls: it adds the marker to the text that
// outbound tools send, puts its built-in preamble and the administrator's
// preamble before every system prompt, and asks for an approval of a
// delete, external_send or payment call after the run read a tool result.
// Main supplies what the worker cannot know: the platform sections
// `ai_disclosure` and `platform_preamble`, the approval policy's rule
// tiers, the project's `ai_disclosure` setting and the root agent's tier.
package workercontentcontrols

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerapprovalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// SettingsReader reads the platform sections (in production a
// platformconfig.CachedSection that an admin save drops).
type SettingsReader interface {
	Get(ctx context.Context) (platformconfig.ContentControlsSettings, error)
}

// ProjectSetting is a project's `ai_disclosure` configuration row.
type ProjectSetting struct {
	MarkerText         string
	OptOutToolkitTypes []string
}

// ProjectReader reads a project's `ai_disclosure` setting. No row is the
// zero setting.
type ProjectReader interface {
	AIDisclosureProjectSetting(ctx context.Context, projectID int64) (ProjectSetting, error)
}

// Resolver computes an agent execution command's content controls. It is
// the commandbus producers' ContentControlsResolver.
type Resolver struct {
	settings SettingsReader
	projects ProjectReader
	inputs   workerdelegation.InputReader
	tiers    workerapprovalpolicy.TierReader
}

// NewResolver builds the resolver.
func NewResolver(
	settings SettingsReader, projects ProjectReader,
	inputs workerdelegation.InputReader, tiers workerapprovalpolicy.TierReader,
) (*Resolver, error) {
	if settings == nil || projects == nil || inputs == nil || tiers == nil {
		return nil, errors.New("worker content controls: the settings, project, input and tier readers are required")
	}
	return &Resolver{settings: settings, projects: projects, inputs: inputs, tiers: tiers}, nil
}

// WorkerContentControls answers the command's controls, or nil while no
// administrator saved the sections or the rule tiers and the run's input
// carries no platform preamble: such a command is unchanged byte for byte,
// so a worker of an earlier release still accepts it (the chart rolls Main
// out first), and the worker applies its built-in defaults. Only an agent
// execution command carries them. The preamble's text stays in the input
// bundle (admission froze it there); the command names only its revision
// and digest. An error is a source that could not be read; the dispatch is
// retried.
func (r *Resolver) WorkerContentControls(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerContentControlsV1, error) {
	if command.GetAgentExecution() == nil {
		return nil, nil
	}
	settings, err := r.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker content controls: read the platform settings: %w", err)
	}
	input, raw, err := r.input(ctx, command)
	if err != nil {
		return nil, err
	}
	preamble := input.GetPlatformPreamble()
	if !settings.Stored && preamble == nil {
		return nil, nil
	}
	// A snapshot is always a saved preamble: the cache of this replica may
	// not have seen the save yet.
	settings.Stored = true
	projectID, _ := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	var project ProjectSetting
	if projectID > 0 {
		project, err = r.projects.AIDisclosureProjectSetting(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("worker content controls: read the project setting: %w", err)
		}
	}
	tier := agentlifecycle.TierLow
	if needsTier(settings) {
		tier, err = r.rootTier(ctx, raw, projectID)
		if err != nil {
			return nil, err
		}
	}
	return Controls(settings, project, tier, preamble), nil
}

// needsTier reports whether the rule's answer depends on the tier.
func needsTier(settings platformconfig.ContentControlsSettings) bool {
	return len(settings.UntrustedReadThenActTiers) > 0 && len(settings.UntrustedReadThenActTiers) < len(agentlifecycle.Tiers)
}

// Controls is the command field for the settings, the project's setting,
// the root agent's tier and the preamble the run's input carries: nil while
// nothing was saved. The project's text
// replaces the platform's; an empty one keeps it. A project's opt-out takes
// effect only for a type the platform allows.
func Controls(
	settings platformconfig.ContentControlsSettings, project ProjectSetting, tier agentlifecycle.Tier,
	preamble *runtimev1.PlatformPreambleSnapshotV1,
) *runtimev1.WorkerContentControlsV1 {
	if !settings.Stored {
		return nil
	}
	text := settings.MarkerText
	if project.MarkerText != "" {
		text = project.MarkerText
	}
	if text == "" {
		text = toolcategory.DefaultDisclosureMarkerText
	}
	allowed := map[string]bool{}
	for _, name := range settings.OptOutToolkitTypes {
		allowed[name] = true
	}
	optOuts := []string{}
	for _, raw := range project.OptOutToolkitTypes {
		name, known := toolcategory.DisclosureToolkitType(raw)
		if known && allowed[name] && !contains(optOuts, name) {
			optOuts = append(optOuts, name)
		}
	}
	if !tier.Valid() {
		tier = agentlifecycle.TierLow
	}
	return &runtimev1.WorkerContentControlsV1{
		DisclosureText:               text,
		DisclosureOptOutToolkitTypes: optOuts,
		PlatformPreambleSha256:       preamble.GetSha256(),
		PlatformPreambleRevision:     preamble.GetRevision(),
		UntrustedReadThenAct:         settings.UntrustedReadThenAct(tier),
	}
}

func contains(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}
	return false
}

// input reads the command's agent input. A command without one (no bundle
// reference) has none.
func (r *Resolver) input(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.AgentExecutionInputV1, []byte, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if bundle == "" || entryID == "" {
		return nil, nil, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, nil, fmt.Errorf("worker content controls: read the command's input: %w", err)
	}
	content, ok := contents[entryID]
	if !ok {
		return nil, nil, nil
	}
	var input runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(content, &input); err != nil {
		return nil, nil, fmt.Errorf("worker content controls: decode the command's input: %w", err)
	}
	return &input, content, nil
}

// rootTier reads the risk tier of the run's root agent, as the approval
// policy does: an ad hoc conversation is low.
func (r *Resolver) rootTier(ctx context.Context, raw []byte, projectID int64) (agentlifecycle.Tier, error) {
	if len(raw) == 0 || projectID <= 0 {
		return agentlifecycle.TierLow, nil
	}
	applicationID := workerdelegation.ApplicationID(raw)
	if applicationID <= 0 {
		return agentlifecycle.TierLow, nil
	}
	return workerapprovalpolicy.ReadTier(ctx, r.tiers, projectID, applicationID)
}
