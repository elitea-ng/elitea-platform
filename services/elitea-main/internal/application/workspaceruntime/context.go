package workspaceruntime

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
)

// RuntimeContextField is the accept-list name a worker sends in
// X-Elitea-Runtime-Context-Accept to receive the `workspace` field.
const RuntimeContextField = "workspace"

// ErrCloudExecutorDisabled: the effective policy recomputed at this claim
// no longer allows the cloud executor (the platform or the project turned it
// off after the handoff). The redemption is refused, so the turn fails before
// any tool runs.
var ErrCloudExecutorDisabled = errors.New("the cloud executor is disabled for this project")

// ErrNotCloudTurn: the claimed execution belongs to a handed-off thread, but
// the cloud executor does not own the thread for THIS turn now (another cloud
// turn runs, a take back or an erase moved it on, or the thread changed hands
// after the turn was admitted). The redemption is refused like
// ErrCloudExecutorDisabled: such a turn must not run outside the workspace.
var ErrNotCloudTurn = errors.New("the thread is not owned by the cloud executor for this turn")

// TurnClaim is the claimed execution as the runtime-context authorizer
// resolved it.
type TurnClaim struct {
	ExecutionID string
	Generation  uint64
	ProjectID   int64
	ActorID     string
	// ConversationUUID is the claimed agent execution's conversation
	// (empty for a claim that has none).
	ConversationUUID string
}

// Turn is the handoff state of the claimed execution's thread, read at claim
// (redemption) time.
type Turn struct {
	ThreadUUID string
	// Epoch is the CLOUD_RUNNING epoch this turn runs under.
	Epoch uint64
	// CloudSHA is refs/elitea/cloud now (the publish's expected old value);
	// nil before the first cloud turn.
	CloudSHA *string
	// HasUpstream: the thread carries refs/elitea/upstream.
	HasUpstream bool
	// UnpackedBytes is the unpacked tree size the desktop declared at send;
	// the worker sizes the disk-backed workspace volume from it.
	UnpackedBytes uint64
	// EnvSecret: the begin stored harvested env for this thread. The values
	// travel only over WorkspaceEnvSecret, never in this field.
	EnvSecret bool
	// Rewind: this execution regenerates the newest cloud turn (ADR-0034
	// decision 7; the server allows only the newest). CloudSHA is that
	// turn's commit; the worker moves refs/elitea/cloud back to RewindTo
	// (the commit that turn started from: its parent, or further back when
	// the turn paused for input and published twice) through the normal
	// publish compare-and-set before the turn runs.
	Rewind   bool
	RewindTo *string
}

// TurnResolver reads the claimed execution's handoff state.
//
// ResolveCloudTurn is called at claim (the runtime-context redemption). For a
// turn of a handed-off thread it STARTS the cloud turn (CLOUD_IDLE →
// CLOUD_RUNNING, the answer stamped as the cloud executor's) and answers it;
// a repeated redemption of the same claim answers the same turn. It returns
// false (and no error) when the conversation is not a handed-off thread, and
// ErrNotCloudTurn or ErrCloudExecutorDisabled when it is one the turn may not
// run in.
//
// HandedOff answers, without changing anything, whether the claimed
// execution's conversation is a handed-off thread (a handoff row in any
// state but LOCAL): a worker that cannot run a workspace turn is refused it.
type TurnResolver interface {
	ResolveCloudTurn(ctx context.Context, claim TurnClaim) (Turn, bool, error)
	HandedOff(ctx context.Context, claim TurnClaim) (bool, error)
}

// TurnLifecycle ends cloud turns. FinishExecution runs after an execution's
// result is projected and after its settlement commits (completion, failure,
// cancellation, a pause for input): when executionID is a thread's running
// cloud turn, the thread goes back to CLOUD_IDLE (or to RECLAIMING when a
// take back waits). It is idempotent: an execution that is no running cloud
// turn is a no-op. ReconcileFinished does the same for every running cloud
// turn whose execution is already terminal (a hook that never ran, e.g. a
// crash between the commit and the hook).
type TurnLifecycle interface {
	FinishExecution(ctx context.Context, executionID string) error
	ReconcileFinished(ctx context.Context) (int, error)
}

// Policy is the effective cloud-executor policy (decision 11): the platform
// ceiling narrowed by the project setting and the device switch.
type Policy struct {
	Enabled          bool
	Shell            bool
	Network          bool
	NetworkAllowlist []string
	MaxSegmentBytes  uint64
	MaxFileBytes     uint64
	MaxFiles         uint64
	MaxUnpackedBytes uint64
	// Chart-provided limits; zero leaves the worker's deployment default.
	TurnWallSeconds uint64
	CPUMillis       uint64
	MemoryBytes     uint64
	DiskBytes       uint64
}

// PolicyEvaluator computes the effective policy. It is called at every
// cloud claim, so turning the network off applies from the next turn.
type PolicyEvaluator interface {
	EffectiveCloudPolicy(ctx context.Context, projectID int64, threadUUID string) (Policy, error)
}

// RuntimeContextLimits is the `limits` object of the `workspace` field.
type RuntimeContextLimits struct {
	MaxSegmentBytes  uint64 `json:"max_segment_bytes"`
	MaxFileBytes     uint64 `json:"max_file_bytes"`
	MaxFiles         uint64 `json:"max_files"`
	MaxUnpackedBytes uint64 `json:"max_unpacked_bytes"`
	UnpackedBytes    uint64 `json:"unpacked_bytes"`
	TurnWallSeconds  uint64 `json:"turn_wall_seconds"`
	CPUMillis        uint64 `json:"cpu_millis"`
	MemoryBytes      uint64 `json:"memory_bytes"`
	DiskBytes        uint64 `json:"disk_bytes"`
}

// RuntimeContext is the runtime-context `workspace` field (contract §4).
// Every key is always present: the worker parses it strictly.
type RuntimeContext struct {
	Thread           string               `json:"thread"`
	Epoch            uint64               `json:"epoch"`
	ExpectedOldCloud *string              `json:"expected_old_cloud"`
	Limits           RuntimeContextLimits `json:"limits"`
	Network          bool                 `json:"network"`
	Shell            bool                 `json:"shell"`
	NetworkAllowlist []string             `json:"network_allowlist"`
	HasUpstream      bool                 `json:"has_upstream"`
	// EnvSecret tells the worker to redeem the thread's env over
	// WorkspaceEnvSecret (contract 6.4). The values are never in this field.
	EnvSecret bool `json:"env_secret"`
	// Rewind: regenerate the newest cloud turn: move refs/elitea/cloud from
	// ExpectedOldCloud back to RewindTo first (decision 7). RewindTo is null
	// unless Rewind is set; it is a first-parent ancestor of ExpectedOldCloud.
	Rewind   bool    `json:"rewind"`
	RewindTo *string `json:"rewind_to"`
}

// ContextResolver builds the `workspace` field for one claimed execution.
type ContextResolver struct {
	turns    TurnResolver
	policies PolicyEvaluator
}

// NewContextResolver returns nil when either dependency is nil: the
// composition root then serves no `workspace` field.
func NewContextResolver(turns TurnResolver, policies PolicyEvaluator) *ContextResolver {
	if turns == nil || policies == nil {
		return nil
	}
	return &ContextResolver{turns: turns, policies: policies}
}

const maxAllowlistHosts = 256

// ResolveWorkspaceContext returns nil (and no error) for an execution that
// is not a cloud turn. The policy is recomputed here, at the claim.
func (r *ContextResolver) ResolveWorkspaceContext(ctx context.Context, claim TurnClaim) (*RuntimeContext, error) {
	if r == nil {
		return nil, nil
	}
	if claim.ExecutionID == "" || claim.Generation == 0 || claim.ProjectID <= 0 {
		return nil, ErrInvalidRequest
	}
	turn, ok, err := r.turns.ResolveCloudTurn(ctx, claim)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if !ok {
		return nil, nil
	}
	if !ValidThreadUUID(turn.ThreadUUID) || turn.Epoch == 0 || turn.Epoch > math.MaxInt64 ||
		(turn.CloudSHA != nil && !ValidSHA1(*turn.CloudSHA)) || !validRewind(turn) {
		return nil, ErrInvalidRequest
	}
	policy, err := r.policies.EffectiveCloudPolicy(ctx, claim.ProjectID, turn.ThreadUUID)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if !policy.Enabled {
		return nil, ErrCloudExecutorDisabled
	}
	allowlist := make([]string, 0, len(policy.NetworkAllowlist))
	for _, host := range policy.NetworkAllowlist {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\@:\x00") {
			return nil, ErrInvalidRequest
		}
		if !slices.Contains(allowlist, host) {
			allowlist = append(allowlist, host)
		}
	}
	if len(allowlist) > maxAllowlistHosts {
		return nil, ErrInvalidRequest
	}
	slices.Sort(allowlist)
	var expected, rewindTo *string
	if turn.CloudSHA != nil {
		value := *turn.CloudSHA
		expected = &value
	}
	if turn.Rewind {
		value := *turn.RewindTo
		rewindTo = &value
	}
	return &RuntimeContext{
		Thread:           turn.ThreadUUID,
		Epoch:            turn.Epoch,
		ExpectedOldCloud: expected,
		Limits: RuntimeContextLimits{
			MaxSegmentBytes:  policy.MaxSegmentBytes,
			MaxFileBytes:     policy.MaxFileBytes,
			MaxFiles:         policy.MaxFiles,
			MaxUnpackedBytes: policy.MaxUnpackedBytes,
			UnpackedBytes:    turn.UnpackedBytes,
			TurnWallSeconds:  policy.TurnWallSeconds,
			CPUMillis:        policy.CPUMillis,
			MemoryBytes:      policy.MemoryBytes,
			DiskBytes:        policy.DiskBytes,
		},
		// Network off is a mode, not a list: an allowlist without network
		// is meaningless and is not served.
		Network:          policy.Network,
		Shell:            policy.Shell,
		NetworkAllowlist: allowlistIf(policy.Network, allowlist),
		HasUpstream:      turn.HasUpstream,
		EnvSecret:        turn.EnvSecret,
		Rewind:           turn.Rewind,
		RewindTo:         rewindTo,
	}, nil
}

// validRewind: a rewind names the cloud commit it moves from and a different
// valid commit it moves to; no rewind names neither.
func validRewind(turn Turn) bool {
	if !turn.Rewind {
		return turn.RewindTo == nil
	}
	return turn.CloudSHA != nil && turn.RewindTo != nil && ValidSHA1(*turn.RewindTo) && *turn.RewindTo != *turn.CloudSHA
}

// RefusesUnaccepted reports whether a worker that did not accept the
// `workspace` field must be refused this claim: the claimed execution is a
// turn of a handed-off thread, which may run only in its workspace.
func (r *ContextResolver) RefusesUnaccepted(ctx context.Context, claim TurnClaim) (bool, error) {
	if r == nil {
		return false, nil
	}
	handedOff, err := r.turns.HandedOff(ctx, claim)
	if err != nil {
		return false, errors.Join(ErrUnavailable, err)
	}
	return handedOff, nil
}

func allowlistIf(network bool, allowlist []string) []string {
	if !network {
		return []string{}
	}
	return allowlist
}
