package workerdelegation

// The run principal's bound (ADR-0044 Amendment 1, rule 1; #2157).
//
// Delegation narrowing used to take "the toolkits attached to the caller" as
// "what the caller may do". The outer bound is the run principal: the acting
// user, or the mandate and project of an unattended run. Main computes what
// that principal may use, for the reachable closure of the root only (never
// the whole project), and puts it on WorkerDelegationPolicyV1.principal_bound.
// The worker intersects every callee with it in both narrowing modes.
//
// What the bound covers today:
//
//   - toolkits (and, per toolkit, tools): a toolkit of the closure is in the
//     bound unless the project's folder access hides it from the acting
//     user (the same restrictive overlay GetCurrentMCPToolkitVisibleToActor
//     reads). Project RBAC grants no per-tool access, so a toolkit carries
//     all its tools;
//   - credential references: in a governed run, the references some active,
//     unexpired ADR-0039 use grant of the project lets a toolkit of the
//     closure use. The broker still decides the exact grant per call;
//   - data classes: a project declares no allowed classes today, so the field
//     stays empty, which is no bound.
//
// The egress allowlist is already on the run (WorkerEgressPolicyV1) and
// needs no second copy here.
//
// The bound is on only while the delegation section's principal_bound switch
// is on (platformconfig.DelegationSettings.PrincipalBound), which is off by
// default: a composition that reaches a toolkit its principal may not use
// keeps running until an administrator turns it on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
)

// The bound's limits, the worker command contract's
// (WorkerPrincipalBoundV1). A bound over them refuses the run; it is never
// truncated, because a truncated allowlist would refuse toolkits nobody
// chose to refuse.
const (
	MaxPrincipalToolkits    = 512
	MaxPrincipalCredentials = 256
	MaxCredentialIDBytes    = 128
	// MaxClosureVersions bounds the saved agents and pipelines the closure
	// walk reads: the Worker builds at most applicationcomposition.
	// MaxApplicationHops of them in one run, so a closure past this is not
	// one a run can use.
	MaxClosureVersions = 4 * applicationcomposition.MaxApplicationHops
)

// ErrPrincipalBoundTooLarge is a closure whose bound exceeds its limits.
// The dispatch is retried within the run's deadline, which retires the run;
// nothing is truncated.
var ErrPrincipalBoundTooLarge = errors.New("worker delegation: the run principal's bound exceeds its limits")

// ErrPrincipalProjectUnknown is a command with no readable project while the
// principal bound is on: it cannot be bounded, so it is not dispatched.
var ErrPrincipalProjectUnknown = errors.New("worker delegation: the command names no project to bound the run's principal in")

// ErrPrincipalInputUnreadable is an input the closure walk cannot read.
var ErrPrincipalInputUnreadable = errors.New("worker delegation: the command's input cannot be read to bound the run's principal")

// Principal is who a run acts for.
type Principal struct {
	// UserID is the acting user (an attended run), or the subject user of
	// the mandate (a delegated background run). 0 for an autonomous mandate
	// and for a command that names no readable user: the bound then holds
	// the project's toolkits with no per-user overlay.
	UserID int64
	// MandateID is the mandate an unattended governed run acts under.
	MandateID string
	// Governed is a run under the credential governance profile.
	Governed bool
	// FrozenCredentialReferences are the references admission froze into the
	// run's security context.
	FrozenCredentialReferences []string
}

// ClosureSeeds are where the closure walk starts: what the command's input
// itself names.
type ClosureSeeds struct {
	// ToolkitIDs are the toolkits the root holds directly (its frozen tools,
	// and an ad hoc conversation's tools).
	ToolkitIDs []int64
	// Versions are the saved agents and pipelines the root is or attaches.
	Versions []applicationcomposition.Identity
}

// PrincipalReader reads what the run principal may use. The production
// reader is repos.WorkerEgressSources.
type PrincipalReader interface {
	// RunPrincipal answers who the command's run acts for.
	RunPrincipal(ctx context.Context, command *runtimev1.WorkerCommandV1) (Principal, error)
	// ReachableToolkits answers every toolkit id the seeds reach: the seeds'
	// own, and those of each saved agent or pipeline reachable through
	// attachments. More than MaxClosureVersions versions is
	// ErrPrincipalBoundTooLarge.
	ReachableToolkits(ctx context.Context, projectID int64, seeds ClosureSeeds) ([]int64, error)
	// HiddenToolkits answers which of toolkitIDs the project's access rules
	// hide from userID. userID 0 hides none.
	HiddenToolkits(ctx context.Context, projectID, userID int64, toolkitIDs []int64) ([]int64, error)
	// UsableCredentialReferences answers the credential references an
	// active, unexpired use grant of the project lets a toolkit of
	// toolkitIDs use.
	UsableCredentialReferences(ctx context.Context, projectID int64, toolkitIDs []int64) ([]string, error)
}

// WithPrincipalBound makes the resolver compute the principal's bound while
// the delegation section's principal_bound switch is on.
func (r *Resolver) WithPrincipalBound(reader PrincipalReader) *Resolver {
	r.principal = reader
	return r
}

// principalBound computes the bound of one command, or nil when it cannot be
// computed for the command (no project): the worker then applies no bound.
func (r *Resolver) principalBound(ctx context.Context, command *runtimev1.WorkerCommandV1, input []byte) (*runtimev1.WorkerPrincipalBoundV1, error) {
	projectID, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 64)
	if err != nil || projectID <= 0 {
		// A run that cannot be bounded must not run unbounded while the
		// switch is on.
		return nil, ErrPrincipalProjectUnknown
	}
	principal, err := r.principal.RunPrincipal(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the run's principal: %w", err)
	}
	seeds, err := SeedsFromInput(input)
	if err != nil {
		return nil, err
	}
	reachable, err := r.principal.ReachableToolkits(ctx, projectID, seeds)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the run's toolkits: %w", err)
	}
	hidden, err := r.principal.HiddenToolkits(ctx, projectID, principal.UserID, reachable)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the principal's toolkit access: %w", err)
	}
	allowed := difference(reachable, hidden)
	var credentials []string
	if principal.Governed {
		credentials, err = r.principal.UsableCredentialReferences(ctx, projectID, allowed)
		if err != nil {
			return nil, fmt.Errorf("worker delegation: read the principal's credential references: %w", err)
		}
		// The references admission froze for the run are usable by
		// construction (it checked them against the grants).
		credentials = append(credentials, principal.FrozenCredentialReferences...)
	}
	return PrincipalBound(allowed, principal.Governed, credentials)
}

// PrincipalBound is the command field for the toolkits the principal may use
// and, when credentialsBounded, the credential references it may use. The
// lists are sorted and de-duplicated, so the signed bytes are deterministic.
// A list over the limits is ErrPrincipalBoundTooLarge.
func PrincipalBound(toolkitIDs []int64, credentialsBounded bool, credentialReferences []string) (*runtimev1.WorkerPrincipalBoundV1, error) {
	ids := slices.Clone(toolkitIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	references := slices.Clone(credentialReferences)
	slices.Sort(references)
	references = slices.Compact(references)
	if len(ids) > MaxPrincipalToolkits || len(references) > MaxPrincipalCredentials {
		return nil, ErrPrincipalBoundTooLarge
	}
	bound := &runtimev1.WorkerPrincipalBoundV1{Bounded: true, CredentialsBounded: credentialsBounded}
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrPrincipalBoundTooLarge
		}
		bound.Toolkits = append(bound.Toolkits, &runtimev1.WorkerPrincipalToolkitV1{
			ToolkitId: uint64(id), //nolint:gosec // positive, checked above
			AllTools:  true,
		})
	}
	if credentialsBounded {
		for _, reference := range references {
			if len(reference) > MaxCredentialIDBytes {
				return nil, ErrPrincipalBoundTooLarge
			}
			bound.CredentialReferenceIds = append(bound.CredentialReferenceIds, reference)
		}
	}
	return bound, nil
}

// difference is the ids of a not in b.
func difference(a, b []int64) []int64 {
	drop := make(map[int64]bool, len(b))
	for _, id := range b {
		drop[id] = true
	}
	out := make([]int64, 0, len(a))
	for _, id := range a {
		if !drop[id] {
			out = append(out, id)
		}
	}
	return out
}

// SeedsFromInput reads where the closure walk starts from an agent
// execution input: the root saved agent (application.id and version_id), the
// toolkits and attached agents of its frozen tools
// (application.version_details.tools) and of the input's own tools (an ad hoc
// conversation's). An input that cannot be decoded is ErrPrincipalInputUnreadable,
// never an empty set: that would read as "nothing allowed" and look like a
// missing access.
func SeedsFromInput(content []byte) (ClosureSeeds, error) {
	var seeds ClosureSeeds
	if len(content) == 0 {
		return seeds, nil
	}
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return ClosureSeeds{}, ErrPrincipalInputUnreadable
	}
	if len(input.GetApplication()) > 0 {
		var application struct {
			ID             json.RawMessage `json:"id"`
			VersionID      json.RawMessage `json:"version_id"`
			VersionDetails struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if err := json.Unmarshal(input.GetApplication(), &application); err == nil {
			applicationID, versionID := jsonInt(application.ID), jsonInt(application.VersionID)
			if applicationID > 0 && versionID > 0 {
				seeds.Versions = append(seeds.Versions, applicationcomposition.Identity{ApplicationID: applicationID, VersionID: versionID})
			}
			if err := seeds.addTools(application.VersionDetails.Tools); err != nil {
				return ClosureSeeds{}, err
			}
		} else {
			return ClosureSeeds{}, ErrPrincipalInputUnreadable
		}
	}
	if err := seeds.addTools(input.GetTools()); err != nil {
		return ClosureSeeds{}, err
	}
	return seeds, nil
}

// addTools collects the toolkit ids and attached versions of a frozen tool
// list.
func (s *ClosureSeeds) addTools(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tools []struct {
		ID       json.RawMessage `json:"id"`
		Type     string          `json:"type"`
		Settings struct {
			ApplicationID        json.RawMessage `json:"application_id"`
			ApplicationVersionID json.RawMessage `json:"application_version_id"`
			VersionID            json.RawMessage `json:"version_id"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return ErrPrincipalInputUnreadable
	}
	for _, tool := range tools {
		if tool.Type == "application" {
			applicationID, versionID := jsonInt(tool.Settings.ApplicationID), jsonInt(tool.Settings.ApplicationVersionID)
			if versionID == 0 {
				versionID = jsonInt(tool.Settings.VersionID)
			}
			if applicationID > 0 && versionID > 0 {
				s.Versions = append(s.Versions, applicationcomposition.Identity{ApplicationID: applicationID, VersionID: versionID})
			}
			continue
		}
		if id := jsonInt(tool.ID); id > 0 {
			s.ToolkitIDs = append(s.ToolkitIDs, id)
		}
	}
	return nil
}

// jsonInt reads an id the stored settings spell as a number or as a quoted
// decimal string (the legacy runtime wrote both), or 0.
func jsonInt(raw json.RawMessage) int64 {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		text = string(raw)
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
