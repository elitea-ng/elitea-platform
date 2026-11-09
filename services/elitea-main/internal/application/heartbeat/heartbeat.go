// Package heartbeat runs the heartbeat of proactive agents (ADR-0050
// decision 2, issue 1618).
//
// The platform scheduler job agent.heartbeat.scan.v1 calls Service.Scan
// once a minute. A pass finds the agents whose next wake is due and wakes
// each one:
//
//  1. GATES. The maintenance switch stops the whole pass, as it stops the
//     pipeline schedules. The platform switch (proactivity.enabled) and the
//     project switch (proactive_agents_enabled) stop the heartbeats they
//     cover. The kill switch and the breaker (an active suspension of the
//     platform, the project or the agent) stop the agent's wake. A wake that
//     a gate stopped writes no heartbeat row: it only moves the next wake.
//  2. RELEASE ONLY. The wake reads the `proactivity` block and the soul's
//     proactive_goals of the agent's current RELEASE, never of a draft. With
//     no release, or a release whose block is off, the agent does not wake.
//  3. GOVERNANCE. The agent must be at least medium risk and the release
//     must have passed a change request with at least one approval. In a
//     personal project with proactivity.personal_self_approve on, neither is
//     needed (ADR-0050 Q3, option D).
//  4. QUIET HOURS AND JITTER. Inside the quiet hours the wake moves to the
//     end of the window. Every next wake is one interval later, moved by at
//     most the block's jitter.
//  5. OVERLAP. A run that an earlier act started and that still streams
//     skips the wake.
//  6. ENVELOPE. The wakes, the acts and the spend of the agent's day are
//     read from elitea_runtime.agent_heartbeats and the gateway request log.
//     A wake or spend limit pauses the heartbeat until the next day and
//     notifies the owner once.
//  7. TRIAGE. The project's low-tier model (else its default model) reads
//     the wake digest: goals, memory, owner notes and the new messages since
//     the cursor. It answers no_op, notify or act.
//  8. OUTCOME. no_op writes only the heartbeat row. notify posts one agent
//     message (cause heartbeat) to the agent's home thread and creates one
//     notification for the owner. act starts a run under the owner's
//     mandate, with notify_and_wait and no sensitive_tools grant. With no
//     usable mandate, at the act limit, or when the start is refused, the act
//     degrades to notify and the message tells the owner why.
//
// The package holds the decisions only. Store is the database
// (repos.AgentHeartbeats), Starter is the chat start use case.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// AttributionPrefix is the gateway request-log execution id prefix of a
// triage call: heartbeat:<wake_id>.
const AttributionPrefix = "heartbeat:"

// The heartbeat outcomes (shared 0240).
const (
	OutcomeNoOp        = "no_op"
	OutcomeNotify      = "notify"
	OutcomeAct         = "act"
	OutcomeActDegraded = "act_degraded"
	OutcomeFailed      = "failed"
	OutcomePaused      = "paused"
)

// The reason codes of a skipped wake (settings.last_skip_reason) and of a
// degraded act or a failed wake (agent_heartbeats.reason_code).
const (
	ReasonPlatformOff       = "platform_off"
	ReasonProjectOff        = "project_off"
	ReasonNoRelease         = "no_release"
	ReasonProactivityOff    = "proactivity_off"
	ReasonReviewRequired    = "review_required"
	ReasonQuietHours        = "quiet_hours"
	ReasonSuspended         = "suspended"
	ReasonRunActive         = "previous_run_active"
	ReasonPaused            = "paused"
	ReasonWakeLimit         = "wake_limit"
	ReasonSpendLimit        = "spend_limit"
	ReasonActLimit          = "act_limit"
	ReasonMandateRequired   = "mandate_required"
	ReasonMandateUnusable   = "mandate_not_usable"
	ReasonMandateNotOwner   = "mandate_not_owner"
	ReasonGovernanceOff     = "mandates_unavailable"
	ReasonOwnerCannotRun    = "owner_cannot_run"
	ReasonStartRefused      = "start_refused"
	ReasonNoModel           = "no_model"
	ReasonTriageFailed      = "triage_failed"
	ReasonTriageUnreadable  = "triage_unreadable"
	ReasonNotifyFailed      = "notify_failed"
	ReasonSuspendedMidCycle = "suspended_during_wake"
)

// The bounds of one pass.
const (
	// MaxAgentsPerPass bounds the wakes of one pass. The rest stay due and
	// the next pass takes them.
	MaxAgentsPerPass = 25
	// MaxMemoryEntries and MaxMessages bound the digest.
	MaxMemoryEntries = 12
	MaxNotes         = 8
	MaxMessages      = 30
	// retryAfter is the next wake after a gate stopped a wake whose block is
	// not known (no release, the block off).
	retryAfter = time.Hour
)

// ErrPassIncomplete is a pass that left due agents for the next one.
var ErrPassIncomplete = errors.New("heartbeat: the pass did not wake every due agent")

// Agent is one agent whose heartbeat an owner turned on.
type Agent struct {
	ProjectID     int64
	ApplicationID int64
	OwnerUserID   int64
	MandateID     string
	// HomeConversationUUID is empty until the first notify or act.
	HomeConversationUUID string
	PausedUntil          *time.Time
	// CursorAt is the newest message the last wake read.
	CursorAt *time.Time
}

// Release is what the wake reads of the agent's current release.
type Release struct {
	ReleaseID int64
	VersionID int64
	AgentName string
	Meta      map[string]any
	Goals     []string
	// Tier is the agent's risk tier (agent_registry).
	Tier agentlifecycle.Tier
	// Reviewed: a change request with at least one approval published it.
	Reviewed bool
	// Personal: the project is the owner's personal project.
	Personal bool
}

// Day is the agent's envelope so far today.
type Day struct {
	Wakes    int
	Acts     int
	SpendUSD float64
}

// Material is what the digest is built from.
type Material struct {
	Memory   []string
	Notes    []string
	Messages []proactivity.DigestMessage
	// NewestAt is the time of the newest message read; nil for none.
	NewestAt *time.Time
}

// Wake is one heartbeat row.
type Wake struct {
	WakeID           string
	ProjectID        int64
	ApplicationID    int64
	ReleaseID        int64
	VersionID        int64
	OwnerUserID      int64
	Outcome          string
	ReasonCode       string
	Reason           string
	DigestSHA256     string
	ExecutionID      string
	ConversationUUID string
	PausedUntil      *time.Time
	// Next is the agent's next wake; CursorAt moves the message cursor
	// (nil keeps it).
	Next     time.Time
	CursorAt *time.Time
}

// Post is one agent message to the owner in the home thread, with its
// notification.
type Post struct {
	WakeID        string
	ProjectID     int64
	ApplicationID int64
	VersionID     int64
	OwnerUserID   int64
	AgentName     string
	Text          string
	Outcome       string
	ReasonCode    string
}

// ActTarget is where an act runs.
type ActTarget struct {
	ConversationUUID string
	ParticipantID    int64
	// Created: the conversation was made for this act; it is discarded
	// when the start is refused.
	Created bool
}

// Store is the database (repos.AgentHeartbeats).
type Store interface {
	// Maintenance reports the platform maintenance switch. An error is
	// read as off, as the pipeline schedules read it.
	Maintenance(ctx context.Context) bool
	Settings(ctx context.Context) (platformconfig.ProactivitySettings, error)
	// Projects lists the projects whose tenant schema holds
	// agent_heartbeat_settings.
	Projects(ctx context.Context) ([]int64, error)
	ProjectEnabled(ctx context.Context, projectID int64) (bool, error)
	// DueAgents lists the enabled agents whose next wake is at or before
	// now, oldest first, at most limit.
	DueAgents(ctx context.Context, projectID int64, now time.Time, limit int) ([]Agent, error)
	// CurrentRelease answers the agent's release; found is false without
	// one.
	CurrentRelease(ctx context.Context, agent Agent) (release Release, found bool, err error)
	// Reschedule moves the next wake without a heartbeat row.
	Reschedule(ctx context.Context, agent Agent, next time.Time, skipReason string) error
	RunActive(ctx context.Context, agent Agent) (bool, error)
	DayEnvelope(ctx context.Context, agent Agent, since time.Time) (Day, error)
	Material(ctx context.Context, agent Agent, block proactivity.Block) (Material, error)
	// RecordWake writes the heartbeat row and the agent's clock in one
	// transaction.
	RecordWake(ctx context.Context, wake Wake) error
	// PostToOwner writes the home-thread message and the notification and
	// answers the home thread.
	PostToOwner(ctx context.Context, post Post) (string, error)
	// PrepareAct finds or creates the act's conversation and writes its
	// notify_and_wait policy (shared 0211).
	PrepareAct(ctx context.Context, agent Agent, release Release, target string) (ActTarget, error)
	// DiscardAct removes a conversation PrepareAct created for a start that
	// was refused.
	DiscardAct(ctx context.Context, agent Agent, target ActTarget)
}

// Starter is the chat start use case.
type Starter interface {
	StartCurrentApplication(context.Context, agentexecution.CurrentApplicationStartRequest) (agentexecution.CurrentApplicationStartOutcome, error)
}

// Mandates is the fire-time mandate check (*runmandate.Service).
type Mandates interface {
	Admit(ctx context.Context, mandateID string, target runmandate.Target) (securitycontext.RunAuthority, error)
	Refuse(ctx context.Context, target runmandate.Target, mandateID string, cause error) error
}

// Gate is the kill switch (*safety.Gate).
type Gate interface {
	Check(ctx context.Context, subject safety.Subject) error
}

// RunPermission reports whether the owner may still run the agent in the
// project (models.chat.messages.create).
type RunPermission func(ctx context.Context, projectID, userID int64) (bool, error)

// Config is the service's dependencies.
type Config struct {
	Store Store
	// Start is required for an act; nil degrades every act to notify.
	Start Starter
	// Mandates is nil with credential governance off: an act then degrades
	// to notify (no mandate can be checked).
	Mandates Mandates
	Gate     Gate
	// Completer and Models are the triage model (runbrief's knob).
	Completer runbrief.Completer
	Models    runbrief.ModelResolver
	// CanRun is nil to skip the owner's permission check (tests).
	CanRun RunPermission
	Logger *slog.Logger
	// Now and Unit are the clock and the jitter draw; nil is the real one.
	Now  func() time.Time
	Unit func() float64
}

// Service runs the heartbeat.
type Service struct {
	cfg Config
}

// New builds the service. The store, the gate and the completer are
// required.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Gate == nil || cfg.Completer == nil {
		return nil, errors.New("heartbeat: a store, a kill switch gate and a triage completer are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Unit == nil {
		cfg.Unit = rand.Float64
	}
	return &Service{cfg: cfg}, nil
}

// PassResult is what one pass did.
type PassResult struct {
	Suppressed bool
	Due        int
	Woken      int
	Skipped    int
	Truncated  bool
}

// Scan runs one pass.
func (s *Service) Scan(ctx context.Context) (PassResult, error) {
	var result PassResult
	if s.cfg.Store.Maintenance(ctx) {
		s.cfg.Logger.Info("heartbeat: maintenance mode is active; no agent wakes")
		result.Suppressed = true
		return result, nil
	}
	settings, err := s.cfg.Store.Settings(ctx)
	if err != nil {
		return result, fmt.Errorf("heartbeat: read the proactivity settings: %w", err)
	}
	projects, err := s.cfg.Store.Projects(ctx)
	if err != nil {
		return result, err
	}
	now := s.cfg.Now().UTC()
	for _, projectID := range projects {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		budget := MaxAgentsPerPass - result.Due
		if budget <= 0 {
			result.Truncated = true
			break
		}
		agents, err := s.cfg.Store.DueAgents(ctx, projectID, now, budget+1)
		if err != nil {
			s.cfg.Logger.Error("heartbeat: list the due agents", "project_id", projectID, "err", err)
			continue
		}
		if len(agents) > budget {
			agents, result.Truncated = agents[:budget], true
		}
		projectOn := true
		if len(agents) > 0 {
			if projectOn, err = s.cfg.Store.ProjectEnabled(ctx, projectID); err != nil {
				s.cfg.Logger.Error("heartbeat: read the project switch", "project_id", projectID, "err", err)
				continue
			}
		}
		for _, agent := range agents {
			// A wake can take a whole triage call: stop while there is
			// time for one more, so the pass ends inside its lease. The
			// rest stay due for the next minute.
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < proactivity.TriageTimeout+5*time.Second {
				result.Truncated = true
				break
			}
			result.Due++
			woke, err := s.wake(ctx, agent, settings, projectOn, now)
			if err != nil {
				// One agent's failure is not the pass's: logged, and the
				// row stays due, because nothing moved its clock.
				s.cfg.Logger.Error("heartbeat: wake", "project_id", agent.ProjectID,
					"application_id", agent.ApplicationID, "err", err)
				continue
			}
			if woke {
				result.Woken++
			} else {
				result.Skipped++
			}
		}
	}
	if result.Truncated {
		return result, ErrPassIncomplete
	}
	return result, nil
}

// wake runs the gates, the envelope, the triage and the outcome of one
// agent. It answers whether the agent was triaged or paused (a heartbeat
// row was written).
func (s *Service) wake(
	ctx context.Context, agent Agent, settings platformconfig.ProactivitySettings, projectOn bool, now time.Time,
) (bool, error) {
	store := s.cfg.Store
	skip := func(next time.Time, reason string) (bool, error) {
		return false, store.Reschedule(ctx, agent, next, reason)
	}
	if !settings.Enabled {
		return skip(now.Add(retryAfter), ReasonPlatformOff)
	}
	if !projectOn {
		return skip(now.Add(retryAfter), ReasonProjectOff)
	}
	if agent.PausedUntil != nil && agent.PausedUntil.After(now) {
		return skip(*agent.PausedUntil, ReasonPaused)
	}
	release, found, err := store.CurrentRelease(ctx, agent)
	if err != nil {
		return false, err
	}
	if !found {
		return skip(now.Add(retryAfter), ReasonNoRelease)
	}
	block := proactivity.FromMeta(release.Meta)
	if !block.Enabled() {
		return skip(now.Add(retryAfter), ReasonProactivityOff)
	}
	next := block.NextWake(now, s.cfg.Unit())
	if !Approved(release, settings) {
		return skip(next, ReasonReviewRequired)
	}
	if block.QuietHours != nil && block.QuietHours.Contains(now) {
		end := block.QuietHours.EndAfter(now)
		// The jitter spreads the wakes of agents that share a window.
		spread := time.Duration(s.cfg.Unit() * float64(block.Interval) * float64(block.JitterPercent) / 100)
		return skip(end.Add(spread), ReasonQuietHours)
	}
	if err := s.cfg.Gate.Check(ctx, safety.Subject{ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID}); err != nil {
		if errors.Is(err, safety.ErrExecutionSuspended) {
			return skip(next, ReasonSuspended)
		}
		// The suspension state is not readable: no wake, and the row stays
		// due for the next pass.
		return false, err
	}
	active, err := store.RunActive(ctx, agent)
	if err != nil {
		return false, err
	}
	if active {
		return skip(next, ReasonRunActive)
	}

	limits := block.Effective(settings.Ceilings, release.Personal)
	day, err := store.DayEnvelope(ctx, agent, block.DayStart(now))
	if err != nil {
		return false, err
	}
	wake := Wake{
		WakeID: uuid.NewString(), ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID,
		ReleaseID: release.ReleaseID, VersionID: release.VersionID, OwnerUserID: agent.OwnerUserID, Next: next,
	}
	if reason := limitReached(day, limits); reason != "" {
		return true, s.pause(ctx, agent, release, block, wake, reason, now)
	}

	material, err := store.Material(ctx, agent, block)
	if err != nil {
		return false, err
	}
	digest := proactivity.Digest{
		AgentName: release.AgentName, Now: now, Goals: release.Goals,
		Memory: material.Memory, Notes: material.Notes, Messages: material.Messages, ReadTools: block.ReadTools(),
	}
	wake.DigestSHA256 = digest.Hash()
	wake.CursorAt = material.NewestAt
	triage, reason, err := s.triage(ctx, agent, wake.WakeID, digest)
	if err != nil {
		wake.Outcome, wake.ReasonCode, wake.Reason = OutcomeFailed, reason, clip(err.Error())
		// A failed triage keeps the cursor: the next wake reads the same
		// messages again.
		wake.CursorAt = nil
		return true, store.RecordWake(ctx, wake)
	}
	wake.Reason = triage.Reason
	switch triage.Decision {
	case proactivity.DecisionNoOp:
		wake.Outcome = OutcomeNoOp
		return true, store.RecordWake(ctx, wake)
	case proactivity.DecisionNotify:
		return true, s.notify(ctx, agent, release, wake, triage.Message, OutcomeNotify, "")
	default:
		return true, s.act(ctx, agent, release, block, wake, triage, day, limits)
	}
}

// Approved is the governance rule of a wake: at least medium risk and a
// reviewed release, or a personal project that self-approves.
func Approved(release Release, settings platformconfig.ProactivitySettings) bool {
	if release.Personal && settings.PersonalSelfApprove {
		return true
	}
	return release.Tier.Valid() && release.Tier != agentlifecycle.TierLow && release.Reviewed
}

func limitReached(day Day, limits proactivity.Limits) string {
	switch {
	case day.Wakes >= limits.WakesPerDay:
		return ReasonWakeLimit
	case limits.SpendPerDayUSD >= 0 && day.SpendUSD >= limits.SpendPerDayUSD:
		return ReasonSpendLimit
	}
	return ""
}

// pause stops the heartbeat until the next day and tells the owner once:
// the next wake is after the pause, so the pause row and its message are
// written once a day at most.
func (s *Service) pause(
	ctx context.Context, agent Agent, release Release, block proactivity.Block, wake Wake, reason string, now time.Time,
) error {
	until := block.NextDay(now)
	wake.Outcome, wake.ReasonCode, wake.PausedUntil, wake.Next = OutcomePaused, reason, &until, until
	text := fmt.Sprintf("I reached my daily %s and paused my heartbeat until %s.",
		limitName(reason), until.In(block.DayLocation()).Format("2006-01-02 15:04 MST"))
	wake.Reason = text
	conversation, err := s.cfg.Store.PostToOwner(ctx, Post{
		WakeID: wake.WakeID, ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID,
		VersionID: release.VersionID, OwnerUserID: agent.OwnerUserID, AgentName: release.AgentName,
		Text: text, Outcome: OutcomePaused, ReasonCode: reason,
	})
	if err != nil {
		s.cfg.Logger.Error("heartbeat: tell the owner about the pause", "project_id", agent.ProjectID,
			"application_id", agent.ApplicationID, "err", err)
	}
	wake.ConversationUUID = conversation
	return s.cfg.Store.RecordWake(ctx, wake)
}

func limitName(reason string) string {
	switch reason {
	case ReasonWakeLimit:
		return "wake limit"
	case ReasonSpendLimit:
		return "spend limit"
	}
	return "limit"
}

// triage asks the model. It answers the reason code of a failure.
func (s *Service) triage(ctx context.Context, agent Agent, wakeID string, digest proactivity.Digest) (proactivity.Triage, string, error) {
	model := ""
	if s.cfg.Models != nil {
		resolved, err := s.cfg.Models.LowTierModel(ctx, agent.ProjectID)
		if err != nil {
			return proactivity.Triage{}, ReasonTriageFailed, fmt.Errorf("read the low-tier model: %w", err)
		}
		model = resolved
	}
	callCtx, cancel := context.WithTimeout(ctx, proactivity.TriageTimeout)
	defer cancel()
	answer, err := s.cfg.Completer.Complete(callCtx, runbrief.CompletionRequest{
		ProjectID: agent.ProjectID, UserID: agent.OwnerUserID, Model: model,
		System: proactivity.TriageSystemPrompt, User: proactivity.TriageUserPrompt(digest),
		AttributionID: AttributionPrefix + wakeID, MaxTokens: proactivity.TriageMaxTokens,
	})
	if errors.Is(err, runbrief.ErrNoModelPlane) {
		return proactivity.Triage{}, ReasonNoModel, err
	}
	if err != nil {
		return proactivity.Triage{}, ReasonTriageFailed, err
	}
	triage, err := proactivity.ParseTriage(answer)
	if err != nil {
		return proactivity.Triage{}, ReasonTriageUnreadable, err
	}
	return triage, "", nil
}

// notify posts the message and writes the row.
func (s *Service) notify(ctx context.Context, agent Agent, release Release, wake Wake, text, outcome, reasonCode string) error {
	conversation, err := s.cfg.Store.PostToOwner(ctx, Post{
		WakeID: wake.WakeID, ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID,
		VersionID: release.VersionID, OwnerUserID: agent.OwnerUserID, AgentName: release.AgentName,
		Text: text, Outcome: outcome, ReasonCode: reasonCode,
	})
	if err != nil {
		wake.Outcome, wake.ReasonCode, wake.Reason, wake.CursorAt = OutcomeFailed, ReasonNotifyFailed, clip(err.Error()), nil
		return s.cfg.Store.RecordWake(ctx, wake)
	}
	wake.Outcome, wake.ReasonCode, wake.ConversationUUID = outcome, reasonCode, conversation
	return s.cfg.Store.RecordWake(ctx, wake)
}

// act starts the run, or degrades to notify and says why.
func (s *Service) act(
	ctx context.Context, agent Agent, release Release, block proactivity.Block, wake Wake,
	triage proactivity.Triage, day Day, limits proactivity.Limits,
) error {
	degrade := func(code, why string) error {
		text := fmt.Sprintf("%s\n\nI wanted to act on this, but I did not: %s", triage.Message, why)
		return s.notify(ctx, agent, release, wake, text, OutcomeActDegraded, code)
	}
	if day.Acts >= limits.ActsPerDay {
		return degrade(ReasonActLimit, fmt.Sprintf("I already started %d run(s) today, which is my daily limit.", day.Acts))
	}
	if s.cfg.Start == nil {
		return degrade(ReasonStartRefused, "this deployment cannot start agent runs.")
	}
	authority, code, why := s.mandate(ctx, agent, release)
	if code != "" {
		return degrade(code, why)
	}
	if s.cfg.CanRun != nil {
		allowed, err := s.cfg.CanRun(ctx, agent.ProjectID, agent.OwnerUserID)
		if err != nil {
			return err
		}
		if !allowed {
			return degrade(ReasonOwnerCannotRun, "my owner no longer has the permission to run agents in this project.")
		}
	}
	// The kill switch again, immediately before the start: a suspension
	// that arrived during the triage stops the run.
	if err := s.cfg.Gate.Check(ctx, safety.Subject{ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID}); err != nil {
		return degrade(ReasonSuspendedMidCycle, "my runs were suspended while I was deciding.")
	}
	target, err := s.cfg.Store.PrepareAct(ctx, agent, release, block.ActTarget)
	if err != nil {
		return err
	}
	runCtx := securitycontext.WithRunAuthority(ctx, authority)
	outcome, err := s.cfg.Start.StartCurrentApplication(runCtx, agentexecution.CurrentApplicationStartRequest{
		ProjectID: agent.ProjectID, ActorUserID: agent.OwnerUserID,
		ConversationUUID: target.ConversationUUID, TargetParticipantID: target.ParticipantID,
		QuestionID: uuid.NewString(), UserInput: ActPrompt(triage),
		TriggerOrigin: executiondomain.TriggerOriginSchedule, ReleaseID: release.ReleaseID,
		// An ADR-0045 chain that only continues: no sensitive_tools grant,
		// ever (decision 1), and the goal check judges the run.
		Unattended: &unattended.Request{ContinueStepLimit: true, ContinueOutputLimit: true, GoalCheck: true},
		Heartbeat:  true,
	})
	if err != nil {
		if target.Created {
			s.cfg.Store.DiscardAct(ctx, agent, target)
		}
		s.cfg.Logger.Warn("heartbeat: the act start was refused", "project_id", agent.ProjectID,
			"application_id", agent.ApplicationID, "err", err)
		return degrade(ReasonStartRefused, "the platform refused to start the run ("+startRefusal(err)+").")
	}
	wake.Outcome, wake.ExecutionID, wake.ConversationUUID = OutcomeAct, outcome.ExecutionID, target.ConversationUUID
	return s.cfg.Store.RecordWake(ctx, wake)
}

// mandate is the owner's mandate check. It answers the authority, or the
// reason code and the owner-facing sentence of a refusal.
func (s *Service) mandate(ctx context.Context, agent Agent, release Release) (securitycontext.RunAuthority, string, string) {
	if s.cfg.Mandates == nil {
		return securitycontext.RunAuthority{}, ReasonGovernanceOff,
			"runs that nobody starts need a mandate, and credential governance is off on this platform."
	}
	target := runmandate.Target{
		Origin: OriginAgentHeartbeat, TenantProjectID: agent.ProjectID,
		ResourceID: fmt.Sprintf("%d", agent.ApplicationID), ApplicationVersionID: release.VersionID,
	}
	authority, err := s.cfg.Mandates.Admit(ctx, agent.MandateID, target)
	if err == nil && authority.SubjectUserID != agent.OwnerUserID {
		// An autonomous mandate (no subject) or another person's: the act
		// must run as its owner (ADR-0050 decision 2a, delegated_background).
		return securitycontext.RunAuthority{}, ReasonMandateNotOwner,
			"my mandate does not act for my owner. Attach a delegated mandate of my owner to my heartbeat."
	}
	if err == nil {
		return authority, "", ""
	}
	if code := runmandate.Code(err); code != "" {
		if refuseErr := s.cfg.Mandates.Refuse(ctx, target, agent.MandateID, err); refuseErr != nil {
			s.cfg.Logger.Error("heartbeat: record the mandate refusal", "err", refuseErr)
		}
	}
	if errors.Is(err, runmandate.ErrMandateRequired) {
		return securitycontext.RunAuthority{}, ReasonMandateRequired,
			"I have no mandate. Attach a delegated mandate of my owner to my heartbeat so that I can act."
	}
	return securitycontext.RunAuthority{}, ReasonMandateUnusable,
		"my mandate is not usable (" + strings.ToLower(runmandate.Code(err)) + "). Renew or replace it."
}

// OriginAgentHeartbeat is the mandate origin of a heartbeat act.
const OriginAgentHeartbeat runmandate.Origin = "agent_heartbeat"

// ActPrompt is the user input of an act run: the triage's task, framed as
// the agent's own decision, with the reason.
func ActPrompt(triage proactivity.Triage) string {
	prompt := "Your heartbeat woke you and you decided to act. Nobody is waiting for an answer. " +
		"Every approval waits for your owner.\n\nTask: " + triage.Message
	if triage.Reason != "" && triage.Reason != triage.Message {
		prompt += "\nWhy: " + triage.Reason
	}
	return prompt
}

func startRefusal(err error) string {
	var refusal *unattended.RefusalError
	if errors.As(err, &refusal) {
		return refusal.Error()
	}
	if errors.Is(err, safety.ErrExecutionSuspended) {
		return "runs are suspended"
	}
	return "an internal error"
}

func clip(text string) string {
	if len(text) <= proactivity.MaxReasonBytes {
		return text
	}
	return strings.ToValidUTF8(text[:proactivity.MaxReasonBytes], "")
}
