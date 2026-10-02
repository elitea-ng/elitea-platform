# Chat observation after a restart

## Contract and source ownership

The runtime owns execution recovery. The browser observes the same execution after a connection failure.
A connection failure does not prove that the execution failed.

| Contract | Source |
| --- | --- |
| Durable event replay and cursor validation | `services/elitea-main/internal/api/v2/executions/events.go` |
| Browser event decoding and cursor delivery | `apps/elitea-web/src/shared/api/sse/executionEvents.ts` |
| Reconnect scheduling and cursor retention | `apps/elitea-web/src/features/chat-messages/model/useChatStreamConnection.ts` |
| Run ownership, message reduction, and explicit Stop | `apps/elitea-web/src/features/chat-messages/model/useChatStreamTransport.ts` |
| Rust invocation fencing and crash limitations | [Toolkit takeover authority](toolkit-takeover-authority.md) |

This change does not port a legacy recovery mechanism. The new runtime requires durable recovery beyond the legacy platform's behavior.
Rust source remains unchanged. The browser consumes Main's existing durable event contract.

## Defect and implementation

The browser previously exhausted four retries after approximately 15 seconds. The transport then recorded an execution failure and discarded its observation identity.
A longer Main outage could therefore leave the browser detached when the runtime recovered.

The browser retains the execution URL and last delivered cursor. It keeps the initial retry delays of one, two, four, and eight seconds.
Later retries use a 30-second delay. The browser keeps one subscription or one scheduled retry.
A delivered event restores the initial retry schedule. Extended-outage notification occurs once per outage through the existing callback.
The chat widget currently does not consume that callback. The existing pending state and Stop control remain available during recovery.
Explicit Stop, conversation changes, and component teardown end observation. A transport outage never submits a replacement execution.
A durable execution failure still uses the existing failure handler.

## Verification and limits

The connection and transport suites pass all 47 tests. The recovery case spans 75 seconds and retains the last cursor.
TypeScript validation passes. The UI container build passes.

The deployed UI image is `elitea-web:chat-reconnect-20260913`.
Its digest is `sha256:b79274b60c818a6ce6bb0a6911b4a5f222c31830403bee8aec15ffa027aadc02`.
Main and Rust images remain unchanged.

This change does not restore chat observation after browser reload. It does not recover an execution ID lost before the admission response.
It does not establish successful continuation after an ambiguous provider invocation. External MCP reconnect remains a separate acceptance gate.
Point 3 remains open until these runtime and client contracts have sufficient acceptance evidence.

## Deferred request lookup experiment

An unfinished request-ID lookup changed toolkit admission keys. Existing authorization tests showed that fresh authorization uses distinct input-bound identities.
The experiment also changed keys across deployment versions. It could therefore weaken retry compatibility.
The experiment was removed before deployment. Existing toolkit admission and authorization tests pass with their original keys.
Initial-response loss still needs a durable lookup design that preserves those contracts.

## Browser acceptance status

The first browser attempt stopped before submission because typing removed the placeholder used by the test locator.
The second attempt used the textarea element and injected five HTTP 503 responses on the execution event route.
The Playwright tool connection closed during this attempt. The subsequent browser inspection also returned `Transport closed`.
No successful browser acceptance result was obtained. Repeat this test before closing the recovery gate.
The test installs route cleanup in a `finally` block. Verify browser state after the automation connection returns.

A subsequent runtime database query shows no execution admitted by either interrupted browser attempt.
The latest admission remains the earlier toolkit discovery at `2026-09-13 11:07:53.294 UTC`.

## Completed local Playwright acceptance

Local Playwright with installed Chrome replaces the unavailable browser tool for this check.
A fresh browser context signs in through the rehearsal identity selector. It exports no browser credentials or session state.
The test submits one chat request and injects five HTTP 503 responses on the execution event stream.
Observed connection attempts occur at 0.12, 1.13, 3.14, 7.15, 15.16, and 45.18 seconds.
The sixth attempt reaches the real event endpoint. Chat 557 displays `RUST_CHAT_RECONNECT_LOCAL_20260913` in the answer.
The marker occurs twice across the page: once in the question and once in the answer.
There is one message-submission POST. The other POSTs create the conversation and its participant.
The browser context closes after verification. The deployed Main includes atomic toolkit admission at this point.

This passes the extended event-stream outage gate. It injects transport failures without crashing Main or Rust.
Actual process-replacement and external MCP reconnection acceptance remain separate requirements.

## Main process crash acceptance

A fresh local Playwright context tests a real Main restart on 2026-09-13.
Chat 564 uses execution `56740dd1287b219bfb3d2ea32efbde52`.
The test requests an orchard story and restarts Main after the title appears in the streamed answer.
The final sentence is absent when Main stops. Rust remains running throughout the test.
Main restarts at `11:58:57.847 UTC` and returns healthy.

The browser reconnects to the same execution with cursor `175624`.
It submits exactly one message request. The other two POST requests create the conversation and participant.
The browser displays the partial answer followed by `The runtime operation failed.`
The durable execution state is `FAILED`. This proves browser reconnection, but fails successful runtime continuation.

At `11:58:57.758 UTC`, Rust reports `model_gateway.stream_transport` through `native_agent.event_failed`.
Lease supervision then reports `claim_lease_control_failure` and retains the command without acknowledgement.
At `11:59:59.754 UTC`, the next claim returns `output_recovery`.
The worker completes with `agent_delivery.ambiguous_invocation_reconciled` and a failed settlement.

| Boundary | Rust source | Observed behavior |
| --- | --- | --- |
| Model stream interruption | `src/transport/openai_compatible_facade.rs` | Classifies the interrupted stream as `model_gateway.stream_transport`. |
| Invocation and lease supervision | `src/execution/native_agent_lifecycle.rs`, `src/execution/agent_preparation.rs` | Retains the command when control fails. |
| Ambiguous invocation recovery | `src/execution/agent_delivery_processor.rs::process_output_recovery` | Maps a running ambiguous invocation to an internal terminal failure. |

Successful continuation needs further implementation and verification. Do not weaken invocation fencing or repeat completed tool effects to pass this check.
The legacy platform has no equivalent crash-continuation contract. Its behavior cannot establish acceptance for this new requirement.
The external MCP reconnect contract also remains open.

Earlier attempts do not restart Main. Two prompts receive model refusals; another test misses a title because of capitalization.
The final test uses a case-insensitive title check. Its timeout occurs after the confirmed runtime failure, not before submission.
