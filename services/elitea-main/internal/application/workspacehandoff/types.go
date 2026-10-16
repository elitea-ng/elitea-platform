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
//	CLOUD_* / RECLAIMING / HANDING_OFF --erase--------------> LOCAL
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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
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
	// IncludedExcluded and EnvNames (decision 5, amended): the explicit
	// includes and the harvested variable names. EnvSecretName names the
	// thread's hidden vault secret, "" when it has none; it is never sent to a
	// client.
	IncludedExcluded []string
	EnvNames         []string
	EnvSecretName    string
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
}

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
