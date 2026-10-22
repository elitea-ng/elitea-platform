#!/usr/bin/env bash
# check-rust-toolchain.sh — one Rust version everywhere: cargo, CI and images.
#
# PR #1297. The repository builds Rust in three places that nothing compared:
#
#   - cargo, which takes its compiler from the rust-toolchain.toml nearest the
#     current directory. There were eight such files, one per subtree, and none
#     at the root, so a script that ran cargo from the root
#     (scripts/runtime/test_sandbox_ledger.py) got the runner's default stable
#     compiler and rebuilt every dependency with it;
#   - CI, which printed `rustc --version` and asserted nothing;
#   - the images, which pull `rust:<version>-trixie`. One Containerfile pinned
#     the digest, five did not, so two builds of the "same" tag could use two
#     different compilers.
#
# THE ONE VALUE. The repository-root rust-toolchain.toml `channel` is the pin.
#
# WHAT IT ASSERTS
#
#   (a) Every tracked rust-toolchain.toml (root included) names that exact
#       channel. The subtree files stay: an image build copies only its
#       subtree, so the root file is not in its context.
#   (b) Every reference to the official rust image is exactly
#       `rust:<pin>-trixie@sha256:<RUST_IMAGE_DIGEST>`, in every stage. It
#       reads every image operand in every tracked Containerfile, Dockerfile
#       and bake file (FROM, with `\` continuations joined and ARG defaults
#       substituted; --from=; --mount=...,from=; docker-image://) and every
#       `image:`/`container:` value in every tracked YAML file. A reference
#       is `rust` after any registry or path, with a tag, a digest, both or
#       neither; an operand that names one of the file's own `AS <stage>`s is
#       a stage, not an image. Every file that references rust must be in
#       RUST_IMAGE_FILES (a new one is reported), with its count.
#   (c) Every non-vendored Cargo.toml states `rust-version` equal to the MSRV
#       below (directly or by inheriting the libs/rust workspace value), the
#       MSRV is <= the pin, and every vendored crate's own upstream value is
#       <= the pin. Manifests that are not built as Rust crates by any of our
#       toolchains are exempt below, each with its reason.
#   (d) Every read that comes back empty FAILS, and so does a file list that
#       shrank below its floor. "Found no pin, so nothing disagreed" is the
#       shape this repository keeps mistaking for a pass.
#   (e) The toolchain release written into code and data equals the pin. A
#       native Cargo bundle records the compiler release that built it
#       (`rust_revision`). The worker (cargo_dependency_bundle.rs) treats any
#       other well-formed value as a stale bundle (a lookup miss that prepares
#       again), and the release audit (rust_compiled_release_manifest.py, which
#       also reads `release: X.Y.Z` from the image's own `rustc -Vv`) refuses
#       it. Main (sandbox_native_bundle.go) only stores and serves bundles, so
#       it checks the release's shape and carries no literal. The code
#       runner reads its release from rustc at build time (build.rs), so it
#       carries no literal. Every such literal in the tracked tree, in any
#       quoting and on either side of a comparison, must equal the pin; each
#       validator's own COMPARISON line must carry it (a literal moved to a
#       constant or a helper fails, test data elsewhere cannot stand in); and
#       there must be at least MIN_RELEASE_LITERALS in all.
#
# HOW TO RAISE THE VERSION
#
#   1. Set `channel` in rust-toolchain.toml at the root AND in every file this
#      script lists (it prints them). One value per file.
#   2. Set `rust:<new>-trixie@sha256:<digest>` in every Containerfile it lists,
#      and RUST_IMAGE_DIGEST in check-rust-toolchain.mjs to the same digest, read from the registry:
#        docker buildx imagetools inspect rust:<new>-trixie   # "Digest:" line
#   3. Set the release literal in every file assertion (e) lists (the native
#      bundle validators, the release audit and their tests), then regenerate
#      the digest-bound native bundle fixtures it lists
#      (services/elitea-code-runner/fixtures/cargo-native-v2.json and its two
#      copies): their `digest` covers the recorded `rust_revision`.
#   4. Leave MSRV alone unless code needs a newer language or library feature;
#      it is the floor, not the pin.
#   5. Run this script. It names every place that still disagrees.
#
# It needs no network, no toolchain and no cluster:
#   bash scripts/ci/check-rust-toolchain.sh
# Point it at another tree (the negative tests do) with RUST_TOOLCHAIN_ROOT.
#
# The checks live in scripts/ci/check-rust-toolchain.mjs, run with the
# runner's own node; the `yaml` package is its only dependency
# (scripts/ci/package.json, installed with `npm ci --prefix scripts/ci`, as for
# check-worker-ignored-tests.mjs). The digest, MSRV, floors, exemptions and
# per-file lists are constants at its top.
set -euo pipefail
exec node "$(dirname "$0")/check-rust-toolchain.mjs" "$@"
