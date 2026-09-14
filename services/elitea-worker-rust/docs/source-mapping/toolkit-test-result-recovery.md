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
The reload extension below addresses the latest pending test. Connection loss before receipt of an execution ID remains open.
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

## Tab reload and authorization recovery

`api/toolkitTestRecovery.ts` stores only the latest tab-local project, toolkit, and execution IDs.
It uses the existing `shared/lib/storage.ts` namespace. Logout removes the record.
The record contains no arguments, results, token references, or credential values.
A matching toolkit pane resumes its authorized result read after remount.
A different toolkit context does not consume the record.
Corrupt identities and disabled browser storage do not prevent normal live execution.

A recovered authorization challenge also needs the original caller input.
Main reads the tool name from its prepared command and arguments from the immutable input bundle.
The read checks project, initiating actor, toolkit, and execution identity.
It returns these arguments only with the existing authorization challenge.
It does not return frozen toolkit settings or resolved provider credentials.
The UI keeps the restored arguments in memory and waits for explicit authorization.
The existing authorization-reference submission contract then performs the retry.

Unit tests cover remount, one submission, logout cleanup, corrupt identity rejection, and explicit authorization after reload.
PostgreSQL tests verify original argument retrieval and refusal of all four foreign identity cases.
Deployed reload acceptance is recorded below.
Connection loss before receipt of an execution ID remains open.

## Deployed reload acceptance: 2026-09-13

Main image: `sha256:12380697f33167a8a9247e54ff90faf98f1345c2a305e2a37792611ec36be4fa`.
UI image: `sha256:22c36a65c26951d0b1c63ef5b348af29cf3116f8f5a1ba1d32033a80c3a0d817`.

Execution `415e48b33ac246551e5bd267310483d5` produces marker `RUST_TOOLKIT_UI_RELOAD_20260913` through the real worker.
Playwright replaces its initial HTTP response with a timeout carrying the original execution ID.
The browser reloads before polling and displays the stored result after one GET.
Observed counts: one POST, one GET, and one page reload. The interception is removed afterward.
This proves reload recovery for the latest pending test in the same tab.
It does not prove a provider invocation resumed after a process crash.

Ten UI tests pass, including argument-free persistence and explicit authorization after remount.
Main result and response-mapping tests pass. The PostgreSQL ownership test also verifies original argument retrieval.
The authorization retry after reload has component and database evidence; its browser OAuth flow remains unverified.

## Combined active-call crash and browser observation

A fresh headed Playwright session starts temporary OpenAPI toolkit 49 from the Toolkit Test pane.
Its inline schema defines one anonymous synthetic delayed read on the existing trusted TLS fixture.
The observer stops the Rust worker immediately after the fixture receives the first request.
The replacement uses the same deployed image and retained runtime mounts.

Execution `49c51791ea126b06f39bd8fbd9c26b0e` moves from `RUNNING` to `FAILED` under claim 2.
The fixture records one request, zero completed responses, and one client disconnection.
No second provider request occurs.
The browser records one POST and two GETs for that exact execution ID.
Its rendered text changes to `The runtime operation failed.` without another submission.
This connects the existing takeover reconciliation to the deployed Toolkit Test result observer.

The initial copied-configuration attempt never reaches the delayed fixture and does not trigger a worker restart.
Its terminal tool failure is excluded from crash evidence. Temporary toolkit 48 is removed with HTTP 204.
The inline-schema attempt supplies the actual crash proof. Toolkit 49 is removed with HTTP 204 afterward.
The uniquely identified temporary fixture process stops. The OAuth emulator remains running with its existing grants.

Main image is `sha256:3134d5fd03d5fe09f5c69695c769b31199a99e45a7d286d65727f0188ee78085`.
Rust image is `sha256:aa57b19e80d837881d1939d2c7d290859846e9ce3a5ee440f8b807ea3603696d`.
The local browser evidence is `elitea-toolkit-crash-browser-v2.log`.
The script is `elitea-toolkit-crash-browser-v2.py`; its DOM observation checks the final failure text.

This proves terminal reconciliation and browser observation after an ambiguous in-flight call.
It does not prove successful provider continuation, cross-replica recovery without local state, or effect rollback.
Those boundaries must remain explicit when evaluating wider durability gates.


## Request receipt extension, 2026-09-14

`toolkit-request-recovery.md` closes the earlier response-loss boundary before receipt of an execution ID.
The deployed browser retrieves the original Rust result without a repeated provider invocation.
An unaccepted request retains an inconclusive receipt and never triggers automatic resubmission.
