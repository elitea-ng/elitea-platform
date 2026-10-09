# Vendored crates

## `leiden-rs/` — leiden-rs 0.8.1

Leiden community detection (ADR-0026 decision 6), shared since ADR-0027.
Nothing calls it directly: `elitea-graph-algos` (`leiden::partition`) is
its one caller, and the engines reach it through that crate — DeepWiki's
Phase 3 through `graph::clustering::LeidenPartitioner`.

**Why vendored.** It is a single-author crate whose repository is on
gitcode, not GitHub. A copy in this tree cannot change or disappear under
us, and every change to it is a reviewed diff.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/leiden-rs/leiden-rs-0.8.1.crate` |
| Archive SHA-256 | `c7bbf4703fdbd25078c1842083c8f4e5551ed118a9d00342e309fee0bea46d42` (equals the crates.io index `cksum`) |
| Upstream commit | `fad950eea78feab2c9a839c93986edc3ac8d716f` (`.cargo_vcs_info.json`), `https://gitcode.com/lileeei/leiden-rs` |
| Tree digest | `7091c5ac7ad29c0012244bfc4de7a145dd52f3f22306744ed3356035be15dec6` (79 files; command below) |
| Licence | `MIT OR Apache-2.0`; both texts are in the directory (`LICENSE-MIT`, `LICENSE-APACHE`) |
| Changes | One: the archive's `Cargo.lock` is removed. Every other file is the archive's, byte for byte. The repository's root `.gitignore` ignores `bin/`, so `src/bin/leiden-cli.rs` is added with `git add -f`; keep it, or `diff -r` and the tree digest fail. |

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/leiden-rs/leiden-rs-0.8.1.crate -o /tmp/l.crate
shasum -a 256 /tmp/l.crate                       # the archive SHA-256 above
tar xzf /tmp/l.crate -C /tmp && rm /tmp/leiden-rs-0.8.1/Cargo.lock
diff -r /tmp/leiden-rs-0.8.1 vendor/leiden-rs    # no output
(cd vendor/leiden-rs && find . -type f | LC_ALL=C sort | xargs shasum -a 256) | shasum -a 256
```

**Why no `Cargo.lock`.** The archive's lockfile pins 139 packages, most of
them dev-dependencies and optional features this build never compiles.
GitHub's dependency graph reads every `Cargo.lock` in the repository, so it
raised alerts for code that does not ship. Cargo ignores the lockfile of a
path dependency (each consuming service's `Cargo.lock` resolves the
crate's dependencies), so removing it changes nothing in the build.

**How it is built.** A path dependency with `default-features = false`:
no `cli` (clap), no `rayon` (the algorithm runs on one thread, so a seed
gives one result on every machine), no `gryf`. It pulls `rand` 0.9,
`rustc-hash` 2 and `thiserror` 2; `Cargo.lock` pins them. The crate is not
a workspace member (`libs/rust/Cargo.toml` excludes `vendor`), so its own tests, benches and dev-dependencies are not
built or resolved. `vendor/rustfmt.toml` stops `cargo fmt --all` (which
also formats path dependencies) from rewriting it.

**Review (2026-10-05).**

- `unsafe`: none. `src/lib.rs` has `#![deny(unsafe_code)]` and no file
  contains an `unsafe` block. The engine's own `unsafe_code = "forbid"`
  is unaffected: the lint table applies per crate.
- Used: `GraphDataBuilder` (CSR build, undirected; a self-loop counts twice
  in the degree, as in igraph), `Leiden::run`,
  `Leiden::run_with_initial_partition`, `QualityType::RBConfiguration`
  (`Σ_c [e_c − γ·K_c²/(4m)]`, the same objective as leidenalg's
  `RBConfigurationVertexPartition`). Read: `leiden.rs`, `algorithm.rs`
  (local moving, refinement, aggregation), `quality.rs`, `partition.rs`,
  `graph/`.
- Randomness: `StdRng::seed_from_u64(seed)`; the OS generator is used only
  when no seed is given, and the engine always gives one.
- I/O, network, environment, threads: none in the paths used. The unused
  modules (Infomap, LFR and other generators, metrics, multiplex, label
  propagation, fluid communities, WASM, CLI) compile but are never called.

Updating: replace the directory with the new archive's contents, remove
its `Cargo.lock`, update
the table and the review, and re-run the Phase 3 gate
(`parity/compare_phase3.py`).

## `adk-agent/`, `adk-runner/`, `adk-sandbox/` — ADK 2.2.0 runtime extensions

Patched copies of three published adk-rust 2.2.0 crates, moved here from
`services/elitea-worker-rust/vendor/` (ADR-0029 decision 2, stage 2) so every
consumer of `elitea-agent-runtime` builds the same patched adk: the worker and
this workspace both apply them through `[patch.crates-io]`, and a future
desktop host must do the same (a library cannot carry a patch for its
consumers). Keep the exact version (`=2.2.0`) in every manifest that patches
them; `cargo tree -i adk-agent` must show the path source, never the registry.

These packages preserve the published ADK 2.2.0 dependency graph.
History extensions change `adk-agent/src/llm_agent.rs` and `adk-runner/src/runner.rs`.
The optional sandbox supervisor extension changes `adk-sandbox/src/workspace/docker.rs` and adds
`docker_code_jobs.rs` with opt-in real-container tests in `docker_live_tests.rs`.
Each package includes the upstream Apache 2.0 license.

Upstream repository: https://github.com/zavora-ai/adk-rust

Published source commit: `74765eb04930648795b53c3db689ddd10032c31e`.

| Package | Published archive SHA-256 |
| --- | --- |
| adk-agent 2.2.0 | `e150b28775ad28d4a4d0a49267a64298052346b8faf6f93ce075d025faecdcb8` |
| adk-sandbox 2.2.0 | `eaca26dc4fce7f1dcef2a4ca6d6ca9f35ce4b6a6850b636a66de295d10a09596` |
| adk-runner 2.2.0 | `31262e6bb997df8daa5bed8cc54bdefb3d92c84014c8715b937eb51731446bb0` |

`retain_prepared_history` lets the agent retain history prepared by successful model callbacks.
Its default is false. Elitea enables it when its durable compaction callbacks own prepared history.

`with_session_event_refresh` reloads the session view after each persisted event.
Its default is false. Elitea enables it to use its durable active-history projection.
All stream fragments still reach the caller. Partial fragments do not accumulate in the refreshed session.
Storage failures stop execution instead of silently using an obsolete session view.

These extensions do not replace the ADK execution loop or implement another summarizer.
The initial session snapshot remains allocated by the native mutable session.
The retention tests do not prove a process RSS bound.

For an ADK upgrade, compare both modified files with these published sources.
Run the worker history, recovery, interruption, and stream delivery tests.
Remove these extensions when upstream supplies equivalent verified behavior.


The sandbox extension adds an explicit non-root offline Code-job policy with finite
CPU/memory limits, no extra swap, a PID limit, read-only root, restricted temporary
mounts, and no added capabilities. It bounds combined decoded output to 1 MiB,
fails on stream/input errors, terminates containers on command failure/timeout,
attempts cleanup after preparation failure, and retains session handles on removal
failure. Shell file-write destinations are arguments, not interpolated commands.
Code-job image snapshots are rejected because tmpfs contents are not captured.

This dependency is optional under `sandbox-supervisor`; the default worker does
not gain Docker access. The upstream in-memory session map is not a durable job
registry. Caller cancellation, lost create acknowledgements, supervisor restart,
persistent receipts, and Kubernetes execution still require supervisor ownership
and integration tests before production admission.


Named Code jobs carry a validated opaque identity and request fingerprint as
Docker metadata. Concurrent creation is arbitrated by Docker's unique name
constraint. A recreated client can observe an existing workload without
repopulating a session or rerunning code. Existing names require reconciliation;
fingerprint mismatch fails. Failed named preparation retains the container/name
for reconciliation after termination. Durable terminal receipts and supervisor
authorization remain required; absence of a container alone never proves that
execution did not happen.


Compiled Code profiles explicitly opt into executable workspace tmpfs through
`with_code_compilation`, which requires the existing finite resource policy.
Default Code workspaces now explicitly specify `noexec`; `/tmp` remains `noexec`
for both profiles. The supervisor binds allowed languages to the image/policy
and keeps Rust-only compilation profiles separate from interpreted profiles.
This is deployment-owned configuration, never a flag supplied by user code.

The Docker extension exposes its configured Code timeout through
`code_job_timeout`. Supervisor admission rejects prepared requests that exceed
this deployment bound before ledger reservation or runtime creation.

The Code job identity also supports an immutable persisted runtime binding.
Docker observations reject a replacement container ID, even when its name and labels match.
The supervisor stores this binding before dispatch in agentstate migration 0008.
