# Internal chat runtime contract

Status: source review complete; message-send implementation remains pending.
This contract follows the user's external-MCP restriction on interrupt decisions.

## Current platform evidence

Paths are relative to `projects/centry/pylon_main/plugins/elitea_core`.

| Source | Business behavior | Replatform owner |
| --- | --- | --- |
| `api/v2/messages.py:195-228` | MCP send uses a conversation UUID, optional participant, and bounded wait. | Main MCP chat adapter and shared agent start service. |
| `api/v2/messages.py:230-263` | A saved-application participant retains its version model; other requests resolve the project default. | Existing participant resolution and configuration catalog. |
| `api/v2/messages.py:267-285` | Blocking and asynchronous modes share one prediction path. | One durable admission followed by optional observation. |
| `api/v2/continue_predict.py:20-65` | Legacy MCP exposes HITL and toolkit-authorization decisions. | Deliberate publication restriction described below. |
| `api/v2/continue_predict.py:118-155` | Completed continuation returns the response and its question. | Existing authorized message projection, if used by an approved caller surface. |

The legacy Python dispatch is not an implementation template.
Do not add a Python bridge or an MCP-specific execution engine.

## Existing Main contracts

`api/v2/agentexecution/route.go` dispatches browser starts through `StartCurrentApplication` or `StartCurrentAdhoc`.
Both methods belong to `application/agentexecution.CurrentApplicationStartService`.
The start response includes execution, command, response-message, and event-stream identities.
The repository validates conversation and participant authority during durable admission.

`mcp.AgentStartUseCase` currently exposes only the saved-application method.
The send adapter also needs the existing ad-hoc method; changing only the tool catalogue cannot supply ordinary chat execution.
`mcp/internal_chat_execute.go` currently dispatches eleven data operations through shared handlers.
It has no message-send operation or durable result observer.

`mcp/execute.go::awaitRunResultWithMode` already observes a bound response message with a finite deadline.
Its timeout does not cancel durable work. Its terminal mapper refuses partial results when guards remain pending.
Reuse the same state interpretation for message sending.
A timeout must return the admitted execution identity; it must not imply that nothing ran.
Keep observation separate from admission so a result read cannot submit the message again.

## Target selection and model defaults

Resolve the requested participant within the authorized conversation before selecting the start method.
A saved application uses its existing version settings. Do not inject the ordinary-chat default over those settings.
An ordinary target uses the shared model catalog when settings are omitted.
Reject inaccessible, unmapped, or unsupported targets before admission.
Do not try another start method after an ambiguous admission failure.

Preserve the caller's existing question identity for idempotent admission.
Bound user input, attachments, result size, and wait duration through the existing contracts.
Return persisted message data for the exact admitted response, not an unrelated latest conversation message.

## Interrupt decisions and user steering

The user requires exported agents and pipelines to run autonomously.
External callers must not approve or resume Elitea interrupts.
Therefore, do not publish the legacy `continue_predict` decision schema unchanged.
The existing browser decision routes retain authority for human and delegated authorization decisions.
The external MCP result reports a pause as incomplete work and identifies the conversation.
This restriction also prevents an internal model from approving its own sensitive operation through the MCP catalogue.

The earlier point 3 audit lists two missing runtime operations without this distinction.
Message sending remains implementation work. Legacy interrupt-decision publication is intentionally excluded by user steering.
This does not remove browser continuation, recovery, or exact interrupt-ownership requirements from the wider worker goal.

## Required verification

Prove ordinary and saved-application targets through the shared start services.
Prove UUID and participant scope, operation permission, preserved application model, and ordinary default model selection.
Prove completed, failed, paused, and timed-out observations with the original execution identity.
Prove reconnecting observation does not admit another message.
Use a real browser and worker for final acceptance; handler doubles alone do not close this contract.
