# Cold compiler native content renewal

This slice closes the single-grant cold hydration gap in the compiled snapshot worker.
Keep compiled snapshot admission disabled until the full runtime assembly passes its gates.
The compiler activation, PreparedJob bytes, intent digest, descriptor journal, and original runtime remain unchanged.

## Source mapping

| Existing source | New source | Purpose |
| --- | --- | --- |
| `client_hydration.rs::hydrate_dependencies` | `client_compiled.rs::compile_snapshot` | Renew separate Compile and Content roles for each native import index. |
| `docker_hydration.rs::hydrate_authorized` | `docker_compiled_hydration.rs::hydrate_snapshot_compile` | Import while the same exact compiler remains Reserved and inert. |
| `docker_dependency_delivery.rs::hydrate_index_owned` | Reused without behavior changes | Keep fixed native helpers, exact lengths, hashes, metadata, and language finalization. |
| Existing bounded `ContentProof` | Narrow parent visibility for the compiled sibling | Verify all earlier native objects before accepting a later index. |
| Existing compiled request and response | Optional request index 9 and response readiness 8 | Preserve ordinary compiled requests and avoid a second ordinary job. |
| Existing Compile reconciliation and capture | `reconcile_snapshot_compile` | Observe the original dispatched or terminal compiler before requesting Content authority. |
| Existing `submit_snapshot_compile` | Final readiness proof before first dispatch | Remove the whole-bundle transfer under one expiring grant. |
| Existing generation-bound compilation journal in `code_compiled.rs` | Unchanged | Retry the same canonical compiler activation after transfer expiry or transport loss. |

The ordinary indexed hydration RPC cannot target a compiled intent.
Its execution grant binds the unchanged PreparedJob digest.
The compiler ledger binds the separate compiled intent digest.
This extension uses the existing Compile and Content roles without changing their authority.

## Contract

Use optional `native_hydration_index` only for an inert Compile request.
Reject an index on Execute, receipt recovery, or a request with Read authority.
Require separate native Content authority and the exact bundle record for every index.
A signed Publish or Execute grant cannot satisfy the existing Compile verifier.

Use indices zero through the native file count.
The file-count index re-verifies all original objects and commits native readiness.
A successful response acknowledges only the requested index.
A busy or fenced owner returns Aborted without an acknowledgement.
Missing final readiness stops the client before compilation dispatch.

Reconcile the existing compiler first with Compile-only authority.
That path cannot reserve, provision, import content, or select another runtime.
A missing or Reserved compiler permits indexed admission.
A dispatched or terminal compiler uses its original receipt or capture state without fresh Content grants.

## Fencing and replay

Verify exact Compile intent, profile, image, native metadata, and current grant lifetime before admission.
Record the immutable compiled intent before provisioning the compiler runtime.
Preserve unknown allocation ownership until its lease expires.
Release a confirmed bound runtime lease after each inert transfer.
Renew current ownership after import before acknowledging it.

Verify every earlier object in the same runtime before importing a later index.
Lost acknowledgements can replay an immutable object with fresh grants.
Reject changed roots, bytes, hashes, metadata, indices, and runtime bindings.
The final dispatch validates the native readiness proof without another network transfer.
A missing proof fails safely in the same original compiler plan.

Observe cancellation and hydration deadlines around each transfer.
Keep the original runtime and request fences across retries and generations.
Do not allocate an ordinary job or restart compilation after unknown dispatch.
Do not use runtime environment values to renew or authorize a role.

## Verification boundary

Run focused client transport, signed role, input mode, prefix proof, and original activation tests.
Use in-memory Tonic HTTP/2 for transport fixtures.
Those fixtures do not prove PostgreSQL lease takeover or Linux helper execution.
Root owns actual PostgreSQL, Docker, Kubernetes, mTLS, and process restart checks.

The captured family and static integration baseline has independent strict Clippy diagnostics.
This slice does not change those unowned files or suppress their lints.
Report owned diagnostics and full assembled checks separately.

## Implementation history

- 2026-10-02: Capture the adopted compiled worker, Main contract, and trace overlay as the exact private baseline.
- 2026-10-02: Add the narrow optional indexed Compile contract and receipt-first reconciliation.
- 2026-10-02: Replace whole-bundle cold hydration with sequential imports and renewed grants.
- 2026-10-02: Add expiry, replay, cancellation, readiness, and role isolation regression checks.
