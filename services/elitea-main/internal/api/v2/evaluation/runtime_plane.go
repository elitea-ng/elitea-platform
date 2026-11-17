package evaluation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// THE RUNTIME_PLANE EXECUTION MODE (issue 2011 step B, issue 2013 step 2).
//
// With execution_mode runtime_plane a case is a REAL AGENT RUN of the run's
// pinned agent version on the worker. It is not a second execution path: the
// AgentRunner (runtimeplane.Runner) admits the case through the chat start use
// case, exactly as an MCP `tools/call` or a pipeline trigger does, so the
// admission gates, the governed tools, the kill switch and the ADR-0040..0045
// controls apply to it unchanged. This file holds only what the orchestrator
// needs to drive it and to turn its steps into evidence.
//
// What the orchestrator does NOT do for such a run: answer approval questions.
// An evaluation run is unattended and has no grant, so a step that needs a
// person pauses the run; the runner cancels it and the case stores an error
// that says so. Tools that need approval therefore fail a case, visibly,
// instead of being approved on nobody's behalf.

// DefaultCaseTimeLimit bounds one runtime_plane case when the suite sets no
// time limit. An agent run has no duration of its own, and a case that never
// settles would hold a worker of the orchestrator's small pool for ever.
const DefaultCaseTimeLimit = 15 * time.Minute

// Why a runtime_plane case did not complete (AgentRunResult.Outcome).
const (
	AgentOutcomeCompleted = "completed"
	AgentOutcomeFailed    = "failed"
	AgentOutcomeTimedOut  = "timed_out"
	AgentOutcomeStepLimit = "step_limit"
	AgentOutcomePaused    = "paused"
	// AgentOutcomeLimit: a token or cost limit was crossed while the case ran.
	AgentOutcomeLimit = "limit"
)

// ErrRuntimePlaneNotConfigured is what a runtime_plane run reports on a
// deployment that composed no agent runner (no runtime plane).
var ErrRuntimePlaneNotConfigured = errors.New("this deployment has no agent runtime, so a runtime_plane evaluation cannot run its cases as agent runs")

// ErrStepLimit is the case error of a run that reached its step limit.
var ErrStepLimit = errors.New("the case's step limit was reached")

// AgentRunner runs ONE case as an agent run and returns when it settles (or
// when its limits or the context end it). It is the orchestrator's only door
// to the runtime plane.
type AgentRunner interface {
	RunCase(ctx context.Context, request AgentRunRequest) (AgentRunResult, error)
}

// AgentRunRequest is one case to run.
type AgentRunRequest struct {
	ProjectID string
	RunID     string
	CaseID    string
	// ActorUserID is the run's author: the identity the agent run signs.
	ActorUserID int
	// ApplicationID and VersionID pin the agent version the run scores.
	ApplicationID int
	VersionID     int
	// Input is the case input with its variables substituted.
	Input string
	// StepLimit and TimeLimit end the run; zero StepLimit is no step limit.
	// TimeLimit is never zero (DefaultCaseTimeLimit).
	StepLimit int
	TimeLimit time.Duration
	// Limits are the run's frozen limits; the runner enforces the token and
	// cost ones while the case runs. Nil is none.
	Limits *RunLimits
	// KeepToolOutputs asks the runner to read the output of every tool step.
	KeepToolOutputs bool
	// Admitted is called once the agent run is admitted, before it settles, so
	// the case's execution is recorded (and its model calls attributed to the
	// run and the case) even when the process dies mid-case.
	Admitted func(ctx context.Context, executionID, conversationUUID string)
}

// AgentRunResult is what a case's agent run did.
type AgentRunResult struct {
	ExecutionID      string
	ConversationUUID string
	// Outcome is one of the AgentOutcome constants.
	Outcome string
	// Output is the run's final answer; empty unless it completed.
	Output string
	// Failure is the readable reason of a run that did not complete.
	Failure string
	Steps   []AgentStep
	// GuardrailOutcomes counts the LLM gateway's guardrail verdicts recorded
	// for the run, by outcome ("blocked", "masked", ...). Nil means the verdicts
	// could not be read: not recorded, which is not "no events".
	GuardrailOutcomes map[string]int
}

// Step kinds, as the run evidence records them (ADR-0042).
const (
	StepKindModelCall  = "model_call"
	StepKindToolCall   = "tool_call"
	StepKindRetrieval  = "retrieval"
	StepKindDelegation = "delegation"
	StepKindApproval   = "approval"
)

// AgentStep is one step of the case's run, read from the run evidence.
type AgentStep struct {
	Seq        int
	Kind       string
	Name       string
	ToolkitID  string
	DurationMs int64
	// Error is the step's error text; empty for a step that succeeded.
	Error string
	// InputsDigest identifies the step's arguments, so a repeated call can be
	// told from a different one; empty when the step had none.
	InputsDigest string
	// Output is the tool output, kept only when the suite stores them.
	Output string
	// Decision and GuardrailType are the summary of an approval step: the
	// worker records every gate that asked a person to decide (decision
	// "requested") with the gate's type. Empty for any other step.
	Decision      string
	GuardrailType string
}

// IsGovernanceGate reports whether the step is a sensitive-tool gate that
// stopped the run for an approval (ADR-0040 governance, and the ADR-0043
// quoted-data rule, which is a conditional sensitive-tool pause): an approval
// request whose gate type is a sensitive-tool one. A connection authorization
// or a clarifying question is not a guardrail decision.
func (s AgentStep) IsGovernanceGate() bool {
	return s.Kind == StepKindApproval && s.Decision == "requested" && strings.HasSuffix(s.GuardrailType, "sensitive_tool")
}

// IsToolStep reports whether the step is the agent using a tool: a tool call,
// a retrieval or a delegation to another agent.
func (s AgentStep) IsToolStep() bool {
	return s.Kind == StepKindToolCall || s.Kind == StepKindRetrieval || s.Kind == StepKindDelegation
}

// CountsAsStep reports whether the step is work the step limit counts.
func (s AgentStep) CountsAsStep() bool { return s.Kind == StepKindModelCall || s.IsToolStep() }

// CaseRunRecorder records the execution a case ran as, so its model calls are
// attributed to the run. The repository implements it; the orchestrator
// type-asserts it, as it does LimitMarker.
type CaseRunRecorder interface {
	RecordCaseRun(ctx context.Context, projectID, runID, caseID, executionID, conversationUUID string) error
}

// WithAgentRunner lets the orchestrator execute runtime_plane runs.
func (o *Orchestrator) WithAgentRunner(runner AgentRunner) *Orchestrator {
	o.runner = runner
	return o
}

// runtimeSettings are the runtime_plane controls of one run, from its frozen
// snapshot.
type runtimeSettings struct {
	stepLimit       int
	timeLimit       time.Duration
	keepToolOutputs bool
}

func settingsOf(run Run) runtimeSettings {
	settings := runtimeSettings{timeLimit: DefaultCaseTimeLimit, keepToolOutputs: true}
	limits := run.Snapshot.Limits
	if limits == nil {
		return settings
	}
	if limits.StepLimitPerCase != nil {
		settings.stepLimit = *limits.StepLimitPerCase
	}
	if limits.TimeLimitPerCaseSeconds != nil {
		settings.timeLimit = time.Duration(*limits.TimeLimitPerCaseSeconds) * time.Second
	}
	settings.keepToolOutputs = limits.KeepsToolOutputs()
	return settings
}

// agentCase is what one case produced, whichever mode ran it.
type agentCase struct {
	output string
	err    error
	// evidence is merged into every result row of the case.
	evidence map[string]any
	// trajectory is the case's steps, stored once (attachTrajectory); nil when
	// the run recorded none.
	trajectory map[string]any
}

// runtimeCase executes one case as an agent run and shapes the outcome the way
// the predict_blocking turn does: an answer, or an error the case stores.
func (o *Orchestrator) runtimeCase(
	ctx context.Context, ref RunRef, run Run, testCase DatasetCase, settings runtimeSettings,
) agentCase {
	request := AgentRunRequest{
		ProjectID: ref.ProjectID, RunID: ref.RunID, CaseID: testCase.ID,
		Input:     substitute(testCase.Input, testCase.Variables),
		StepLimit: settings.stepLimit, TimeLimit: settings.timeLimit,
		KeepToolOutputs: settings.keepToolOutputs,
		Admitted: func(admitCtx context.Context, executionID, conversationUUID string) {
			o.recordCaseRun(admitCtx, ref, testCase.ID, executionID, conversationUUID)
		},
	}
	if limits := run.Snapshot.Limits; limits != nil && limits.HasAny() {
		request.Limits = limits
	}
	if run.CreatedBy != nil {
		request.ActorUserID = *run.CreatedBy
	}
	if run.ApplicationID != nil {
		request.ApplicationID = *run.ApplicationID
	}
	if run.ApplicationVersionID != nil {
		request.VersionID = *run.ApplicationVersionID
	}

	started := time.Now()
	result, err := o.runner.RunCase(ctx, request)
	evidence := agentTurnEvidence(time.Since(started), nil)
	evidence["execution_mode"] = ExecutionModeRuntimePlane
	if result.ExecutionID != "" {
		evidence["execution_id"] = result.ExecutionID
	}
	if result.ConversationUUID != "" {
		evidence["conversation_uuid"] = result.ConversationUUID
	}
	// ABSENCE IS NOT ZERO: a run that recorded no steps (it never started, or
	// the step record is not composed) has no trajectory, not an empty one.
	var trajectory map[string]any
	if len(result.Steps) > 0 {
		trajectory = BuildTrajectory(result.Steps, settings.keepToolOutputs)
	}
	done := agentCase{evidence: evidence, trajectory: trajectory}
	if guardrail := BuildGuardrail(result.Steps, result.GuardrailOutcomes); guardrail != nil {
		evidence["guardrail"] = guardrail
	}

	switch {
	case err != nil:
		done.err = err
	case result.Outcome == AgentOutcomeTimedOut:
		evidence["agent_timed_out"] = true
		done.err = fmt.Errorf("%w: the case's agent run did not finish within %s and was cancelled",
			context.DeadlineExceeded, settings.timeLimit)
	case result.Outcome == AgentOutcomeStepLimit:
		evidence["agent_step_limit"] = true
		done.err = fmt.Errorf("%w (%d steps); the run was cancelled", ErrStepLimit, settings.stepLimit)
	case result.Outcome == AgentOutcomeLimit:
		evidence["agent_limit_reached"] = true
		done.err = errors.New(cleanText(result.Failure) + "; the run was cancelled")
	case result.Outcome != AgentOutcomeCompleted:
		reason := cleanText(result.Failure)
		if reason == "" {
			reason = "the agent run did not complete"
		}
		done.err = errors.New(reason)
	default:
		done.output = cleanText(result.Output)
	}
	return done
}

// cleanText makes text storable in a jsonb value: NUL bytes are removed and
// invalid UTF-8 is replaced, so one odd byte of a tool or model answer cannot
// make the whole result row fail to store.
func cleanText(text string) string {
	if strings.IndexByte(text, 0) >= 0 {
		text = strings.ReplaceAll(text, "\x00", "")
	}
	return strings.ToValidUTF8(text, "\uFFFD")
}

// recordCaseRun stores the execution a case ran as. A failure is logged and
// does not stop the case: the run's work is real, and the cost is only that
// its spend is not attributed until the next read finds the row.
func (o *Orchestrator) recordCaseRun(ctx context.Context, ref RunRef, caseID, executionID, conversationUUID string) {
	recorder, ok := o.repo.(CaseRunRecorder)
	if !ok {
		return
	}
	if err := recorder.RecordCaseRun(context.WithoutCancel(ctx), ref.ProjectID, ref.RunID, caseID, executionID, conversationUUID); err != nil {
		o.logger.Error("evaluation: could not record the case's agent run; its spend is not attributed",
			"project_id", ref.ProjectID, "run_id", ref.RunID, "case_id", caseID,
			"execution_id", executionID, "error", err)
	}
}

// maxTrajectoryItems bounds the per-step list kept in a case's evidence. The
// counts are always over every step; only the list is cut, and `truncated`
// says so.
const maxTrajectoryItems = 60

// maxToolOutputRunes bounds one kept tool output.
const maxToolOutputRunes = 1000

// BuildTrajectory turns a case's steps into the evidence a result row keeps
// (`evidence.trajectory`): the counts the Trajectory card reads and a bounded
// list of the steps.
//
// A RETRY is a tool step with the same kind, name, toolkit and arguments as an
// earlier step of the case that ended in an error: the agent tried the same
// call again. A different argument is a new call, not a retry.
//
// A REDUNDANT call is a tool step identical to an earlier step of the case
// (same kind, toolkit, name and arguments digest) that had already SUCCEEDED:
// the answer was in hand and the agent asked again. A repeat of a failed call
// is a retry and is not counted twice, so retries and redundant calls never
// overlap. DISTINCT tools are the unique (kind, toolkit, name) the case used.
func BuildTrajectory(steps []AgentStep, keepToolOutputs bool) map[string]any {
	type toolFigures struct {
		name, toolkit string
		calls, errors int
	}
	var steppedCount, modelCalls, toolCalls, toolErrors, retries, redundant int
	failed := map[string]bool{}
	succeeded := map[string]bool{}
	tools := map[string]*toolFigures{}
	var toolOrder []string
	items := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		if step.CountsAsStep() {
			steppedCount++
		}
		if step.Kind == StepKindModelCall {
			modelCalls++
		}
		if step.IsToolStep() {
			toolCalls++
			key := step.Kind + "\x00" + step.ToolkitID + "\x00" + step.Name
			figures := tools[key]
			if figures == nil {
				figures = &toolFigures{name: cleanText(step.Name), toolkit: cleanText(step.ToolkitID)}
				tools[key] = figures
				toolOrder = append(toolOrder, key)
			}
			figures.calls++
			call := key + "\x00" + step.InputsDigest
			if failed[call] {
				retries++
			} else if succeeded[call] {
				redundant++
			}
			if step.Error != "" {
				toolErrors++
				figures.errors++
				failed[call] = true
			} else {
				succeeded[call] = true
			}
		}
		if len(items) < maxTrajectoryItems {
			items = append(items, trajectoryItem(step, keepToolOutputs))
		}
	}
	toolList := make([]map[string]any, 0, len(toolOrder))
	for _, key := range toolOrder {
		figures := tools[key]
		toolList = append(toolList, map[string]any{
			"name": figures.name, "toolkit_id": figures.toolkit, "calls": figures.calls, "errors": figures.errors,
		})
	}
	return map[string]any{
		"steps":       steppedCount,
		"model_calls": modelCalls,
		"tool_calls":  toolCalls,
		"tool_errors": toolErrors,
		"retries":     retries,
		// Added with issue 2192; a case stored before has neither key, and the
		// card reads that as not recorded.
		"redundant_calls": redundant,
		"distinct_tools":  len(toolList),
		"tools":           toolList,
		"items":           items,
		"truncated":       len(steps) > maxTrajectoryItems,
	}
}

// BuildGuardrail turns the guardrail decisions recorded for a case's run into
// the evidence figures (`evidence.guardrail`). A guardrail EVENT is:
//
//   - an LLM gateway guardrail verdict (ADR-0043) whose outcome acted or
//     warned: blocked (and error_closed, a fail-closed detector that stopped
//     the request), masked, warned or flagged. A pass, and a failure under fail
//     mode open, changed nothing and are not events;
//   - a governance gate (ADR-0040/0043): a sensitive-tool approval request,
//     which is also what the quoted-data rule raises.
//
// A nil outcomes map means the verdicts could not be read, and the figures are
// absent (nil), not zero. Steps may be empty when the step record is not
// composed; the gate count is then 0 and the verdict counts still stand.
func BuildGuardrail(steps []AgentStep, outcomes map[string]int) map[string]any {
	if outcomes == nil {
		return nil
	}
	gated := 0
	for _, step := range steps {
		if step.IsGovernanceGate() {
			gated++
		}
	}
	blocked := outcomes["blocked"] + outcomes["error_closed"]
	masked := outcomes["masked"]
	warned := outcomes["warned"] + outcomes["flagged"]
	return map[string]any{
		"events":  blocked + masked + warned + gated,
		"blocked": blocked, "masked": masked, "warned": warned, "gated": gated,
	}
}

func trajectoryItem(step AgentStep, keepToolOutputs bool) map[string]any {
	item := map[string]any{
		"seq": step.Seq, "kind": cleanText(step.Kind), "name": cleanText(step.Name), "duration_ms": step.DurationMs,
	}
	if step.ToolkitID != "" {
		item["toolkit_id"] = cleanText(step.ToolkitID)
	}
	if step.Error != "" {
		item["error"] = cleanText(step.Error)
	}
	if keepToolOutputs && step.IsToolStep() && step.Output != "" {
		item["output"] = truncateRunes(cleanText(step.Output), maxToolOutputRunes)
	}
	return item
}

// EvidenceReadCheck answers whether the caller holds runs.evidence.read in the
// route's project. A nil check, an error and false all answer no: tool outputs
// are withheld unless the permission is known to be held.
type EvidenceReadCheck func(*http.Request) (bool, error)

func (check EvidenceReadCheck) holds(r *http.Request) bool {
	if check == nil {
		return false
	}
	held, err := check(r)
	return err == nil && held
}

// stripToolOutputs removes the tool outputs a runtime_plane case copied into
// its result's evidence. A copy is made for people who may read the run
// evidence, and a scorecard reader without runs.evidence.read must not get them
// through the result instead.
func stripToolOutputs(result *RunResult) {
	trajectory, ok := result.Evidence["trajectory"].(map[string]any)
	if !ok {
		return
	}
	items, ok := trajectory["items"].([]any)
	if !ok {
		// Rows read back from jsonb hold []any; rows built in process hold
		// []map[string]any.
		typed, typedOK := trajectory["items"].([]map[string]any)
		if !typedOK {
			return
		}
		for _, item := range typed {
			delete(item, "output")
		}
		return
	}
	for _, raw := range items {
		if item, isMap := raw.(map[string]any); isMap {
			delete(item, "output")
		}
	}
}
