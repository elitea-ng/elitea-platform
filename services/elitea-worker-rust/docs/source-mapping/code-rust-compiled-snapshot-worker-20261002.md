# Rust compiled snapshot worker integration

This integrated source adds exact Rust compiled snapshot selection and production.
The constructors keep snapshot support disabled.
Deployment assembly must provide the verified image and toolchain profile.
This slice does not change the ordinary PreparedJob bytes or fingerprint.

## Source mapping

| Existing source or behavior | New source | Purpose |
| --- | --- | --- |
| SDK `runtime/langchain/remote_sandbox.py::RemoteSandbox.execute` | `src/agents/graph/code_remote.rs` | Preserve one Code result for one activation. The SDK has no Rust compiled snapshot authority. |
| Worker `request.rs::PreparedJob` | `sandbox/compiled_snapshot.rs` | Bind unchanged source, input, timeout, image, policy, and native content through the existing fingerprint. |
| Worker `graph/code_remote.rs` and `sandbox/dispatch.rs` | `graph/code_compiled.rs` and durable descriptor journal | Select before admission. Preserve the original request, selected descriptor, and supervisor across generations. |
| Existing claim-bound control and signed sandbox grants | `sandbox_authority.rs`, `sandbox_compiled_grant.rs`, and `control_grpc.rs` | Request separate Compile, Publish, Read, and Execute roles. Preserve the existing signing domain. |
| Main compiled source contract in `libs/proto/elitea/runtime/v1/compiled_code.proto` | Worker `build.rs` and `wire.rs` | Generate Rust consumers from authoritative source. Verify signatures before strict revision 4 decoding. |
| Existing supervisor RPC and mTLS service | `sandbox.proto`, `client_compiled.rs`, and `service_compiled.rs` | Carry bounded control and descriptor proofs. Carry no executable bytes in RPC. |
| Existing job reserve, lease, runtime, dispatch, receipt, and cleanup ledger | `ledger_compiled.rs` and `docker_compiled.rs` | Record immutable intent before allocation. Record exact verified export provenance and confirmed cleanup. |
| Existing private content client and staging directory | `dependency_compiled_content.rs` | Transfer one bounded executable with exact hashes and a separate signed content role. |
| Existing Docker runtime and binary content transfer | `runtime.rs`, vendor `docker_compiled_content.rs`, and narrow Docker hooks | Start the fixed compiled PID 1 role. Use only fixed import, verification, export, and release helpers. |
| Existing Kubernetes runtime and UID checks | `kubernetes/compiled_content.rs`, `client.rs`, and `runtime.rs` | Bind the same helpers to the original pod UID and request digest. |
| Frozen runner `compiled_code.rs`, `compiled_content.rs`, and `rust_execute.rs` | Five `compiled-snapshot-v1` JSON fixtures | Preserve exact JSON field order, key domains, descriptor hash, and fixed wrapper receipt. |

The SDK source supplies business behavior only.
This slice does not port its header trust or remote retry authority.
The existing ADK host executor remains outside this snapshot path.

## Selection and recovery

Check the recorded Code activation before a renewed snapshot lookup.
Return to its original runtime when the canonical descriptor is already recorded.
Use Execute-only reconciliation for a dispatched or terminal runtime.
That reconciliation does not reserve, provision, download, or dispatch a Reserved job.
A missing or Reserved job requires fresh pinned Read and Execute grants before admission.

An initial authorized Read miss can enter the deterministic cold compilation path.
Keep an ordinary recorded job on its original ordinary path.
Keep ordinary execution available when snapshot support is disabled.
Reject a missing, changed, or corrupt descriptor after selection.
Do not compile again after an unknown dispatch result.

Derive the compilation activation from the original Code activation with a separate domain.
Bind its exact content through the compiled intent digest.
Record that intent before compilation admission.
Preserve its supervisor and captured descriptor across retries and generations.
Compile and capture without calling the user entrypoint.
Stage the verified executable before releasing its original compiler runtime.
Require its exact successful compiler receipt and confirmed cleanup before Ready publication.
Execute the selected snapshot in a fresh execution runtime.
The Execute snapshot branch does not compile the user program.
It verifies the toolchain through the trusted fixed `cargo -Vv` metadata command.

## Authority and provenance

Keep the existing PreparedJob digest domain and bytes unchanged.
Use a separate compiled intent digest for Compile and Execute.
Bind tenant, project, source, input, image, policy, platform, target, and profile digests.
The profile also binds manifest, lockfile, Cargo configuration, vendor content, adapter, wrapper, toolchain, and flags.
Main resolves descriptor roots and publication provenance.
A caller cannot select another descriptor root after admission.

Use the existing Ed25519 signing domain with revision 4 claims.
Reject unknown fields, duplicate fields, invalid roles, stale grants, and mismatched workload identity.
Compile and Execute grants authorize no content route.
Read authorizes GET only.
Publish authorizes bounded staging and Ready publication only.
Use the fixed compiled grant header and mTLS content client.

The export lease epoch identifies immutable capture provenance.
A later publication claim uses the current owner and lease epoch for every mutation.
Release the captured compiler claim before publication obtains a fresh claim.
Main must accept that captured provenance when issuing the first Publish grant.
Main must require a live current claim again before executable staging.
Ready publication requires the original successful receipt and cleanup, not retained ownership.

## Bounds

Limit each canonical descriptor and control to 16 KiB.
Limit the executable to 32 MiB.
Limit each fixed helper header to 4096 bytes.
Keep transfer buffering bounded and verify exact lengths and SHA-256 values.
Reject links and files with multiple hard links in staged exports.
Keep the existing capacity admission, deadlines, cancellation, and heartbeat lease fences.

Use the existing sandbox jobs and dispatch tables.
Main migration `0011_rust_compiled_snapshots.sql` owns the new columns and snapshot index.
Default-disabled worker reads do not require the dispatch descriptor column.
Enabled compiled operations require the migration before admission.

## Verification

Six focused contract tests pass.
Five signed role and fencing tests pass.
One deterministic compilation activation test passes.
Four Tonic component transport tests pass over in-memory HTTP/2.
They cover expired lookup after a recorded receipt, Reserved admission, selected absence, and changed descriptors.
The test doubles do not prove PostgreSQL or Linux behavior.

Strict all-target and all-feature Clippy passes with locked offline dependencies.
The tests use one Cargo job and a private target directory.
The macOS test linker reports a compact unwind size warning.
That warning does not fail these tests.
No throughput or latency claim follows from these checks.

## Integrated Main and runner proof

Root integrates Main authority, storage, quota, profile, and optional database-pool wiring.
The owning pinned generator reproduces all 40 Go and Python protocol outputs.
The optional configuration keeps compiled support disabled unless operators supply verified deployment material.
See the [Main and runner acceptance record](code-rust-compiled-main-runner-20261002.md) for exact boundaries.

Ten required PostgreSQL test instances pass with zero failures or skips under the race detector.
They cover original publication provenance, quota contention, expiry, row-lock fencing, and receipt retention.
The executable retention fixture checks the owning foreign-key constraint and PostgreSQL SQLSTATE `23001`.
Runner checks pass 64 test instances; two existing network preparation tests remain ignored.
The shipping Rust runtime image builds successfully.
These checks do not prove enabled production cache behavior or restart recovery.

## Remaining assembly and proof

Supply the verified SnapshotProfile to the worker factory and Supervisor constructor.
Install Main migration 0011 and verified profile material before enabling support.
Deploy the integrated Main roles, index, content handlers, and captured-publication lease amendment.
Deploy the shipping runner image for each supported architecture.

Prove quiet compiler export and descendant reaping on Linux.
Prove Docker and Kubernetes runtime identity, helper import, fixed purpose, and fresh cached execution.
Prove real mTLS grants, publication quota, selected disappearance, and original receipt recovery.
Prove worker, Main, and Supervisor restarts at each durable boundary.
Prove the job and lease budgets during compiler capture and publication.
Prove large native bundle hydration with expiring content grants.

The cold compiler currently hydrates the native bundle under one bounded grant.
An expired grant fails that transfer and preserves the original compiler plan.
This slice does not add indexed grant renewal for that cold hydration path.
Keep support disabled until the complete assembly and required runtime checks pass.

## Implementation history

- 2026-10-02: Capture the dirty worker baseline without shared edits.
- 2026-10-02: Add exact contract consumers, role verification, and selected execution recovery.
- 2026-10-02: Add the deterministic cold producer, retained publication, and both runtime adapter hooks.
- 2026-10-02: Verify focused component behavior and strict Clippy in a private target.
- 2026-10-02: Freeze input hashes, output hashes, protocol hashes, source patch, and known proof gaps.
