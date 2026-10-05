# Code platform call observation

The current SDK supplies platform calls from Code nodes through its sandbox client.
The replatform preserves these calls through Main's claim-scoped operation journal.

Worker previously waits for Supervisor completion before it services platform calls.
Runner waits for a platform reply before it can complete.
These waits form a cycle.

`src/agents/graph/code_platform_drive.rs` drives the submit future and the platform service future together.
`code_attempt_remote.rs` uses this helper for compiled and noncompiled submissions.
The compiled future includes the initial retained-process reconciliation call.
Both futures use the original activation, authority, request fingerprint, and absolute observation deadline.
No task survives cancellation of its owning attempt.

Supervisor retains its runtime lease and heartbeat.
Main retains the operation identity and dispatch journal.
Worker records committed or unknown operation identities for recovery.
An unknown result does not authorize another operation dispatch.

Pure Code nodes do not request platform service.
Platform authorization failures remain failures.
Only an authenticated owner response can report that admission is not ready.
That response authorizes no runtime access or operation.

Focused tests require platform service before a submit receipt becomes available.
They verify one submission, one operation, the original deadline, and cancellation of both futures.
Deployed four-language acceptance remains a separate check.
