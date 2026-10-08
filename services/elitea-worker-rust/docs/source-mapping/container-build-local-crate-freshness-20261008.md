# Container build: local crate freshness with a shared target cache

Build-time only. No runtime behaviour, contract or schema changes.

## Business behaviour

None taken from the current platform. A freshly tagged image must contain the
code of the checkout it was built from. On 2026-10-08 a standalone stack image
built from one worktree ran another branch's worker code.

## Defects and changes

Rust images keep their target directory in a BuildKit cache mount. The mount id
defaults to its target path, so every build on a host shares it: every
worktree, both worker stages and the DeepWiki engine (`/cargo-target`), and the
code runner (`/target`). Cargo treats a path crate as fresh when its source
mtimes predate its last compile. `COPY` keeps the build context's mtimes, so a
context last edited before another checkout's build looked up to date, and
cargo reused that checkout's binary.

Each cargo build step now:

1. holds the target mount with `sharing=locked`, so concurrent builds cannot
   compile or overwrite the binary between this step's touch and its copy;
2. runs `find /src -type f -exec touch {} +` before `cargo build`, so every
   local crate and build-script input is newer than any compile in the mount.

Registry and git crates are not touched and stay cached. Mount ids and targets
are unchanged, so the CI `buildkit-cache-dance` map still matches.

A per-file content-hash stamp was considered and rejected. Builds that compile
different unit sets (worker, supervisor, engine) share the mount, so a stamp
can say "unchanged" for a unit last compiled from other content.

| Path | Change |
| --- | --- |
| `services/elitea-worker-rust/Containerfile` | `builder` and `supervisor-builder` steps |
| `services/elitea-deepwiki-engine/Containerfile` | `builder` step |
| `services/elitea-code-runner/Containerfile` | `builder` step |
| `scripts/contract/test_cargo_target_cache_freshness.py` | gate test |

## Tests

`scripts/contract/test_cargo_target_cache_freshness.py`: 3 tests, 0 skips. CI
`ci-python` collects every `scripts/contract/test_*.py`, so no workflow changes.

- Every Containerfile/Dockerfile in the repository that builds with
  `CARGO_TARGET_DIR` on a cache mount must use `sharing=locked` and touch
  `/src` before the build.
- The three named Containerfiles must contain the expected number of such
  builds, so a rename fails the test instead of making it pass vacuously.
- The checker is shown to reject an unlocked mount, a missing touch, and a
  touch placed after the build.

Red/green: against origin/main's worker Containerfile, 2 of the 3 tests fail.
With the fix, all 3 pass. `docker buildx build --call=check` reports no
warnings for any of the three Containerfiles.

## Performance

- Mechanism: the cache mounts are kept, and only local crates are recompiled
  (Containerfile `RUN` steps above).
- Measured (code runner, isolated builder, cache already warm): the fixed build
  compiled only `elitea-code-runner` in 22 s. All 71 other packages came from
  the cache.
- Cost: a build that reruns the step now also recompiles unchanged local path
  crates. These are the worker's `vendor/adk-*` and the engine's
  `libs/rust/*`. A context with no changes still hits the BuildKit layer cache
  and runs nothing. With `sharing=locked`, builds on one host that share a
  mount run one at a time. Cargo's own target-directory lock already
  serialised their compiles.

## Durability

Not applicable. No runtime state, checkpoints or identities change.

## Resilience

The touch and the build run in one `RUN` under one lock, and a failed build
leaves nothing that a later build can treat as fresh. If touch fails, the step
fails (`&&`).

## Security

The change ensures shipped images contain the reviewed checkout (supply-chain
integrity). No secrets, no network access, no new dependencies or base images.
`rules/security.md` categories: trust boundaries, authorization, parsing,
injection, egress and secrets do not apply (the change touches no runtime code
path). Supply chain applies and is covered by the tests above. Dependency
audits are unchanged because no dependency changed.

## Recovery guarantees

No runtime (component × phase) row changes. This is build-time only.

## Real-build evidence

Four throwaway worktrees were created before any build ran: origin/main with
and without a unique marker string in `main.rs`, and this branch with and
without it. They were built in a dedicated `docker-container` builder (never
the shared host cache), in the order marked, then unmarked.

| Tree | Marker hits | Binary sha256 (prefix) | Local crates compiled |
| --- | --- | --- | --- |
| main, marked | 1 | `dfb18b5961071db2` | full build |
| main, unmarked | **1** (stale) | `dfb18b5961071db2` | 0 (`Finished` in 0.12 s) |
| fix, marked | 1 | `dfb18b5961071db2` | 1 |
| fix, unmarked | **0** | `459800e419d1d3fa` | 1 (22 s) |

Image: `services/elitea-code-runner/Containerfile`, `--target builder`, with
the release profile unmodified.

The same proof against the worker image could not finish on the verification
host. The Docker VM (16 GiB) was shared with about 137 running containers.
Swap was full and memory pressure was 70–84 %. `rustc` for `elitea-worker-rust`
was OOM-killed on every attempt, including with `CARGO_BUILD_JOBS=1` and a
reduced-memory profile used only in the fixture. The worker steps use the same
mechanism as the code runner step, and the gate test checks both.

No browser evidence: there is no runtime or UI behaviour to observe.

## Follow-ups

- Rerun the worker-image proof on a host with free memory, or in CI.
- Content-based freshness (`-Zchecksum-freshness`) would allow unchanged local
  crates to be reused once it is stable in cargo.
