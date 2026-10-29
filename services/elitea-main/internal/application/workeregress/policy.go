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
// A governed run also gets the targets its frozen credential references are
// bound to (GovernedTargets): the target identity and the audience of each,
// the hosts the worker reaches with a redeemed credential, which need not be
// in any setting (a SharePoint credential is used against Microsoft Graph).
//
// # Fail closed
//
// A malformed platform or project entry, an assembled list longer than
// MaxEntries (or larger than MaxBytes), or an input the command names that is
// missing or unreadable is a *RefusalError: it can never succeed, so the
// dispatcher fails the run at once with the refusal's message. A source that
// could not be read is a plain error, and the dispatch is retried within the
// run's deadline. Neither is ever truncated or skipped: a truncated allowlist
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

// ErrPolicyRefused matches every *RefusalError.
var ErrPolicyRefused = errors.New("worker egress policy refused")

// ErrProjectSettingCorrupt is a project `worker_egress` row whose data does
// not decode. A ProjectReader wraps it; the resolver refuses the command
// (re-saving the setting is the fix), where any other read error is retried.
var ErrProjectSettingCorrupt = errors.New("the project worker egress setting does not decode")

// ErrSecurityContextInvalid is a governed command whose stored security
// context is missing or does not match its reference. A GovernedTargetReader
// wraps it; the resolver refuses the command.
var ErrSecurityContextInvalid = errors.New("the run's security context is missing or does not match its reference")

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

// GovernedTargetReader answers the targets a governed run's credentials are
// bound to: the target identity and the audience (canonical URLs) of the
// target binding of every credential reference its security context froze,
// and, for a toolkit frozen at its digest alone (no reference: the broker
// picks the run's one usable grant per operation at redemption), of every
// active `use` grant of the run's agent binding for that toolkit. The
// context is read by the command's reference and its digest is checked.
type GovernedTargetReader interface {
	GovernedEgressTargets(ctx context.Context, reference *runtimev1.ExecutionSecurityContextReferenceV1) ([]string, error)
}

// Resolver computes a command's policy. It is the commandbus producers'
// EgressPolicyResolver.
type Resolver struct {
	platform PlatformReader
	project  ProjectReader
	inputs   InputReader
	governed GovernedTargetReader
}

// NewResolver builds the resolver.
func NewResolver(platform PlatformReader, project ProjectReader, inputs InputReader, governed GovernedTargetReader) (*Resolver, error) {
	if platform == nil || project == nil || inputs == nil || governed == nil {
		return nil, errors.New("worker egress: the platform, project, input and governed target readers are required")
	}
	return &Resolver{platform: platform, project: project, inputs: inputs, governed: governed}, nil
}

// WorkerEgressPolicy answers the command's policy, or nil when it is open. A
// *RefusalError can never succeed; any other error is a source that could
// not be read.
func (r *Resolver) WorkerEgressPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error) {
	if command == nil {
		return nil, refuse("The run has no worker command.")
	}
	settings, err := r.platform.WorkerEgressSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the platform setting: %w", err)
	}
	governed := command.GetSecurityContextRef() != nil
	if settings.Mode != platformconfig.WorkerEgressAllowlist && !governed {
		return nil, nil
	}
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if err != nil || projectID <= 0 {
		return nil, refuse("The run names no project, so its worker egress allowlist cannot be assembled.")
	}
	project, err := r.project.WorkerEgressProjectEntries(ctx, projectID)
	if errors.Is(err, ErrProjectSettingCorrupt) {
		return nil, refuse("The project's worker egress setting cannot be read. %s", projectFixer)
	}
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the project setting: %w", err)
	}
	toolkits, err := r.toolkitHosts(ctx, command)
	if err != nil {
		return nil, err
	}
	if governed {
		targets, err := r.governed.GovernedEgressTargets(ctx, command.GetSecurityContextRef())
		if errors.Is(err, ErrSecurityContextInvalid) {
			return nil, refuse("The run's security context does not match the run, so the credentials it may use are unknown. Start the run again.")
		}
		if err != nil {
			return nil, fmt.Errorf("worker egress: read the governed credential targets: %w", err)
		}
		for _, target := range targets {
			if host, ok := hostOf(target); ok {
				toolkits = append(toolkits, host)
			}
		}
	}
	allow, err := Assemble(settings.Allowlist, project, toolkits)
	if err != nil {
		return nil, err
	}
	return &runtimev1.WorkerEgressPolicyV1{Mode: runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST, Allow: allow}, nil
}

// Assemble merges the three sources into the policy's entries: normalised,
// deduplicated and sorted. A malformed platform or project entry, or a list
// over the bounds, is a *RefusalError naming the entry or the bound.
func Assemble(platform, project, toolkits []string) ([]string, error) {
	seen := map[string]bool{}
	var allow []string
	add := func(entries []string, fixer, owner string) error {
		for _, raw := range entries {
			entry, err := egresslib.ParseEntry(raw)
			if err != nil {
				if fixer != "" {
					return refuse("The %s worker egress allowlist entry %s is not a valid host, host:port, *.domain or CIDR block (%v). %s",
						owner, quoteEntry(raw), err, fixer)
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
	if err := add(platform, platformFixer, "platform's"); err != nil {
		return nil, err
	}
	if err := add(project, projectFixer, "project's"); err != nil {
		return nil, err
	}
	if err := add(toolkits, "", ""); err != nil {
		return nil, err
	}
	if len(allow) > MaxEntries {
		return nil, refuse("This run would need %d worker egress allowlist entries, more than the %d a run may have. %s",
			len(allow), MaxEntries, boundsFixer)
	}
	size := 0
	for _, entry := range allow {
		size += len(entry) + 3 // the field tag and length of each repeated string
	}
	if size > MaxBytes {
		return nil, refuse("This run's worker egress allowlist would be %d bytes, more than the %d a run may carry. %s",
			size, MaxBytes, boundsFixer)
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
	// The input bundle is immutable: an entry that is absent or does not
	// decode stays so, and the run is refused. A failed read is retried.
	const inputFixer = "Start the run again; if it fails the same way, report it."
	if bundle == "" || entryID == "" {
		return nil, refuse("The run's worker command names no toolkit input, so its worker egress allowlist cannot be assembled. %s", inputFixer)
	}
	contents, err := r.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the command's input: %w", err)
	}
	content, ok := contents[entryID]
	if !ok {
		return nil, refuse("The run's toolkit input is missing, so its worker egress allowlist cannot be assembled. %s", inputFixer)
	}
	hosts, err := decode(content)
	if err != nil {
		return nil, refuse("The run's toolkit settings cannot be read (%v), so its worker egress allowlist cannot be assembled. %s", err, inputFixer)
	}
	return hosts, nil
}
