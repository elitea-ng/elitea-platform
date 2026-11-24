// Package workerdelegation assembles the delegation policy an agent execution
// command carries (ADR-0044 decision 3, #1501).
//
// When an agent calls a saved agent or pipeline, a pipeline node runs one, or
// an agent starts a sub-agent, the worker runs the callee with the
// intersection of its own grants and its caller's, no deeper than the
// administrator's maximum depth. Main supplies what the worker cannot know:
// the platform's `delegation` section (maximum depth and mode) and the root
// agent's declared data classes (its elitea_lifecycle.agent_registry row).
//
// Data classes are declared metadata. An owner writes them on the agent's
// registry row; nothing classifies the data a run reads. Narrowing on them
// stops a delegation chain from reaching an agent whose owner declared a
// class the root agent does not declare. It does not inspect content.
package workerdelegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// SettingsReader reads the platform `delegation` section (in production a
// platformconfig.CachedSection the admin save drops).
type SettingsReader interface {
	Get(ctx context.Context) (platformconfig.DelegationSettings, error)
}

// InputReader returns the content of the named entries of an input bundle,
// by entry id. An entry it does not return is absent.
type InputReader interface {
	InputEntries(ctx context.Context, inputBundleID string, entryIDs []string) (map[string][]byte, error)
}

// DataClassReader reads the declared data classes of one agent's registry
// row. No row (or no registry table) is none.
type DataClassReader interface {
	AgentDataClasses(ctx context.Context, projectID, applicationID int64) ([]string, error)
}

// Resolver computes an agent execution command's delegation policy. It is
// the commandbus producers' DelegationPolicyResolver.
type Resolver struct {
	settings SettingsReader
	inputs   InputReader
	classes  DataClassReader
	// principal computes the run principal's bound; nil, or the section's
	// principal_bound switch off, sends none (ADR-0044 Amendment 1).
	principal PrincipalReader
	// grants, edges and grantsEnforced are the delegation grants a run carries
	// (grants.go). Off by default.
	grants         GrantSource
	edges          CompositionEdgeSource
	grantsEnforced bool
}

// NewResolver builds the resolver.
func NewResolver(settings SettingsReader, inputs InputReader, classes DataClassReader) (*Resolver, error) {
	if settings == nil || inputs == nil || classes == nil {
		return nil, errors.New("worker delegation: the settings, input and data class readers are required")
	}
	return &Resolver{settings: settings, inputs: inputs, classes: classes}, nil
}

// WorkerDelegationPolicy answers the command's policy. It is nil only when no
// administrator saved the `delegation` section and neither the grant switch
// (grants.go) nor the principal bound (principal.go) needs one; such a command
// is unchanged byte for byte. Since the delegation migration (epic 2156, S6)
// both are on by default, so an unsaved section sends the compiled-in defaults
// (enforce, depth 4, no data class narrowing) with the bound and the grants.
// A worker from an earlier release rejects the new fields: the chart rolls
// Main out first, and an operator who has such workers running sets the switch
// and the section's principal_bound off for the rollout window (see the
// upgrade note). Only an agent execution command carries one. A governed
// command (one with a security context) is always enforced. An error is a
// source that could not be read; the dispatch is retried.
func (r *Resolver) WorkerDelegationPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerDelegationPolicyV1, error) {
	if command.GetAgentExecution() == nil {
		return nil, nil
	}
	settings, err := r.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the platform setting: %w", err)
	}
	bounded := settings.PrincipalBound && r.principal != nil
	if !settings.Stored && !bounded && (r.grants == nil || !r.grantsEnforced) {
		// Nothing was saved and no switch needs the policy: the command is
		// unchanged and reads nothing.
		return nil, nil
	}
	if command.GetSecurityContextRef() != nil {
		settings.Mode = platformconfig.DelegationEnforce
	}
	input, err := r.input(ctx, command)
	if err != nil {
		return nil, err
	}
	var policy *runtimev1.WorkerDelegationPolicyV1
	switch {
	case settings.Stored:
		classes, err := r.rootDataClasses(ctx, command, input)
		if err != nil {
			return nil, err
		}
		policy = Policy(settings, classes)
	case bounded:
		// Never saved, but the principal bound is on by default (ADR-0044
		// Amendment 1, A1.3): send the compiled-in defaults (enforce, depth 4,
		// no data class narrowing) with the bound. The worker applies the
		// same defaults to a command that carries none.
		defaults := settings
		defaults.Stored = true
		policy = Policy(defaults, nil)
	}
	if bounded {
		if policy.PrincipalBound, err = r.principalBound(ctx, command, input); err != nil {
			return nil, err
		}
	}
	return r.withGrants(ctx, command, input, policy)
}

// Policy is the command field for settings and the root's data classes:
// nil when the section was never saved.
func Policy(settings platformconfig.DelegationSettings, rootDataClasses []string) *runtimev1.WorkerDelegationPolicyV1 {
	if !settings.Stored {
		return nil
	}
	mode := runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_ENFORCE
	if settings.Mode == platformconfig.DelegationReport {
		mode = runtimev1.WorkerDelegationModeV1_WORKER_DELEGATION_MODE_V1_REPORT
	}
	depth := settings.MaxDepth
	if depth < platformconfig.MinDelegationMaxDepth || depth > platformconfig.MaxDelegationMaxDepth {
		depth = platformconfig.DefaultDelegationMaxDepth
	}
	return &runtimev1.WorkerDelegationPolicyV1{
		MaxDepth:        uint32(depth), //nolint:gosec // bounded to 1..16 above
		Mode:            mode,
		RootDataClasses: append([]string(nil), rootDataClasses...),
	}
}

// rootDataClasses reads the declared data classes of the run's root agent:
// the saved agent the command's input names, in the command's project. An
// ad hoc conversation names none.
func (r *Resolver) rootDataClasses(ctx context.Context, command *runtimev1.WorkerCommandV1, input []byte) ([]string, error) {
	applicationID := ApplicationID(input)
	if applicationID <= 0 {
		return nil, nil
	}
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if err != nil || projectID <= 0 {
		return nil, nil
	}
	stored, err := r.classes.AgentDataClasses(ctx, projectID, applicationID)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the root agent's data classes: %w", err)
	}
	classes, err := agentlifecycle.NormalizeDataClasses(stored)
	if err != nil {
		// The registry writes only normalised classes; a row that is not is
		// never read as "no class", which would lift the constraint.
		return nil, fmt.Errorf("worker delegation: the root agent's data classes: %w", err)
	}
	return classes, nil
}

// input reads the command's agent execution input: nothing for a command
// that names none.
func (r *Resolver) input(ctx context.Context, command *runtimev1.WorkerCommandV1) ([]byte, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if bundle == "" || entryID == "" {
		return nil, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the command's input: %w", err)
	}
	return contents[entryID], nil
}

// ApplicationID is the saved agent id an agent execution input names in its
// `application.id`, or 0.
func ApplicationID(content []byte) int64 {
	if len(content) == 0 {
		return 0
	}
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil || len(input.GetApplication()) == 0 {
		return 0
	}
	var application struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
		return 0
	}
	id, err := application.ID.Int64()
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// SavedSectionDataClasses reads a child's declared data classes only once an
// administrator saved the `delegation` section, and none before. A served
// child version then carries `data_classes` only when the workers can read
// it (see DelegationSettings.Stored).
type SavedSectionDataClasses struct {
	settings SettingsReader
	classes  DataClassReader
}

// NewSavedSectionDataClasses builds the gated reader.
func NewSavedSectionDataClasses(settings SettingsReader, classes DataClassReader) (*SavedSectionDataClasses, error) {
	if settings == nil || classes == nil {
		return nil, errors.New("worker delegation: the settings and data class readers are required")
	}
	return &SavedSectionDataClasses{settings: settings, classes: classes}, nil
}

// AgentDataClasses answers the registry row's classes, or none while the
// section was never saved.
func (g *SavedSectionDataClasses) AgentDataClasses(ctx context.Context, projectID, applicationID int64) ([]string, error) {
	settings, err := g.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the platform setting: %w", err)
	}
	if !settings.Stored {
		return nil, nil
	}
	return g.classes.AgentDataClasses(ctx, projectID, applicationID)
}
