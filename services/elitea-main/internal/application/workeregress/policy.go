// Package workeregress assembles the public egress policy Main signs into
// every worker command (ADR-0044 decision 2, #1490;
// WorkerCommandV1.egress_policy).
//
// # The effective mode
//
// `allowlist` when the platform's `worker_egress` mode is `allowlist`, or when
// the command is governed (it carries security_context_ref: the governed
// credential profile forces the allowlist). Otherwise `open`, and the policy
// is OMITTED, so the command's bytes are exactly what they were before the
// field existed.
//
// # The entries
//
// The union of:
//
//   - the platform's entries (platform_config `worker_egress.allowlist`);
//   - the project's entries (its `worker_egress` project setting);
//   - the host of every URL a toolkit, MCP server or configuration in the
//     command is configured with (HostsFromSettings): the frozen toolkit set of
//     an agent run, the toolkit of a direct read, a tool call or a tool
//     discovery, the toolkit an index ingest reads, the configuration a
//     validation checks.
//
// Only the toolkits' SETTINGS are read, never user input, chat history or tool
// arguments, so nothing a caller types can widen a run's allowlist.
//
// # Fail closed
//
// A malformed platform or project entry, an unreadable setting, or an
// assembled list longer than MaxEntries (or larger than MaxBytes) refuses the
// command with ErrPolicyRefused. It is never truncated: a truncated allowlist
// would refuse hosts nobody chose to refuse. A toolkit URL whose host does not
// parse adds nothing, which narrows the run, never widens it.
package workeregress

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// ErrPolicyRefused refuses a command whose policy cannot be assembled.
var ErrPolicyRefused = errors.New("worker egress policy refused")

// Bounds of an assembled policy. MaxEntries is the command contract's; MaxBytes
// keeps the policy inside the 32 KiB worker command with room for the rest of
// it.
const (
	MaxEntries = platformconfig.MaxWorkerEgressPolicyEntries
	MaxBytes   = 24 * 1024
)

// PlatformReader reads the platform section.
type PlatformReader interface {
	WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error)
}

// ProjectReader reads a project's own entries (its `worker_egress` project
// setting); none is an empty list.
type ProjectReader interface {
	WorkerEgressProjectEntries(ctx context.Context, projectID int64) ([]string, error)
}

// InputReader returns the content of the named entries of an input bundle,
// by entry id. An entry it does not return is treated as absent.
type InputReader interface {
	InputEntries(ctx context.Context, inputBundleID string, entryIDs []string) (map[string][]byte, error)
}

// Resolver computes a command's policy. It is the commandbus producers'
// EgressPolicyResolver.
type Resolver struct {
	platform PlatformReader
	project  ProjectReader
	inputs   InputReader
}

// NewResolver builds the resolver.
func NewResolver(platform PlatformReader, project ProjectReader, inputs InputReader) (*Resolver, error) {
	if platform == nil || project == nil || inputs == nil {
		return nil, errors.New("worker egress: the platform, project and input readers are required")
	}
	return &Resolver{platform: platform, project: project, inputs: inputs}, nil
}

// WorkerEgressPolicy answers the command's policy, or nil when it is open.
func (r *Resolver) WorkerEgressPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error) {
	if command == nil {
		return nil, fmt.Errorf("%w: no command", ErrPolicyRefused)
	}
	settings, err := r.platform.WorkerEgressSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the platform setting is unreadable: %v", ErrPolicyRefused, err)
	}
	governed := command.GetSecurityContextRef() != nil
	if settings.Mode != platformconfig.WorkerEgressAllowlist && !governed {
		return nil, nil
	}
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if err != nil || projectID <= 0 {
		return nil, fmt.Errorf("%w: the command names no project", ErrPolicyRefused)
	}
	project, err := r.project.WorkerEgressProjectEntries(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("%w: the project setting is unreadable: %v", ErrPolicyRefused, err)
	}
	toolkits, err := r.toolkitHosts(ctx, command)
	if err != nil {
		return nil, err
	}
	allow, err := Assemble(settings.Allowlist, project, toolkits)
	if err != nil {
		return nil, err
	}
	return &runtimev1.WorkerEgressPolicyV1{Mode: runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST, Allow: allow}, nil
}

// Assemble merges the three sources into the policy's entries: normalised,
// deduplicated and sorted. A malformed platform or project entry, or a list
// over the bounds, is ErrPolicyRefused.
func Assemble(platform, project, toolkits []string) ([]string, error) {
	seen := map[string]bool{}
	var allow []string
	add := func(source string, entries []string, strict bool) error {
		for _, raw := range entries {
			entry, err := egresslib.ParseEntry(raw)
			if err != nil {
				if strict {
					return fmt.Errorf("%w: a %s entry is malformed: %v", ErrPolicyRefused, source, err)
				}
				continue
			}
			if text := entry.String(); !seen[text] {
				seen[text] = true
				allow = append(allow, text)
			}
		}
		return nil
	}
	if err := add("platform", platform, true); err != nil {
		return nil, err
	}
	if err := add("project", project, true); err != nil {
		return nil, err
	}
	if err := add("toolkit", toolkits, false); err != nil {
		return nil, err
	}
	if len(allow) > MaxEntries {
		return nil, fmt.Errorf("%w: the run's allowlist has %d entries, more than %d; remove platform or project entries",
			ErrPolicyRefused, len(allow), MaxEntries)
	}
	size := 0
	for _, entry := range allow {
		size += len(entry) + 3 // the field tag and length of each repeated string
	}
	if size > MaxBytes {
		return nil, fmt.Errorf("%w: the run's allowlist is %d bytes, more than %d; remove platform or project entries",
			ErrPolicyRefused, size, MaxBytes)
	}
	sort.Strings(allow)
	if allow == nil {
		allow = []string{}
	}
	return allow, nil
}

// toolkitHosts reads the hosts of the toolkits and configurations the command
// names, from its input bundle.
func (r *Resolver) toolkitHosts(ctx context.Context, command *runtimev1.WorkerCommandV1) ([]string, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	var entryID string
	decode := HostsFromSettings
	switch {
	case command.GetAgentExecution() != nil:
		entryID, decode = command.GetAgentExecution().GetRequestEntryId(), HostsFromAgentInput
	case command.GetToolkitExecuteRead() != nil:
		entryID, decode = command.GetToolkitExecuteRead().GetRequestEntryId(), HostsFromToolkitReadInput
	case command.GetToolkitCallTool() != nil:
		entryID = command.GetToolkitCallTool().GetSettingsEntryId()
	case command.GetToolkitAvailableTools() != nil:
		entryID = command.GetToolkitAvailableTools().GetSettingsEntryId()
	case command.GetIndexIngest() != nil:
		entryID = command.GetIndexIngest().GetToolkitConfigurationEntryId()
	case command.GetConfigurationValidation() != nil:
		entryID = command.GetConfigurationValidation().GetSettingsEntryId()
	default:
		return nil, nil
	}
	if bundle == "" || entryID == "" {
		return nil, fmt.Errorf("%w: the command names no input", ErrPolicyRefused)
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, fmt.Errorf("%w: the command's input is unreadable: %v", ErrPolicyRefused, err)
	}
	content, ok := contents[entryID]
	if !ok {
		return nil, fmt.Errorf("%w: the command's input entry is missing", ErrPolicyRefused)
	}
	hosts, err := decode(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyRefused, err)
	}
	return hosts, nil
}
