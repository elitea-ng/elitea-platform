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
