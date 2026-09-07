package mcp

// Running a TOOLKIT tool — the other half of `tools/call` (#616).
//
// The agent half (execute.go) admits a chat turn and waits for the projection.
// This half admits a `toolkit.call_tool.v1` execution and waits for its
// settlement. Both waits are bounded, both name the execution id when they
// expire, and neither ever answers an empty successful result.
//
// # WHAT IS *NOT* CREATED HERE
//
// No conversation, no participant, no chat rows. execute.go creates a
// conversation because the agent use case demands one and because an agent run
// is a transcript a person may want to read. A tool run is neither: the SDK call
// has no transcript, the runtime execution row IS the record, and inventing a
// conversation per tool call would fill the chat list with one-line entries that
// say nothing an execution row does not already say.
//
// # IDEMPOTENCY, WHICH THE AGENT HALF DOES NOT HAVE
//
// #616 asks that "a retry with the same idempotency key does not run the tool
// twice". The tool-run producer derives its key from the project, the actor, the
// toolkit, the tool and the EXACT arguments, so two identical `tools/call`
// requests share an admission and the second returns the first one's answer.
//
// That is the opposite of the choice execute.go documents for agents, where a
// fresh question id per call is deliberate. The difference is real and is not an
// inconsistency: an agent turn is a conversation with history, so two identical
// prompts are two legitimate turns; a tool run against fixed arguments is a
// question with one answer, and running the provider work twice is the surprise.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// ToolkitRunUseCase is the narrow slice of the tool-run producer this package
// needs. It is declared here rather than imported for the same reason
// AgentStartUseCase is: the dependency points one way.
type ToolkitRunUseCase interface {
	RunTool(context.Context, toolkitcalltoolapp.RunRequest) (toolkitcalltoolapp.RunOutcome, error)
}

// runToolkitTool dispatches one toolkit tool and maps its settlement onto the
// MCP result shape.
//
// It returns the CallToolResult body — either the tool's answer, or an
// `isError` result whose text says what happened. It never returns a successful
// result with no content, for the reason stated in the handler.go header: an
// agent host reads an empty successful result as "the tool ran and produced
// nothing", which would be a lie for every branch below.
func (h *Handler) runToolkitTool(
	ctx context.Context,
	projectID int64,
	actorUserID int64,
	tool Tool,
	arguments json.RawMessage,
) map[string]any {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	outcome, err := h.toolRuns.RunTool(ctx, toolkitcalltoolapp.RunRequest{
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ToolkitID:   tool.toolkitID,
		// The SDK tool name, not the MCP tool name. `tools/list` publishes
		// `<toolkit>_<tool>` with pylon's sanitiser applied, and handing that
		// composed string to the worker would name a tool no toolkit has.
		ToolName:  tool.toolkitToolName,
		Arguments: arguments,
	})
	if err != nil {
		return toolkitRunError(tool, err)
	}
	switch outcome.Status {
	case toolkitcalltoolapp.RunStatusOK:
		if outcome.Truncated {
			return errorResult(fmt.Sprintf(
				"'%s' produced a result too large to return through this protocol (execution %s). "+
					"It is reported as truncated with no body, because half a JSON document is not a result.",
				tool.Name, outcome.ExecutionID))
		}
		text := strings.TrimSpace(outcome.ResultJSON)
		if text == "" {
			return errorResult(fmt.Sprintf(
				"'%s' finished (execution %s) without producing any value.",
				tool.Name, outcome.ExecutionID))
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}
	case toolkitcalltoolapp.RunStatusToolError:
		// An isError RESULT and not a protocol error: the request was fine and
		// the tool did not deliver, which is exactly the distinction the
		// specification draws.
		return errorResult(fmt.Sprintf("'%s' failed (execution %s): %s",
			tool.Name, outcome.ExecutionID, outcome.ErrorMessage))
	case toolkitcalltoolapp.RunStatusUnsupportedToolkit:
		return errorResult(fmt.Sprintf(
			"'%s' cannot run on this deployment: %s. Nothing was executed and nothing was changed.",
			tool.Name, outcome.ErrorMessage))
	case toolkitcalltoolapp.RunStatusUnknownTool:
		return errorResult(fmt.Sprintf(
			"the toolkit behind '%s' has no tool by that name, so nothing was executed.", tool.Name))
	default:
		message := outcome.ErrorMessage
		if message == "" {
			message = "the run failed without a reported reason"
		}
		return errorResult(fmt.Sprintf("'%s' failed to run (execution %s): %s",
			tool.Name, outcome.ExecutionID, message))
	}
}

func toolkitRunError(tool Tool, err error) map[string]any {
	var pending *toolkitcalltoolapp.PendingRun
	switch {
	case errors.As(err, &pending):
		// EXPIRY. The execution is durable and still running; it is owned by
		// the runtime, not by this request. Naming it is the whole point.
		return errorResult(fmt.Sprintf(
			"'%s' did not finish within the bounded wait. It is STILL RUNNING as execution %s, "+
				"and its result will be recorded there. No partial output is reported here.",
			tool.Name, pending.ExecutionID))
	case errors.Is(err, toolkitcalltoolapp.ErrToolkitNotVisible):
		// The listing and the run resolve visibility separately, so this is
		// reachable: a toolkit deleted between `tools/list` and `tools/call`.
		return errorResult(fmt.Sprintf(
			"the toolkit behind '%s' is no longer visible in this project, so nothing was executed.", tool.Name))
	case errors.Is(err, toolkitcalltoolapp.ErrUnsupportedToolkitType):
		return errorResult(fmt.Sprintf(
			"'%s' cannot run on this deployment: %s. Nothing was executed and nothing was changed.",
			tool.Name, err.Error()))
	case errors.Is(err, toolkitcalltoolapp.ErrInvalidToolRun):
		return errorResult(fmt.Sprintf(
			"the arguments given to '%s' were not a JSON object this service can pass on, "+
				"so nothing was executed.", tool.Name))
	default:
		// The cause is not put on the wire — this package never sends
		// err.Error() for a server fault.
		return errorResult(fmt.Sprintf(
			"'%s' could not be run on this deployment; nothing was executed.", tool.Name))
	}
}
