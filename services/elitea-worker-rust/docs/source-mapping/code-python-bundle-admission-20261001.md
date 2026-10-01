# Python bundle admission and execution

Date: 2026-10-01. Gate 5 remains active.

This change adds the dependency identity and execution consumer.
It follows the [Python preparation component](code-python-demand-preparation-20261001.md).
Automatic supervisor preparation, shared storage, and delivery remain open.
The deployed application still uses image dependency profiles.

## Source mapping

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` resolves and installs missing packages during execution. | `adapters/prepare_python_code.mjs` and `adapters/python.mjs::executePythonRequest` | Separate preparation resolves content. The execution consumer verifies its admitted digest before it evaluates source. |
| SDK inline micropip calls retain installation order and constraints. | `python.mjs::executePython` | Calls use the verified frozen package closure with network access disabled. |
| No legacy immutable package receipt is assumed. | `sandbox/request.rs::PreparedJob::with_python_dependency_bundle` | Revision 2 binds the dependency root into the existing request digest. |
| Main owns job authorization. | `internal/transport/runtimegrpc/control/sandbox_grant.go` and Rust `protocol/sandbox_grant.rs` | The existing signed grant covers the changed request digest. No new grant format is needed. |
| Native ADK workspace manifests provision execution files. | `PreparedJob::manifest` and runner `src/execute.rs` | The manifest carries the bounded request. Package content still needs separate delivery. |

## Contract and ownership

PreparedJob revision 1 remains byte-for-byte compatible.
Its fingerprint domain, field order, and exact fixture digest remain unchanged.
Revision 2 adds `dependency_bundle_sha256` for Python only.
This field has exactly 64 lowercase hexadecimal characters.
The fingerprint covers the revision and bundle field through the existing length-prefixed serialization.
The bundle digest covers the native runtime, requirements, file names, lengths, and content hashes.
Main signs this complete request digest through its existing sandbox grant.

The consumer rejects missing identities, explicit nulls, invalid digests, other languages, and revision downgrades.
Transport decoding rejects duplicate and unknown request fields.
Changing or dropping the root invalidates the signed grant and prevents receipt reuse.
An independently recorded preparation receipt must supply the root.
Cache metadata does not supply authorization.

The launcher uses the fixed `/workspace/wheels` path.
Revision 1 copies image-owned Python assets into the private job cache.
Revision 2 uses delivered assets and skips that copy.
This prevents the baseline image lock from replacing the admitted lock.
The Python adapter verifies every referenced file before it creates the interpreter or evaluates user source.
It retains the existing structured result and terminal receipt formats.
Network, subprocess, FFI, user identity, and resource policies remain unchanged.

Package bytes do not enter the one MiB control request.
The protobuf source documents both request revisions without changing field numbers or generated wire types.
No application database migration is needed for this consumer change.
Older supervisors reject revision 2. Deploy the complete preparation path before enabling automatic revision 2 submissions.

## Verification

Ten focused request checks pass, including the exact revision 1 bytes and fixture fingerprint.
The grant checks verify an accepted root, a substituted root, and removal of the root.
Nine runner unit checks pass.
Runner and worker library Clippy pass with warnings denied.
Rust formatting, adapter formatting, adapter lint, and patch whitespace checks pass.

Four offline adapter checks pass in Linux containers.
They cover automatic imports, inline requirements, substituted content identity, and revision downgrades.
Both successful cases process 20,000 values and retain this exact result:

```json
{"records":20000,"total":1001850000,"formatted":"1,001,850,000","size":"20.0 kB","status":"PASS"}
```

Fifteen real PID-1 probes pass through the built launcher and terminal receipt path.
They include baseline Python, JavaScript, TypeScript, execution errors, denied network, denied subprocesses, and timeout.
Bundle checks include success, wrong root, missing directory, missing metadata, altered wheel, altered requirements, downgrade, and another language.
Every rejected bundle stops before the user-code marker is printed.
Repeated log reads return the identical terminal envelope.
Each container stops and is removed after verification.

The immutable Docker image is `sha256:014f9284c69f9ca5283918a9cce0ce46b8c15f49aae64318b9704564a08dbb95`.
The tested package root is `71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625`.
Tests use UID 10001, one CPU, 512 MiB memory, 64 PIDs, disabled network, and a read-only root.
The package mount is read-only. The writable workspace has a 256 MiB bound.

An initial probe uses the build config digest instead of the local image identity.
Docker refuses that image before execution. The corrected probe reads the canonical identity with `docker image inspect`.
Another probe cannot mutate its read-only tamper fixture.
The fixture now changes its bytes before it applies read-only permissions.
Neither correction changes runtime permissions.
The local worker test linker reports its existing compact-unwind size warning. All selected tests pass.

## Remaining acceptance

The producer must persist a scoped preparation receipt before requesting the execution grant.
It must retain that root across retries and restarts.
Implement bounded registry acquisition, cancellation, atomic publication, shared content, and delivery on Docker and Kubernetes.
The current Kubernetes helper provisions only the two control files.
This change does not extend its bulk package data plane.
Verify persistent chat and ephemeral editor execution after deployment.
Prove missing-package guidance and downstream suppression through the complete application.
These component and container checks do not prove integrated on-demand installation or browser acceptance.
