# Verified Python package content client

Date: 2026-10-01. Gate 5 remains active.

This component transfers native Python bundles between a supervisor and Main's private content service.
It does not dispatch dependency preparation or change the application deployment.
Package bytes remain outside control RPCs, graph checkpoints, and Redis.

## Source mapping

The current SDK revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
SDK `infra/data/sandbox/main.ts::install_imports` supplies the missing-package behavior reference.
The replatform separates native resolution, immutable publication, and offline execution.

| Reference behavior or new contract | New source | Implementation |
| --- | --- | --- |
| SDK installs missing Python packages before source execution. | Code runner `adapters/prepare_python_code.mjs` | Existing native preparation produces frozen content. This client adds no resolver. |
| Prepared content must survive supervisor replacement. | `sandbox/dependency_content.rs::DependencyContentClient` | The supervisor uploads files through Main's existing shared object store. |
| Execution must use the previously recorded root. | `PythonDependencyBundle::parse` | Strict metadata validation reconstructs the native digest and compares an independent root. |
| Package content requires separate authority. | `DependencyContentClient::{download,publish}` and `protocol/wire.rs` | Each transfer requests a root-bound content grant. Main verifies the signature and TLS recipient. |
| Corrupt content must not reach user code. | `write_verified_file` and `open_verified_file` | File length and SHA-256 checks precede returned staging or uploads. |
| Staging is temporary, with one owner. | `StagedPythonDependencies` | Owned private directories are removed after release or failed download. |
| Failed preparation must not admit execution. | `sandbox/preparation.rs` and `protocol/sandbox_grant.rs` | A separate preparation request and authority bind their own fingerprint. |

See the [content service record](code-python-bundle-transport-20261001.md) for Main's routes and claim checks.
See the [preparation authority record](code-python-preparation-authority-20261001.md) for the separate request identity.

## Transfer contract

The client accepts an HTTPS origin, private CA, supervisor client identity, private staging path, capacity, and deadline.
It disables ambient proxies, public trust roots, and redirects.
TLS verification remains mandatory.
Construction refuses unsafe origins, invalid TLS material, non-private staging, and unbounded capacity.

Each metadata or file request invokes the grant factory again.
The client refuses execution, cancellation, and wrong-root grants before transport.
This purpose check is not signature authorization. Main verifies the exact signature and authenticated recipient.
The header is marked sensitive. Safe transport errors omit private URLs and response bodies.

Metadata has a 128 KiB limit. The lock file has a 1 MiB limit.
A wheel has a 32 MiB limit. Total bundle content has a 128 MiB limit.
The bundle admits at most 257 files and 128 bounded ASCII requirements.
Names are sorted, distinct, and safe for one directory entry.
Strict JSON refuses duplicate fields, unknown fields, unsupported revisions, and changed roots.

Downloads stream files to mode 0600 within an owned mode 0700 directory.
Exact content length, media type, and SHA-256 must match the recorded bundle.
Metadata becomes available only after every file passes verification.
No successful staging owner is returned for partial or corrupt content.

Publication opens regular files without following final symlinks and verifies their exact bytes.
Uploads stream through the same file handle with a 64 KiB buffer.
Main publishes metadata only after all file uploads succeed.
Cancellation or failed publication leaves no durable local authority; recovery retains the same recorded root.

Transfer admission uses a fixed semaphore with no waiting queue.
Capacity permits 1–32 operations. The default composition is not enabled by this component.
Connection timeout is 10 seconds. Each HTTP request has a 30-second timeout.
The whole operation has a caller-selected deadline of at most five minutes.
Grant acquisition is inside that deadline and has no detached task.

## Verification and history

Eight focused Rust tests pass. One native mTLS case runs separately with its fixture.
Tests cover metadata bounds, independent roots, duplicate fields, path refusal, authority purpose, symlinks, corruption, deadlines, and admission capacity.
Strict supervisor-feature Clippy passes. Assigned-file formatting and whitespace checks pass.

The native case uses the real Main content handler and rehearsal RustFS.
It closes the first TLS listener, removes its staging directory, and creates a replacement listener and store.
The Rust client downloads and republishes all four native files with verified mTLS.
It requests authority for each transfer, refuses a tampered signature, and removes its owned staging directory.
Another project cannot read the stored bundle. Test-scoped objects are removed afterward.

The bundle root is `71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625`.
The component case completes in 0.27 seconds locally; the Rust portion takes 0.13 seconds.
These timings are not a capacity benchmark.
The Rust and Go test programs run natively on macOS. RustFS runs in the rehearsal Docker deployment.
A disposable non-root forwarding container provides bounded access to its test endpoint.
The test does not prove Linux execution, active database claim issuance, or supervisor crash recovery.

The first native attempt exposes a test certificate issue.
The fixture used a CA certificate as the server leaf, which rustls correctly rejects.
The fixture now uses separate CA, server, and client certificates. TLS verification is not weakened.
Focused tests also expose and fix invalid-port acceptance, default staging permissions, and declared-length handling.

User-authorized cleanup previously recovers approximately 190 GiB of allocated build storage.
Cold local tests take approximately two minutes with incremental artifacts disabled and line-table debug information.
The rebuilt worker target occupies approximately 2.9 GiB at this check.
No source, pending work, Git history, or deployed image is removed.

## Integration remains open

Connect the trusted preparation runner to Docker and Kubernetes supervision.
Retain native package files until shared publication succeeds.
Persist a bounded preparation receipt before execution admission.
Supply fresh content grants through the active worker claim.
Recover the same preparation identity and root after interruption.
Verify runtime delivery, cancellation, restart recovery, and both chat interfaces after deployment.

JavaScript/TypeScript on-demand acquisition, user-selected Cargo dependencies, and compiled-artifact caching remain separate open work.
Prepared Python, JavaScript, TypeScript, and Rust execution already has deployed acceptance evidence.
This component does not close Gate 5.
