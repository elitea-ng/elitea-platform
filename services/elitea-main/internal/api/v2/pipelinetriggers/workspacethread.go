package pipelinetriggers

// Schedules and webhook triggers that target a handed-off workspace thread
// (ADR-0045 decision 9, issue 1440, tenant 0159).
//
// A schedule or a trigger keeps its version route: the version is where the
// setting lives, who may change it, and what a governed mandate binds. With
// `target_kind: workspace_thread` an occurrence does not run the version. It
// starts a cloud turn on a Local work thread that was handed off to the
// cloud executor, as a message that a person posts on that thread starts
// one (handoffcontinue.Service.StartThreadTurn). This file holds the target:
// its body, its checks at save time, and the occurrence.
//
//   - THE TARGET. The thread (a conversation of this project), the prompt
//     each occurrence starts with, and the grant. The save checks that the
//     conversation is in the project, that the caller may write to it (a
//     user participant, the posted message's own rule), and that it was
//     handed off at least once. The thread's current state is not checked:
//     the occurrence checks it.
//   - THE GRANT. Only the `continue` and `chain` parts of the ADR-0045
//     decision 1 grant, and `goal_check`. A schedule or a webhook has no
//     person at the start, so `sensitive_tools`, `hook_asks` and
//     `remote_confirmations` are refused with 422 unattended_grant_refused
//     (decision 2); `on_approval_required` still applies. An absent grant is
//     the decision 10 default: continue at the step and output limits, with
//     the platform's chain limits and the goal check.
//   - THE OCCURRENCE. One new chain per occurrence, with the trigger origin
//     `schedule` or `webhook`, under the row's run identity (the author, the
//     creator, or the governed mandate). A thread that is not CLOUD_IDLE
//     starts nothing: the schedule records `skipped_thread_busy` and the
//     occurrence is used up (last_run is stamped, so the next attempt is
//     the next cron time), and the webhook answers 409 `thread_busy`. A
//     thread that is gone records the existing missing result. `last_execution_id` links to the run, and
//     its outcome comes as `run_finished`.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/unattendedwire"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/handoffcontinue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// TargetKindWorkspaceThread is the target of a schedule or a trigger that
// starts a cloud turn on a handed-off thread (0159).
const TargetKindWorkspaceThread = runmandate.ScheduleTargetWorkspaceThread

// maxThreadPrompt bounds the prompt, in characters (the continue prompt's
// bound, workspacehandoff.MaxContinuePromptRunes).
const maxThreadPrompt = workspacehandoff.MaxContinuePromptRunes

// ThreadRuns is the handed-off thread seam (*handoffcontinue.Service).
type ThreadRuns interface {
	HandedOffThread(
		ctx context.Context, projectID int64, conversationUUID string,
	) (handoffcontinue.ThreadTarget, bool, error)
	RunActive(ctx context.Context, projectID int64, executionID string) (bool, error)
	StartThreadTurn(ctx context.Context, turn handoffcontinue.ThreadTurn) (handoffcontinue.ThreadTurnStarted, error)
}

// WithThreadRuns lets a schedule or a trigger target a handed-off thread.
// Without it a save that names `workspace_thread` answers 503, and an
// occurrence of a stored one fails.
func WithThreadRuns(runs ThreadRuns) Option {
	return func(h *Handler) {
		if present(runs) {
			h.threads = runs
		}
	}
}

// ErrThreadRunsUnavailable: this deployment cannot start a turn on a
// handed-off thread (no cloud executor, or no runtime).
var ErrThreadRunsUnavailable = errors.New("pipelinetriggers: workspace thread targets are not available on this deployment")

// threadTarget is the stored thread target (0159). Every field is nil for
// any other kind.
type threadTarget struct {
	ConversationUUID *string
	Prompt           *string
	// Grant is the stored grant (threadGrantBody, JSON); nil is the
	// default grant.
	Grant []byte
}

// grantArg is the grant as a statement argument: NULL, or the JSON text.
func (t threadTarget) grantArg() any {
	if len(t.Grant) == 0 {
		return nil
	}
	return string(t.Grant)
}

func (t threadTarget) conversation() string {
	if t.ConversationUUID == nil {
		return ""
	}
	return *t.ConversationUUID
}

func (t threadTarget) prompt() string {
	if t.Prompt == nil {
		return ""
	}
	return *t.Prompt
}

// threadGrantBody is the stored and the shown grant: the `continue` and
// `chain` parts and `goal_check`, in the wire form of ChatUnattendedRequest.
type threadGrantBody struct {
	ContinueStepLimit   bool                  `json:"continue_step_limit"`
	ContinueOutputLimit bool                  `json:"continue_output_limit"`
	GoalCheck           bool                  `json:"goal_check"`
	Chain               *unattendedwire.Chain `json:"chain,omitempty"`
}

// defaultThreadGrant is the ADR-0045 decision 10 grant: continue at the
// step and the output limits, the platform's chain limits, and the goal
// check.
var defaultThreadGrant = threadGrantBody{ContinueStepLimit: true, ContinueOutputLimit: true, GoalCheck: true}

// request is the occurrence's grant.
func (g threadGrantBody) request() *unattended.Request {
	return &unattended.Request{
		ContinueStepLimit: g.ContinueStepLimit, ContinueOutputLimit: g.ContinueOutputLimit,
		GoalCheck: g.GoalCheck, Chain: g.Chain.Request(),
	}
}

// storedGrant reads the stored grant; nil or unreadable is the default.
func storedGrant(raw []byte) threadGrantBody {
	if len(raw) == 0 {
		return defaultThreadGrant
	}
	var grant threadGrantBody
	if err := json.Unmarshal(raw, &grant); err != nil {
		return defaultThreadGrant
	}
	return grant
}

// threadGrantWire is what a body's `grant` may hold: the cloud start's
// grant (ChatUnattendedRequest) and the desktop parts, so a refused part is
// named rather than ignored.
type threadGrantWire struct {
	GrantID              string                `json:"grant_id"`
	ContinueStepLimit    bool                  `json:"continue_step_limit"`
	ContinueOutputLimit  bool                  `json:"continue_output_limit"`
	ContinueToolCallCap  bool                  `json:"continue_tool_call_cap"`
	HookAsks             bool                  `json:"hook_asks"`
	RemoteConfirmations  bool                  `json:"remote_confirmations"`
	SensitiveTools       []unattended.Tool     `json:"sensitive_tools"`
	GoalCheck            *bool                 `json:"goal_check"`
	Chain                *unattendedwire.Chain `json:"chain"`
	ChainRootExecutionID string                `json:"chain_root_execution_id"`
	Kind                 string                `json:"kind"`
}

// threadTargetBody is the target part of a schedule or a trigger body.
type threadTargetBody struct {
	TargetKind       *string          `json:"target_kind"`
	ConversationUUID *string          `json:"conversation_uuid"`
	Prompt           *string          `json:"prompt"`
	Grant            *json.RawMessage `json:"grant"`
}

// requestedTarget is what a body asks for.
type requestedTarget struct {
	// named: the body has `target_kind`. Without it the stored target stays.
	named bool
	// thread: the body names `workspace_thread`.
	thread bool
	// kind is the `target_kind` the body names, when named.
	kind   string
	target threadTarget
	grant  *threadGrantWire
}

// errInvalidThreadTarget is a target the body cannot hold; its text is the
// 400 answer.
type errInvalidThreadTarget struct{ message string }

func (e *errInvalidThreadTarget) Error() string { return e.message }

func invalidTarget(message string) error { return &errInvalidThreadTarget{message: message} }

// parseThreadTarget reads the target from a settings body. An absent body,
// or one with no `target_kind`, names no target.
func parseThreadTarget(raw []byte) (requestedTarget, error) {
	var requested requestedTarget
	if len(bytes.TrimSpace(raw)) == 0 {
		return requested, nil
	}
	var body threadTargetBody
	if err := json.Unmarshal(raw, &body); err != nil {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil {
			for _, field := range []string{"target_kind", "conversation_uuid", "prompt", "grant"} {
				if _, ok := probe[field]; ok {
					return requested, invalidTarget("`" + field + "` has the wrong type")
				}
			}
		}
		return requested, nil
	}
	if body.TargetKind == nil {
		if body.ConversationUUID != nil || body.Prompt != nil || body.Grant != nil {
			return requested, invalidTarget("`conversation_uuid`, `prompt` and `grant` need `target_kind: workspace_thread`")
		}
		return requested, nil
	}
	requested.named = true
	kind := strings.TrimSpace(*body.TargetKind)
	requested.kind = kind
	switch kind {
	case TargetKindWorkspaceThread:
	case TargetKindPipeline, TargetKindAgent:
		if body.ConversationUUID != nil || body.Prompt != nil || body.Grant != nil {
			return requested, invalidTarget("`conversation_uuid`, `prompt` and `grant` belong to `target_kind: workspace_thread` only")
		}
		return requested, nil
	default:
		return requested, invalidTarget("`target_kind` must be `workspace_thread`, `pipeline` or `agent`")
	}
	requested.thread = true
	if body.ConversationUUID == nil || !canonicalUUID(*body.ConversationUUID) {
		return requested, invalidTarget("`conversation_uuid` must be the thread's conversation uuid")
	}
	if body.Prompt == nil || !validThreadPrompt(*body.Prompt) {
		return requested, invalidTarget("`prompt` must be 1 to 32768 characters of UTF-8 text without NUL characters")
	}
	conversation := strings.ToLower(*body.ConversationUUID)
	prompt := *body.Prompt
	requested.target = threadTarget{ConversationUUID: &conversation, Prompt: &prompt}
	if body.Grant != nil && string(*body.Grant) != "null" {
		var grant threadGrantWire
		if err := json.Unmarshal(*body.Grant, &grant); err != nil {
			return requested, invalidTarget("`grant` must be an unattended grant object")
		}
		requested.grant = &grant
	}
	return requested, nil
}

// scheduleKindError: a schedule runs a pipeline or starts a thread turn; it
// has no `agent` target (an agent version is only a thread target's anchor).
func (r requestedTarget) scheduleKindError() error {
	if r.named && !r.thread && r.kind != TargetKindPipeline {
		return invalidTarget("`target_kind` of a schedule must be `pipeline` or `workspace_thread`")
	}
	return nil
}

// triggerKindError: a trigger's `pipeline` or `agent` kind is the version's
// own kind; a body that names the other one contradicts the version.
func (r requestedTarget) triggerKindError(versionKind string) error {
	if r.named && !r.thread && r.kind != versionKind {
		return invalidTarget("`target_kind` must be `" + versionKind + "` (this version's kind) or `workspace_thread`")
	}
	return nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

func validThreadPrompt(prompt string) bool {
	return strings.TrimSpace(prompt) != "" && utf8.ValidString(prompt) &&
		utf8.RuneCountInString(prompt) <= maxThreadPrompt && !strings.ContainsRune(prompt, 0)
}

// admitThreadGrant checks the grant against ADR-0045 decision 2 and the
// platform's chain limits, and answers the stored form. A refused part is
// an unattended.RefusalError (422 unattended_grant_refused).
func (h *Handler) admitThreadGrant(ctx context.Context, grant *threadGrantWire) ([]byte, error) {
	if grant == nil {
		return nil, nil
	}
	switch {
	case len(grant.SensitiveTools) > 0:
		return nil, unattended.Refused("sensitive_tools", "", unattended.RefusedOrigin)
	case grant.HookAsks:
		return nil, unattended.Refused("hook_asks", "", unattended.RefusedOrigin)
	case grant.RemoteConfirmations:
		return nil, unattended.Refused("remote_confirmations", "", unattended.RefusedOrigin)
	case grant.ContinueToolCallCap:
		// A desktop part: a cloud turn has no tool call cap.
		return nil, unattended.Refused("continue_tool_call_cap", "", unattended.RefusedInvalid)
	case grant.ChainRootExecutionID != "":
		// Each occurrence starts its own chain.
		return nil, unattended.Refused("chain_root_execution_id", "", unattended.RefusedInvalid)
	case grant.Kind != "":
		return nil, unattended.Refused("kind", "", unattended.RefusedInvalid)
	}
	settings, err := platformconfig.LoadUnattendedSettings(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	if _, err := unattended.ResolveLimits(settings, grant.Chain.Request(), 0); err != nil {
		return nil, err
	}
	stored := threadGrantBody{
		ContinueStepLimit: grant.ContinueStepLimit, ContinueOutputLimit: grant.ContinueOutputLimit,
		GoalCheck: grant.GoalCheck == nil || *grant.GoalCheck, Chain: grant.Chain,
	}
	return json.Marshal(stored)
}

// codeThreadNotGovernable: credential governance is on, and the thread's
// next turn does not address an agent at the version the row's mandate
// binds (a model thread, or another version). Every occurrence would skip,
// so the save is refused.
const codeThreadNotGovernable = "thread_not_governable"

// checkThreadTarget is the save's check of a thread target: the grant, the
// conversation in this project, the caller's right to write to it, a
// handoff row, and under credential governance an agent participant at the
// row's version. It answers the target to store; false means the response
// is written.
func (h *Handler) checkThreadTarget(
	w http.ResponseWriter, r *http.Request, projectID, versionID int64, requested requestedTarget,
) (threadTarget, bool) {
	if h.threads == nil {
		writeError(w, http.StatusServiceUnavailable, ErrThreadRunsUnavailable.Error())
		return threadTarget{}, false
	}
	target := requested.target
	grant, err := h.admitThreadGrant(r.Context(), requested.grant)
	if err != nil {
		if unattendedwire.WriteRefusal(w, err) {
			return threadTarget{}, false
		}
		h.log().Error("pipelinetriggers: read the unattended settings", "err", err)
		writeError(w, http.StatusInternalServerError, "the grant could not be checked")
		return threadTarget{}, false
	}
	if requested.grant != nil {
		target.Grant = grant
	}
	conversation := target.conversation()
	err = repos.NewConversationsRepo(h.pool).AuthorizeChatWrite(r.Context(),
		strconv.FormatInt(projectID, 10), "conversation", conversation)
	var refused *apierr.APIError
	switch {
	case errors.As(err, &refused) && (refused.Status == http.StatusNotFound || refused.Status == http.StatusForbidden):
		// A conversation the caller may not write answers as a missing one.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such conversation in this project",
			"code":  "thread_not_found",
		})
		return threadTarget{}, false
	case err != nil:
		h.log().Error("pipelinetriggers: check the thread", "err", err)
		writeError(w, http.StatusInternalServerError, "the conversation could not be checked")
		return threadTarget{}, false
	}
	thread, handedOff, err := h.threads.HandedOffThread(r.Context(), projectID, conversation)
	if err != nil {
		h.log().Error("pipelinetriggers: read the thread's handoff", "err", err)
		writeError(w, http.StatusInternalServerError, "the conversation could not be checked")
		return threadTarget{}, false
	}
	if !handedOff {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "this conversation was never handed off to the cloud; hand it off from the desktop first",
			"code":  "not_handed_off",
		})
		return threadTarget{}, false
	}
	// GOVERNED: a mandate binds the version, so only an agent thread at that
	// version can run under it. The occurrence checks again
	// (skipped_mandate_scope), for a thread whose participant changes later.
	if h.governed != nil && (!thread.Agent || thread.VersionID != versionID) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "credential governance is on: a governed run is bound to this version, and this thread's " +
				"next turn does not address an agent at this version",
			"code": codeThreadNotGovernable,
		})
		return threadTarget{}, false
	}
	return target, true
}

// threadView is the target part of a schedule or a trigger read.
type threadView struct {
	ConversationUUID string           `json:"conversation_uuid,omitempty"`
	Prompt           string           `json:"prompt,omitempty"`
	Grant            *threadGrantBody `json:"grant,omitempty"`
}

func threadViewOf(kind string, target threadTarget) threadView {
	if kind != TargetKindWorkspaceThread {
		return threadView{}
	}
	grant := storedGrant(target.Grant)
	return threadView{ConversationUUID: target.conversation(), Prompt: target.prompt(), Grant: &grant}
}

// threadRunText is the text an occurrence starts with. A schedule's is the
// prompt. A webhook's is the prompt, then the caller's `input`, or the
// delivery's payload inside the untrusted-data envelope (agentrun.go). The
// payload is never the prompt.
func threadRunText(prompt, input string, payload []byte, source payloadSource) (string, string, error) {
	extra, fromPayload, err := agentRunInput(input, payload, source)
	switch {
	case errors.Is(err, ErrAgentInputRequired):
		return prompt, "", nil
	case err != nil:
		return "", "", err
	case fromPayload:
		return prompt + "\n\n" + extra, inputSourcePayload, nil
	}
	return prompt + "\n\n" + extra, inputSourceInput, nil
}

// ErrThreadNotWritable: the run identity is no longer a user participant of
// the thread, so it may not post the occurrence's message. It is an
// ErrRunForbidden: the schedule records skipped_unauthorized, and the
// webhook gets the refusal for a missing permission.
var ErrThreadNotWritable = fmt.Errorf("%w: the run identity is not a participant of the workspace thread", ErrRunForbidden)

// admitThread is admit for a `workspace_thread` target: authorize the run
// identity and its right to write to the thread, then start the cloud turn
// on the thread.
func (h *Handler) admitThread(
	ctx context.Context, schema string, request runRequest, target threadTarget, questionID string,
) (runOutcome, error) {
	if h.threads == nil {
		return runOutcome{}, ErrThreadRunsUnavailable
	}
	if err := checkRunInput(request.Input); err != nil {
		return runOutcome{}, err
	}
	ctx, request, err := h.authorizeRun(ctx, request)
	if err != nil {
		return runOutcome{}, err
	}
	// The turn runs as the run identity and its policy row names it as the
	// owner, so an identity that is not a user is refused before the start.
	// A schedule's author and a trigger's creator are always users; this
	// stops a row that holds no identity from running with no owner, or
	// with the previous turn's policy.
	if request.ActorUserID <= 0 {
		return runOutcome{}, ErrRunForbidden
	}
	// The save checked the right to write to the thread (checkThreadTarget);
	// it is checked again for each occurrence, so an author removed from
	// the thread since stops the occurrence.
	if err := h.threadWritable(ctx, schema, request.ProjectID, request.ActorUserID, target.conversation()); err != nil {
		return runOutcome{}, err
	}
	text, inputSource, err := threadRunText(target.prompt(), request.Input, request.Payload, request.PayloadSource)
	if err != nil {
		return runOutcome{}, err
	}
	if request.TriggerID != 0 {
		// The agent trigger's run limit, over the thread's turns this
		// trigger started.
		if err := h.threadTriggerAdmissible(ctx, schema, request.TriggerID, target.conversation()); err != nil {
			return runOutcome{}, err
		}
	}
	run, err := h.threadRun(ctx, request)
	if err != nil {
		return runOutcome{}, err
	}
	turn := handoffcontinue.ThreadTurn{
		ProjectID: request.ProjectID, ConversationUUID: target.conversation(), ActorUserID: request.ActorUserID,
		QuestionID: questionID, Prompt: text, Grant: storedGrant(target.Grant).request(),
		TriggerOrigin: executionTriggerOrigin(request.Origin), Run: run,
	}
	if request.Authority != nil {
		// A governed run: the mandate binds the version, so the thread's
		// agent must be that version.
		turn.VersionID = request.VersionID
	}
	started, err := h.threads.StartThreadTurn(ctx, turn)
	if err != nil {
		if errors.Is(err, handoffcontinue.ErrThreadVersion) && request.Authority != nil && h.governed != nil {
			// A mandate refusal like the fire-time check's: the security
			// audit records MANDATE_SCOPE_MISMATCH (scheduleAuthority).
			if auditErr := h.governed.Refuse(ctx, request.MandateTarget, request.Authority.MandateID,
				fmt.Errorf("%w: %w", runmandate.ErrScopeMismatch, err)); auditErr != nil {
				h.log().Error("pipelinetriggers: record the mandate refusal",
					"project_id", request.ProjectID, "err", auditErr)
			}
		}
		return runOutcome{}, err
	}
	return runOutcome{
		ExecutionID: started.ExecutionID, ConversationUUID: started.ConversationUUID,
		ResponseMessageID: started.ResponseMessageID, VersionID: request.VersionID, InputSource: inputSource,
	}, nil
}

// threadRun is what the turn's admission records (agentexecution.ThreadRun):
// the row that started it, and the run's on_approval_required snapshot
// (ADR-0041 decision 5). The admission writes the snapshot over the
// thread's previous one, behind the CLOUD_IDLE fence, so a refused
// occurrence never changes the snapshot of a turn that runs. An error
// refuses the run, as admit's recordRunApprovalPolicy does.
func (h *Handler) threadRun(ctx context.Context, request runRequest) (*agentexecutionapp.ThreadRun, error) {
	run := &agentexecutionapp.ThreadRun{TriggerID: request.TriggerID, ScheduleID: request.ScheduleID}
	action, err := h.effectiveOnApprovalRequired(ctx, request.ProjectID, approvalTargetOf(request.Origin), request.VersionID)
	if err != nil {
		return nil, fmt.Errorf("pipelinetriggers: read the run's approval action: %w", err)
	}
	run.Policy = &agentexecutionapp.RunPolicy{
		VersionID: request.VersionID, OwnerUserID: request.ActorUserID, OnApprovalRequired: action,
	}
	return run, nil
}

// threadWritable is the save's write check (AuthorizeChatWrite: a user
// participant of the conversation) for the run identity at fire time. A
// conversation the identity may not write is ErrThreadNotWritable; one that
// is gone is workspacehandoff.ErrNotFound, the existing missing result.
func (h *Handler) threadWritable(ctx context.Context, schema string, projectID, userID int64, conversationUUID string) error {
	id := strconv.FormatInt(userID, 10)
	err := repos.NewConversationsRepo(h.pool).AuthorizeChatWrite(
		auth.ContextWithUser(ctx, auth.User{ID: id, UserID: id}),
		strconv.FormatInt(projectID, 10), "conversation", conversationUUID)
	var refused *apierr.APIError
	switch {
	case errors.As(err, &refused) && (refused.Status == http.StatusNotFound || refused.Status == http.StatusForbidden):
		var exists bool
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT EXISTS (SELECT 1 FROM %s.chat_conversations WHERE uuid = $1::uuid)`, schema),
			conversationUUID).Scan(&exists); err != nil {
			return fmt.Errorf("pipelinetriggers: read the thread: %w", err)
		}
		if !exists {
			return workspacehandoff.ErrNotFound
		}
		return ErrThreadNotWritable
	case err != nil:
		return fmt.Errorf("pipelinetriggers: check the thread's writers: %w", err)
	}
	return nil
}

// threadTriggerAdmissible is agentTriggerAdmissible for a trigger that
// targets a handed-off thread. It makes no conversation of its own, so the
// limit counts the thread's questions this trigger started (the question's
// `trigger_id` meta, agentexecution.ThreadRun) and the thread's answers
// that still stream. The limits, the refusal (ErrRunLimited, 429) and the
// UTC wall-clock window are the same.
func (h *Handler) threadTriggerAdmissible(ctx context.Context, schema string, triggerID int64, conversationUUID string) error {
	var recent, inFlight int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT
    count(*) FILTER (WHERE question.meta ->> 'trigger_id' = $2
                       AND question.created_at > (now() AT TIME ZONE 'UTC') - $3::interval),
    count(*) FILTER (WHERE question.is_streaming
                       AND question.created_at > (now() AT TIME ZONE 'UTC') - $4::interval)
  FROM %[1]s.chat_message_group AS question
  JOIN %[1]s.chat_conversations AS conversation ON conversation.id = question.conversation_id
 WHERE conversation.uuid = $1::uuid
   AND question.created_at > (now() AT TIME ZONE 'UTC') - GREATEST($3::interval, $4::interval)`, schema),
		conversationUUID, strconv.FormatInt(triggerID, 10), agentTriggerWindow, agentTriggerInFlightHorizon,
	).Scan(&recent, &inFlight)
	if err != nil {
		return fmt.Errorf("pipelinetriggers: read the thread trigger's run limit: %w", err)
	}
	if recent >= agentTriggerMaxPerWindow || inFlight >= agentTriggerMaxInFlight {
		return ErrRunLimited
	}
	return nil
}

// threadRunsActive is the schedule's overlap check for a thread target: the
// run its last occurrence started still goes (its execution, or its chain).
func (h *Handler) threadRunActive(ctx context.Context, projectID int64, schedule scheduleRow) (bool, error) {
	if h.threads == nil || schedule.LastExecutionID == nil {
		return false, nil
	}
	return h.threads.RunActive(ctx, projectID, *schedule.LastExecutionID)
}

var scheduleOccurrenceNamespace = uuid.MustParse("6b1f0f9e-2c4d-4e7a-9f31-5d8a0c7e2b44")

// scheduleQuestionID is the question of one occurrence: derived from the
// schedule and the tick, so a repeated fire of the same occurrence is the
// same start.
func scheduleQuestionID(projectID, scheduleID int64, now time.Time) string {
	return uuid.NewSHA1(scheduleOccurrenceNamespace, []byte(strconv.FormatInt(projectID, 10)+"\x00"+
		strconv.FormatInt(scheduleID, 10)+"\x00"+strconv.FormatInt(now.UTC().Unix(), 10))).String()
}

// threadOutcome maps a refused thread occurrence to the schedule's
// last_result and its detail; "" for an error that is not one of them.
func threadOutcome(err error) (string, string) {
	switch {
	case errors.Is(err, ErrThreadNotWritable):
		return resultSkippedAuth, "the schedule author is no longer a participant of the workspace thread"
	case errors.Is(err, handoffcontinue.ErrThreadBusy):
		return resultSkippedThreadBusy, "the workspace thread was not idle in the cloud; no turn started"
	case errors.Is(err, workspacehandoff.ErrNotFound):
		return resultSkippedMissing, "the workspace thread is gone, or it is no longer handed off"
	case errors.Is(err, handoffcontinue.ErrThreadVersion):
		return resultSkippedMandateScope,
			"the thread's agent is not the version this schedule's mandate binds; no turn started"
	case errors.Is(err, ErrThreadRunsUnavailable), errors.Is(err, workspacehandoff.ErrUnavailable):
		return resultFailed, "this deployment cannot start a turn on a handed-off thread"
	}
	if refusal, ok := unattended.AsRefusal(err); ok {
		return resultFailed, "the schedule's grant was refused: " + refusal.Reason
	}
	return "", ""
}

// isGrantRefusal: the start refused the stored grant (422
// unattended_grant_refused), for example a chain limit over a maximum that an
// administrator lowered after the save.
func isGrantRefusal(err error) bool {
	_, ok := unattended.AsRefusal(err)
	return ok
}
