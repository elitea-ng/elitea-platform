# Toolkit takeover authority

## Ownership and source mapping

Current Core `projects/centry/pylon_main/plugins/elitea_core/api/v2/test_toolkit_tool.py`
provides the named-operation Test behavior. Its task implementation is a business
reference, not the durable recovery contract for the replacement worker.

Main `internal/infra/db/repos/claims.go::ClaimValidation` owns replacement
execution authority. Rust `src/execution/toolkit_delivery_processor.rs` consumes
that authority for direct MCP reads, Toolkit Test calls, and discovery.
Its `process_output_recovery` reconciles a possibly-started invocation without
calling the provider again. Its `execute_fresh` refuses an already-started call.

## Reproduced defect: 2026-09-13

The Main recovery-only predicate uses `NodeEventCapability`, which covers agents
and indexing but excludes all three toolkit capabilities. After a toolkit claim
expires in `RUNNING/MAY_HAVE_STARTED`, Main issues fresh executable authority.
Rust's Begin check then returns `AlreadyStarted` and retains the command.
This mismatch can keep the command pending instead of reaching terminal recovery.

A real PostgreSQL integration test reproduces the incorrect claim disposition for
`toolkit.execute.read.v1`, `toolkit.call_tool.v1`, and
`toolkit.available_tools.v1`. The existing indexing case passes.
The test first proves takeover in PREPARING, then durably authorizes invocation,
expires that claim, and claims it under a different workload identity and session.

## Implementation

Extend the recovery-only predicate to those three existing toolkit capabilities.
Retain the current durable-result-first ordering. Do not broaden the node-event
capability definition: toolkit results use their own output protocol.
PREPARING remains executable; MAY_HAVE_STARTED receives ambiguous-invocation
recovery authority. Rust already maps that authority to terminal reconciliation.
No database migration, protocol, dependency, or Rust source change is required.

## Verification and limits

`TestPostgresServiceBackedInvocationFence` passes all four capability cases after
the fix; all three toolkit cases failed before it.
`TestPostgresToolkitInboxTerminalRecovery` passes direct read, Toolkit Test,
discovery, and the negative configuration-validation case.
Direct-read output is explicitly marked projected before takeover, matching its
separate projection path; Toolkit Test and discovery complete projection during
claim recovery. This distinction is intentional.
Tests use a disposable migrated PostgreSQL database, not the application database.
No test is skipped in these two suites.

This proves Main's stored authority transition and terminal-result recovery.
It does not prove deployed process replacement, provider request count, browser
termination, or cross-replica spool independence. Deployment and the live crash
acceptance remain open. No claim of resumed provider execution is made: an
ambiguous invocation must not be silently repeated.


## Deployed crash experiment: 2026-09-13

Main repair image `sha256:b0fdf09950766b4119ab50faa77ffb87ae952f4fe60fe3997967ba9f09d9f778`
replaces only Main. It retains the existing image assets and helper binaries,
live environment, six mounts, networks, and resource limits. The binary is
verified as Linux ARM64. Source is `f73a4224` with Main files from the deferred
`cfaf26f2` HITL change restored from its parent in an isolated build context.
No worktree changes are reverted and no migrations run.

A temporary toolkit (33) exposes a synthetic TLS read delayed by 60 seconds.
An observer restarts the existing Rust worker immediately when the fixture
receives the second request. The first test attempt is not crash proof because
it settled too close to the restart.

Controlled execution `73a225b7dbefed5d8c9974214adaef22` remains
`RUNNING/PREPARING` after its provider request starts. Claim 1 expires and the
replacement receives claim 2 through normal runtime mechanisms.
The provider counter rises from 2 to 3: the interrupted operation is invoked again.
The browser reports its bounded wait expired and exposes the same job ID.
No claim, job, output, or Redis state is edited to cause this result.

This contradicts complete invocation recovery. Source confirms that
`toolkit_delivery_processor.rs` wraps both direct execution and shared Test/
discovery execution in `run_pre_invocation` without calling the existing final
invocation authorization. Main cannot classify an unrecorded submission as
MAY_HAVE_STARTED. The repaired Main predicate is necessary but insufficient.

Next implementation must reuse the existing owned invocation-authority contract
in `protocol/control.rs::authorize_agent_invocation` (and its payload/permit
machinery), preserve terminal/no-ACK handling under uncertainty, and cover the
actual provider-submission boundary. Do not add automatic retries to hide this
missing state transition. Repeat the controlled crash test after the Rust fix.

The retried execution eventually settles SUCCEEDED while still PREPARING.
Temporary toolkit 33 is deleted through the normal API (204), and only the
temporary delayed-read server process is stopped. The OAuth emulator stays running.


## Rust invocation boundary implementation

`protocol/toolkit_invocation.rs` adapts the existing owned
`InvocationAuthorizationPayload` contract. Only the shared control client's
`AUTHORIZED_NOW` outcome constructs
`AuthorizedToolkitExecution`; terminal and transport-unknown outcomes do not
expose an executable state. The borrowed execution view retains the existing
input/result binding methods but cannot be consumed to mint another permit.

`toolkit_delivery_processor.rs` materializes and validates the complete request
before crossing this fence. Both direct reads and the shared Toolkit Test/
discovery path require the authorized wrapper before entering runtime operations.
Preparation failures retain their existing terminal path. Authenticated refusal
uses existing typed terminal mapping. Transport uncertainty closes the lease,
leaves the command unacknowledged, and does not call the provider or publish an
invented result. No wire fields, tables, or migrations change.

The control-outcome test covers authorized, already-authorized, rejected, and
transport-unknown responses using the existing signed control fixture.
This is component proof, not a toolkit provider/crash proof. Full recovery still
requires the repeat deployed crash experiment and shared-state takeover gates.
An ambiguous provider outcome is not equivalent to completed work or a successful
continuation; retain that distinction when assessing the overall recovery goal.

An isolated candidate passes all 130 execution component tests with no ignored
tests. Strict all-target/all-feature Clippy passes. Other pending worktree changes
are excluded from this candidate. Runtime deployment and crash re-verification
remain pending for this Rust change.


## Invocation fence deployment and crash verification

Worker image `sha256:929f460521978f2cb4f2189b8319b9074ed0c45daa2ae2c444b86b108cfc1d8b`
contains the Rust invocation fence. The deployment retains existing runtime configuration and mounts.
No database migration runs.

Normal Toolkit Test execution `6dc13869bfbefcdba8efcfc75cbca2b0` settles `SUCCEEDED/MAY_HAVE_STARTED`.
The browser receives marker `RUST_TOOLKIT_INVOCATION_NORMAL_20260913`, mode `stored`, and generation `1`.

A separate temporary toolkit starts one delayed TLS provider request.
The observer restarts the worker immediately after that request arrives.
Execution `80e4eb10605c424dbc11273ef202d8df` retains `RUNNING/MAY_HAVE_STARTED` until its first claim expires.
The replacement receives claim 2 and settles `FAILED/MAY_HAVE_STARTED`.
The ledger contains one output record and one committed failed settlement.
The provider records one call and one disconnect. No second provider call occurs.
Temporary toolkit 34 is deleted through the normal API with HTTP 204.
The temporary fixture stops; the OAuth emulator remains running.

This proves duplicate-submission prevention and terminal reconciliation after a worker crash.
It does not prove successful continuation from an unknown provider outcome.
The UI still displays its earlier bounded-wait message after terminal settlement.
`features/toolkits/api/toolkitTestRun.ts` returns a timeout outcome without subsequent result retrieval.
The result panel therefore does not follow the durable execution after its initial HTTP wait ends.
Runtime continuation and client result recovery remain separate acceptance requirements.

## Deployment compatibility regression

Independent PAT checks pass for toolkit 31 and autonomous pipeline version 18 on this worker.
Saved-agent execution `cababb851b7c013cb44815b947983ee7` fails during native assembly admission.
It returns `UNSUPPORTED_CAPABILITY` before model generation.
The isolated build omits pending skill/persona support present in the earlier deployed worker.
The committed assembly still rejects attached, invoked, applied, and version skills.
Pending `instruction_authority.rs` and its assembly integration remain preserved in the worktree.
Restore and verify that support before accepting the new deployment for saved agents.
The temporary PAT is revoked with HTTP 204. Toolkit sharing returns to its previous disabled setting.


## Compatible deployment restored

The replacement Main image is `sha256:e26fc9aa9049f7f32b7d889a8ca0abe7382b8312cad2ac4c3a3b77bc0983763f`.
The replacement Rust image is `sha256:b354bd321d46a0c21ad8287c5a435235b120b8ffba3d429dfa6f9ad1aa373c72`.
Main retains the takeover repair and restores the preserved instruction snapshot producer.
Rust retains the invocation fence and restores the preserved instruction consumer and persona support.
The Main build excludes deferred direct-HITL history changes.
The worker build includes preserved instruction support from the worktree, beyond committed HEAD.
These images are not described as builds of committed HEAD alone.
Existing Main and worker mounts, environment, networks, and limits remain unchanged.

All 304 focused Rust agent tests pass without ignored tests.
Strict Clippy passes for all targets and features after test-only lint repairs.
Main skill and instruction tests pass in the isolated matching source snapshot.
Runtime composition compiles there; its broad tests require repository fixtures absent from that snapshot.
The full worktree agent execution and storage package tests also pass.

An independent Python client repeats protocol initialization, discovery, and invocation with a new UI-created PAT.
Toolkit 31, saved-agent version 20, and autonomous pipeline version 18 all pass.
Markers are `RUST_PAT_TOOLKIT_RESTORED_20260913`, `RUST_PAT_AGENT_RESTORED_20260913`, and `RUST_PAT_PIPELINE_RESTORED_20260913`.
The temporary PAT is revoked with HTTP 204.
Toolkit 31 sharing returns to disabled through the UI and remains disabled after reload.
This closes the deployment regression, not the outstanding client result recovery gate.
