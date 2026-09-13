# Toolkit Test result recovery

## Functional source and ownership

Current Core `api/v2/test_toolkit_tool.py` waits for a synchronous SDK result.
The replatform's `api/v2/toolkitrun/response.go` records that reference and its response differences.
Current-platform crash recovery is not a compatibility reference for this new capability.

Rust writes toolkit terminal output through `execution/toolkit_delivery_processor.rs` and the owned output protocol.
Main persists that output in `elitea_runtime.output_inbox`.
The initial Toolkit Test HTTP request polls this inbox with a bounded deadline.
Its deadline does not cancel the durable execution.
Previously, the UI retained the timeout message without retrieving the later result.

## Implementation

Main exposes `GET /elitea_core/test_tool/prompt_lib/{project_id}/{tool_id}/{execution_id}`.
The existing toolkit test permission gate protects the route.
The repository also checks the initiating actor, project, tenant, capability, and frozen toolkit identity.
Another caller or mismatched identity receives 404.
The repository reads the existing frozen input and output records. It adds no table or migration.
The read cannot admit or dispatch another tool call.

Pending executions return 202. Terminal outputs use the existing result and authorization response mapper.
A terminal job without output returns an explicit failure, rather than remaining pending forever.
Responses disable caching.
The UI polls every two seconds after a timeout supplies the execution ID.
Main unavailability retains the observation; it does not resubmit the tool.
Unmount, reset, or a newer press prevents a stale result from replacing the current pane.
A delayed authorization challenge retains the original arguments for the existing authorization flow.

## Verification and remaining scope

Focused Go tests cover pending states, stored results, terminal failure, and refusal before result reading.
A real PostgreSQL test verifies project, actor, toolkit, and execution ownership.
The UI tests prove one POST, two GETs, and the final recovered result.
They also cover Main unavailability and terminal runtime failure.
Deployed browser acceptance is recorded below.

This change handles a surviving tab after its initial bounded wait.
Page reload recovery and connection loss before the initial response exposes an execution ID remain open.
External MCP request recovery is a separate contract and is not proved by these browser tests.

## Deployed browser acceptance: 2026-09-13

Main image: `sha256:9f8b20f67031f30aea480ac36b80ad1a34820148b18edc2c8a31c3bfd77a89c9`.
UI image: `sha256:95c0a1d5df78bcd889c57c19c7dc7a61eebaa7b3e2355d85e7d8751aa72b8d07`.
Rust remains on the compatible instruction and invocation-fence image documented in `toolkit-takeover-authority.md`.

The browser submits one real Toolkit Test request against saved toolkit 31.
Execution `d3c8363761ee3ba55fbdb29a18fcd69c` produces marker `RUST_TOOLKIT_UI_RECOVERY_20260913`.
Playwright replaces only the initial successful HTTP response with a timeout carrying that execution ID.
The deployed UI then makes one GET and displays the actual persisted marker.
Observed counts: one POST and one GET. The test removes its interception afterward.
This proves client recovery after a bounded wait, not worker crash continuation by itself.

A separate browser read retrieves the earlier crash execution `80e4eb10605c424dbc11273ef202d8df` as terminal runtime failure.
It also retrieves successful execution `6dc13869bfbefcdba8efcfc75cbca2b0` with HTTP 200.
The UI unit suites pass all 19 tests. TypeScript type checking passes.
The PostgreSQL ownership test passes all four negative identity cases without skips.
