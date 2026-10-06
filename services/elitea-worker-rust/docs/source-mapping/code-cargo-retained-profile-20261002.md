# Retained Cargo profile delivery

This change extends the indexed Python lifecycle through the native revision 2 contract.
The shared native implementation owns grants, transfers, storage, runtime fences, phase clocks, cancellation, and readiness proof.
The Cargo implementation owns dependency semantics, native record verification, safe import, and supplied-profile compilation.

## Current to new mapping

| Current source | New source | Behavior |
| --- | --- | --- |
| `services/elitea-code-runner/src/rust_prepare.rs` standalone helper | `rust_profile.rs`; `rust_prepare.rs` fixed commands | Keep one Cargo resolver and the original deterministic component record. |
| `rust_prepare.rs::prepare`, `verify_preparation` | `rust_native.rs::retain_in` | Acquire once. Retain the original fingerprint, objects, marker, and deadline until release. |
| `rust_prepare_archive.rs::verify_archive` | `extract_archive`, `verify_tree` | Verify regular files, bounds, parent paths, hashes, modes, and complete archive framing. |
| Python fixed wheel import | `rust_native.rs::hydrate`, `hydrate_fixed` | Import the exact Cargo manifest, lock, vendor configuration, wrapper, placeholder, and inventory. |
| Static `/opt/elitea-rust` selection | `rust_native.rs::execution_profile`; `rust_execute.rs::execute` | Revision 3 requires the supplied ready profile. Revision 1 retains the static profile. |
| Fixed Rust wrapper input and result paths | Same image-owned wrapper and parent job paths | Keep selected input, nested values, result bounds, and graph state projection unchanged. |
| Image-build Cargo cache | Fresh invocation Cargo home and target output | Keep compilation tied to this invocation. Add no compiled artifact cache. |
| Code YAML and invocation contract | Graph `code.rs`, `code_runtime.rs`, `code_preparation.rs` | Bind the saved TOML declaration into node identity, acquisition source, and execution metadata. |
| Saved-literal source admission | Graph `code_remote.rs::validate_invocation` | Keep state-supplied source disabled. Dependency metadata cannot replace source approval. |
| Shared native descriptor | Worker `cargo_dependency_bundle.rs` | Admit exactly two ordered objects and the approved Linux GNU host profile. |

## Contract and ownership

Use native canonical JSON with recursively sorted keys.
Hash native content directly with SHA-256. Exclude its digest field.
Keep the existing preparation fingerprint domain and typed request field order.
Use the matching Cargo fixture across Rust, Go, and the runner helper.

Keep record and archive object limits at 8 MiB and 128 MiB.
Keep native control metadata at 128 KiB.
Map amd64 to x86_64-unknown-linux-gnu. Map arm64 to aarch64-unknown-linux-gnu.
Require the pinned Rust 1.97.1 profile and the current image architecture.

The shared finalizer owns `native-bundle/.elitea-native-finalizing` until all child processes exit.
The Cargo helper stages extraction inside that fixed directory.
The shared finalizer removes interrupted scratch after it reaps the process group.
The published profile remains outside the scratch directory.
The final helper writes elitea-native-ready-v2.json only after verified profile import.
The supervisor proves those exact bytes before dispatch.
The helper does not execute user code during acquisition or hydration.
The supervisor confirms termination of the original workload after publication or cancellation.
Direct child exit does not prove descendant termination.

The original source objects remain authoritative.
The compiler and user code share writable invocation scratch.
File hashes detect changed retained files. They do not form a separate same-UID security boundary.
Never publish or reuse changed scratch as original content.

## Verification record

Private rustfmt parsing and patch application checks pass.
Independent fixture hashing matches the shared native root and preparation fingerprint.
Cargo builds and tests do not run during extraction.
These initial extraction checks use no live runtime, network, credentials, commits, or deployments.

Run locked helper and worker tests before merging.
Run non-root Docker and Kubernetes acceptance for preparation, import, compilation, cancellation, recovery, and descendant cleanup.
Exercise aliases, features, structs, traits, async calls, selected state, and safe native-library failure.
Prove acquisition has registry egress and execution has no registry access or credentials.

Budget record, archive, extraction, registry scratch, target output, and compiler memory before enabling Cargo.
The maximum record, archive, and extracted tree can need 392 MiB before compilation scratch.
The legacy 256 MiB workspace cannot admit that worst case.
The shared native profile requires 512 MiB to 4 GiB of workspace and separate memory admission.
Verify compiler, target, registry scratch, and finalization time against the selected profile.
Keep Cargo preparation absent until resource admission and complete runtime acceptance pass.

## Real registry runner verification, 2026-10-02

The immutable arm64 production runner passes actual registry acquisition and offline execution.
The saved declaration adds `csv = "=1.3.1"`, `futures-lite = "=2.6.0"`, and serde derive.
The locked closure contains 21 packages.
The retained archive contains 749 regular files and 13,655,040 raw bytes.
Acquisition invokes only lock generation and vendoring.
Process samples observe no compiler, build script, or user program during acquisition.

A bounded TLS CONNECT relay permits only `index.crates.io` and `static.crates.io`.
It denies direct external access and unrelated registry hosts.
It does not terminate TLS or receive platform credentials.
The trusted ancestor Cargo configuration selects the relay.

Production fixed-role helpers export, import, and hydrate the retained profile.
A distinct non-root container compiles with `cargo build --locked --offline -j 2`.
That execution container has no network.
Structs, traits, generic aggregation, async yielding, CSV parsing, and selected-state processing produce the exact expected result.
No user-entry marker exists before dispatch.

An undeclared `regex` import fails before user entry.
The baseline image without the imported profile fails for missing csv and futures-lite.
Both publish failed child receipts without acquiring packages during execution.
The runner container exit status alone does not indicate successful user execution.
Cleanup read-back confirms removal of all five owned containers and the private network.

Local acquisition through export takes 3.514 seconds.
Local hydration takes 0.273 seconds.
Local dispatch through terminal read takes 3.890 seconds.
The maximum sampled compiler RSS is 273,117,184 bytes.
The observed execution cgroup memory peak is 659,918,848 bytes, including charged tmpfs pages.
The rehearsal profile therefore needs more than the previous 512 MiB limit.
These measurements are local samples, not universal performance or capacity claims.

This proof excludes live Main grants, object storage, worker journals, cancellation, takeover, Kubernetes scheduling, and browser acceptance.
It does not implement or benchmark a compiled executable cache.
Each execution compiles its fresh invocation workspace.
