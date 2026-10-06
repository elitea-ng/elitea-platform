# Rust compiled cache cohort selection

## Observed behavior

The four-language editor fixture reaches Rust after Python, JavaScript, and TypeScript complete.
Execution `8d5e80c7f98fbaf032a63ba8700aa83a` then fails before Rust sandbox admission.
Its original `rust_verify` visit exists, but no Rust preparation or execution dispatch exists.
The Rust source uses the built-in dependencies and has no additional Cargo declaration.
The recorded error is generic and does not retain the underlying control response.

The deployment selects a compiled profile for one exact native Cargo dependency bundle.
Its catalogue contains no dependency-free profile for the current execution image.
The startup parser validates that bundle, then discards its identity from the selected profile.
The runtime therefore requests a compiled grant for dependency-free Rust under a different dependency profile.
Main independently requires the exact dependency bundle and rejects that profile mismatch.
Persisted request metadata and the deployed catalogue establish this incompatibility.
They do not recover the discarded RPC response from the failed execution.

## Source mapping

| Behavior or owner | Correction | Required boundary |
| --- | --- | --- |
| SDK `runtime/langchain/remote_sandbox.py::RemoteSandbox.execute` | Preserve the existing ordinary Code execution path. | The SDK supplies behavior, not compiled-cache authority. |
| `src/sandbox/compiled_profile_config.rs` | Retain the operator-selected dependency bundle in `SnapshotProfile`. | Keep canonical manifest, image, policy, platform, and pin validation. |
| `src/sandbox/compiled_snapshot.rs` | Compare the prepared job with the exact dependency cohort. | Keep request bytes and fingerprints unchanged. |
| `src/agents/graph/code_compiled.rs` | Select ordinary execution for a fresh unmatched cohort before requesting a compiled grant. | Refuse this change after recorded compilation or execution work. |
| Main `internal/domain/runtime/compiled_snapshot.go` | Preserve the existing exact profile validator. | Do not weaken grants or reinterpret authorization failure as a cache miss. |

This correction changes no protocol, migration, dependency, sandbox policy, or release manifest.
An ordinary recorded dispatch keeps its original path.
A compiled record keeps its original dependency cohort, descriptor, supervisor, and recovery requirements.
A missing profile after compilation begins remains an error.
An upstream authorization error remains an error.

## Separate browser proof

Saved pipeline 145, version 158, supplies the configured CSV and futures dependency bundle.
Persistent chat 822 completes execution `5ba4a11a7823a936e96d1b0520446c54` at generation 1.
Its result contains three accepted records, total 30, and groups alpha 19 and beta 11.
The recorded execution lasts about 84 seconds.

The Web image then updates to source commit `ca8fee8f4bf8751861858329a86c95f447679894`.
Main, Worker, Supervisor, and both database schemas remain unchanged.
Fresh execution `e2d97c561cb5f0a954f528ea3e737645` completes in about six seconds with the same typed result.
Both results remain visible after browser reload.
These local times describe this fixture and its cache state.
The passing dependency-enabled fixture does not prove the dependency-free correction.

## Verification boundary

Focused source tests cover exact, empty, and different dependency cohorts.
They also cover unavailable profiles and recorded compilation or execution fences.
The 33 focused tests pass with no failures or skipped cases.
Strict all-feature, all-target Clippy passes with warnings denied.
The final edited test passes again after the test-only lint correction.
These checks use the locked dependencies and one compiler job.
Worker source `dfd0d6d6f` builds and deploys in rehearsal without changing Main, Supervisor, Web, or either database schema.
Its image is `sha256:eba818c6dd1afd1e397aa9d63250b9b05eee648abcb6bfa57c300bb251e795b1`.
The retained August backlog keeps its exact row fingerprints, expired commands, and zero live leases.

The unchanged four-language fixture completes execution `5285977bc0d74431d45691c9e4584e5e` in about 16 seconds.
Its UI result shows `PASS`, five records, sorted IDs `e,b,d,a,c`, and total 31.
All seven sandbox dispatches resolve without a compiled descriptor.
The preceding failed run has only six resolved dispatches and no Rust dispatch.

The Cargo regression completes execution `1ce8695086c238b83a6d528a8116c521` in about five seconds.
Its two dispatches resolve, including one compiled execution descriptor.
The expected three accepted records and total 30 remain visible after persistent chat reload.
These timings are local fixture measurements, not production throughput results.

The user's repeated error reference is `06087c4f-48f9-55b0-8288-01bb83809875`.
Its execution `4f7e917641cce6ada7bcbe5d86266f4e` fails at 12:25:56 UTC on the prior Worker.
The corrected Worker starts at 12:28:51 UTC, before both passing executions.

The previous complete CI matrix has 69 successful checks and three configured skips at `ca8fee8f4`.
That matrix precedes this correction and does not verify it.
Replacement CI runs on the corrected source. Its final result remains a separate delivery check.
The Main migration transition and current-cohort restart acceptance remain separate open gates.
