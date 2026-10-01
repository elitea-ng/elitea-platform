# Python dependency bundle transport

Date: 2026-10-01. Gate 5 remains active.

This change connects verified package storage to Main's private content listener.
It does not connect automatic preparation to supervisor dispatch.
The application database schema and graph checkpoint ownership do not change.
The application deployment and UI do not change during these component checks.

## Source mapping

The inspected current SDK revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
Its `infra/data/sandbox/main.ts::install_imports` discovers missing imports and installs their packages through micropip.
The SDK supplies the package behavior reference. It does not define this shared-content authority contract.

| Behavior or target contract | New source | Result |
| --- | --- | --- |
| Current SDK installs missing Python packages. | `adapters/prepare_python_code.mjs` and `preload.mjs` | Existing native preparation resolves a frozen bundle before offline execution. |
| Main authorizes a supervisor under the worker's active execution claim. | `libs/proto/elitea/runtime/v1/sandbox.proto` and `control/sandbox_grant.go` | A revision 3 grant binds tenant, resource project, supervisor audience, and bundle root. |
| Package content survives the original local staging directory. | `storage/sandbox_bundle.go` | Main's existing object store retains verified files. Local staging contains temporary content only. |
| The supervisor transfers content without object-store credentials. | `storage/sandbox_bundle_http.go` | The private HTTP service accepts a signed grant and verified recipient certificate. |
| Content authority cannot start or cancel code. | `protocol/sandbox_grant.rs` and `sandbox/client.rs` | Normal job and cancellation paths require an empty content-root field and their own grant revision. |
| Unconfigured deployments do not expose broken package routes. | `runtimecomposition/composition.go` and `ContentServer::Routes` | Agent dispatch, object storage, and the sandbox issuer must exist before composition enables content grants and routes. |
| Temporary staging has one lifecycle owner. | `runtimecomposition/runtime.go` | Construction failure removes its directory. Runtime closure removes it after listener drain. |

The [preparation record](code-python-demand-preparation-20261001.md) describes native package resolution.
The [admission record](code-python-bundle-admission-20261001.md) describes offline job identity.
The [storage record](code-python-shared-storage-20261001.md) describes publication and file bounds.

## Authority and compatibility

The worker requests content authority through the existing sandbox authorization RPC.
Main checks its verified workload session, signed command, active claim, resource scope, capability, and configured audience.
The request supplies exactly one 32-byte bundle root.
Content requests cannot select cancellation authority.
Stopped or stale claims cannot receive a new content grant.

Revision 3 grants authorize package content operations only.
Their lifetime does not exceed 30 seconds.
The existing Ed25519 domain and exact protobuf-byte signature remain unchanged.
Normal submission and cancellation grants keep their existing revisions and omit the new field.
Older supervisors reject revision 3 rather than treating it as code authority.
Go and Python stubs are regenerated through the version-pinned repository script.
Rust's build regenerates its protocol types.

The HTTP recipient must present a verified client certificate.
Its canonical DNS or SPIFFE identity must match the signed audience and the configured recipient list.
The HTTP service verifies the signature before decoding the claims.
Strict protobuf checks reject duplicate or unknown fields.
Single-value, canonical base64 header parsing rejects ambiguous grant headers.
The signed tenant and resource project select the platform object namespace.

The grant permits bounded upload, publication, and download for one root.
It does not authorize code execution, graph checkpoints, arbitrary object keys, or public-project sharing.
The HTTP service does not recheck the worker claim on every transfer.
Claim validation occurs at grant issuance. Existing grants retain their bounded lifetime after Stop.
Do not describe this behavior as immediate content-grant revocation.

## Transfer and resource limits

`X-Elitea-Sandbox-Bundle-Grant` contains the signed envelope.
`PUT /sandbox-bundles/{root}/files/{name}` accepts exactly two multipart parts: bundle metadata, then file content.
`POST /sandbox-bundles/{root}` verifies shared files and publishes metadata last.
`GET /sandbox-bundles/{root}` returns verified metadata.
The file GET route stages and verifies complete bytes before sending success headers.
The recipient must still verify length, hash, and recorded root before execution.

Requests require bounded content lengths. Chunked uploads are refused.
Metadata permits 128 KiB. A wheel permits 32 MiB.
Upload framing has a separate 4 KiB allowance.
The service admits four transfers without an unbounded waiting queue.
Main composes four storage operations and retains its existing listener deadlines.
Files use private temporary storage and are removed after each operation.
Runtime closure removes only its owned staging directory.

Corrupt content returns a package-content error before binary response headers.
Invalid authority returns 403. Incomplete bundles return 404.
Transfer overload returns 503 with same-identity retry guidance.
No package bytes enter the sandbox control request or Redis.
Object-store credentials, certificate keys, grants, and source code do not enter logs.

## Verification

Three HTTP tests exercise real local TLS listeners and their transfer and refusal cases.
They verify recipient binding, signature rejection, expiry, future grants, incorrect roots, and incorrect grant purposes.
They verify project separation, metadata-last publication, corrupt downloads, malformed multipart input, absent routes, and bounded admission.
The issuer cases verify content-root binding, disabled storage, invalid roots, cancellation separation, and stopped claims.
A runtime test verifies owned-directory cleanup and repeated closure.
Focused race checks and `go vet` pass for the affected Go packages.

The complete four affected Go packages report 791 passing test and subtest results and 15 prerequisite skips.
The skips require PostgreSQL, configuration secrets, or storage emulators.
A separate native S3 case runs with the required rehearsal service and bundle fixture.
These component results do not prove real database claim issuance or application recovery.

Five Rust supervisor grant tests pass.
The content-grant test refuses execution and cancellation, including purpose changes that retain a root.
The first Rust command uses the default feature set and selects zero supervisor tests.
The corrected command enables `sandbox-supervisor` and runs all five selected tests.
The host test linker reports a large debug unwind section. The tests still pass.
This host warning is not a release-container result.
Strict Clippy passes with the supervisor feature and all targets.
Its first run finds an existing allocation lint in the legacy receipt test's hexadecimal formatting.
The test now appends to one string. The production fingerprint and golden receipt do not change.

The native S3 case uses a disposable non-root Linux container.
It transfers all four native bundle files through verified TLS into rehearsal RustFS.
It then closes the first listener, removes its staging directory, and creates a replacement listener and store.
All metadata and file bytes match after download. Another project cannot read the stored root.
The case removes only its own test objects and completes in 0.19 seconds locally.
This small result is not a capacity benchmark or supervisor crash-recovery proof.

The downloaded bundle root is `71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625`.
Four offline Deno checks pass against that content in a separate restricted container.
Automatic imports and explicit version ranges return this exact result:

```json
{"records":20000,"total":1001850000,"formatted":"1,001,850,000","size":"20.0 kB","status":"PASS"}
```

Wrong-root and revision-downgrade checks refuse execution before user code.
The four checks complete in approximately two seconds locally.
The image is `sha256:014f9284c69f9ca5283918a9cce0ce46b8c15f49aae64318b9704564a08dbb95`.
Execution uses two CPUs, 512 MiB memory, disabled network access, and a read-only package mount.
The writable workspace has a 256 MiB limit.

Protocol lint, build, and compatibility checks pass.
Configuration-validation fixture and Python contract checks also pass with the existing tools and shared Python environment.
The changed sandbox protocol passes formatting.
The full protocol-format command also finds an existing extra blank line in `errors.proto`.
That unrelated file is not changed here.

After these checks, user-authorized Cargo cleanup removes the worker and ADK sandbox build directories.
No local Rust build process or running container uses those directories.
Available disk space rises from approximately 67 GiB to 257 GiB.
The checkout occupies approximately 1.34 GiB after cleanup.
Source, pending changes, shared Git history, and deployed images remain intact.
The next local Rust build requires fresh compilation.
Local builds can disable incremental artifacts and use line-table debug information to reduce generated storage.
The release profile retains its existing line-table diagnostics.

## Integration remains open

Connect bounded native preparation to the owning supervisor.
Persist its preparation receipt and resolved root before code submission.
Acquire fresh, exact-root content grants during publication and recovery.
Deliver verified content to both Docker and Kubernetes execution backends.
Never repeat registry resolution for an already admitted job.

Verify preparation cancellation, interrupted publication, replacement, incompatible packages, and unavailable registries through the deployment.
Complete persistent main-chat and ephemeral-editor acceptance after integration.
JavaScript/TypeScript on-demand acquisition, user-selected Cargo dependencies, and compilation caching remain separate open contracts.
Platform-client access, workspace authority, debug artifacts, and source-variable admission remain in the Code functional audit.
Gate 5 is not closed by this transport change.
