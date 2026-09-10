# Complete tool results across bounded events

## Current platform evidence

`projects/EliteaUI/src/components/Chat/hooks.js` handles `AgentToolEnd` by appending string output.
`projects/EliteaUI/src/common/convertChatConversationMessages.js` reads saved tool output from trace details.
This behavior permits progressive output and subsequent history reads.
It does not define replay identity, byte offsets, or integrity checks for fragments.

## New platform mapping

| Responsibility | New source |
| --- | --- |
| Versioned fragment contract | `libs/proto/elitea/runtime/v1/node_event.proto` |
| Complete result projection | `services/elitea-worker-rust/src/agents/events.rs` |
| Claim-fenced ordered frame delivery | `services/elitea-worker-rust/src/execution/output_delivery.rs` |
| Durable result assembly | `services/elitea-main/internal/infra/db/repos/agent_tool_output_chunks.go` |
| Existing trace row persistence | `services/elitea-main/internal/infra/db/repos/agent_trace.go` |
| Browser assembly | `apps/elitea-web/src/features/chat-messages/lib/toolOutputChunks.ts` |
| Browser lifecycle state | `apps/elitea-web/src/features/chat-messages/lib/chatStreamToolFrames.ts` |

## Implementation

The MCP adapter admits larger values than the previous tool-event projection limit.
A valid selected GitHub schema exceeds that projection limit after JSON escaping.
The worker now splits large serialized results into UTF-8 fragments of at most 8192 bytes.
The maximum complete result is one MiB at this projection boundary.
Individual tools retain their own admission limits.
The model receives the original result without event-fragment formatting.

Each fragment has a byte offset, total length, complete-result SHA-256 digest, and final marker.
Execution identity and tool-call identity scope the fragments.
The existing output sequence and claim fence govern persistence and replay.
The worker publishes the durable partial-message frame before the corresponding browser tool-end frame.
Main rejects gaps, conflicting content, changed result identity, and an invalid final digest.
An exact duplicate does not append content again.

Main stores the assembled result in the existing `chat_message_trace_step.tool_output` column.
Small fragment metadata uses the existing trace attributes column.
The browser preserves input fields and marks completion only after the final fragment.
No application table or migration is added.
The existing node-event and stream-frame limits remain unchanged.

## Verification

Targeted Rust tests reconstruct escaped JSON containing multibyte text from bounded frames.
Go tests check reconstruction, duplicate replay, gaps, and integrity failures.
A PostgreSQL test reconstructs 100 KB across separate transactions and exact duplicate deliveries.
Browser tests check byte offsets, replay, and final-only completion.
The full Rust library suite passes: 955 tests, with no failures or ignored tests.
Rust Clippy passes with warnings denied.
The UI reducer and chunk suites pass 92 tests.
UI type checks and focused lint checks pass.
The focused Go checks pass eight tests, including PostgreSQL persistence checks, without skips.

## Deployed chat evidence

Main, Rust worker, and UI images run in the rehearsal deployment.
Their respective image digests are:

- Main: `sha256:bce760ae76b0f76d5a6b96faa1099a43fa454b2ea3f99023230d48ad4f10df53`.
- Rust: `sha256:2267c5e9390ab6febccca003e08f30c91309aaba6f67f618a606482ba15f9345`.
- UI: `sha256:e39f5c760dc05e070ef5443fbde85a43ad4c117770118d9cd9de705831f06752`.

Playwright submits a read-only discovery request in chat 545.
The agent lists toolkit types, then requests the complete GitHub schema.
Neither call reports a tool failure or resource-limit error.
The agent produces a schema report, which remains visible after a page reload.
Trace row 7226 stores 42,316 bytes with final fragment metadata and a `stop` finish reason.
Trace row 7224 stores the separate type catalog result.
These rows demonstrate distinct calls, rather than duplicate fragments stored as separate tool results.

This check covers successful schema transport and a saved chat answer.
It does not establish recovery after the earlier interrupted execution in chat 542.
It does not establish credential creation, toolkit execution, or model accuracy for every schema field.

## Repeat after the deployment pause

On 2026-09-10, Playwright repeats the same read-only discovery in chat 545.
The three deployment containers remain running; Main reports healthy.
The repeated call produces trace row 7231 in message group 5812 at `2026-09-10T18:10:44Z`.
The row contains 42,316 bytes, valid JSON, the `github` schema, and a `stop` finish reason.
PostgreSQL computes the stored result SHA-256 and confirms that it matches the final fragment metadata.
The preceding result in row 7226 also passes the digest check.
The browser shows no tool failure or resource-limit error for the repeated turn.
Playwright reloads the page and verifies that the repeated report remains visible.
The model report includes a conceptual credential example; this example does not prove the accepted credential creation contract.
