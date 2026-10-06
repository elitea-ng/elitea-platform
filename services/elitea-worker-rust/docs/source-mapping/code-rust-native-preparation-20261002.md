# Native Cargo dependency preparation slice

This slice adds a separate native Cargo acquisition component.
It retains the exact resolved dependency snapshot for later offline use.
It does not enable dynamic Cargo execution in the deployed Code runtime.

## Source mapping

| Existing source or behavior | New source | Purpose |
| --- | --- | --- |
| Image-owned Rust manifest and wrapper under `services/elitea-code-runner/adapters/rust/` | [rust_prepare.rs](../../../elitea-code-runner/src/rust_prepare.rs) | Retain the image wrapper and a native resolved Cargo root. |
| Native Cargo version, feature, and alias semantics | `parse_dependencies`, `compose_manifest`, and `cargo_command` in the helper | Validate source restrictions before native resolution. |
| Existing static image package installation in [Containerfile](../../../elitea-code-runner/Containerfile) | Separate `elitea-code-rust-prepare` binary in `rust-runtime` | Keep acquisition separate from compilation and user execution. |
| Existing non-root runner policy in [AGENTS.md](../../../elitea-code-runner/AGENTS.md) | [rust_prepare_archive.rs](../../../elitea-code-runner/src/rust_prepare_archive.rs) | Bound files, archive bytes, paths, hashes, and deadlines. |
| Existing static [rust_execute.rs](../../../elitea-code-runner/src/rust_execute.rs) | Unchanged | Dynamic prepared content import remains future work. |
| Existing supervisor preparation and content authority | Unchanged by this slice | Platform publication and tenant authorization remain separate contracts. |

The upstream ADK assessment remains [code-node-isolation-assessment-20260928.md](code-node-isolation-assessment-20260928.md).
This slice does not change ADK dependencies or its host executor.

## Component contract

Use `/workspace/.elitea-rust-prepare.json` for the strict revision 1 JSON request.
Use `/workspace/rust-dependencies.toml` for the declaration.
Use `/workspace/rust-prepared` for the initially absent output directory.
The request supplies only `revision` and `timeout_seconds`.
The timeout must range from 1 through 600 seconds.
The helper accepts no argument for preparation.
The `--verify` argument checks retained content without Cargo or network use.

Accept only a root `[dependencies]` table with registry version strings.
Permit dependency tables with `version`, `package`, `features`, and `default-features`.
Reject local paths, Git sources, custom registries, workspace options, optional dependencies, and other manifest sections.
Reserve `serde_json` and aliases that target the wrapper package.
Cargo retains version resolution and feature selection ownership.

The helper runs `cargo generate-lockfile` and `cargo vendor --locked --versioned-dirs vendor`.
It does not compile source or execute dependency build scripts and procedural macros during acquisition.
It creates fresh private Cargo state and clears inherited process variables.
Keep ancestor directories and the trusted image free from untrusted Cargo configuration.
Enforce registry egress, TLS, CPU, memory, PID, disk, and cancellation policy outside the helper.

Retain `Cargo.toml`, `Cargo.lock`, `.cargo/config.toml`, `src/main.rs`, `src/user.rs`, and the complete native `vendor/` tree.
The configuration selects a relative `vendor` directory.
The archive uses sorted USTAR regular files and fixed gzip metadata.
The record contains SHA-256 values, file modes, file lengths, and archive lengths.
The safe stdout result contains status and digests.
The result is a component receipt. It is not platform authority.

A declaration can use version ranges.
Registry changes can alter a later resolution of the same declaration.
Reuse the first authorized manifest, lockfile, and vendor snapshot without resolution.
Bind its original record and archive digests to authenticated platform identity before reuse.

## Resource and lifecycle boundaries

The request limit is 4 KiB. The declaration limit is 64 KiB.
The helper admits at most 128 direct dependencies and 128 features per dependency.
Cargo output capture has a combined 128 KiB limit per command.
The lockfile limit is 4 MiB. The record input limit is 8 MiB.
The archive retains at most 16,384 regular files, each at most 32 MiB.
The raw archive limit is 256 MiB. The compressed archive limit is 128 MiB.
One deadline covers resolution, vendoring, hashing, archive creation, and verification.
Cargo temporary disk use remains subject to the outer container limit.

The helper kills its owned child on failure or cancellation.
The container runtime must confirm descendant termination.
The helper exits after writing its receipt.
A future holding wrapper must retain content until authenticated publication and confirmed cleanup finish.
A stopped container receipt alone does not provide durable package content.

## Build and cache policy

The runner dependencies remain exact and locked.
The slice adds parser, archive, hash, and temporary-directory dependencies without changing existing resolved versions.
The builder and runtime retain the existing pinned Rust 1.97.1 image.
The helper uses two Cargo jobs and disables incremental compilation.
The generated dev and test profiles use line-table debug data and disable incremental compilation.
The runner dev and test profiles use the same bounded build policy.

BuildKit caches runner image compilation. These caches do not cache Code jobs.
The helper retains sources and native checksums. It does not retain compiled artifacts.
Dynamic jobs still need separate offline compilation under execution limits.
No performance improvement or throughput claim follows from this source audit.

## Verification status

This extraction performs static source review and patch checks only.
No Cargo build, test, network probe, container probe, credential access, deployment, commit, or push runs.
The previous WIP document reports older focused and manual Linux probe results.
Those results are not reverified for this slice or its current base.
They do not prove integrated worker delivery or Kubernetes parity.

Run helper unit tests and Clippy before delivery.
Run the ignored native Cargo test in an explicitly authorized acquisition environment.
Build the `rust-runtime` target from a clean runner context for each supported architecture.
Verify non-root acquisition with fresh Cargo state and bounded registry egress.
Verify archive integrity before offline extraction and after execution.
Verify descendant cleanup on timeout, output overflow, cancellation, and worker loss.
Keep dynamic Rust Code execution disabled until the platform integration gates pass.

## Implementation history

- 2026-10-01: The preserved WIP adds the native helper, archive verifier, and focused tests.
- 2026-10-02: This slice extracts those sources against the phase deadline base.
- 2026-10-02: The slice excludes Python packaging and unsupported verification-stage helper installation.
- 2026-10-02: The source audit records remaining publication, execution, cache, and backend gates.
