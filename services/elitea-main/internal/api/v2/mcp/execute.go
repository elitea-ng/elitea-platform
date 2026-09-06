package mcp

// Running an agent tool — the execution half of `tools/call`.
//
// MCP `tools/call` is ONE request and ONE response. The agent runtime is
// admit-then-stream: `StartCurrentApplication` returns as soon as the turn is
// durably admitted, and the answer arrives later on the execution's replay
// stream and in the chat projection. This file is the bridge between the two
// shapes: admit, then wait, bounded, for the turn to settle, then hand the
// assistant's text back as the tool result.
//
// # THE DECISION: an MCP run creates a conversation
//
// The choice was between (a) admitting a turn with no chat rows at all — which
// `AdmissionService.Submit` permits, every `Current*Turn` field being optional
// — and (b) creating a conversation so the run is an ordinary turn like any
// other. This is (b), for three reasons, in order of weight:
//
//  1. (a) IS NOT ACTUALLY AVAILABLE THROUGH THIS SEAM. `StartUseCase` is the
//     dependency this package is given, and every one of its entry points
//     requires a conversation: `CurrentApplicationStartRequest.Validate`
//     demands a `ConversationUUID` and a `TargetParticipantID`, and
//     `ResolveCurrentApplication` then RE-RESOLVES the target by joining
//     `chat_participant_mapping` → `chat_participants` →
//     `application_versions` (internal/db/queries/agent_chat.sql,
//     ResolveCurrentApplicationTurn). Choosing (a) would mean reaching past
//     the use case to `AdmissionService.Submit` directly and assembling the
//     runtime input — variables, frozen version details, chat history, LLM
//     settings, guardrails, skills — a second time. Two assemblers for one
//     contract is how the support assistant explicitly refused to be built.
//  2. An operator asked "what did Claude Desktop just run against my project?"
//     needs an answer. With a conversation the run is a normal transcript: it
//     shows up in the chat list, the message-trace machinery applies, cancel
//     works, and the usual budget and governance attribution is unchanged.
//     Under (a) the only trace is the runtime execution row.
//  3. pylon's `do_predict` is the reference for (a), and it is not free of
//     conversations either — it is reached from a socket that already has one.
//     The reference's shape is not an argument for a transcript-less run here.
//
// What (b) COSTS, stated rather than hidden: every `tools/call` writes rows. A
// client that calls a tool a hundred times leaves a hundred conversations. They
// are marked `source = 'mcp'` so they can be told apart from what a person
// typed, and they are NOT hidden — a hidden conversation would defeat reason 2.
//
// # What is SYNTHESISED, on the record
//
// `SubmitRequest.Validate` requires `ClientStreamID`, `ClientMessageID` and
// `SIOEvent`, all three of which exist for a BROWSER: they are how a socket.io
// frame finds the tab that is waiting for it. An MCP client has no socket and
// no tab. They are supplied anyway, by the use case rather than by this file —
// the conversation uuid, the question-derived response message uuid, and the
// literal `chat_predict` — and this run does not read any of them back.
//
// That is a synthesis and not a fact about the caller, and it is worth knowing
// because it is load-bearing elsewhere: `chat_predict` is one of only two
// values `AgentExecutionDispatch.Validate` accepts, and the terminal projection
// matches the response group on `client_message_id` and `client_stream_id`, so
// an MCP turn is indistinguishable from a chat turn to everything downstream.
// The bridge below relies on exactly that — it reads the same projected row the
// browser would have streamed.
//
// The question id is minted SERVER-SIDE (a fresh uuid per call), which differs
// from every other caller of this use case: the browser and the support widget
// send their own, so a retried POST resumes the same turn instead of starting a
// second one. MCP has no equivalent — `tools/call` carries no idempotency key —
// so a client that retries a call gets a second run. Inventing a key from the
// tool name and arguments would be worse: two deliberate identical calls would
// then silently return the first one's answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

// mcpConversationSource marks the conversations this file creates.
//
// It is a new `chat_conversations.source` value alongside pylon's `elitea` and
// the support assistant's `support`. The column is a free VARCHAR(64) with no
// enumeration to extend, and the listing endpoint filters on `source` only when
// a caller asks for one, so an MCP run appears in the ordinary chat list — see
// reason 2 above — while still being separable with `?source=mcp`.
const mcpConversationSource = "mcp"

// applicationEntityName is the `chat_participants.entity_name` an agent
// participant carries. The turn resolver matches on the literal string.
const applicationEntityName = "application"

// userEntityName is the caller's own participant. The turn resolver requires
// one whose `entity_meta ->> 'id'` is the acting user: a conversation with no
// user participant cannot have a question attributed to anybody.
const userEntityName = "user"

// mcpRunDeadline bounds one `tools/call`.
//
// It is a REAL bound and not a formality: an agent run has no upper duration of
// its own — it can loop over tools for minutes — while the MCP client on the
// other end of this HTTP request has its own timeout, and a client that gives
// up sees a transport error rather than a tool result. 90 seconds is chosen to
// sit UNDER the timeouts clients actually use (the MCP TypeScript SDK's default
// request timeout is 60 s, its maximum 300 s; Go's own `http.Client` has none
// but every proxy in front of one has 60–120 s) and comfortably ABOVE an
// ordinary agent turn.
//
// On expiry the answer is an `isError` result NAMING THE EXECUTION ID, never an
// empty success and never a silent truncation: the run is still going, and the
// execution id is what lets a person find it — in the chat transcript, in the
// traces, or to cancel it. This package's rule (see the handler.go header) is
// that an empty successful result reads to an agent host as "the tool ran and
// produced nothing", which would be a lie here.
const mcpRunDeadline = 90 * time.Second

// The settle poll. The chat projection commits the terminal state; there is no
// notification channel this package can subscribe to without taking on the SSE
// stack's authorization and cursor machinery, so it polls. The interval backs
// off so a long run is not a busy loop, and the first polls are quick because a
// short answer is the common case.
const (
	mcpPollInitialInterval = 250 * time.Millisecond
	mcpPollMaxInterval     = 2 * time.Second
)

// ToolkitExecutionUnavailableReason is what a `tools/call` naming a TOOLKIT
// tool gets when this deployment did not compose the separately gated durable
// direct-read runtime.
const ToolkitExecutionUnavailableReason = "this MCP server can list this project's toolkit tools but cannot run " +
	"them on this deployment because the durable direct-tool runtime is disabled. Agent tools in this project " +
	"may still be available. Nothing was executed and nothing was changed."

// runToolkitTool admits one exact catalog-selected operation and waits for its
// fenced terminal result. Unlike an agent tool it creates no conversation: a
// direct operation already has a durable execution record and one typed result.
func (h *Handler) runToolkitTool(
	ctx context.Context,
	projectID int64,
	actorUserID int64,
	tool Tool,
	arguments map[string]any,
) map[string]any {
	if projectID <= 0 || projectID > math.MaxInt32 || actorUserID <= 0 || actorUserID > math.MaxInt32 ||
		tool.toolkitID <= 0 || tool.toolkitID > math.MaxInt32 || !tool.runnableToolkit() {
		return errorResult("the toolkit invocation identity is invalid, so nothing was executed")
	}

	deadline, cancel := context.WithTimeout(ctx, mcpRunDeadline)
	defer cancel()
	project := strconv.FormatInt(projectID, 10)
	actor := strconv.FormatInt(actorUserID, 10)
	outcome, err := h.toolkitExecute.Execute(deadline, toolkitexecutionapp.ExecuteCurrentReadToolRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: project, ResourceProjectID: project,
			ProjectionProjectID: project, ActorID: actor,
		},
		IdempotencyKey: uuid.NewString(),
		ProjectID:      int32(projectID),
		ActorID:        int32(actorUserID),
		ToolkitID:      int32(tool.toolkitID),
		ToolName:       tool.toolkitToolName,
		Arguments:      arguments,
	})
	if err != nil {
		if outcome.ExecutionID != "" {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return errorResult(fmt.Sprintf(
					"the toolkit operation '%s' did not finish within %s. It remains a durable execution %s; "+
						"no partial result is reported here.",
					tool.Name, mcpRunDeadline, outcome.ExecutionID,
				))
			case errors.Is(err, context.Canceled):
				return errorResult(fmt.Sprintf(
					"the request ended while toolkit operation '%s' was running as durable execution %s; "+
						"no partial result is reported here.",
					tool.Name, outcome.ExecutionID,
				))
			case errors.Is(err, toolkitexecutionapp.ErrToolkitExecuteReadResultMismatch):
				return errorResult(fmt.Sprintf(
					"the result for toolkit operation '%s' was rejected because it did not match execution %s.",
					tool.Name, outcome.ExecutionID,
				))
			default:
				return errorResult(fmt.Sprintf(
					"the toolkit operation '%s' failed as execution %s; no result is reported here.",
					tool.Name, outcome.ExecutionID,
				))
			}
		}
		slog.Default().WarnContext(ctx, "external MCP toolkit operation was not admitted",
			"event", "mcp.toolkit.admission_failed",
			"project_id", projectID,
			"toolkit_id", tool.toolkitID,
			"tool_name", tool.toolkitToolName,
			"admission_stage", toolkitexecutionapp.CurrentReadToolAdmissionStageOf(err),
			"error_code", currentReadToolAdmissionErrorCode(err),
		)
		switch {
		case errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolkitNotVisible),
			errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolNotSelected):
			return errorResult("the toolkit operation is no longer exposed by this project; nothing was executed")
		case errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolRestricted):
			return errorResult("the toolkit operation is restricted by current policy; nothing was executed")
		default:
			return errorResult("the toolkit operation could not be admitted; nothing was executed")
		}
	}
	return toolkitResult(tool.Name, outcome.Completion.ResultJSON)
}

func currentReadToolAdmissionErrorCode(err error) string {
	switch {
	case errors.Is(err, toolkitexecutionapp.ErrInvalidCurrentReadTool):
		return "invalid_request"
	case errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolkitNotVisible):
		return "toolkit_not_visible"
	case errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolNotSelected):
		return "tool_not_selected"
	case errors.Is(err, toolkitexecutionapp.ErrCurrentReadToolRestricted):
		return "policy_restricted"
	case errors.Is(err, toolkitexecutionapp.ErrInvalidAuthoritativeToolkitReadInput):
		return "invalid_frozen_input"
	case errors.Is(err, toolkitexecutionapp.ErrInvalidToolkitExecuteReadAdmission):
		return "invalid_durable_admission"
	default:
		return "dependency_failure"
	}
}

func toolkitResult(toolName string, result json.RawMessage) map[string]any {
	var text string
	if err := json.Unmarshal(result, &text); err != nil {
		text = string(result)
	}
	if strings.TrimSpace(text) == "" {
		return errorResult("the toolkit operation '" + toolName + "' finished without producing any content")
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// runAgentTool admits one turn and waits for it, bounded.
//
// It returns the CallToolResult body — either the agent's answer, or an
// `isError` result whose text says what happened. It never returns a successful
// result with no content.
func (h *Handler) runAgentTool(
	ctx context.Context,
	schema string,
	projectID int64,
	actorUserID int64,
	tool Tool,
	task string,
) map[string]any {
	conversationUUID, participantID, err := h.prepareRunConversation(
		ctx, schema, projectID, actorUserID, tool,
	)
	if err != nil {
		// The cause is not put on the wire — this package never sends
		// err.Error() — but it is the difference between "your call was wrong"
		// and "this deployment is broken", so it is a result rather than a
		// protocol error and it says which of the two it is.
		return errorResult("could not prepare a conversation for this run, so nothing was executed")
	}

	questionID := uuid.NewString()
	outcome, err := h.start.StartCurrentApplication(ctx, agentexecutionapp.CurrentApplicationStartRequest{
		ProjectID:           projectID,
		ActorUserID:         actorUserID,
		ConversationUUID:    conversationUUID,
		TargetParticipantID: participantID,
		QuestionID:          questionID,
		UserInput:           task,
	})
	if err != nil {
		// The turn was never admitted, so the conversation created a moment ago
		// holds nothing and will never hold anything. Left behind, a client
		// retrying against a misconfigured agent fills the chat list with empty
		// transcripts — the cost reason 2 above accepts for RUNS, not for
		// refusals. Best effort: a failure to clean up is not worth turning
		// into a second, more confusing error.
		h.discardRunConversation(ctx, schema, conversationUUID)
		slog.Default().ErrorContext(ctx, "external MCP agent operation was not admitted",
			"event", "mcp.agent.admission_failed",
			"project_id", projectID,
			"application_id", tool.applicationID,
			"application_version_id", tool.applicationVersionID,
			"error_code", currentAgentStartErrorCode(err),
			"err", err,
		)
		if errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart) ||
			errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
			// The agent exists and is listed, but this deployment's runtime
			// refuses to admit it — an agent whose version fails the admission
			// contract (no LLM settings, an unsupported agent_type, empty
			// instructions). Naming the tool is what makes that actionable.
			return errorResult("the agent behind '" + tool.Name + "' could not be started on this deployment; " +
				"nothing was executed")
		}
		return errorResult("the agent behind '" + tool.Name + "' could not be started; nothing was executed")
	}

	return h.awaitRunResult(ctx, schema, outcome, tool)
}

func currentAgentStartErrorCode(err error) string {
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart),
		errors.Is(err, agentexecutionapp.ErrInvalidAgentAdmission):
		return "invalid_request"
	case errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart):
		return "unsupported_configuration"
	default:
		return "dependency_failure"
	}
}

// awaitRunResult waits for the admitted turn to settle.
func (h *Handler) awaitRunResult(
	ctx context.Context,
	schema string,
	outcome agentexecutionapp.CurrentApplicationStartOutcome,
	tool Tool,
) map[string]any {
	deadline, cancel := context.WithTimeout(ctx, mcpRunDeadline)
	defer cancel()

	interval := mcpPollInitialInterval
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		// The expiry check is a STATEMENT rather than one arm of the select
		// below. Both channels are ready once the deadline has passed, and
		// select picks a ready arm at random, so the select alone would report
		// expiry only about half the time — and the other half would report
		// whatever the extra poll happened to hit.
		if deadline.Err() != nil {
			// EXPIRY, or the client hung up. Either way the execution is still
			// running: it is durable and owned by the runtime, not by this
			// request. Naming it is the whole point — see mcpRunDeadline.
			return errorResult(fmt.Sprintf(
				"the agent behind '%s' did not finish within %s. It is STILL RUNNING as execution %s; "+
					"its answer will appear in that conversation, and it can be cancelled there. "+
					"No partial output is reported here.",
				tool.Name, mcpRunDeadline, outcome.ExecutionID))
		}
		select {
		case <-deadline.Done():
			continue // reported by the expiry check at the top of the next pass
		case <-timer.C:
		}

		state, err := h.readTurnState(deadline, schema, outcome.ResponseMessageID)
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			continue // ditto: the expiry check owns that report
		case err != nil:
			return errorResult(fmt.Sprintf(
				"the answer for execution %s could not be read back, so nothing is reported here. "+
					"The run itself was admitted and may have completed.", outcome.ExecutionID))
		}

		if state.settled {
			return state.result(tool, outcome.ExecutionID)
		}

		interval *= 2
		if interval > mcpPollMaxInterval {
			interval = mcpPollMaxInterval
		}
		timer.Reset(interval)
	}
}

// turnState is what one poll of the response message group sees.
//
// The four discriminators are the ones the projection actually writes, all on
// the SAME row and all in the SAME transaction as the terminal state
// (internal/db/queries/agent_chat.sql: FinalizeCurrentAgentFullMessage,
// FinalizeCurrentAgentHITLPause, FinalizeCurrentAgentAuthorizationPause, and
// repos/configuration_validation_results.go's persistCurrentAgentRuntimeTerminal
// for the failure case). Reading them here rather than replaying the durable
// event stream is deliberate: the replay stream carries the three TERMINAL node
// events and nothing for a FAILED run, so a failure would be indistinguishable
// from a slow one and would burn the whole deadline before being reported.
type turnState struct {
	settled bool
	// isError is the projected runtime failure. `error` is the SafeMessage the
	// runtime chose; it is already bounded to 256 bytes and safe to relay.
	isError bool
	failure string
	// The two pauses. They have NO MCP representation — the protocol has no way
	// for a tool result to say "answer this question and resume me" — so each
	// is terminal here, and terminal as an ERROR, because a pause is precisely
	// the case where the tool did not produce its answer. Silently waiting
	// would burn the deadline on a run that will never move on its own.
	hitlPause          bool
	authorizationPause bool
	// text is the assistant's answer, the response group's text items
	// concatenated in item order.
	text string
}

func (s turnState) result(tool Tool, executionID string) map[string]any {
	switch {
	case s.isError:
		message := s.failure
		if message == "" {
			message = "the run failed without a reported reason"
		}
		return errorResult(fmt.Sprintf("the agent behind '%s' failed (execution %s): %s",
			tool.Name, executionID, message))
	case s.hitlPause:
		return errorResult(fmt.Sprintf(
			"the agent behind '%s' PAUSED for human approval and execution %s is waiting on it. "+
				"MCP has no way to answer that from here: open the conversation to approve or reject, "+
				"and the run will continue there.", tool.Name, executionID))
	case s.authorizationPause:
		return errorResult(fmt.Sprintf(
			"the agent behind '%s' PAUSED to ask for MCP authorization and execution %s is waiting on it. "+
				"MCP has no way to answer that from here: open the conversation to authorize, "+
				"and the run will continue there.", tool.Name, executionID))
	case strings.TrimSpace(s.text) == "":
		// A settled run with nothing to say. Reported as an error rather than
		// as an empty success for the reason stated in the package header: an
		// agent host reads an empty successful result as "the tool ran and
		// produced nothing", which is indistinguishable from a broken tool.
		return errorResult(fmt.Sprintf(
			"the agent behind '%s' finished (execution %s) without producing any text.",
			tool.Name, executionID))
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s.text}},
	}
}

// errorResult is the CallToolResult shape for "the tool ran and failed".
//
// It is a RESULT and not a JSON-RPC error, which is the distinction the
// specification draws and the one this package already applies to
// ToolExecutionUnavailableReason: a protocol error says the request was wrong,
// while this says the request was fine and the tool did not deliver. It is also
// what puts the sentence in front of the model driving the client rather than
// only in the client's console.
func errorResult(text string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// readTurnState reads the response message group by its uuid.
func (h *Handler) readTurnState(ctx context.Context, schema, responseMessageID string) (turnState, error) {
	if h.pool == nil {
		return turnState{}, errNoPool
	}
	// `string_agg` over the group's text items IN ITEM ORDER is how the
	// transcript assembles them too: the terminal projection appends one
	// `text_message` item per chunk (InsertCurrentAgentTextItem uses the
	// existing item count as `order_index`), and the provisional streaming
	// items are deleted inside the same terminal transaction, so a row read
	// after `is_streaming` clears sees the final text and nothing else.
	statement := fmt.Sprintf(`
SELECT response.is_streaming,
       COALESCE(response.meta ->> 'is_error', 'false') = 'true',
       COALESCE(response.meta ->> 'error', ''),
       response.meta ? 'hitl_interrupt',
       response.meta ? 'authorization_requests',
       COALESCE((
           SELECT string_agg(item_text.content, '' ORDER BY item.order_index, item.id)
           FROM %[1]s.chat_message_items AS item
           JOIN %[1]s.chat_messages_text AS item_text ON item_text.id = item.id
           WHERE item.message_group_id = response.id
             AND item.item_type = 'text_message'
       ), '')
FROM %[1]s.chat_message_group AS response
WHERE response.uuid = $1::uuid`, schema)

	var state turnState
	var streaming bool
	err := h.pool.QueryRow(ctx, statement, responseMessageID).Scan(
		&streaming, &state.isError, &state.failure,
		&state.hitlPause, &state.authorizationPause, &state.text,
	)
	if err != nil {
		if isNoRows(err) {
			// The admission committed the row before it returned, so this is
			// not a "not yet" — it means somebody removed the turn (a cancel
			// deletes an empty response group). Treat it as settled-with-
			// nothing rather than polling a row that will never appear.
			return turnState{settled: true}, nil
		}
		return turnState{}, fmt.Errorf("mcp: read agent turn state: %w", err)
	}
	state.settled = !streaming
	return state, nil
}

// prepareRunConversation creates the conversation this run is recorded in and
// returns its uuid together with the agent's participant id.
//
// One transaction covers all five writes. A half-built conversation — one with
// an agent participant and no user participant, say — would make
// `ResolveCurrentApplicationTurn` find nothing, and the run would be refused
// with a reason that pointed at the agent rather than at this function.
func (h *Handler) prepareRunConversation(
	ctx context.Context,
	schema string,
	projectID, actorUserID int64,
	tool Tool,
) (string, int64, error) {
	if h.pool == nil {
		return "", 0, errNoPool
	}
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("mcp: begin run conversation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	meta, err := json.Marshal(map[string]any{
		// `conversation_type` is what the chat surface reads to label a
		// transcript. Naming the tool as well is what answers "which MCP call
		// was this?" without opening the transcript.
		"conversation_type": mcpConversationSource,
		"mcp_tool":          tool.Name,
	})
	if err != nil {
		return "", 0, err
	}

	var conversationID int64
	var conversationUUID string
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4)
RETURNING id, uuid::text`, schema),
		runConversationName(tool.Name), actorUserID, string(meta), mcpConversationSource,
	).Scan(&conversationID, &conversationUUID); err != nil {
		return "", 0, fmt.Errorf("mcp: create run conversation: %w", err)
	}

	authorMeta, err := json.Marshal(map[string]any{"id": actorUserID})
	if err != nil {
		return "", 0, err
	}
	authorID, err := findOrCreateParticipant(ctx, transaction, schema, userEntityName, authorMeta)
	if err != nil {
		return "", 0, err
	}
	if err := mapParticipant(ctx, transaction, schema, conversationID, authorID, []byte(`{}`)); err != nil {
		return "", 0, err
	}

	agentMeta, err := json.Marshal(map[string]any{
		"id":         tool.applicationID,
		"project_id": projectID,
	})
	if err != nil {
		return "", 0, err
	}
	agentParticipantID, err := findOrCreateParticipant(ctx, transaction, schema, applicationEntityName, agentMeta)
	if err != nil {
		return "", 0, err
	}
	// `version_id` on the MAPPING, not on the participant: the participant is
	// shared across conversations (the identity lookup below finds it again),
	// while the version this run is pinned to belongs to this conversation.
	// ResolveCurrentApplicationTurn reads it from exactly here.
	agentSettings, err := json.Marshal(map[string]any{"version_id": tool.applicationVersionID})
	if err != nil {
		return "", 0, err
	}
	if err := mapParticipant(ctx, transaction, schema, conversationID, agentParticipantID, agentSettings); err != nil {
		return "", 0, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("mcp: commit run conversation: %w", err)
	}
	return conversationUUID, agentParticipantID, nil
}

// discardRunConversation removes a conversation whose turn was never admitted.
//
// The DELETE is guarded on `source` and on the conversation having NO message
// group. Both matter: the source predicate means a bug in the caller cannot
// reach a person's own chat, and the emptiness predicate means a conversation
// that somehow did receive a turn is never removed under it. Nothing here
// depends on the delete succeeding — the caller is already answering an error.
func (h *Handler) discardRunConversation(ctx context.Context, schema, conversationUUID string) {
	if h.pool == nil || conversationUUID == "" {
		return
	}
	// The request context may already be dead (the client hung up on the very
	// error being reported). Cleanup belongs to the row, not to the request.
	ctx = context.WithoutCancel(ctx)
	_, _ = h.pool.Exec(ctx, fmt.Sprintf(`
DELETE FROM %[1]s.chat_conversations AS conversation
WHERE conversation.uuid = $1::uuid
  AND conversation.source = $2
  AND NOT EXISTS (
      SELECT 1 FROM %[1]s.chat_message_group AS existing
      WHERE existing.conversation_id = conversation.id
  )`, schema), conversationUUID, mcpConversationSource)
}

// findOrCreateParticipant is find-then-create, the order
// `ConversationsRepo.AddParticipant` settled on after the reverse order left
// duplicate rows behind. `chat_participants` has no unique key on
// (entity_name, entity_meta), so an INSERT cannot conflict and a create-first
// shape would make a second participant every time.
func findOrCreateParticipant(
	ctx context.Context, transaction pgx.Tx, schema, entityName string, entityMeta []byte,
) (int64, error) {
	var participantID int64
	err := transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = $1::text
  AND entity_meta ->> 'id' IS NOT DISTINCT FROM $2::jsonb ->> 'id'
  AND entity_meta ->> 'project_id' IS NOT DISTINCT FROM $2::jsonb ->> 'project_id'
ORDER BY id
LIMIT 1`, schema), entityName, entityMeta).Scan(&participantID)
	if err == nil {
		return participantID, nil
	}
	if !isNoRows(err) {
		return 0, fmt.Errorf("mcp: look up %s participant: %w", entityName, err)
	}
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json)
RETURNING id`, schema), entityName, entityMeta).Scan(&participantID); err != nil {
		return 0, fmt.Errorf("mcp: create %s participant: %w", entityName, err)
	}
	return participantID, nil
}

// mapParticipant attaches a participant to the conversation.
//
// ON CONFLICT names the COLUMNS and not the constraint, for the reason recorded
// in repos/conversations.go: the constraint carries a different generated name
// on a ledgered tenant database than it does in the bootstrap schema, and
// naming it there answered 500 on every real deployment.
func mapParticipant(
	ctx context.Context, transaction pgx.Tx, schema string, conversationID, participantID int64, settings []byte,
) error {
	if _, err := transaction.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings)
VALUES ($1, $2, $3::jsonb)
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
		conversationID, participantID, settings); err != nil {
		return fmt.Errorf("mcp: map participant: %w", err)
	}
	return nil
}

// maxRunConversationName is the `chat_conversations.name` column's own width in
// the bootstrap schema's sibling columns; the column is an unbounded VARCHAR,
// but a name is a list entry a person reads, so it is bounded here rather than
// letting a 900-character tool name become a list row.
const maxRunConversationName = 120

func runConversationName(toolName string) string {
	name := "MCP: " + toolName
	if utf8.RuneCountInString(name) <= maxRunConversationName {
		return name
	}
	runes := []rune(name)
	return string(runes[:maxRunConversationName])
}
