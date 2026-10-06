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
Image deployment and the original four-language UI rerun still require separate evidence.
The previous complete CI matrix has 69 successful checks and three configured skips at `ca8fee8f4`.
That matrix precedes this correction and does not verify it.
The Main migration transition and current-cohort restart acceptance remain separate open gates.
