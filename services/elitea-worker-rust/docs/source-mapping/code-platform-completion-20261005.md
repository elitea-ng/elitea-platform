# Code platform result collection

The current SDK exposes platform operations through `elitea_sdk/runtime/clients/sandbox_client.py::SandboxClient`.
The Rust Code node preserves these operations through Main's scoped platform journal.
The operation journal and sandbox receipt have separate owners.

The deployed Python fixture completes both platform operations and exits successfully.
Supervisor saves its successful terminal receipt.
Worker still reports a failure because its concurrent platform observer sees the stopped runtime.
The observer failure cancels the scoped submission future before Worker resolves the sandbox dispatch.
This failure is a result collection race. The Code program does not require an approval or recovery pause.

| Responsibility | Source | Behavior |
| --- | --- | --- |
| Scoped submission and observation | `src/agents/graph/code_platform_drive.rs`, `code_attempt_remote.rs` | Drive both futures under the original deadline. Cancel both when the owning attempt ends. |
| Durable sandbox result | `src/sandbox/docker_supervisor.rs::persist_receipt` | Commit the original result before cleanup. Preserve the result when cleanup needs reconciliation. |
| Authenticated completion observation | `src/sandbox/ledger_code_platform.rs`, `docker_code_platform_owner.rs` | Require the exact original grant, request digest, bindings, and valid result. Never restart Code. |
| Closed wire response | `src/sandbox/service_code_platform.rs`, `libs/proto/contracts/sandbox-code-platform-owner-v1.md` | Return only the declared read state and null outputs. Refuse publication and malformed selectors. |
| Current execution authority | Main `internal/infra/storage/code_platform_pump.go`, `code_sandbox_owner_client.go` | Recheck the original current claim and snapshot before returning idle. Dispatch no new operation. |

Two timing boundaries require separate observations.
`completed` requires the exact persisted terminal result.
`completing` observes a valid terminal receipt from the exact stopped runtime before the original submission commits it.
The completing read requires a live original owner lease and unchanged runtime identity before and after receipt observation.
Cancelled, failed, missing, invalid, replaced, expired, or unauthorized observations remain failures.
Neither observation returns a result, runtime handle, mailbox, or publication authority.
The original submission remains the only result authority.

The first focused Rust packet passes 62 tests with no failures, ignored tests, or skips.
The corresponding Main packet passes 123 tests and subtests with no failures or skips.
These packets verify committed completion and preserve the earlier admission and cancellation fences.
The completing extension passes 68 Rust tests and 180 Main tests and subtests.
Both direct test commands exit zero. No selected tests fail, skip, or remain ignored.
Rust links once against pinned native dependencies. Five existing warnings remain.
These focused checks do not prove deployed PostgreSQL or browser behavior.
Code acceptance remains open until deployed browser verification passes.
