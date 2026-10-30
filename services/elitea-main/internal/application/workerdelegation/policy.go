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
}

// NewResolver builds the resolver.
func NewResolver(settings SettingsReader, inputs InputReader, classes DataClassReader) (*Resolver, error) {
	if settings == nil || inputs == nil || classes == nil {
		return nil, errors.New("worker delegation: the settings, input and data class readers are required")
	}
	return &Resolver{settings: settings, inputs: inputs, classes: classes}, nil
}

// WorkerDelegationPolicy answers the command's policy, or nil when every
// value is the default (depth 4, enforced, no root data class), so such a
// command is unchanged byte for byte. Only an agent execution command carries
// one. A governed command (one with a security context) is always enforced.
// An error is a source that could not be read; the dispatch is retried.
func (r *Resolver) WorkerDelegationPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerDelegationPolicyV1, error) {
	if command.GetAgentExecution() == nil {
		return nil, nil
	}
	settings, err := r.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the platform setting: %w", err)
	}
	if command.GetSecurityContextRef() != nil {
		settings.Mode = platformconfig.DelegationEnforce
	}
	classes, err := r.rootDataClasses(ctx, command)
	if err != nil {
		return nil, err
	}
	return Policy(settings, classes), nil
}

// Policy is the command field for settings and the root's data classes:
// nil when every value is the default.
func Policy(settings platformconfig.DelegationSettings, rootDataClasses []string) *runtimev1.WorkerDelegationPolicyV1 {
	if settings.IsDefault() && len(rootDataClasses) == 0 {
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
func (r *Resolver) rootDataClasses(ctx context.Context, command *runtimev1.WorkerCommandV1) ([]string, error) {
	applicationID, err := r.rootApplication(ctx, command)
	if err != nil || applicationID <= 0 {
		return nil, err
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

// rootApplication reads the saved agent id from the command's agent input,
// or 0 when the input names none (an ad hoc conversation).
func (r *Resolver) rootApplication(ctx context.Context, command *runtimev1.WorkerCommandV1) (int64, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if bundle == "" || entryID == "" {
		return 0, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return 0, fmt.Errorf("worker delegation: read the command's input: %w", err)
	}
	return ApplicationID(contents[entryID]), nil
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
