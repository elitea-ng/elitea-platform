# Python indexed dependency delivery

Date: 2026-10-01.

The worker retains one resolved bundle across publication, hydration, and execution retries.
The supervisor keeps package bytes outside control RPCs and graph checkpoints.
Automatic preparation remains optional and requires separate deployment admission.

## Source mapping

| Current-platform behavior or existing contract | Rust implementation | Result |
| --- | --- | --- |
| SDK Code execution permits package setup before source execution. | `src/agents/graph/code_preparation.rs` | Preparation returns an immutable bundle before the final execution journal is registered. |
| Native Pyodide resolution freezes exact wheel content. | `src/sandbox/dependency_bundle.rs` | Bounded metadata identifies safe files, lengths, SHA-256 digests, and one recorded root. |
| Main grants bind content access to project and execution scope. | `src/sandbox/dependency_content_transfer.rs` | Each indexed RPC obtains fresh authority and transfers one recorded file. |
| ADK Docker and Kubernetes adapters own original runtime identities. | `docker_dependency_delivery.rs` and native content helpers | Hydration imports verified bytes into the original inert runtime. |
| Existing runtime receipts prevent duplicate execution. | `docker_hydration.rs` and `docker_supervisor.rs` | Hydration stays Reserved. Submission verifies final metadata before durable dispatch. |
| Existing cancellation intent fences later dispatch. | `docker_hydration.rs` | Heartbeats observe Stop and terminate the original runtime before returning terminal readiness. |

Rust paths are relative to `services/elitea-worker-rust`.
Current SDK source and commit evidence remain in the linked Code preparation mappings.
The new transfer protocol repairs deployment and recovery boundaries without copying the current runtime implementation.

## Indexed acknowledgement

`HydrateSandboxDependencies` requires execution and content grants for the same activation, fingerprint, and recorded root.
A successful reply acknowledges one selected file.
A busy ownership lease returns Aborted and never acknowledges a file.
The final index verifies every imported file through a bounded streaming digest sink.
It imports metadata last and returns Ready without executing user source.
A dispatched or terminal receipt returns Ready so submission can reconcile that original job.

Submission revision 2 carries the same bounded metadata used during hydration.
Submission verifies exact metadata from the original runtime before dispatch.
Legacy revision 1 carries neither metadata nor content authority.
Metadata remains limited to 128 KiB. Package content never enters the protobuf request.

## Expiring authority and retained exports

A native export can finish after its content grant expires.
The content client retains verified export staging under the exact root and index.
The cache owns private temporary directories and stores no grant or credential.
Four cache entries hold at most 128 MiB of package files.
Access prunes entries after five minutes without use.
Active transfer handles retain their directories until release.
Successful publication removes the cache entry.
The deployment separately bounds its private staging filesystem.

An expired data-plane grant returns Aborted after admission has verified the request.
The worker retries the same index with fresh authority and the retained export.
Replacement processes can export again from the original retained preparer.
Exact published metadata permits completion after a crash between shared publication and terminal receipt persistence.
Registry resolution never repeats after its durable bundle record exists.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
