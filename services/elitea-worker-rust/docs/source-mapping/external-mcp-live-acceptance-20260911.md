# External MCP deployed acceptance: 2026-09-11

## Scope and source mapping

This record covers the deployed external MCP bridge and Rust runtime.
The [contract mapping](external-mcp-completion.md) identifies current Core and SDK business sources.
Main `internal/api/v2/mcp/execute.go` starts the exact saved application version.
Rust `src/agents/ordinary.rs` executes the agent and its configured tools.
Rust `src/agents/pipeline.rs` executes the saved pipeline graph.
The shared chat projection supplies the external terminal response.

## Saved agent terminal success

Playwright uses the authenticated browser session to call `/app/2/mcp/agent/20`.
`tools/list` returns `rust-gate3-joke-agent-20260909` with the required `task` argument.
`tools/call` requests an OpenAPI echo and a joke from the linked skill.
The response returns HTTP 200 without `isError`.
It contains marker `RUST_GATE3_EXTERNAL_AGENT_20260911`, mode `stored`, and generation `1`.
It also contains a computer joke and its wordplay explanation.

Execution `9d0487418367372be7e5e0b2b7bd9f8d` settles as `SUCCEEDED`.
Conversation `547`, message group `5850`, and trace `7336` correlate the invocation.
The trace records `echo_marker`, the exact marker argument, and the provider result.
Its `is_error` field is false.
This proves actual provider invocation and external terminal return for this saved agent.
It does not prove authorization resume, replacement, or all toolkit families.

## Pipeline approval and resume

Playwright calls the existing `Hitl_node` pipeline through `/app/2/mcp/pipeline/8`.
The task contains marker `RUST_GATE3_EXTERNAL_HITL_20260911`.
Execution `2fe737fcd3e08182c202193463e6a7c6` reaches a human-approval pause.
The external response sets `isError: true` and directs the caller to the conversation.
It does not return the intermediate joke as successful final output.

Conversation `548` has UUID `a0d73798-bbc4-4d68-ad22-60c5ad3c1068`.
Playwright opens that conversation and clicks its existing Approve button.
Resume execution `cd95978ec4f39820056dd9a370ea7c87` settles as `SUCCEEDED`.
Message group `5852` stops streaming and clears its pending HITL metadata.
The Approve button remains absent after browser reload.

The final conversation displays the joke twice, including after reload.
Stored trace rows `7339` and `7340` contain identical joke text.
This is a confirmed duplicate-output symptom, not only a transient streaming observation.
The source of the duplicate remains under investigation.
This proof establishes approval and settlement but does not close resume-output correctness.

## Remaining proof

Pipeline terminal completion, failure, mixed authorization, resume, and replay remain separate checks.
A new external call creates a new conversation; it is not a replay request.
