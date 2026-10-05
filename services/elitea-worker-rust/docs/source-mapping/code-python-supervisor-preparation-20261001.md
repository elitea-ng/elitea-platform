# Python supervisor preparation lifecycle

Date: 2026-10-01.

The native resolver produces Python lock metadata and wheel files before source execution.
The supervisor now retains that original runtime until shared publication and confirmed termination.
The worker receives bundle metadata and obtains fresh content grants.
Package bytes remain on the supervisor data plane.

## Source mapping

| Existing behavior and source | Supervisor source | Result |
| --- | --- | --- |
| `services/elitea-code-runner/adapters/prepare_python_code.mjs` resolves dependencies and freezes their content identity. | `src/sandbox/docker_preparation.rs::prepare_authorized` | The supervisor admits one bounded preparation request under typed preparation authority. |
| Native preparation output must survive until shared upload. | `python_preparation_job.mjs` and `PreparationJob::manifest` | An image-owned program retains resolved files. Literal source appears only in the request file. |
| The runner supervisor owns durable receipts and original runtime binding. | `provision_preparer` and `observe_preparer` | The existing runtime adapter and ledger retain one original instance across replacement. |
| The existing ledger permits active and terminal receipts. | `JobLedger::record_preparation_bundle` and `JobLedger::release` | Resolution remains Dispatched. The bounded record remains immutable after its first durable write. |
| Main revision 3 grants authorize root-bound private content transfer. | `publish_authorized` and `DependencyContentClient::publish_index` | One RPC transfers one recorded file or final metadata under fresh content authority. |
| ADK runtime adapters export exact original workspace bytes. | `stage_preparation_index` | Private staging contains one file. The content client verifies its size and SHA-256 before upload. |
| Completion requires durable shared content and stopped preparer resources. | `complete_preparation` | The supervisor persists Completed only after metadata publication and confirmed runtime termination. |
| Existing revision 2 stop grants record irreversible cancellation intent. | `stop_if_requested` and `cancel_authorized` | Cancellation fences dispatch, publication boundaries, bundle recording, and successful completion. |
| Prepared execution identifies a recorded dependency root. | `hydrate_authorized`, `docker_hydration.rs`, and `docker_dependency_delivery.rs` | Each Reserved execution call imports one verified index under fresh authority. Submission verifies final metadata before dispatch. |

Paths in the supervisor column are relative to `services/elitea-worker-rust`.
The native program remains in `services/elitea-code-runner/adapters`.

## Admission and identity

Preparation admission and execution admission are mutually exclusive on one supervisor instance.
The preparation profile admits Python preparation with its configured image digest, policy revision, and timeout.
The deployment owns registry egress for that profile.
The execution profile retains its existing offline isolation.

`PreparationJob` retains its exact revision 1 transport bytes and separate fingerprint domain.
Existing Main revision 1 signing authorizes that typed request.
Revision 2 authorizes stop intent.
Revision 3 authorizes the recorded root for private shared content transfer.
Content authority cannot provision a runtime or invoke dependency resolution.

The preparation marker binds source SHA-256, image digest, policy revision, timeout, and start and deadline times.
The parser rejects unknown or duplicate fields, invalid time bounds, and invalid nested bundle metadata.
Bundle metadata identifies every safe filename, length, and SHA-256 digest.

## Durable transition and recovery

The existing `sandbox_jobs` receipt stores one bounded `preparation_bundle_json` value.
Migration `0009_sandbox_preparation_bundle.sql` adds that nullable column without a new table, purpose, or application schema change.
The existing terminal result column cannot hold pending metadata because its CHECK constraint permits results only for Completed receipts.
The preparation record remains present after cancellation, failed publication, or supervisor replacement.

The supervisor records resolution while Dispatched, then releases its lease before returning Pending metadata.
Lease release expires the current ownership interval while retaining its owner and epoch fields.
The next claim increments the epoch.
Old owners cannot renew, publish completion, or change the recorded root.
An occupied publication lease returns a retryable fencing error.
Pending acknowledges a transferred publication index; lease contention never acknowledges that index.

Publication accepts indices from zero through the recorded file count.
Each lower index transfers its exact recorded file.
The final index publishes metadata; Main verifies all referenced files first.
Retries use the same recorded root and original runtime.
The supervisor never repeats registry resolution after durable resolution.

Loss of the original runtime leaves the record recoverable and prevents runtime recreation after dispatch.
An uncertain transfer acknowledgement permits an idempotent retry with fresh authority.
The content client retains verified export staging across an expired-grant retry.
Its bounded cache owns the private directories and stores no authority.
The cache remains disposable and never replaces the durable preparation record.
Before export, the supervisor checks for exact shared metadata under fresh content authority.
Verified shared metadata permits completion after loss of the original files.
This check repairs a crash between successful metadata publication and durable terminal receipt storage.
An uncertain release acknowledgement still requires confirmed termination.
Failed termination retains Dispatched and the recorded root.
Cleanup follows the durable terminal receipt and never removes a live holding runtime.

## Indexed execution hydration

Indexed hydration requires current execution authority and separate content authority for the same job scope and recorded root.
Its bounded bundle metadata identifies each transfer.
The shared inert provision helper retains one original runtime and never signals source execution.
Each successful RPC acknowledges its exact index.
Only the final verified metadata index reports Ready while the receipt remains Reserved.
An already dispatched or terminal receipt reports Ready for submission to reconcile its existing result.

Confirmed provisioning permits lease release after a transfer success or failure.
An uncertain provision error retains its lease until expiry because a remote create request can still finish.
Cancellation observes that barrier before it confirms runtime absence.
The hydration heartbeat checks cancellation during final file proofs and stops those proofs before dispatch.
Final submission verifies the original runtime's complete bundle metadata before marking Dispatched.
Dispatched recovery never downloads packages or recreates execution.
No hydration progress table or additional application schema is introduced.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
