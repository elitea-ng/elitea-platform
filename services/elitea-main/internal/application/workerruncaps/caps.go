// Package workerruncaps assembles the run caps an agent execution command
// carries (ADR-0041 decision 3, #1498), checks an author's per-agent caps,
// and grants the cap override.
//
// The worker counts tool calls, model turns and records read; Main counts
// spend and wall time. The caps of one run are the root agent's tier caps
// (platform configuration section `run_caps`) lowered by the caps its author
// set on the agent version (meta `run_caps`): the lower of the two wins, and
// an author can never raise a cap. An agent's own step_limit still bounds
// each agent loop; with max_model_turns the lower of the two stops first.
//
// Main writes the effective caps on the execution (execution_jobs.run_caps)
// when it prepares the command, consuming an open override grant of the
// conversation on the way. It sends WorkerCommandV1.run_caps (field 69) only
// once an administrator saved a per-tier key, so a worker of an earlier
// release keeps accepting commands during the Main-first rollout.
package workerruncaps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerapprovalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// MetaKey is the agent version meta key of the author's caps.
const MetaKey = "run_caps"

// SettingsReader reads the platform `run_caps` section (in production a
// platformconfig.CachedSection the admin save drops).
type SettingsReader interface {
	Get(ctx context.Context) (platformconfig.RunCaps, error)
}

// Binder records the effective caps on the execution and answers the caps
// the run gets: the first record of the execution wins (a redispatch keeps
// it), and an open override grant of the conversation raises its cap once.
// (*repos.RunCapsRepository).
type Binder interface {
	BindRunCaps(ctx context.Context, binding Binding) (platformconfig.TierRunCaps, error)
}

// Binding is one execution's caps before the grant.
type Binding struct {
	ExecutionID string
	Generation  uint64
	ProjectID   int64
	Caps        platformconfig.TierRunCaps
}

// Resolver computes an agent execution command's run caps. It is the
// commandbus producers' RunCapsResolver.
type Resolver struct {
	settings SettingsReader
	inputs   workerdelegation.InputReader
	tiers    workerapprovalpolicy.TierReader
	binder   Binder
}

// NewResolver builds the resolver. binder may be nil (no record, no grant).
func NewResolver(
	settings SettingsReader,
	inputs workerdelegation.InputReader,
	tiers workerapprovalpolicy.TierReader,
	binder Binder,
) (*Resolver, error) {
	if settings == nil || inputs == nil || tiers == nil {
		return nil, errors.New("worker run caps: the settings, input and tier readers are required")
	}
	return &Resolver{settings: settings, inputs: inputs, tiers: tiers, binder: binder}, nil
}

// WorkerRunCaps answers the command's caps, or nil when no administrator
// saved a per-tier key: the command is then unchanged byte for byte. Only an
// agent execution command carries caps. An error is a source that could not
// be read; the dispatch is retried.
func (r *Resolver) WorkerRunCaps(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerRunCapsV1, error) {
	if command.GetAgentExecution() == nil {
		return nil, nil
	}
	settings, err := r.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker run caps: read the platform setting: %w", err)
	}
	projectID, _ := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	applicationID, author, err := r.rootAgent(ctx, command)
	if err != nil {
		return nil, err
	}
	tier := agentlifecycle.TierLow
	if applicationID > 0 && projectID > 0 {
		if tier, err = workerapprovalpolicy.ReadTier(ctx, r.tiers, projectID, applicationID); err != nil {
			return nil, fmt.Errorf("worker run caps: %w", err)
		}
	}
	caps := Effective(settings.For(tier), author)
	if r.binder != nil && command.GetExecutionId() != "" && command.GetGeneration() > 0 && projectID > 0 {
		caps, err = r.binder.BindRunCaps(ctx, Binding{
			ExecutionID: command.GetExecutionId(), Generation: command.GetGeneration(),
			ProjectID: projectID, Caps: caps,
		})
		if err != nil {
			return nil, fmt.Errorf("worker run caps: record the run's caps: %w", err)
		}
	}
	if !settings.TierCapsStored {
		return nil, nil
	}
	return Proto(caps), nil
}

// Effective is the run's caps: the tier's, each lowered by the author's.
func Effective(tier, author platformconfig.TierRunCaps) platformconfig.TierRunCaps {
	return tier.Lower(author)
}

// Proto is the command field of caps.
func Proto(caps platformconfig.TierRunCaps) *runtimev1.WorkerRunCapsV1 {
	return &runtimev1.WorkerRunCapsV1{
		MaxToolCalls:   caps.MaxToolCalls,
		MaxModelTurns:  caps.MaxModelTurns,
		MaxRecordsRead: caps.MaxRecordsRead,
	}
}

// rootAgent reads the run's root agent from the command's input: its id and
// its author's caps. An ad hoc conversation names no agent.
func (r *Resolver) rootAgent(ctx context.Context, command *runtimev1.WorkerCommandV1) (int64, platformconfig.TierRunCaps, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	entryID := command.GetAgentExecution().GetRequestEntryId()
	if bundle == "" || entryID == "" {
		return 0, platformconfig.TierRunCaps{}, nil
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return 0, platformconfig.TierRunCaps{}, fmt.Errorf("worker run caps: read the command's input: %w", err)
	}
	content := contents[entryID]
	return workerdelegation.ApplicationID(content), AuthorCapsFromInput(content), nil
}

// AuthorCapsFromInput reads the author's caps from an agent execution input:
// application.version_details.meta.run_caps. None when absent or unreadable.
func AuthorCapsFromInput(content []byte) platformconfig.TierRunCaps {
	if len(content) == 0 {
		return platformconfig.TierRunCaps{}
	}
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil || len(input.GetApplication()) == 0 {
		return platformconfig.TierRunCaps{}
	}
	var application struct {
		VersionDetails struct {
			Meta map[string]any `json:"meta"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
		return platformconfig.TierRunCaps{}
	}
	caps, _ := AuthorCaps(application.VersionDetails.Meta[MetaKey])
	return caps
}

// AuthorCaps reads the author's caps from the value of meta.run_caps. Absent
// or null is none. A key whose value is not a positive cap in range is
// skipped and named in invalid; at run time it sets no cap, and the save
// refuses it (CheckAuthorCaps).
func AuthorCaps(raw any) (platformconfig.TierRunCaps, []string) {
	object, ok := raw.(map[string]any)
	if !ok {
		if raw == nil {
			return platformconfig.TierRunCaps{}, nil
		}
		return platformconfig.TierRunCaps{}, []string{MetaKey}
	}
	var caps platformconfig.TierRunCaps
	var invalid []string
	for key, value := range object {
		if value == nil {
			continue
		}
		number, isNumber := value.(float64)
		if !isNumber || number <= 0 || !platformconfig.ValidRunCap(key, number) {
			invalid = append(invalid, key)
			continue
		}
		switch key {
		case platformconfig.RunCapMaxToolCalls:
			caps.MaxToolCalls = uint32(number)
		case platformconfig.RunCapMaxModelTurns:
			caps.MaxModelTurns = uint32(number)
		case platformconfig.RunCapMaxRecordsRead:
			caps.MaxRecordsRead = uint64(number)
		case platformconfig.RunCapMaxSpend:
			caps.MaxSpend = number
		default:
			invalid = append(invalid, key)
		}
	}
	return caps, invalid
}
