package workspacehandoff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

// ObjectEraser erases a thread's objects (*workspacestore.Service).
type ObjectEraser interface {
	EraseThreadObjects(ctx context.Context, thread Thread) (bool, error)
}

// TurnCanceller stops a running cloud turn, the way the chat's stop control
// does (agentexecution.CurrentAgentCancellationService).
type TurnCanceller interface {
	CancelCloudTurn(ctx context.Context, projectID, actorUserID int64, responseMessageID string) error
}

// Service is the handoff state machine.
type Service struct {
	repo      Repository
	policy    PolicySource
	objects   ObjectEraser
	canceller TurnCanceller
	recorder  audit.Recorder
	now       func() time.Time
	logger    *slog.Logger

	mu       sync.Mutex
	orphanAt Thread
	// idleAt is where the next sweep continues the CLOUD_IDLE walk, so a
	// long list of idle threads is covered across sweeps.
	idleAt IdleCursor
	vault  EnvVault
}

// WithEnvVault supplies the vault of the harvested environment. Without it a
// send that carries variables answers ErrUnavailable; every other path works.
func (s *Service) WithEnvVault(vault EnvVault) *Service {
	s.vault = vault
	return s
}

// NewService builds the state machine. Every dependency is required: a send
// without a policy could not refuse, and an erase without a canceller would
// leave a cloud turn writing into a thread nobody owns.
func NewService(
	repo Repository, policy PolicySource, objects ObjectEraser, canceller TurnCanceller,
	recorder audit.Recorder, logger *slog.Logger,
) (*Service, error) {
	if repo == nil || policy == nil || objects == nil || canceller == nil || recorder == nil {
		return nil, errors.New("workspace handoff dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo: repo, policy: policy, objects: objects, canceller: canceller,
		recorder: recorder, now: time.Now, logger: logger,
	}, nil
}

// WithClock replaces the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func validThread(thread Thread) bool {
	return thread.ProjectID > 0 && thread.ProjectID <= 2147483647 && workspacestore.ValidUUID(thread.ConversationUUID)
}

func validActor(id int64) bool { return id > 0 && id <= 2147483647 }

// EffectivePolicy is the policy a send and every cloud turn claim use. It is
// recomputed on every call, so turning the network off applies from the next
// turn (decision 11).
func (s *Service) EffectivePolicy(ctx context.Context, projectID int64) (Policy, error) {
	policy, err := s.policy.Effective(ctx, projectID)
	if err != nil {
		s.logger.ErrorContext(ctx, "workspace handoff: policy unreadable", "project_id", projectID, "err", err)
		return Policy{}, ErrUnavailable
	}
	return policy, nil
}

// BeginRequest is a send (contract §2.1).
type BeginRequest struct {
	Thread      Thread
	ActorUserID int64
	TokenID     string
	DeviceID    string
	HistoryMode string
	Stats       Stats
	HasUpstream bool
	Note        string
	AuditRoute  string
	// IncludedExcluded are the excluded paths the user explicitly includes.
	IncludedExcluded []string
	// Env is the harvested environment: nil keeps the thread's previous one,
	// an empty map clears it.
	Env map[string]string
}

// BeginOutcome answers a send.
type BeginOutcome struct {
	Handoff     Handoff
	AllowedRefs []string
	ExpectedOld map[string]*string
	Limits      workspacestore.Limits
	BytesLeft   int64
}

// Begin moves LOCAL to HANDING_OFF.
func (s *Service) Begin(ctx context.Context, request BeginRequest) (BeginOutcome, error) {
	started := s.now()
	if !validThread(request.Thread) || !validActor(request.ActorUserID) || request.TokenID == "" ||
		request.DeviceID == "" || len(request.DeviceID) > 128 ||
		len(request.Note) > MaxNoteBytes || !utf8.ValidString(request.Note) ||
		request.Stats.Files < 0 || request.Stats.Bytes < 0 || request.Stats.UnpackedBytes < 0 ||
		len(request.Stats.Excluded) > 10000 {
		return BeginOutcome{}, ErrInvalid
	}
	mode := request.HistoryMode
	if mode == "" {
		mode = HistoryNone
	}
	if mode != HistoryNone && mode != HistoryFull {
		return BeginOutcome{}, ErrInvalid
	}
	policy, err := s.EffectivePolicy(ctx, request.Thread.ProjectID)
	if err != nil {
		return BeginOutcome{}, err
	}
	if !policy.Enabled {
		return BeginOutcome{}, disabled(policy)
	}
	if mode == HistoryFull && !policy.HistoryFullAllowed {
		return BeginOutcome{}, ErrHistoryModeNotAllowed
	}
	limits := policy.Limits
	if request.Stats.Files > limits.MaxFiles || request.Stats.Bytes > limits.MaxSegmentBytes ||
		request.Stats.UnpackedBytes > limits.MaxUnpackedBytes {
		return BeginOutcome{}, ErrOverLimits
	}
	// The amendment's rules, re-applied here whatever the desktop checked.
	if err := checkIncludes(request.IncludedExcluded, policy); err != nil {
		return BeginOutcome{}, err
	}
	envNames, err := checkEnv(request.Env, policy)
	if err != nil {
		return BeginOutcome{}, err
	}
	envChange := EnvChange{Keep: request.Env == nil, Names: envNames}
	if len(request.Env) > 0 {
		if s.vault == nil {
			return BeginOutcome{}, ErrUnavailable
		}
		name, err := newEnvSecretName()
		if err != nil {
			return BeginOutcome{}, err
		}
		// The secret is written first and deleted again when the send is
		// refused, so a row never names a secret that does not exist.
		if err := s.vault.PutEnv(ctx, request.Thread.ProjectID, name, request.Env); err != nil {
			s.logger.ErrorContext(ctx, "workspace handoff: env secret write failed", "project_id", request.Thread.ProjectID, "err", err)
			return BeginOutcome{}, ErrUnavailable
		}
		envChange.SecretName = name
	}
	includes := request.IncludedExcluded
	if includes == nil {
		includes = []string{}
	}
	excluded := request.Stats.Excluded
	if excluded == nil {
		excluded = []string{}
	}
	request.Stats.Excluded = excluded
	stats, err := json.Marshal(request.Stats)
	if err != nil {
		return BeginOutcome{}, ErrInvalid
	}
	result, err := s.repo.Begin(ctx, BeginRecord{
		Thread: request.Thread, ActorUserID: request.ActorUserID, TokenID: request.TokenID,
		DeviceID: request.DeviceID, HistoryMode: mode, HasUpstream: request.HasUpstream,
		Stats: stats, Excluded: excluded, Note: request.Note, TTL: HandingOffTTL,
		MaxProjectWorkspaceBytes: limits.MaxProjectWorkspaceBytes, DeclaredBytes: request.Stats.Bytes,
		IncludedExcluded: includes, Env: envChange,
	})
	if err != nil {
		if envChange.SecretName != "" {
			s.deleteEnv(ctx, request.Thread.ProjectID, envChange.SecretName)
		}
		return BeginOutcome{}, err
	}
	if result.PreviousEnvSecret != "" && result.PreviousEnvSecret != result.Handoff.EnvSecretName {
		s.deleteEnv(ctx, request.Thread.ProjectID, result.PreviousEnvSecret)
	}
	allowed := workspacestore.AllowedRefs(workspacestore.WriterDesktop, workspacestore.StateHandingOff, request.HasUpstream)
	expected := map[string]*string{}
	for _, name := range allowed {
		expected[name] = nil
	}
	left := limits.MaxProjectWorkspaceBytes - result.ProjectBytesUsed
	if left < 0 {
		left = 0
	}
	s.record(ctx, request.Thread, request.ActorUserID, "POST", request.AuditRoute, started,
		fmt.Sprintf("handoff.begin: epoch %d, %d files, %d bytes, %d excluded, history %s%s",
			result.Handoff.Epoch, request.Stats.Files, request.Stats.Bytes, len(excluded), mode,
			includeAndEnvSummary(result.Handoff.IncludedExcluded, result.Handoff.EnvNames)), false)
	return BeginOutcome{
		Handoff: result.Handoff, AllowedRefs: allowed, ExpectedOld: expected, Limits: limits, BytesLeft: left,
	}, nil
}

// CompleteRequest is contract §2.4.
type CompleteRequest struct {
	ProjectID   int64
	HandoffID   string
	ActorUserID int64
	Epoch       int64
	HandoffRef  string
	SHA         string
	AuditRoute  string
}

// Complete moves HANDING_OFF to CLOUD_IDLE once the store holds the snapshot.
func (s *Service) Complete(ctx context.Context, request CompleteRequest) (Handoff, error) {
	started := s.now()
	if request.ProjectID <= 0 || !workspacestore.ValidUUID(request.HandoffID) || !validActor(request.ActorUserID) ||
		request.Epoch <= 0 || request.HandoffRef != workspacestore.RefHandoff1 || !workspacestore.ValidSHA1(request.SHA) {
		return Handoff{}, ErrInvalid
	}
	handoff, err := s.repo.Complete(ctx, CompleteRecord{
		ProjectID: request.ProjectID, HandoffID: request.HandoffID, ActorUserID: request.ActorUserID,
		Epoch: request.Epoch, HandoffRef: request.HandoffRef, SHA: request.SHA,
		Message: EventMessage{Event: "handoff.complete"},
	})
	if err != nil {
		return Handoff{}, err
	}
	s.record(ctx, Thread{ProjectID: handoff.ProjectID, ConversationUUID: handoff.ConversationUUID},
		request.ActorUserID, "POST", request.AuditRoute, started,
		fmt.Sprintf("handoff.complete: epoch %d, snapshot %s%s", handoff.Epoch, request.SHA,
			includeAndEnvSummary(handoff.IncludedExcluded, handoff.EnvNames)), false)
	return handoff, nil
}

// CompleteMessage is the event message of a completed send: the counts, the
// files left on the laptop and the handoff note (decision 8).
func CompleteMessage(stats json.RawMessage, note string) string {
	var declared Stats
	_ = json.Unmarshal(stats, &declared)
	text := fmt.Sprintf("Work moved to the cloud executor (%d files, %d left on the laptop).",
		declared.Files, len(declared.Excluded))
	if note = strings.TrimSpace(note); note != "" {
		text += "\n\n" + note
	}
	return text
}

// ReclaimRequest is a desktop take back (contract §2.6).
type ReclaimRequest struct {
	Thread      Thread
	ActorUserID int64
	Epoch       int64
	DeviceID    string
	AuditRoute  string
}

// Reclaim takes the thread back: CLOUD_IDLE → RECLAIMING, or, during a cloud
// turn, reclaim_pending (Handoff.ReclaimPending, state CLOUD_RUNNING).
func (s *Service) Reclaim(ctx context.Context, request ReclaimRequest) (Handoff, error) {
	started := s.now()
	if !validThread(request.Thread) || !validActor(request.ActorUserID) || request.Epoch <= 0 ||
		len(request.DeviceID) > 128 {
		return Handoff{}, ErrInvalid
	}
	handoff, err := s.repo.Reclaim(ctx, request.Thread, request.ActorUserID, request.Epoch, request.DeviceID)
	if err != nil {
		return Handoff{}, err
	}
	s.record(ctx, request.Thread, request.ActorUserID, "POST", request.AuditRoute, started,
		fmt.Sprintf("handoff.reclaim: state %s, epoch %d, pending %t", handoff.State, handoff.Epoch, handoff.ReclaimPending), false)
	return handoff, nil
}

// RequestReclaim is the web or mobile Take back (contract §2.7): it notifies
// the owner's desktop and changes no state.
func (s *Service) RequestReclaim(ctx context.Context, thread Thread, actorUserID int64) error {
	if !validThread(thread) || !validActor(actorUserID) {
		return ErrInvalid
	}
	handoff, ok, err := s.repo.Read(ctx, thread)
	if err != nil {
		return err
	}
	if !ok || handoff.OwnerUserID != actorUserID {
		return ErrNotFound
	}
	if !handoff.State.Cloud() {
		return ErrNotInCloud
	}
	return s.repo.NotifyReclaimRequest(ctx, handoff)
}

// ReleaseRequest is contract §2.8.
type ReleaseRequest struct {
	Thread      Thread
	ActorUserID int64
	Epoch       int64
	Outcome     string
	AppliedSHA  *string
	AuditRoute  string
}

// Release moves RECLAIMING to LOCAL. The cloud copy is kept for the project's
// return grace (decision 12), then erased.
func (s *Service) Release(ctx context.Context, request ReleaseRequest) (Handoff, error) {
	started := s.now()
	if !validThread(request.Thread) || !validActor(request.ActorUserID) || request.Epoch <= 0 {
		return Handoff{}, ErrInvalid
	}
	switch request.Outcome {
	case OutcomeApplied, OutcomeWorktree, OutcomeDiscarded:
	default:
		return Handoff{}, ErrInvalid
	}
	if request.AppliedSHA != nil && !workspacestore.ValidSHA1(*request.AppliedSHA) {
		return Handoff{}, ErrInvalid
	}
	policy, err := s.EffectivePolicy(ctx, request.Thread.ProjectID)
	if err != nil {
		return Handoff{}, err
	}
	handoff, err := s.repo.Release(ctx, ReleaseRecord{
		Thread: request.Thread, ActorUserID: request.ActorUserID, Epoch: request.Epoch,
		Grace: policy.ReturnGrace, Message: EventMessage{Event: "handoff.release"},
	})
	if err != nil {
		return Handoff{}, err
	}
	applied := "none"
	if request.AppliedSHA != nil {
		applied = *request.AppliedSHA
	}
	s.record(ctx, request.Thread, request.ActorUserID, "POST", request.AuditRoute, started,
		fmt.Sprintf("handoff.release: outcome %s, applied %s, epoch %d", request.Outcome, applied, handoff.Epoch), false)
	return handoff, nil
}

// ReleaseMessage is the event message of a release.
func ReleaseMessage(deviceName string) string {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "the desktop"
	}
	return "Work returned to " + deviceName + "."
}

// EraseMessage is the event message of an erase (decision 12).
const EraseMessage = "The cloud copy of this thread was erased. The file changes of the cloud turns were " +
	"discarded and are not in the local folder."

// View is the banner state (contract §2.9).
type View struct {
	Handoff        Handoff
	Present        bool
	WorkspaceBytes int64
	// DeleteAfter is when the cloud copy is erased: the release grace, or, in
	// CLOUD_IDLE, the end of the idle retention.
	DeleteAfter *time.Time
}

// State reads the banner state.
func (s *Service) State(ctx context.Context, thread Thread) (View, error) {
	if !validThread(thread) {
		return View{}, ErrInvalid
	}
	handoff, ok, err := s.repo.Read(ctx, thread)
	if err != nil {
		return View{}, err
	}
	if !ok {
		return View{}, nil
	}
	view := View{Handoff: handoff, Present: true, DeleteAfter: handoff.DeleteAfter}
	if handoff.DeletedAt != nil {
		view.DeleteAfter = nil
	}
	if handoff.State == workspacestore.StateCloudIdle {
		if policy, err := s.EffectivePolicy(ctx, thread.ProjectID); err == nil && policy.IdleRetention > 0 {
			deadline := handoff.StateChangedAt.Add(policy.IdleRetention).UTC()
			view.DeleteAfter = &deadline
		}
	}
	return view, nil
}

// EraseRequest is the erase (contract §2.9 DELETE).
type EraseRequest struct {
	Thread      Thread
	ActorUserID int64
	Manager     bool
	AuditRoute  string
}

// Erase ends the handoff: the state becomes LOCAL, a running cloud turn is
// cancelled, and the sweeper erases the objects (deleted_at only after an
// empty listing).
func (s *Service) Erase(ctx context.Context, request EraseRequest) (Handoff, error) {
	started := s.now()
	if !validThread(request.Thread) || !validActor(request.ActorUserID) {
		return Handoff{}, ErrInvalid
	}
	result, err := s.repo.Erase(ctx, EraseRecord{
		Thread: request.Thread, ActorUserID: request.ActorUserID, Manager: request.Manager,
		Message: EventMessage{Event: "handoff.erase", Text: EraseMessage},
	})
	if err != nil {
		return Handoff{}, err
	}
	s.cancelRunning(ctx, result.Previous)
	// The objects go now, so a desktop that cancelled a send can send again at
	// once; the sweeper finishes an erasure this pass could not.
	if result.Changed && result.Handoff.State == workspacestore.StateLocal {
		thread := threadOf(result.Handoff)
		if err := s.eraseThread(ctx, result.Handoff); err != nil {
			s.logger.WarnContext(ctx, "workspace handoff: erase failed; the sweeper retries",
				"project_id", thread.ProjectID, "conversation_uuid", thread.ConversationUUID, "err", err)
		}
	}
	if result.Changed {
		s.record(ctx, request.Thread, request.ActorUserID, "DELETE", request.AuditRoute, started,
			fmt.Sprintf("handoff.erase: from %s, epoch %d", result.Previous.State, result.Handoff.Epoch), false)
	}
	return result.Handoff, nil
}

func (s *Service) cancelRunning(ctx context.Context, previous Handoff) {
	if previous.State != workspacestore.StateCloudRunning || previous.RunningResponseID == "" {
		return
	}
	if err := s.canceller.CancelCloudTurn(ctx, previous.ProjectID, previous.OwnerUserID, previous.RunningResponseID); err != nil {
		s.logger.WarnContext(ctx, "workspace handoff: cancel running cloud turn failed",
			"project_id", previous.ProjectID, "conversation_uuid", previous.ConversationUUID,
			"execution_id", previous.RunningExecutionID, "err", err)
	}
}

// StartCloudTurn moves CLOUD_IDLE to CLOUD_RUNNING for one cloud turn: the
// later executor calls it at claim, with the epoch of its command. It is
// refused while a take back is pending and when the effective policy no longer
// allows the cloud executor. The policy it answers is the one the turn runs
// under (recomputed now).
func (s *Service) StartCloudTurn(
	ctx context.Context, thread Thread, epoch int64, executionID, responseMessageID string,
) (Handoff, Policy, error) {
	if !validThread(thread) || epoch <= 0 || executionID == "" || len(executionID) > 64 ||
		!workspacestore.ValidUUID(responseMessageID) {
		return Handoff{}, Policy{}, ErrInvalid
	}
	policy, err := s.EffectivePolicy(ctx, thread.ProjectID)
	if err != nil {
		return Handoff{}, Policy{}, err
	}
	if !policy.Enabled {
		return Handoff{}, Policy{}, disabled(policy)
	}
	handoff, err := s.repo.StartCloudTurn(ctx, thread, epoch, executionID, responseMessageID)
	if err != nil {
		return Handoff{}, Policy{}, err
	}
	return handoff, policy, nil
}

// FinishCloudTurn ends a cloud turn (end, cancellation or a pause for input):
// CLOUD_RUNNING → CLOUD_IDLE, or → RECLAIMING with a new epoch when a take back
// is pending.
func (s *Service) FinishCloudTurn(ctx context.Context, thread Thread, epoch int64, executionID string) (Handoff, error) {
	if !validThread(thread) || epoch <= 0 || executionID == "" {
		return Handoff{}, ErrInvalid
	}
	handoff, err := s.repo.FinishCloudTurn(ctx, thread, epoch, executionID, nil)
	if err != nil {
		return Handoff{}, err
	}
	if handoff.State == workspacestore.StateReclaiming {
		s.record(ctx, thread, handoff.OwnerUserID, "", "", s.now(),
			fmt.Sprintf("handoff.reclaim: turn ended, epoch %d", handoff.Epoch), true)
	}
	return handoff, nil
}

func (s *Service) record(ctx context.Context, thread Thread, userID int64, method, route string, started time.Time, action string, system bool) {
	status := int32(200)
	duration := float64(s.now().Sub(started).Microseconds()) / 1000
	event := audit.Event{
		Timestamp: s.now().UTC(), ProjectID: audit.ID(thread.ProjectID),
		EventType: "agent", Action: truncate(action, 512), HTTPMethod: method, HTTPRoute: route,
		StatusCode: &status, DurationMS: &duration,
		EntityType: workspacestore.AuditEntityType, EntityName: thread.ConversationUUID,
	}
	if system {
		event.EventType = "schedule"
	}
	if userID > 0 {
		event.UserID = audit.ID(userID)
	}
	s.recorder.Record(ctx, event)
}

func truncate(value string, limit int) string {
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}

// eraseThread erases a LOCAL thread's cloud copy: its objects, then its
// environment secret, and only then marks the row erased, so a failure of
// either leaves the row for the sweeper's next pass.
func (s *Service) eraseThread(ctx context.Context, handoff Handoff) error {
	thread := threadOf(handoff)
	empty, err := s.objects.EraseThreadObjects(ctx, thread)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	if handoff.EnvSecretName != "" {
		if s.vault == nil {
			return errors.New("the env secret cannot be erased: no vault is composed")
		}
		if err := s.vault.DeleteEnv(ctx, handoff.ProjectID, handoff.EnvSecretName); err != nil {
			return fmt.Errorf("erase env secret: %w", err)
		}
	}
	return s.repo.MarkErased(ctx, thread, handoff.Epoch)
}

func (s *Service) deleteEnv(ctx context.Context, projectID int64, name string) {
	if s.vault == nil || name == "" {
		return
	}
	if err := s.vault.DeleteEnv(ctx, projectID, name); err != nil {
		s.logger.WarnContext(ctx, "workspace handoff: env secret delete failed", "project_id", projectID, "err", err)
	}
}

// CloudTurnEnv redeems the thread's harvested environment for the running
// cloud turn (decision 5 amended, contract 6.4). The later executor unit calls
// it from its claim-bound runtime secret path, with the claim's epoch and
// execution id; it answers only for that running turn. A thread without a
// secret answers nil.
func (s *Service) CloudTurnEnv(ctx context.Context, thread Thread, epoch int64, executionID string) (map[string]string, error) {
	if !validThread(thread) || epoch <= 0 || executionID == "" {
		return nil, ErrInvalid
	}
	handoff, ok, err := s.repo.Read(ctx, thread)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if handoff.State != workspacestore.StateCloudRunning || handoff.Epoch != epoch || handoff.RunningExecutionID != executionID {
		return nil, ErrStaleEpoch
	}
	if handoff.EnvSecretName == "" {
		return nil, nil
	}
	if s.vault == nil {
		return nil, ErrUnavailable
	}
	env, err := s.vault.ReadEnv(ctx, thread.ProjectID, handoff.EnvSecretName)
	if err != nil {
		s.logger.ErrorContext(ctx, "workspace handoff: env secret unreadable", "project_id", thread.ProjectID, "err", err)
		return nil, ErrUnavailable
	}
	return env, nil
}

// includeAndEnvSummary is the audit's record of the amendment: include paths
// and env NAMES, never values.
func includeAndEnvSummary(includes, names []string) string {
	out := ""
	if len(includes) > 0 {
		out += fmt.Sprintf(", %d included (%s)", len(includes), strings.Join(includes, " "))
	}
	if len(names) > 0 {
		out += fmt.Sprintf(", env names (%s)", strings.Join(names, " "))
	}
	return out
}

func newEnvSecretName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "workspace_env_" + hex.EncodeToString(b[:]), nil
}
