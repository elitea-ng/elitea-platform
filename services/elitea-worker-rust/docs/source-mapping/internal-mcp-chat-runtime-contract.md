# Internal chat runtime contract

Status: message sending is implemented and deployed. Remaining acceptance boundaries appear below.
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

Before this change, `mcp.AgentStartUseCase` exposes only the saved-application method.
The send adapter also needs the existing ad-hoc method; changing only the tool catalogue cannot supply ordinary chat execution.
The existing data adapter dispatches eleven operations through shared handlers.
The new send adapter adds durable admission and observation beside those operations.

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
Message sending now uses the shared runtime. Legacy interrupt-decision publication is intentionally excluded by user steering.
This does not remove browser continuation, recovery, or exact interrupt-ownership requirements from the wider worker goal.

## Required verification

Prove ordinary and saved-application targets through the shared start services.
Prove UUID and participant scope, operation permission, preserved application model, and ordinary default model selection.
Prove completed, failed, paused, and timed-out observations with the original execution identity.
Prove reconnecting observation does not admit another message.
Use a real browser and worker for final acceptance; handler doubles alone do not close this contract.

## Target resolver implementation

`mcp/internal_chat_send_target.go` selects the requested participant through the shared conversation detail handler.
The handler enforces actor visibility; the resolver checks the returned project and conversation identity.
It refuses absent, ambiguous, and unsupported participant selections.
The existing authenticated principal, including token identity, passes unchanged to shared reads.

Application targets retain their saved model and reject ordinary model overrides.
Ordinary targets resolve omitted model settings through the shared typed model handler.
The configured default must match an entry in its authorized catalog.
Shared model lookup retains the requesting project context.
The resolver creates no execution and has no fallback admission path.

`TestInternalChatSendTarget*` passes application isolation, principal preservation, catalog membership, and target-refusal cases.
These tests use handler doubles. They do not establish database authority or deployed message sending.
The send adapter below connects this resolver to the published tool.

## Send adapter and deployed acceptance

`internal_chat_send.go` connects target resolution to the shared ordinary and saved-application start services.
`post_elitea_core_messages` requires `models.chat.messages.create` through the existing internal MCP permission gate.
The endpoint supplies project authority. The schema excludes caller identity and interrupt-decision fields.
A caller can retain `question_id` for admission retries; an omitted identity is generated once for that call.
The adapter never retries a failed admission through another target path.

Observation reads only the admitted response message through the existing MCP terminal mapper.
The default wait is 30 seconds; accepted explicit values range from -1 through 300.
Zero and -1 return immediately. Expired or unavailable observation returns pending execution references.
The durable execution remains owned by the worker and existing Main authority.
A terminal guard produces an MCP error with the execution identity; partial text is not a successful result.
The response envelope contains execution, response-message, question, and conversation identities plus status and the terminal MCP result.
It does not reproduce the legacy `message_groups` envelope. Existing authorized conversation reads retain transcript access.

Focused chat tests pass target selection, single-path admission, exact response binding, asynchronous mode, mixed-guard refusal, and forbidden decision arguments.
The MCP and Main route test selection also passes. These tests use doubles and do not replace deployed evidence.
No schema migration or protocol regeneration is required.

Main deploys as `elitea-main:chat-send-20260913`, image `sha256:8bdea8b44c21992dac359c57522f602f7d03a2fc64efd65e24e646826982cea4`.
The replacement retains all six mounts, environment, networks, and resource limits.
The first deployment review refuses an opaque-script operation. Read-only scope validation then permits the rehearsal-only replacement.

A UI-created temporary PAT drives independent MCP calls for ordinary chat and a saved-agent participant.
Ordinary execution `571ee3011ec04002e9ee34e004d3643e` completes in chat 578 with the project default model.
Saved-agent execution `1e93ccb84deb83587bcb1409b9345e42` completes in chat 543, targeting mapped participant 30.
Both responses contain the expected synthetic marker and `status: completed`.
A fresh headed browser verifies both persisted answers. The PAT is revoked with HTTP 204.
Evidence is `elitea-live-chat-send-debug.log`; screenshots are `elitea-chat-send-ordinary.png` and `elitea-chat-send-saved.png`.

The preceding ordinary run in chat 577 completes, but the model declines to repeat its marker.
Its persisted answer confirms model refusal; that run does not pass the marker assertion.
No transport fix is inferred from the later successful run.

Live send-operation denial and paused send remain unverified for this adapter.
General external MCP pause and replay proofs do not substitute for those adapter-specific checks.

## Repeated-question acceptance

Two independent MCP submissions reuse question `df87c297-5c8a-4c18-b47a-ed428dc4360b` in chat 579.
Both return execution `f0407b3b9ae1f8d495614a70f722e691` and response `adc4af00-282d-567a-bd5b-b426b3fade02`.
Both report completion with the same saved answer.
A headed browser verifies the answer after each submission.
A subsequent database query counts exactly one question row for that UUID.
The temporary PAT revocation returns HTTP 204.
Evidence is `elitea-chat-send-repeat.log`.
This proves sequential repeated-question admission; it does not prove simultaneous submissions or disconnected observation.
