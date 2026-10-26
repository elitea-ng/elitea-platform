package workspacehandoff

import (
	"context"
	"encoding/json"
	"time"
)

// EventMessage is a system-authored conversation message a transition appends
// in its own transaction (decision 8). It is authored by the tenant schema's
// `system` participant, never by the user's participant, so it does not move
// the memory-recall boundary (repos.MemoriesRepo.ResolveCurrentMemoryRecall
// treats a group authored by the user's participant as a turn).
type EventMessage struct {
	Event string
	Text  string
}

// BeginRecord is a send.
type BeginRecord struct {
	Thread      Thread
	ActorUserID int64
	TokenID     string
	DeviceID    string
	HistoryMode string
	HasUpstream bool
	Stats       json.RawMessage
	Excluded    []string
	Note        string
	TTL         time.Duration
	// MaxProjectWorkspaceBytes and DeclaredBytes are the quota check, made
	// under the thread's lock: live workspace bytes of the project (without
	// this thread's, which the send supersedes) plus DeclaredBytes must stay
	// within the ceiling.
	MaxProjectWorkspaceBytes int64
	DeclaredBytes            int64
	// IncludedExcluded and the environment (decision 5, amended).
	IncludedExcluded []string
	Env              EnvChange
}

// EnvChange is what a send does to the thread's environment secret.
type EnvChange struct {
	// Keep leaves the previous secret (the send carried no `env`).
	Keep bool
	// SecretName is the new secret ("" with Keep false clears it).
	SecretName string
	Names      []string
}

// BeginResult answers a send.
type BeginResult struct {
	Handoff          Handoff
	ProjectBytesUsed int64
	// PreviousEnvSecret is the secret the send replaced or cleared, for the
	// caller to delete from the vault.
	PreviousEnvSecret string
}

// CompleteRecord moves HANDING_OFF to CLOUD_IDLE.
type CompleteRecord struct {
	ProjectID   int64
	HandoffID   string
	ActorUserID int64
	Epoch       int64
	HandoffRef  string
	SHA         string
	Message     EventMessage
}

// ReleaseRecord moves RECLAIMING to LOCAL.
type ReleaseRecord struct {
	Thread      Thread
	ActorUserID int64
	Epoch       int64
	Grace       time.Duration
	// RestPending: the release left cloud turns not taken; the owner's
	// desktop may take them back within the return grace.
	RestPending bool
	Message     EventMessage
}

// EraseRecord ends a handoff and erases the cloud copy.
type EraseRecord struct {
	Thread      Thread
	ActorUserID int64
	// Manager is true for a caller holding models.project_settings.edit: they
	// may erase another user's copy.
	Manager bool
	Message EventMessage
}

// EraseResult answers an erase. Previous is the row before, so the caller can
// cancel a running turn.
type EraseResult struct {
	Handoff  Handoff
	Previous Handoff
	Changed  bool
}

// TransitionResult answers a transition a retry may replay (a retried take
// back or release, a repeated claim redemption). Changed is false when the
// call found the thread where it would have put it and wrote nothing a reader
// can see; the live event is sent only on a change.
type TransitionResult struct {
	Handoff Handoff
	Changed bool
}

// TimedTransition is one sweeper-driven transition the repository performs by
// epoch compare-and-set.
type TimedTransition struct {
	Thread  Thread
	Epoch   int64
	Message *EventMessage
}

// Repository is the state machine's persistence (repos.WorkspaceHandoffsRepository).
type Repository interface {
	Read(ctx context.Context, thread Thread) (Handoff, bool, error)
	// ReadByRunningExecution answers the thread whose running cloud turn is
	// this execution.
	ReadByRunningExecution(ctx context.Context, executionID string) (Handoff, bool, error)
	Begin(ctx context.Context, record BeginRecord) (BeginResult, error)
	// ProjectWorkspaceBytes sums the project's live workspace bytes, without
	// the thread's own.
	ProjectWorkspaceBytes(ctx context.Context, thread Thread) (int64, error)
	Complete(ctx context.Context, record CompleteRecord) (Handoff, error)
	// Reclaim takes the thread back: CLOUD_IDLE → RECLAIMING (epoch + 1), or
	// CLOUD_RUNNING → reclaim_pending. Replayed answers a retried reclaim.
	Reclaim(ctx context.Context, thread Thread, actorUserID, epoch int64, deviceID string) (TransitionResult, error)
	// ReclaimRest takes back the turns a release left: a released,
	// rest_pending LOCAL row within its return grace → RECLAIMING
	// (epoch + 1). A retry answers the RECLAIMING row unchanged.
	ReclaimRest(ctx context.Context, thread Thread, actorUserID, epoch int64, deviceID string) (TransitionResult, error)
	Release(ctx context.Context, record ReleaseRecord) (TransitionResult, error)
	Erase(ctx context.Context, record EraseRecord) (EraseResult, error)
	StartCloudTurn(ctx context.Context, thread Thread, epoch int64, executionID, responseMessageID string) (TransitionResult, error)
	FinishCloudTurn(ctx context.Context, thread Thread, epoch int64, executionID string, message *EventMessage) (Handoff, error)
	// NotifyReclaimRequest writes the owner's notification (deduplicated per
	// thread, epoch and hour).
	NotifyReclaimRequest(ctx context.Context, handoff Handoff) error

	// The sweeper's claims and transitions.
	DueHandingOff(ctx context.Context, now time.Time, limit int) ([]Handoff, error)
	ExpireHandingOff(ctx context.Context, transition TimedTransition) (Handoff, error)
	DueReclaimGrace(ctx context.Context, before time.Time, limit int) ([]Handoff, error)
	ForceReclaim(ctx context.Context, transition TimedTransition) (Handoff, error)
	DueAbandonedReclaim(ctx context.Context, before time.Time, limit int) ([]Handoff, error)
	// DeadCloudTurns answers CLOUD_RUNNING threads whose worker died without
	// reporting: the running execution is not terminal and its newest claim
	// was never released, with a lease that ran out more than `grace` ago (the
	// database clock). A released claim (retry, requeue) is not a dead worker.
	DeadCloudTurns(ctx context.Context, grace time.Duration, limit int) ([]Handoff, error)
	AbandonReclaim(ctx context.Context, transition TimedTransition) (Handoff, error)
	// DueRestExpired answers rest take backs whose return grace ran out;
	// ExpireRestReclaim returns one to LOCAL (released), never CLOUD_IDLE.
	DueRestExpired(ctx context.Context, now time.Time, limit int) ([]Handoff, error)
	ExpireRestReclaim(ctx context.Context, transition TimedTransition) (Handoff, error)
	// DueAbandonedRestReclaim answers rest take backs RECLAIMING since before
	// `before` whose return grace is still open; AbandonRestReclaim returns
	// one to LOCAL, released, with the rest still pending.
	DueAbandonedRestReclaim(ctx context.Context, before time.Time, limit int) ([]Handoff, error)
	AbandonRestReclaim(ctx context.Context, transition TimedTransition) (Handoff, error)
	// DueIdle walks the CLOUD_IDLE rows in (state_changed_at, project,
	// conversation) order, after the cursor. A short page means the walk
	// reached the end.
	DueIdle(ctx context.Context, after IdleCursor, limit int) ([]Handoff, error)
	// NotifyIdleExpiring notifies the owner that the thread's cloud copy is
	// erased at deleteAt (ADR-0034 decision 12: a warning 3 days before).
	// Deduplicated per thread, epoch and idle period.
	NotifyIdleExpiring(ctx context.Context, handoff Handoff, deleteAt time.Time) (bool, error)
	// IdleErase erases a CLOUD_IDLE thread past its idle retention.
	IdleErase(ctx context.Context, transition TimedTransition) (Handoff, error)
	// OrphanedThreads answers rows whose conversation or project is gone,
	// walking the table from `after`.
	OrphanedThreads(ctx context.Context, after Thread, limit int) (orphans []Handoff, next Thread, err error)
	// OrphanErase erases a thread whose conversation is gone, whatever its
	// state.
	OrphanErase(ctx context.Context, transition TimedTransition) (Handoff, error)
	// ClaimErasures leases LOCAL rows whose objects are due (lease: a minute).
	ClaimErasures(ctx context.Context, now time.Time, limit int) ([]Handoff, error)
	// MarkErased sets deleted_at, when the row is still LOCAL at that epoch,
	// and forgets the environment secret's name (the caller deleted it).
	MarkErased(ctx context.Context, thread Thread, epoch int64) error
}

// EnvVault keeps a thread's harvested environment as ONE hidden secret of the
// project vault (decision 5, amended). Values never reach the database row,
// the bundle, a log or the audit.
type EnvVault interface {
	PutEnv(ctx context.Context, projectID int64, name string, env map[string]string) error
	DeleteEnv(ctx context.Context, projectID int64, name string) error
	ReadEnv(ctx context.Context, projectID int64, name string) (map[string]string, error)
}

// IdleCursor is a position in the CLOUD_IDLE walk. The zero value is the
// start.
type IdleCursor struct {
	StateChangedAt   time.Time
	ProjectID        int64
	ConversationUUID string
}

// Next answers the cursor after handoff.
func (IdleCursor) Next(handoff Handoff) IdleCursor {
	return IdleCursor{StateChangedAt: handoff.StateChangedAt, ProjectID: handoff.ProjectID, ConversationUUID: handoff.ConversationUUID}
}
