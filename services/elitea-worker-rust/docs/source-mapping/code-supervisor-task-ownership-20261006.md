# Code Supervisor task ownership

## Observed failure

Persistent chat 825 runs saved pipeline version 166 with marker `gate5-worker-recovery-delay-20261006-v6`.
Execution `924ae265ca5475442509680619d241d1`, generation 1, loses and restores only its Worker.
The Worker uses source `fee3d059dc493b8061f356f0f6e13c5c94f06190`.
The Supervisor uses source `9ae93a9eee2121beea76c79d24572ec0095d1563`.

The retained JavaScript runtime has identity `ac4ed93dc36fe7bb72457d1d8cdc53bf`.
It starts at `2026-10-06T15:48:50.888850792Z` and exits successfully at `15:49:23.070177293Z`.
Its completed receipt has revision 1, exit code 0, and the expected fixture result.
The exact Docker log envelope has SHA256 `b36d468780be72d80e30bc8f3412a479324c57ca29c5c5b284003407bba058da`.
Only its public shape, lengths, checksum, and validation results enter the evidence packet.

The database job remains Dispatched with no persisted receipt.
Its last update equals dispatch time, `15:48:51.502946Z`.
Its lease expires at `15:49:51.438013Z`; the JavaScript node records failure at `15:49:51.527Z`.
Both Python reads and both JavaScript reads commit exactly once.
TypeScript and Rust do not start.
The final generic error remains visible after browser reload.
The stopped JavaScript container remains present. Cleanup acceptance does not pass.

Evidence resides under `/private/tmp/elitea-code-recovery-readiness-20261006/worker-only-operator-v6/failed-terminal-1/`.
`ORIGINAL_JS_RECEIPT_TIMING.json` supersedes the earlier incomplete receipt-shape check in `ORIGINAL_JS_EXIT.json`.

## Source boundary

The [isolation assessment](code-node-isolation-assessment-20260928.md#current-platform-behavior) records current-platform state and output behavior.
This correction extends the new durability contract. It does not copy the current platform's process lifetime.

`sandbox/service.rs` owns the authenticated Submit request.
`sandbox/docker_supervisor.rs` owns `run_owned`, its execution permit, lease heartbeat, receipt persistence, and runtime cleanup.
Previously, the request directly awaits `run_owned`.
Dropping that request also drops the lease heartbeat and result collector.
The durable job and its original runtime remain present.
Replacement requests cannot acquire a live lease, even with the same Supervisor identity.

After lease expiry, the Supervisor rejects platform-owner observation.
Main preserves this refusal, and the Worker ends the observation with `AuthorizationDenied`.
The failure occurs before the existing 120-second Submit timeout and 150-second Worker observation deadline.
Neither deadline is the cause of this failure.

## Correction

Give authenticated execution tasks a bounded Supervisor service owner.
Keep their normal capacity permits, immutable intent, lease renewal, original deadlines, and terminal persistence.
Let an RPC disconnect stop only its response wait.
Reap completed tasks and stop admission during service shutdown.
Abort and drain service-owned tasks before the process closes its database pool.
Normal shutdown and listener failure await every owned task.
Forced outer cancellation closes admission and aborts the bounded task set through its scope guard.
That synchronous guard cannot await task joins. It preserves the existing 15-second outer shutdown boundary.
Sender loss during owner shutdown returns `Unavailable` so the Worker reconciles the same job.
Unexpected sender loss while admission remains open returns a safe `Internal` error.

The shared Supervisor wraps Docker and Kubernetes runtime adapters.
Apply the same ownership boundary to compiled execution where its request awaits an admitted runtime.
Dependency preparation has the same request-owned heartbeat and receives the same bounded owner.
Compiled receipt recovery also claims and renews a Dispatched lease while collecting its original result.
Retain these existing-dispatch collectors through the same owner.
Their receipt-only guards still prohibit new admission, provisioning, and content authority.
Keep short validation, publication, and indexed hydration operations in their existing scope.
Do not add live-lease takeover, capacity, or authorization exceptions.

## Verification boundary

Six controlled lifecycle tests, nine service tests, and nine compiled tests pass with no failures or ignored cases.
Strict locked, offline, all-features library/test Clippy passes with warnings denied.
Formatting passes for all four changed source paths. The lockfile remains unchanged.
An independent source review identifies no remaining ownership blocker.
The initial lint failure and intermediate checks remain in the private evidence packet.
Native linking reports the existing macOS large-`__eh_frame` warning.
The source packet is `/private/tmp/elitea-code-supervisor-owned-jobs-20261006/`.
These checks use one Cargo job and no service, database, or credential environment.
Native lifecycle tests do not prove deployed recovery. The separate acceptance below supplies that proof for Worker loss.

## Deployed Worker restart acceptance

The unchanged fixture runs with marker `gate5-worker-recovery-delay-20261006-v7` in persistent chat 825.
It uses pipeline 143, saved version 166, and execution `a616b7ad27dba1187664b51da230a260`, generation 1.
Only the Worker restarts during the active JavaScript node. Main, Supervisor, and Web remain running.

| Component | Source revision | Deployed image |
| --- | --- | --- |
| Worker | `fee3d059dc493b8061f356f0f6e13c5c94f06190` | `sha256:38c6306b4d9fa004608b56bc35767ce8dea6bd3205ecf8345bf0501e703570bd` |
| Supervisor | `45b152a92854997911401a8d08263b220e065a99` | `sha256:d3b2bca6a3ccab0a23cb1a8570ea38c31e6a298ce3bad924155abf415d9939a4` |

The replacement Worker claim has attempt 2. The execution and original JavaScript runtime remain unchanged.
The runtime identity is `b10535207f183a0ab0cb42b02ea4b1e8`.
All four language nodes complete: Python, JavaScript, TypeScript, and Rust.
Eight unique reads commit, with one `user_get` and one `application_list` operation per language.
The operator records five checkpoint identities and seven dispatches: three preparations and four Code executions.
All seven runtime containers are absent after completion. No owned sandbox lease remains active.

The final response has identity `10e07ff2-e015-53ce-8b96-b7a0bdce5239` in response group 7368.
Its persisted, live browser, and reloaded browser forms share canonical SHA256 `08f00e7d57e57cad3fe4581551278ad6f06bc312734baacd3c0a6e6218e2f4bc`.
The reloaded chat contains one v7 request and one final result. No Stop control remains.
The selected version remains `gate5-recovery-delay`.
Earlier failed results remain in history and are not removed to make this acceptance pass.

Evidence resides under `/private/tmp/elitea-code-recovery-readiness-20261006/worker-only-operator-v7/`.
`watch-1/CONTROL_RECEIPT.json` records the actual fault and restoration. It does not claim recovery by itself.
`verify-1/VERIFY_RECEIPT.json` proves ledger, effect, runtime, and cleanup outcomes.
`verify-1/BROWSER_RECEIPT.json` supplies the separate live and reload proof.
The database receipt retains its original `browser_verified: false` value. The browser receipt references its exact checksum.
`verify-1/LIVE.png` and `verify-1/RELOAD.png` preserve the displayed result.

This acceptance closes this exact Worker-loss recovery boundary.
It does not prove Supervisor or Main replacement, Kubernetes restart recovery, complete-cohort load, or typed failure display.
These gates remain separate. Production capability registration remains disabled. Point 5 remains open.
