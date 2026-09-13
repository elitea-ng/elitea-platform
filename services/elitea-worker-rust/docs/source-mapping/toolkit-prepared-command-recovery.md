# Prepared toolkit command recovery

## Contract

A Main restart must not discard an admitted toolkit command whose signed bytes are already durable.
Recovery appends those exact bytes. It does not create an execution or reconstruct inputs from current toolkit settings.

| Responsibility | Source |
| --- | --- |
| Existing durable signed envelope and publication state | `services/elitea-main/internal/db/queries/runtime_toolkit_call_tool.sql` |
| Sign and store the command within admission | `services/elitea-main/internal/infra/db/repos/toolkit_call_tool_preparation.go` |
| Select pending prepared commands and recheck authority | `services/elitea-main/internal/infra/db/repos/toolkit_call_tool_dispatch.go` |
| Publish the stored envelope without signing | `services/elitea-main/internal/application/toolkitcalltool/dispatch.go` |
| Own and stop the background recovery loop | `services/elitea-main/internal/runtimecomposition/toolkit_call_tool_runtime.go` |
| Attach recovery to the existing publisher lifecycle | `services/elitea-main/internal/runtimecomposition/composition.go` |
| Fence repeated delivery before provider invocation | `services/elitea-worker-rust/src/protocol/toolkit_invocation.rs` |

The current platform is a business reference. It does not supply the new durable crash-recovery contract.
The Rust invocation fence remains unchanged. This Main change restores delivery to that existing fence.

## Implementation

The publisher selects at most 32 prepared, unpublished toolkit commands every ten seconds.
Each iteration has an eight-second context limit. It retries transient publication errors on the next iteration.
The query scopes candidates to the configured stream and `toolkit.call_tool.v1` capability.
It excludes expired, retired, cancelled, and authority-granted commands.
The dispatcher rechecks the durable binding before publication. It never calls the signing method during recovery.
Publication uses the original outbox identity and signed bytes. Existing worker admission handles exact redelivery.
The existing schema supplies all required fields. No migration or new table is added.

## Evidence

`TestPostgresToolkitPreparedRecovery` passes against a disposable PostgreSQL database without skipping.
It admits work, stores a prepared envelope, and replaces the repository and dispatcher instances.
A simulated Redis failure leaves the command recoverable. The next attempt publishes identical bytes and preserves one execution.
The test rejects reconstruction and signing. A cancellation prevents a stale recovery candidate from appending.
The toolkit application tests pass. Toolkit and runtime composition packages compile.

This test models Main replacement using new instances. It does not prove a live container restart with Redis and a provider.

## Remaining recovery windows

Production admission now prepares and stores the signed envelope inside the existing admission transaction.
A signing or storage failure rolls back the job, outbox, and input bundle. A committed admission always has a prepared command.
The single production composition supplies the existing command producer as its preparer. The repository can omit preparation for legacy-row tests.
Legacy unprepared rows from older deployments remain unrecoverable without the original command scalars. They retain the existing expiry behavior.
The existing SQL header describes the original synchronous-only implementation. Prepared commands now have the recovery path documented here.

External MCP client reconnection and successful continuation after an ambiguous provider invocation remain separate acceptance gates.
Terminal reconciliation that avoids duplicate effects is not successful continuation.

## Deployment

Main uses image `elitea-main:prepared-recovery-20260913`.
Its digest is `sha256:4457c5fcf267827507e52301a573c20135e24dfadd22effec4afbb11f19e3cba`.
The replacement preserves the six mounts, environment, networks, and resource limits. No active claim existed during replacement.
The container reports `running healthy`. UI and Rust images remain unchanged.

## Atomic admission evidence

`TestPostgresToolkitAtomicPreparation` passes without skipping against a disposable PostgreSQL database.
It verifies rollback after signing failure and recovery immediately after admission without foreground dispatch.
It compares the prepared command identity, input references, tool binding, and deadline with the committed admission.
An idempotent replay preserves the execution and does not sign again. Changed arguments produce an idempotency conflict.
The existing prepared-recovery test also passes after this change.
These tests use controlled envelopes. Live acceptance must still verify the real signer, Redis, worker, and external client together.

Atomic admission is deployed as `elitea-main:atomic-toolkit-admission-20260913`.
Its digest is `sha256:788fe5a093445918e18608046b71011170039fd1a742cebeb2cc0a7b10dbc8d7`.
Main reports `running healthy`. The replacement preserves the environment and six mounts without schema changes.
