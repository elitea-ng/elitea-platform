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
