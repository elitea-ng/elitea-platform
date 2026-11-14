// Package runtimeplane runs one evaluation case as a real agent run on the
// runtime plane (issue 2011 step B, issue 2013 step 2).
//
// It is NOT a second execution path. A case is admitted through the chat start
// use case, the way an MCP `tools/call` and a pipeline trigger are
// (internal/api/v2/mcp/execute.go, internal/api/v2/pipelinetriggers/run.go):
// a conversation, a user participant for the run's author, an agent
// participant pinned to the run's version through the mapping's
// `entity_settings.version_id`, then StartCurrentApplication. Admission, the
// kill switch, governed tools, credentials, guardrails and the run evidence
// (ADR-0040..0045) are therefore the ones every chat turn meets, and the worker
// runs the agent exactly as it runs a chat turn.
//
// What the runner adds is the bridge back to a synchronous answer: it waits for
// the admitted turn to settle, bounded by the case's time limit, and enforces
// the case's step limit while it waits. Both end the run by the chat stop use
// case, so a limit is a cancelled run with a reason and not a dropped
// goroutine.
//
// An evaluation run is unattended and holds no grant: a step that needs a
// person (an approval, a tool authorization) pauses the run, and the runner
// cancels it and reports why. Nothing is approved on anyone's behalf.
package runtimeplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// Store is the chat-side database access of a case.
type Store interface {
	// PrepareConversation creates the conversation of one case and returns its
	// uuid and the agent participant's id.
	PrepareConversation(ctx context.Context, projectID, actorUserID, applicationID, versionID int64, name string) (string, int64, error)
	// DiscardConversation removes a conversation whose turn was never admitted.
	DiscardConversation(ctx context.Context, projectID int64, conversationUUID string)
	// ReadTurn reads the response message group by uuid.
	ReadTurn(ctx context.Context, projectID int64, responseMessageID string) (Turn, error)
	// StepCount counts the steps the run has recorded so far, by the rule of
	// evaluation.AgentStep.CountsAsStep.
	StepCount(ctx context.Context, projectID int64, executionID string) (int, error)
	// StepsSettled reports whether the run's step record is final: the run's
	// settlement was prepared, so no step is still to arrive.
	StepsSettled(ctx context.Context, projectID int64, executionID string) (bool, error)
}

// Authorizer answers whether a person may read the run evidence of a project
// (the runs.evidence.read permission).
type Authorizer interface {
	MayReadRunEvidence(ctx context.Context, userID, projectID int64) (bool, error)
}

// Starter is the chat start use case.
type Starter interface {
	StartCurrentApplication(
		context.Context, agentexecutionapp.CurrentApplicationStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

// Canceller is the chat stop use case.
type Canceller interface {
	Cancel(context.Context, agentexecutionapp.CurrentAgentCancelRequest) (agentexecutionapp.CurrentAgentCancelOutcome, error)
}

// Evidence reads the run's step record (ADR-0042).
type Evidence interface {
	Steps(ctx context.Context, projectID int64, runID string) ([]runevidence.Record, error)
	RecordContentRead(ctx context.Context, userID, projectID int64, runID, stepID string, side runevidence.Side, route string) error
}

// Content reads a step's stored content.
type Content interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// Runner implements evaluation.AgentRunner.
type Runner struct {
	store     Store
	starter   Starter
	canceller Canceller
	evidence  Evidence
	content   Content
	logger    *slog.Logger
	// usage and authorizer are optional (WithUsage, WithAuthorizer).
	usage      evaluation.UsageReader
	authorizer Authorizer

	// pollInitial, pollMax and settleWait are the waits, shortened in tests.
	pollInitial, pollMax, settleWait time.Duration
}

var _ evaluation.AgentRunner = (*Runner)(nil)

// New builds the runner. The evidence and the content store may be nil: the
// case then has no steps (and no tool outputs), and still has its answer.
func New(store Store, starter Starter, canceller Canceller, evidence Evidence, content Content, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store: store, starter: starter, canceller: canceller, evidence: evidence, content: content, logger: logger,
		pollInitial: 250 * time.Millisecond, pollMax: 2 * time.Second, settleWait: 300 * time.Millisecond,
	}
}

// WithUsage lets the runner stop a case that crosses a token or cost limit.
func (r *Runner) WithUsage(usage evaluation.UsageReader) *Runner {
	r.usage = usage
	return r
}

// WithAuthorizer lets the runner copy tool outputs into the evaluation result:
// only when the run's author may read the run evidence. Without one no output
// is copied.
func (r *Runner) WithAuthorizer(authorizer Authorizer) *Runner {
	r.authorizer = authorizer
	return r
}

// ErrNotComposed is returned when the runner was built without a starter.
var ErrNotComposed = errors.New("the agent runtime is not composed")

// RunCase runs one case and returns when it settles, is cancelled by a limit,
// or the context ends. A context that ends cancels the agent run too.
func (r *Runner) RunCase(ctx context.Context, request evaluation.AgentRunRequest) (evaluation.AgentRunResult, error) {
	if r == nil || r.store == nil || r.starter == nil {
		return evaluation.AgentRunResult{}, ErrNotComposed
	}
	project, err := strconv.ParseInt(request.ProjectID, 10, 64)
	if err != nil || project <= 0 || request.ActorUserID <= 0 || request.ApplicationID <= 0 || request.VersionID <= 0 {
		return evaluation.AgentRunResult{}, errors.New("the case's agent run needs a project, an author, an agent and a version")
	}
	started, err := r.admit(ctx, project, request)
	if err != nil {
		return evaluation.AgentRunResult{}, err
	}
	result := evaluation.AgentRunResult{ExecutionID: started.outcome.ExecutionID, ConversationUUID: started.conversation}
	if request.Admitted != nil {
		request.Admitted(ctx, result.ExecutionID, result.ConversationUUID)
	}

	turn, outcome, reason, waitErr := r.await(ctx, project, started.outcome, request)
	if waitErr != nil {
		// The caller's context ended (a cancelled run, a shutdown): the agent
		// run must not outlive the evaluation that wanted it.
		r.stop(ctx, project, int64(request.ActorUserID), started.outcome.ResponseMessageID)
		return result, waitErr
	}
	if outcome != "" {
		r.stop(ctx, project, int64(request.ActorUserID), started.outcome.ResponseMessageID)
	}
	result.Steps = r.collectSteps(ctx, project, request, result.ExecutionID)
	r.finish(&result, turn, outcome, reason, request)
	return result, nil
}

type admitted struct {
	outcome      agentexecutionapp.CurrentApplicationStartOutcome
	conversation string
}

func (r *Runner) admit(ctx context.Context, project int64, request evaluation.AgentRunRequest) (admitted, error) {
	name := "Evaluation run " + request.RunID + ", case " + request.CaseID
	conversation, participant, err := r.store.PrepareConversation(ctx, project,
		int64(request.ActorUserID), int64(request.ApplicationID), int64(request.VersionID), name)
	if err != nil {
		return admitted{}, fmt.Errorf("could not prepare the case's conversation: %w", err)
	}
	outcome, err := r.starter.StartCurrentApplication(ctx, agentexecutionapp.CurrentApplicationStartRequest{
		ProjectID: project, ActorUserID: int64(request.ActorUserID), ConversationUUID: conversation,
		TargetParticipantID: participant, QuestionID: uuid.NewString(), UserInput: request.Input,
		// A programmatic start under the author's identity, like an MCP call.
		TriggerOrigin: executiondomain.TriggerOriginAPI,
	})
	if err != nil {
		r.store.DiscardConversation(ctx, project, conversation)
		return admitted{}, startError(err)
	}
	return admitted{outcome: outcome, conversation: conversation}, nil
}

// startError names why a case's run was not admitted. The cause is classified
// and not relayed: it can carry internals, and the run's author needs to know
// which kind of refusal it was.
func startError(err error) error {
	if suspended, found := safety.AsSuspended(err); found {
		return fmt.Errorf("the agent run was not started: %s (%s)", suspended.Message(), suspended.Code())
	}
	switch {
	case errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart):
		return errors.New("the agent run was not started: this version does not meet the runtime's admission contract")
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart):
		return errors.New("the agent run was not started: the request was invalid")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	}
	return errors.New("the agent run could not be started")
}

// await waits for the turn to settle. It returns the last turn read, and a
// non-empty outcome (with a reason for a limit) when a limit ended the wait
// instead of the run.
func (r *Runner) await(
	ctx context.Context, project int64, started agentexecutionapp.CurrentApplicationStartOutcome, request evaluation.AgentRunRequest,
) (Turn, string, string, error) {
	deadline := time.NewTimer(request.TimeLimit)
	defer deadline.Stop()
	poll := time.NewTimer(0)
	defer poll.Stop()
	interval := r.pollInitial
	var turn Turn
	for {
		select {
		case <-ctx.Done():
			return turn, "", "", ctx.Err()
		case <-deadline.C:
			return turn, evaluation.AgentOutcomeTimedOut, "", nil
		case <-poll.C:
		}
		read, err := r.store.ReadTurn(ctx, project, started.ResponseMessageID)
		if err != nil {
			if ctx.Err() != nil {
				return turn, "", "", ctx.Err()
			}
			return turn, "", "", fmt.Errorf("the case's answer could not be read back: %w", err)
		}
		turn = read
		// A pause is a settled row too (the projection clears is_streaming),
		// so it is read BEFORE settlement: nobody is coming to answer it.
		if turn.Paused != "" {
			return turn, evaluation.AgentOutcomePaused, "", nil
		}
		if turn.Settled {
			return turn, "", "", nil
		}
		if outcome, reason := r.overLimit(ctx, project, started, request); outcome != "" {
			return turn, outcome, reason, nil
		}
		if interval *= 2; interval > r.pollMax {
			interval = r.pollMax
		}
		poll.Reset(interval)
	}
}

// overLimit checks the case's step, token and cost limits while it runs, at the
// cadence of the poll. A read that fails is not a limit: the next poll asks
// again, and the run's own checks (before each case) still hold.
func (r *Runner) overLimit(
	ctx context.Context, project int64, started agentexecutionapp.CurrentApplicationStartOutcome, request evaluation.AgentRunRequest,
) (string, string) {
	if request.StepLimit > 0 {
		if count, err := r.store.StepCount(ctx, project, started.ExecutionID); err == nil && count > request.StepLimit {
			return evaluation.AgentOutcomeStepLimit, ""
		}
	}
	if request.Limits == nil || r.usage == nil || !request.Limits.HasAny() {
		return "", ""
	}
	// The usage is the gateway's attribution to the run and the case: the case's
	// own execution, the executions linked to it, and every judge call so far.
	readCtx := context.WithoutCancel(ctx)
	runUsage, runErr := r.usage.EvaluationUsage(readCtx, request.ProjectID, request.RunID, "")
	caseUsage, caseErr := r.usage.EvaluationUsage(readCtx, request.ProjectID, request.RunID, request.CaseID)
	if runErr != nil || caseErr != nil {
		return "", ""
	}
	if reason := evaluation.RuntimeLimitReason(*request.Limits, runUsage, caseUsage); reason != "" {
		return evaluation.AgentOutcomeLimit, reason
	}
	return "", ""
}

// stop cancels the agent run by the chat stop use case. The context that asked
// for the stop may be the one that ended, so the stop uses its own.
func (r *Runner) stop(ctx context.Context, project, actor int64, responseMessageID string) {
	if r.canceller == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.canceller.Cancel(stopCtx, agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: project, ActorUserID: actor, ResponseMessageID: responseMessageID,
	}); err != nil {
		// A run that settled between the last read and the stop is not an
		// error worth more than a log line.
		r.logger.Warn("evaluation: could not stop a case's agent run",
			"project_id", project, "response_message_id", responseMessageID, "error", err)
	}
}

// finish decides the outcome of a case that was not ended by the caller.
func (r *Runner) finish(result *evaluation.AgentRunResult, turn Turn, outcome, reason string, request evaluation.AgentRunRequest) {
	steps := 0
	for _, step := range result.Steps {
		if step.CountsAsStep() {
			steps++
		}
	}
	switch {
	case outcome == evaluation.AgentOutcomeTimedOut:
		result.Outcome = evaluation.AgentOutcomeTimedOut
	case outcome == evaluation.AgentOutcomeLimit:
		result.Outcome, result.Failure = evaluation.AgentOutcomeLimit, reason
	case outcome == evaluation.AgentOutcomeStepLimit, request.StepLimit > 0 && steps > request.StepLimit:
		result.Outcome = evaluation.AgentOutcomeStepLimit
	case outcome == evaluation.AgentOutcomePaused:
		result.Outcome, result.Failure = evaluation.AgentOutcomePaused, pauseReason(turn.Paused)
	case turn.Failed:
		result.Outcome, result.Failure = evaluation.AgentOutcomeFailed, failureText(turn.Failure)
	case turn.Text == "":
		result.Outcome, result.Failure = evaluation.AgentOutcomeFailed, "the agent run finished without producing any text"
	default:
		result.Outcome, result.Output = evaluation.AgentOutcomeCompleted, turn.Text
	}
}

func failureText(reported string) string {
	if reported == "" {
		return "the agent run failed without a reported reason"
	}
	return "the agent run failed: " + reported
}

func pauseReason(kind string) string {
	return "the agent run paused for " + kind + ", and an evaluation run answers no questions; it was cancelled"
}

// maxSettleWaits bounds how long collectSteps waits for the step record to be
// final. The worker reports steps in batches, so the last ones may land a
// moment after the run settles; once the run's settlement is prepared
// (StepsSettled) nothing more arrives and the wait ends at once. A run that
// never reports a settlement (a cancelled one) costs at most this many waits.
const maxSettleWaits = 4

func (r *Runner) collectSteps(ctx context.Context, project int64, request evaluation.AgentRunRequest, executionID string) []evaluation.AgentStep {
	if r.evidence == nil {
		return nil
	}
	for wait := 0; wait < maxSettleWaits; wait++ {
		settled, err := r.store.StepsSettled(context.WithoutCancel(ctx), project, executionID)
		if err != nil || settled {
			break
		}
		select {
		case <-ctx.Done():
			wait = maxSettleWaits
		case <-time.After(r.settleWait):
		}
	}
	records, err := r.evidence.Steps(context.WithoutCancel(ctx), project, executionID)
	if err != nil {
		r.logger.Warn("evaluation: could not read a case's run steps",
			"project_id", project, "execution_id", executionID, "error", err)
		return nil
	}
	if len(records) == 0 {
		return nil
	}
	mayRead := r.mayCopyOutputs(ctx, project, request)
	steps := make([]evaluation.AgentStep, 0, len(records))
	for _, record := range records {
		steps = append(steps, r.step(ctx, project, request, record, mayRead))
	}
	return steps
}

// mayCopyOutputs decides once per case whether tool outputs may be copied into
// the evaluation result: the suite keeps them, and the run's AUTHOR holds
// runs.evidence.read in the project. A copy outlives the run evidence's own
// retention and is readable by whoever reads the result, so it is made only
// from what the author could read.
func (r *Runner) mayCopyOutputs(ctx context.Context, project int64, request evaluation.AgentRunRequest) bool {
	if !request.KeepToolOutputs || r.authorizer == nil || r.content == nil {
		return false
	}
	allowed, err := r.authorizer.MayReadRunEvidence(context.WithoutCancel(ctx), int64(request.ActorUserID), project)
	if err != nil {
		r.logger.Warn("evaluation: could not check the author's evidence permission; tool outputs are not copied",
			"project_id", project, "error", err)
		return false
	}
	return allowed
}

// maxKeptOutputs bounds how many tool outputs one case reads from storage: the
// scorecard keeps a bounded list of steps (evaluation.BuildTrajectory), and an
// output beyond it would be read and dropped.
const maxKeptOutputs = 60

func (r *Runner) step(ctx context.Context, project int64, request evaluation.AgentRunRequest, record runevidence.Record, mayRead bool) evaluation.AgentStep {
	step := evaluation.AgentStep{
		Seq: int(record.Seq), Kind: string(record.Kind), Name: record.Name, ToolkitID: record.ToolkitID,
		DurationMs: record.EndedAt.Sub(record.StartedAt).Milliseconds(), Error: summaryError(record.Summary),
	}
	if len(record.Inputs.Digest) > 0 {
		step.InputsDigest = hex.EncodeToString(record.Inputs.Digest)
	}
	if mayRead && step.IsToolStep() && int(record.Seq) <= maxKeptOutputs {
		step.Output = r.toolOutput(ctx, project, request, record)
	}
	return step
}

// maxVerifiedOutputBytes is the largest stored output the runner reads. The
// digest covers the whole stored object, so a copy is verified over all of it
// and a partial read could not be; a larger output is not copied (it stays in
// the run evidence). maxOutputExcerptBytes is how much of a verified output is
// kept.
const (
	maxVerifiedOutputBytes = 32 * 1024
	maxOutputExcerptBytes  = 4 * 1024
)

// toolOutput reads a tool step's stored output for the result's evidence,
// under the rules of the run evidence itself:
//
//   - the read is audited FIRST, as the run's author, and refused when the audit
//     write fails;
//   - the bytes are checked against the step's stored SHA-256 and dropped when
//     they differ;
//   - expired, filtered-away or oversized content is simply absent.
//
// The audit rows are written one per step, sequentially: the evidence store has
// no batch insert for them.
func (r *Runner) toolOutput(ctx context.Context, project int64, request evaluation.AgentRunRequest, record runevidence.Record) string {
	if r.evidence == nil || record.Outputs.ObjectKey == "" || record.ContentExpiredAt != nil ||
		len(record.Outputs.Digest) == 0 || record.Outputs.Size > maxVerifiedOutputBytes {
		return ""
	}
	if err := r.evidence.RecordContentRead(context.WithoutCancel(ctx), int64(request.ActorUserID), project,
		record.RunID, record.StepID, runevidence.SideOutputs, "evaluation"); err != nil {
		r.logger.Warn("evaluation: the tool output read could not be audited, so it was not copied",
			"run_id", record.RunID, "step_id", record.StepID, "error", err)
		return ""
	}
	body, err := r.content.Get(context.WithoutCancel(ctx), record.Outputs.ObjectKey)
	if err != nil {
		return ""
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], record.Outputs.Digest) {
		r.logger.Warn("evaluation: a stored tool output does not match its digest, so it was not copied",
			"run_id", record.RunID, "step_id", record.StepID)
		return ""
	}
	return outputExcerpt(body)
}

// outputExcerpt makes a verified output storable in a jsonb value: at most
// maxOutputExcerptBytes (cut on a character boundary), NUL bytes removed, and
// binary or invalid UTF-8 content kept as base64, as the evidence content route
// serves it.
func outputExcerpt(body []byte) string {
	if len(body) > maxOutputExcerptBytes {
		cut := maxOutputExcerptBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
	}
	body = bytes.ReplaceAll(body, []byte{0}, nil)
	if !utf8.Valid(body) {
		return "base64:" + base64.StdEncoding.EncodeToString(body)
	}
	return string(body)
}

// summaryError is the step summary's `error`, empty for none.
func summaryError(summary []byte) string {
	var fields struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(summary, &fields) != nil {
		return ""
	}
	switch value := fields.Error.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}
