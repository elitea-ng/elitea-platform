# Python preparation in the worker

Date: 2026-10-01. Gate 5 remains active.

The worker now coordinates trusted preparation, indexed publication, and offline execution.
Preparation remains disabled unless the selected Python runtime contains its optional `preparation` profile.
This record does not claim deployed browser acceptance.

## Source mapping

| Current behavior or target contract | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` installs missing packages during Python execution. | `agents/graph/code_preparation.rs::prepare_python_dependencies` | The worker coordinates separate preparation before it requests execution authority. Native adapters retain package behavior. |
| Saved Code nodes use an invocation-owned sandbox runtime. | `agents/graph/code_remote.rs::RemoteCodeRuntime::execute` | Literal-source admission remains in place; Cargo declarations are outside this feature. |
| Preparation must retain one admitted identity. | `sandbox/preparation.rs::preparation_activation` and `PreparationJob::fingerprint` | Preparation uses a separate activation and content domain. Worker replacement does not select another activation. |
| Main owns claim-bound authorization. | `protocol/sandbox_authority.rs` and `sandbox/client_preparation.rs` | Preparation, publication, and execution retain the sealed worker session and producer identity. |
| Package files must remain outside control messages. | `SandboxClient::publish` | Each RPC selects one recorded file index. The supervisor transfers bytes through Main's private content service. |
| Execution must consume the recorded frozen bundle. | `PreparedJob::with_python_dependency_bundle` and `SandboxClient::submit_with_dependencies` | The final request fingerprint includes the root. Separate fresh grants authorize content delivery and execution. |
| Content authority expires after a short admission interval. | `sandbox/client_hydration.rs` and `code_preparation.rs::hydrate_python_dependencies` | Each selected file uses fresh execution and content grants. Successful hydration acknowledges one index before user dispatch. |
| Stop must survive worker loss during preparation. | `sandbox/dispatch.rs` and `CodeRuntimeFactory::stop` | The journal records preparation before authorization. Stop selects both preparation and execution audiences. |
| Preparation must not advance the graph frontier. | Existing `CodeNode::execute_code`, graph compiler, and PostgreSQL checkpointer | State updates follow the final execution receipt. Preparation does not add graph state or another checkpoint owner. |

## Configuration and compatibility

The optional profile contains `target`, `audience`, `image_digest`, `policy_revision`, and `timeout_seconds`.
The configuration validator accepts preparation only for Python.
It rejects mutable images, invalid audiences, unsafe targets, and unbounded timeouts.
One audience must map to one target because durable Stop metadata records the audience.
Bootstrap creates a separate verified transport for the configured preparation backend.

Profiles without preparation retain the existing revision 1 execution bytes.
The worker rejects bundle execution through a submission method that supplies no content grant.
Configured preparation produces revision 2 execution requests after confirmed publication.
State-supplied source retains its separate approval requirement.

## Journal and receipt transitions

Register preparation activation, preparation digest, and supervisor audience before requesting a grant.
Retain Pending status when the supervisor returns ready bundle metadata.
Validate metadata through the shared strict bundle model.
Retain the verified root and file list for every publication retry.
Publish file indices from zero through the final file index.
Publish metadata at the index equal to the file count.
Acquire fresh exact-root authority for each indexed operation.

Only confirmed terminal status resolves preparation delivery.
Completed permits execution. Failed and Cancelled stop dependent pipeline nodes.
Pending, unknown transport outcomes, and uncertain receipts retain the journal entry.
Terminal cleanup metadata describes cleanup after confirmed termination; it does not change terminal status.
A concurrent terminal publication reply remains valid under authority for the locally retained root.

Bind the recorded root into the final execution request.
Register final execution under the original Code activation and its resolved request digest.
Retain the immutable bundle metadata from the preparation receipt.
Hydrate file indices from zero through the final file index.
Hydrate metadata at the index equal to the file count.
Each hydration attempt requests fresh execution and content grants for the final activation and digest.
An acknowledged file advances its index. A transient failure keeps the same index.
Ready permits submission of the same prepared job.
Hydration uses a fixed observation deadline derived from the preparation timeout plus 90 seconds and does not dispatch user source.
Execution observation starts only after hydration reports Ready.
The supervisor preserves readiness time from runtime binding and execution time from durable dispatch; retries do not reset either phase.
Request separate content and execution grants for that same request.
Final submission carries the same bounded bundle metadata that hydration verified.
Retain each activation and digest when grants expire or transport retries occur.
Package bytes and grants do not enter the command bus or graph checkpoints.

Preparation, hydration, and final submission retry temporary supervisor capacity refusal.
`ResourceExhausted` retains the same activation, request, root, and selected index.
Authorization rejection remains a hard refusal.
All retries remain inside their existing hard deadlines.

## Configuration changes during recovery

A worker can stop after preparation completes but before it registers final execution.
Disabling preparation at that point could otherwise admit a revision 1 execution without the recorded root.
`DispatchJournal::contains_activation` checks prior preparation registration across generations, including resolved rows.
The disabled Python path refuses that downgrade and asks the operator to restore the preparation profile.
Changed enabled profiles conflict with the immutable preparation request before another resolution can occur.
This correction uses the existing journal schema.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
