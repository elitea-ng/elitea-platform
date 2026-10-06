# Vendored crates

## `leiden-rs/` — leiden-rs 0.8.1

Leiden community detection for Phase 3 clustering (ADR-0026 decision 6).
The engine calls it only through `graph::clustering::LeidenPartitioner`,
behind the `Partitioner` trait.

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

**Verify.**

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
path dependency (the engine's `Cargo.lock` resolves the crate's
dependencies), so removing it changes nothing in the build.

**How it is built.** A path dependency with `default-features = false`:
no `cli` (clap), no `rayon` (the algorithm runs on one thread, so a seed
gives one result on every machine), no `gryf`. It pulls `rand` 0.9,
`rustc-hash` 2 and `thiserror` 2; `Cargo.lock` pins them. The crate is not
a workspace member, so its own tests, benches and dev-dependencies are not
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
