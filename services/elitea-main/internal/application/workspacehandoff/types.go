// Package workspacehandoff is the ownership state machine of a desktop ⇄
// cloud handoff (ADR-0034 decision 7).
//
// Exactly one executor, the desktop or the cloud, owns a Local work thread at
// a time. PostgreSQL is the authority: the thread's
// elitea_runtime.workspace_handoffs row holds the state and an epoch, and
// every ownership change is one `UPDATE ... WHERE state = $from AND epoch =
// $epoch`, so two devices racing on a send get exactly one winner. The ref
// compare-and-set of the workspace store (internal/application/workspacestore)
// is the second fence.
//
//	LOCAL         --send (desktop)-------------------------> HANDING_OFF
//	HANDING_OFF   --complete (desktop; refs verified)-------> CLOUD_IDLE
//	HANDING_OFF   --cancel, or 15 min without complete------> LOCAL (objects erased)
//	CLOUD_IDLE    --cloud turn start------------------------> CLOUD_RUNNING
//	CLOUD_RUNNING --turn end / cancel / pause for input-----> CLOUD_IDLE
//	CLOUD_IDLE    --take back (desktop)---------------------> RECLAIMING
//	CLOUD_RUNNING --take back (desktop)---------------------> reclaim_pending; RECLAIMING at turn end or after 60 s
//	RECLAIMING    --release (desktop)-----------------------> LOCAL
//	RECLAIMING    --abandoned for 7 days--------------------> CLOUD_IDLE
//	LOCAL (released, turns not taken) --take back the rest---> RECLAIMING (rest)
//	RECLAIMING (rest) --release (desktop)--------------------> LOCAL
//	RECLAIMING (rest) --abandoned for 1 hour-----------------> LOCAL (released, rest kept)
//	RECLAIMING (rest) --return grace over--------------------> LOCAL (then erased)
//	CLOUD_* / RECLAIMING / HANDING_OFF --erase--------------> LOCAL
//
// Take back up to turn k (§4.6): a release that applied only some of the
// cloud turns sends turns_not_taken, and the released LOCAL row is
// rest_pending. Within the return grace the owner's desktop may take the
// rest back: LOCAL → RECLAIMING (epoch + 1), keeping rest_pending and the
// release's delete_after. A rest RECLAIMING row never returns to CLOUD_IDLE:
// when the grace runs out the sweeper returns it to LOCAL (released), and the
// erasure removes the cloud copy. A rest take back nobody released within
// RestReclaimAbandonedAfter (the desktop gave up, crashed or lost its record)
// goes back to LOCAL, released and still rest_pending (epoch + 1), so the
// folder is writable again and the rest can be asked for once more while the
// grace lasts. Every other transition clears rest_pending.
//
// The epoch increments on every ownership change. A cloud turn's
// CLOUD_IDLE ⇄ CLOUD_RUNNING keeps it: the turn is fenced by its execution id,
// and its runtime context carries the epoch of the CLOUD_IDLE state (wire
// contract §4).
//
// The REST routes (internal/api/v2/workspacehandoff) and the later worker
// gRPC adapter call the same Service methods.
package workspacehandoff

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// Timings of the state machine (decision 7).
const (
	// HandingOffTTL: a send that is not completed within it falls back to LOCAL.
	HandingOffTTL = 15 * time.Minute
	// ReclaimGrace: a running cloud turn may publish this long after a take
	// back; then it is cancelled and the thread becomes RECLAIMING.
	ReclaimGrace = 60 * time.Second
	// ReclaimAbandonedAfter: a RECLAIMING thread nobody released returns to
	// CLOUD_IDLE, so it never waits on an offline laptop.
	ReclaimAbandonedAfter = 7 * 24 * time.Hour
	// RestReclaimAbandonedAfter: a rest take back nobody released returns to
	// LOCAL (released, the rest still pending) after this long. It is short:
	// the thread is read-only while RECLAIMING, and the cloud copy stays
	// within its return grace either way.
	RestReclaimAbandonedAfter = time.Hour
	// MaxNoteBytes bounds the handoff note.
	MaxNoteBytes = 8 * 1024
)

// History modes (decision 6).
const (
	HistoryNone = "none"
	HistoryFull = "full"
)

// Release outcomes (contract §2.8).
const (
	OutcomeApplied   = "applied"
	OutcomeWorktree  = "worktree"
	OutcomeDiscarded = "discarded"
)

// ExecutedByCloud is the `executed_by` meta value of a cloud turn.
const ExecutedByCloud = "cloud_executor"

// Thread is a Local work thread.
type Thread = workspacestore.Thread

// State is the ownership state.
type State = workspacestore.State

// Handoff is a thread's handoff row.
type Handoff struct {
	ProjectID          int64
	ConversationUUID   string
	HandoffID          string
	State              State
	Epoch              int64
	ReclaimPending     bool
	ReclaimRequestedAt *time.Time
	OwnerUserID        int64
	DeviceID           string
	DeviceName         string
	HistoryMode        string
	HasUpstream        bool
	BaseSHA            *string
	UpstreamSHA        *string
	HandoffSHA         *string
	CloudSHA           *string
	Stats              json.RawMessage
	Excluded           []string
	LastSeq            int64
	RunningExecutionID string
	RunningResponseID  string
	ExpiresAt          *time.Time
	StateChangedAt     time.Time
	UpdatedAt          time.Time
	DeleteAfter        *time.Time
	DeletedAt          *time.Time
	// Released is TRUE when the row came to LOCAL by a release. RestPending
	// is TRUE on a released LOCAL row whose release left cloud turns not
	// taken, and on the RECLAIMING row of the rest take back that fetches
	// them (shared/0176).
	Released    bool
	RestPending bool
	// ReturnGraceOpen: delete_after is still ahead by the database clock
	// (the clock that stamped it, and that ReclaimRest compares with).
	ReturnGraceOpen bool
	// IncludedExcluded and EnvNames (decision 5, amended): the explicit
	// includes and the harvested variable names. EnvSecretName names the
	// thread's hidden vault secret, "" when it has none; it is never sent to a
	// client.
	IncludedExcluded []string
	EnvNames         []string
	EnvSecretName    string
	// RewindTo is set only on the answer of StartCloudTurn: the turn is a
	// regenerate of the newest cloud turn, and the worker moves
	// refs/elitea/cloud from CloudSHA back to this commit before it runs.
	RewindTo *string
}

// EnvSecret reports whether the thread has harvested variables (the runtime
// context's `env_secret`).
func (h Handoff) EnvSecret() bool { return h.EnvSecretName != "" }

// Stats is what the desktop declares at send (contract §2.1).
type Stats struct {
	Files         int64    `json:"files"`
	Bytes         int64    `json:"bytes"`
	UnpackedBytes int64    `json:"unpacked_bytes"`
	Excluded      []string `json:"excluded"`
	ScanFindings  int64    `json:"scan_findings"`
}

// Policy is the effective cloud executor policy of a project: the platform
// ceiling narrowed by the project setting and the device switch (decision 11).
type Policy struct {
	Enabled            bool
	Shell              bool
	Network            bool
	NetworkAllowlist   []string
	HistoryFullAllowed bool
	Limits             workspacestore.Limits
	IdleRetention      time.Duration
	ReturnGrace        time.Duration
	// Amendment 2026-10-15 (decision 5). Exclusions are the platform
	// patterns then the project additions; PathDeny is the effective
	// local_work_path_deny, which nothing can include.
	Exclusions []string
	// ExclusionsPlatform and ExclusionsProject split Exclusions by origin:
	// the platform ceiling's patterns, and the project's additions that the
	// platform does not already list. The desktop shows the second as
	// "added by this project".
	ExclusionsPlatform []string
	ExclusionsProject  []string
	PathDeny           []string
	AllowUserInclude   bool
	EnvHarvest         bool
	EnvDeny            []string
	// EnvAllow, when not nil, is the only list of names that can be harvested.
	EnvAllow *[]string
	// DisabledReason is why Enabled is false (one of the Disabled* values),
	// empty when it is true. Clients show one message per reason instead of
	// reading every refusal as the organisation's policy.
	DisabledReason string
}

// Why the effective policy turns the cloud executor off, in the order they
// are reported when several apply: the deliberate switches first (platform,
// project, device), then what the deployment lacks.
const (
	// DisabledPlatformOff: the platform ceiling (cloud_executor.enabled) is off.
	DisabledPlatformOff = "platform_off"
	// DisabledProjectOff: the project's cloud_executor setting turns it off,
	// or the project set nothing and the platform's project default
	// (cloud_executor.project_default_enabled) is off.
	DisabledProjectOff = "project_off"
	// DisabledDeviceOff: the native client policy (local_work.allowed or
	// local_work_cloud_handoff) does not allow handoff.
	DisabledDeviceOff = "device_off"
	// DisabledNoExecutor: the deployment runs no cloud executor (no
	// workspace-run supervisor), so a handed-off turn could never run.
	DisabledNoExecutor = "no_executor"
	// DisabledStorageEncryptionUnconfirmed: the startup check could not
	// confirm the object store's default encryption at rest (decision 4).
	DisabledStorageEncryptionUnconfirmed = "storage_encryption_unconfirmed"
)

// DisabledError is ErrDisabled with its reason; errors.Is(err, ErrDisabled)
// holds for it.
type DisabledError struct{ Reason string }

func (e *DisabledError) Error() string {
	if e.Reason == "" {
		return ErrDisabled.Error()
	}
	return ErrDisabled.Error() + ": " + e.Reason
}

// Is makes a DisabledError match ErrDisabled.
func (e *DisabledError) Is(target error) bool { return target == ErrDisabled }

// disabled answers the refusal of a policy that is off.
func disabled(policy Policy) error { return &DisabledError{Reason: policy.DisabledReason} }

// PolicySource answers the effective policy, recomputed on every call.
type PolicySource interface {
	Effective(ctx context.Context, projectID int64) (Policy, error)
}

// Errors. The routes map each to its contract code.
var (
	ErrInvalid = errors.New("invalid handoff request")
	// ErrNotFound: no such thread or handoff for this caller (an absent and a
	// foreign thread are one answer).
	ErrNotFound = errors.New("handoff not found")
	// ErrNotLocalWork: the conversation is not a Local work thread.
	ErrNotLocalWork = errors.New("conversation is not a Local work thread")
	// ErrDisabled: the effective policy does not allow the cloud executor.
	ErrDisabled = errors.New("cloud handoff is disabled")
	// ErrExecutorIsCloud: the cloud executor owns the thread.
	ErrExecutorIsCloud = errors.New("thread executor is the cloud")
	// ErrInProgress: a handoff, a take back or an erasure is already under way.
	ErrInProgress = errors.New("handoff in progress")
	// ErrLocalTurnInProgress: a local turn of the thread is not committed.
	ErrLocalTurnInProgress = errors.New("local turn in progress")
	// ErrQuotaExceeded: the project's workspace copies would pass the ceiling.
	ErrQuotaExceeded = errors.New("workspace quota exceeded")
	// ErrHistoryModeNotAllowed: history `full` is not allowed for the project.
	ErrHistoryModeNotAllowed = errors.New("history mode not allowed")
	// ErrOverLimits: the declared send is over a cap.
	ErrOverLimits = errors.New("handoff over limits")
	// ErrStaleEpoch: the state or the epoch no longer matches the request.
	ErrStaleEpoch = errors.New("stale handoff epoch")
	// ErrErased: a release (or its retry) for a thread that came back to
	// LOCAL by an erase, not a release. The route answers 409
	// handoff_erased: the cloud copy is gone, so nothing was handed back.
	ErrErased = errors.New("handoff was erased")
	// ErrRefsMissing: complete names a snapshot the store does not hold.
	ErrRefsMissing = errors.New("handoff refs are not published")
	// ErrNotInCloud: a take-back request for a thread the cloud does not own.
	ErrNotInCloud = errors.New("thread is not in the cloud")
	// ErrUnavailable: the policy could not be read.
	ErrUnavailable = errors.New("handoff dependencies are unavailable")
)

// The answer meta keys of a cloud turn (decision 8). The desktop imports cloud
// turns by them on release: `executed_by` = ExecutedByCloud, the turn's
// `workspace_epoch` and its `workspace_commit` (the refs/elitea/cloud sha the
// turn published). The regenerate SQL strips the last two.
const (
	MetaExecutedBy      = "executed_by"
	MetaWorkspaceEpoch  = "workspace_epoch"
	MetaWorkspaceCommit = "workspace_commit"
	// MetaWorkspaceBase is the commit the answer's turn started from (the
	// cloud ref at its first claim, the handoff snapshot for the first cloud
	// turn, or the rewind target of a regeneration). A continuation keeps it.
	// workspace_commit != workspace_base_commit means the turn published.
	MetaWorkspaceBase = "workspace_base_commit"
	// MetaRewindFrom and MetaRewindTo are written by a regenerate of the
	// newest cloud turn that published (decision 7): the claim rewinds
	// refs/elitea/cloud from the first to the second while the ref still
	// holds the first. The regenerate strips all three before it marks.
	MetaRewindFrom = "workspace_rewind_from"
	MetaRewindTo   = "workspace_rewind_to"
)

// CloudTurnMeta is the meta a cloud turn's answer group carries. The executor
// unit merges it into the answer's meta when the turn ends. commit is "" when
// the turn published nothing; the key is then omitted.
func CloudTurnMeta(epoch int64, commit string) map[string]any {
	meta := map[string]any{MetaExecutedBy: ExecutedByCloud, MetaWorkspaceEpoch: epoch}
	if commit != "" {
		meta[MetaWorkspaceCommit] = commit
	}
	return meta
}
