# Python dependency bundle shared storage

Date: 2026-10-01. Gate 5 remains active.

This change stores verified dependency content through Main's existing object-store contract.
It does not connect automatic preparation to production dispatch.
No application schema, public API, deployed runtime, or graph checkpoint changes occur.

## Source mapping

The inspected current SDK revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
Its `infra/data/sandbox/main.ts::install_imports` resolves imports and installs missing packages through micropip.
The SDK supplies behavior references. It does not define a shared dependency receipt contract.

| Current behavior or target contract | New source | Result |
| --- | --- | --- |
| SDK installs missing Python packages. | `adapters/prepare_python_code.mjs` and `preload.mjs` | Native resolution creates frozen content before execution. Requirements reject controls and non-ASCII text before resolution. |
| Preparation retains an independently recorded bundle root. | `libs/proto/elitea/runtime/v1/sandbox.proto` | Comments define JSON ordering, file ordering, byte limits, and publication order. No wire fields change. |
| Execution retains package content after owner replacement. | `services/elitea-main/internal/infra/storage/sandbox_bundle.go::SandboxBundleStore` | Main's S3, Azure, or GCS abstraction supplies shared content. Local files serve as temporary upload staging only. |
| Bundle identity matches native Deno output. | `ParsePythonSandboxBundle` | Strict metadata validation checks the recorded root against canonical ASCII JSON. Disabled HTML escaping preserves native version ranges. |
| Invalid uploads cannot replace verified content. | `SandboxBundleStore::PutFile` | Bounded private staging verifies size and SHA-256 before any object-store write. |
| Completed preparation cannot reference missing files. | `SandboxBundleStore::Publish` | Every shared file passes size and hash checks before bundle metadata is published. |
| Delivery detects corrupt or incomplete shared content. | `OpenBundle` and `FetchFile` | Metadata and files have independent bounds. Downloaded files require verification before use. |
| Execution admission owns the dependency root. | `sandbox/request.rs`, grant verification, and the Code runner | The earlier revision 2 contract binds the exact root. This storage component does not issue execution authority. |

The [preparation record](code-python-demand-preparation-20261001.md) describes native discovery and resolution.
The [admission record](code-python-bundle-admission-20261001.md) describes signed request identity and offline consumption.

## Ownership and bounds

Main's platform object namespace contains the logical `sandbox-dependencies` bucket.
Each key includes a tenant hash, resource project, bundle root, and validated file name.
Numeric user-project object namespaces cannot address this platform namespace.
Public-project sharing does not grant access to these objects.

The scope constructor validates identity shape. It does not authorize a caller.
Production callers must derive scope and root from verified execution authority.
This component has no HTTP route. It cannot accept user-selected tenant headers.

Metadata permits 128 requirements of at most 256 ASCII bytes each.
There are at most 257 files and 128 MiB of content per bundle.
The lock limit is 1 MiB. The wheel limit is 32 MiB.
Metadata has a 128 KiB limit.
Unknown fields, unsafe names, and unordered or duplicate file entries are rejected.
Nil or zero-value bundle metadata cannot be published.

The constructor requires a private real staging directory and bounded concurrency.
Upload files use mode `0600`. Their complete content is verified before upload.
Wheel content is streamed through hashing rather than retained in memory.
Admission rejects overload without spawning a waiting task.
Request cancellation remains distinct from content rejection.
Body closure and staging cleanup errors are retained.
The caller owns its staging directory, deadlines, and input-body cancellation.

Bundle publication writes metadata last.
Concurrent writes of the same verified content preserve the same identity.
The supervisor must commit a preparation receipt only after successful publication.
An ambiguous storage response requires verification or an exact-content retry, not registry resolution.

## Verification

Eight focused Go tests and their admission subcases pass.
They check native Deno identity, metadata limits, invalid scope, corrupt uploads, corrupt shared content, and namespace separation.
They also check publication order, bounded admission, cancellation, private staging, cleanup, and response-body closure.
Replacement tests remove the original staging directory before reading shared files.
Race checks and `go vet` pass for the changed storage package.
The full storage package and conformance package pass their default test commands.
Unconfigured S3, Azure, and GCS emulator cases skip in that default command.

A separate Linux test runs against the existing rehearsal RustFS service.
It uses a disposable non-root container and an isolated test namespace.
Credentials enter through a private temporary environment file.
They are not printed or passed as command arguments.
Only objects created by this test are deleted.

The native range bundle has root `71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625`.
All four files pass upload verification, publication, replacement-store reads, and byte comparison.
Another resource project cannot read that bundle through the store component.
The Linux storage case completes in 0.08 seconds locally. This small case is not a capacity benchmark.

Downloaded content then passes four native Deno execution checks in a separate non-root, offline container.
Automatic imports and inline version constraints produce this exact result:

```json
{"records":20000,"total":1001850000,"formatted":"1,001,850,000","size":"20.0 kB","status":"PASS"}
```

Both runs preserve the mounted content and recorded root.
Wrong-root and revision-downgrade checks reject execution before user code.
Four preparation-policy checks also pass, including controls and non-ASCII requirement rejection.
Execution uses a two-CPU quota, 512 MiB memory, and disabled network access.
The writable workspace has a 256 MiB bound. The root and package mounts are read-only.
The test image is `sha256:014f9284c69f9ca5283918a9cce0ce46b8c15f49aae64318b9704564a08dbb95`.
Changed adapter sources are mounted read-only for these checks.
This is component proof, not a new deployment.

The initial cross-build encounters a restricted host Go cache.
The corrected command uses a task-owned temporary cache and bounded build parallelism.
The first unsafe-name test exposes inconsistent error classification.
The storage boundary now rejects names before object-reference construction.

## Integration remains open

Compose authenticated upload, publication, and download transport with this store.
Bind supervisor content access to the verified recipient, resource project, operation, and bundle root.
Do not treat a worker execution grant as unrestricted supervisor storage authority.
Keep package bytes outside the sandbox control request and shared queue.

The supervisor still needs bounded preparation admission, durable preparation receipts, and content delivery to both runtime backends.
Worker recovery must reuse an admitted bundle without repeating registry resolution.
Preparation cancellation, unavailable packages, registry denial, incompatible wheels, and interrupted publication require deployed verification.
Persistent main-chat and ephemeral-editor acceptance remain mandatory after integration.
Azure and GCS live bundle tests remain unverified.

JavaScript/TypeScript on-demand preparation and user-selected Cargo dependencies remain separate contracts.
This change does not close platform-client access, workspace authority, debug artifacts, or source-variable admission.
