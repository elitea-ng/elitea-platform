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
