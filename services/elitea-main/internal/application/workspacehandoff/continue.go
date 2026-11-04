package workspacehandoff

// Send a running task to the cloud and continue; take it back when done
// (ADR-0045 decision 8, #1439).
//
// A complete may carry `continue`: the prompt the cloud turn starts with,
// the thread's mode and its unattended grant. Main then completes the send
// and starts the cloud turn in ONE transaction (the turn's admission,
// agentexecution/handoff_continue.go): the send, the person's message, the
// turn's command and CLOUD_IDLE → CLOUD_RUNNING commit together. A refused
// grant or chain continuation fails the whole complete, and the send stays
// HANDING_OFF. When the grant names the chain of the running local turn, the
// cloud turn is a continuation of it (link kind `handoff`): the chain's
// limits count across both executors.
//
// A cloud turn has no plan mode (ADR-0034 tool gaps), so a continue in mode
// `plan` is refused before anything changes (ContinueRefusedError, 422
// continue_refused): the desktop completes again without `continue`.
//
// A complete WITHOUT `continue` may name the chain of the unattended local
// turn the desktop stopped for the send (StoppedChainRootExecutionID). The
// send then ends that chain as `waiting`, in the transaction that completes
// it: the thread waits in the cloud for the person's next message. With
// `continue` the field is ignored (the continue links the chain).
//
// take_back_when_done is stored on the row. Main never takes a thread back
// by itself: the owner's desktop reads the flag and the last finished cloud
// run of the handoff (State) and reclaims. State reads that run only for a
// handoff with the flag set: the banner polls State, and nothing else reads
// the run.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// MaxContinuePromptRunes bounds the continue prompt.
const MaxContinuePromptRunes = 32768

// Thread modes a continue may name: the desktop's (build, plan, bypass).
// The cloud turn runs as any cloud turn does; the mode is recorded in the
// audit. A cloud copy has no plan mode (ADR-0034 tool gaps): a continue in
// `plan` is refused (ContinueRefusedError).
const (
	ModeBuild  = "build"
	ModePlan   = "plan"
	ModeBypass = "bypass"
)

// Continue is a complete's `continue`.
type Continue struct {
	Prompt string
	Mode   string
	// Grant is the thread's unattended grant; nil for an attended turn.
	// With ChainRootExecutionID set, the cloud turn continues that chain.
	Grant *unattended.Request
}

// ContinueRefusedReasonMode: a continue in mode `plan` (a cloud turn has no
// plan mode).
const ContinueRefusedReasonMode = "mode"

// ContinueRefusedError refuses a well-formed `continue` the cloud cannot run:
// 422 continue_refused with reason and field. Nothing changes; the send stays
// HANDING_OFF and the desktop completes again without `continue`.
type ContinueRefusedError struct {
	Reason string
	Field  string
}

func (e *ContinueRefusedError) Error() string {
	return "handoff continue refused: " + e.Reason
}

// refusal answers why the cloud cannot run a well-formed continue, or nil.
func (c *Continue) refusal() error {
	if c != nil && c.Mode == ModePlan {
		return &ContinueRefusedError{Reason: ContinueRefusedReasonMode, Field: "mode"}
	}
	return nil
}

func (c *Continue) valid() bool {
	if c == nil {
		return true
	}
	prompt := strings.TrimSpace(c.Prompt)
	if prompt == "" || !utf8.ValidString(c.Prompt) || utf8.RuneCountInString(c.Prompt) > MaxContinuePromptRunes ||
		strings.ContainsRune(c.Prompt, 0) {
		return false
	}
	switch c.Mode {
	case ModeBuild, ModePlan, ModeBypass:
	default:
		return false
	}
	return true
}

// ContinueStart is what the starter needs to complete the send and start
// the cloud turn in one transaction.
type ContinueStart struct {
	ProjectID        int64
	HandoffID        string
	ActorUserID      int64
	Epoch            int64
	HandoffRef       string
	SHA              string
	TakeBackWhenDone bool
	Prompt           string
	Grant            *unattended.Request
}

// ContinuedTurn is the cloud turn a complete started.
type ContinuedTurn struct {
	ExecutionID string
	// QuestionID is the person's message; ResponseMessageID the turn's
	// answer.
	QuestionID        string
	ResponseMessageID string
	// ConversationUUID is the thread.
	ConversationUUID string
}

// ContinueStarter completes a send and starts its cloud turn in one
// transaction (cmd/elitea-main binds the chat start use case).
type ContinueStarter interface {
	StartContinue(ctx context.Context, start ContinueStart) (ContinuedTurn, error)
}

// WithContinueStarter lets a complete carry `continue`. Without it such a
// complete answers ErrUnavailable and nothing changes.
func (s *Service) WithContinueStarter(starter ContinueStarter) *Service {
	if starter != nil {
		s.continuer = starter
	}
	return s
}

// completeAndContinue is Complete with `continue`.
func (s *Service) completeAndContinue(ctx context.Context, request CompleteRequest, started time.Time) (Handoff, error) {
	if s.continuer == nil {
		return Handoff{}, ErrUnavailable
	}
	continued, err := s.continuer.StartContinue(ctx, ContinueStart{
		ProjectID: request.ProjectID, HandoffID: request.HandoffID, ActorUserID: request.ActorUserID,
		Epoch: request.Epoch, HandoffRef: request.HandoffRef, SHA: request.SHA,
		TakeBackWhenDone: request.TakeBackWhenDone, Prompt: request.Continue.Prompt, Grant: request.Continue.Grant,
	})
	if err != nil {
		return Handoff{}, err
	}
	thread := Thread{ProjectID: request.ProjectID, ConversationUUID: continued.ConversationUUID}
	handoff, ok, err := s.repo.Read(ctx, thread)
	if err != nil {
		return Handoff{}, err
	}
	if !ok {
		return Handoff{}, ErrNotFound
	}
	s.emit(ctx, handoff)
	chain := "attended"
	if request.Continue.Grant != nil {
		chain = "new chain"
		if root := request.Continue.Grant.ChainRootExecutionID; root != "" {
			chain = "chain " + root
		}
	}
	s.record(ctx, thread, request.ActorUserID, "POST", request.AuditRoute, started,
		fmt.Sprintf("handoff.complete: epoch %d, snapshot %s%s, continued execution %s (mode %s, %s), take back when done %t",
			handoff.Epoch, request.SHA, includeAndEnvSummary(handoff.IncludedExcluded, handoff.EnvNames),
			continued.ExecutionID, request.Continue.Mode, chain, request.TakeBackWhenDone), false)
	handoff.Continued = &continued
	return handoff, nil
}

// Outcomes of a finished cloud run (the chain end states, and those of a
// cloud turn that is not a chain link).
const (
	CloudRunCompleted    = unattended.EndCompleted
	CloudRunFailed       = unattended.EndFailed
	CloudRunWaiting      = unattended.EndWaiting
	CloudRunLimitReached = unattended.EndLimitReached
	CloudRunCancelled    = unattended.EndCancelled
)

// CloudRun is the last finished cloud chain or turn of a handoff epoch.
type CloudRun struct {
	Outcome     string
	ExecutionID string
	FinishedAt  time.Time
}

// CloudRunReader answers the last finished cloud run of the thread's
// current cloud epoch (repos.WorkspaceHandoffsRepository).
type CloudRunReader interface {
	LastCloudRun(ctx context.Context, handoff Handoff) (*CloudRun, error)
}

// WithCloudRuns lets State answer last_cloud_run. Without it the state
// answers null.
func (s *Service) WithCloudRuns(reader CloudRunReader) *Service {
	if reader != nil {
		s.cloudRuns = reader
	}
	return s
}

// lastCloudRun answers the last finished run while the cloud owns the
// thread and the owner asked to take it back when done; nil in any other
// case, or when it cannot be read (the banner must not fail on it). Only
// the take back reads the run, and the banner polls State, so a handoff
// without the flag never pays for the scan.
func (s *Service) lastCloudRun(ctx context.Context, handoff Handoff) *CloudRun {
	if s.cloudRuns == nil || !handoff.TakeBackWhenDone || !handoff.State.Cloud() {
		return nil
	}
	run, err := s.cloudRuns.LastCloudRun(ctx, handoff)
	if err != nil {
		s.logger.WarnContext(ctx, "workspace handoff: last cloud run unreadable",
			"project_id", handoff.ProjectID, "conversation_uuid", handoff.ConversationUUID, "err", err)
		return nil
	}
	return run
}
