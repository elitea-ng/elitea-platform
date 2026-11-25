# How CI runs a pull request

A pull request runs the workflow `CI` (`.github/workflows/ci.yml`). It runs in two tiers and ends in one
verdict job, `ci-gate`.

## The two tiers

| Tier | Starts | What runs | Signal |
|---|---|---|---|
| Fast | Every push to the pull request | Static gates, Go lint (golangci-lint, govet included) and unit shards, web unit, typecheck and lint, Rust format and Clippy, the small contract suites, the dependency scanners | About 5 minutes to the first result |
| Heavy | Only when the pull request is **not a draft** and the fast tier passed | Web E2E (every lane, visual), web coverage, the Rust test chains, desktop, Helm and kind smoke, WebKit | About 35 minutes |

The path filter decides which jobs of a tier run. It lives in `scripts/ci/tier-paths.json`. A docs-only change
runs the docs and static gates and nothing else.

### Work as a draft, then mark ready

1. Open the pull request as a **draft**: `gh pr create --draft`.
2. Push until the fast tier is green. Only the fast tier runs while the pull request is a draft.
3. Mark it ready: `gh pr ready`. The event `ready_for_review` starts `CI` again. The fast tier runs first and
   the heavy tier follows.
4. A push to a ready pull request runs the fast tier, then the heavy tier. A newer push cancels the older run.

To go back to the fast loop, run `gh pr ready --undo`.

A draft never gets the heavy tier. This is on purpose: 43% of the runner time before this change went to runs
that a later push cancelled.

## `ci-gate`

`ci-gate` is the one verdict. It runs even when other jobs fail or skip (`if: always()`):

- A job that the path filter skipped counts as a success.
- A job that failed or was cancelled fails the gate.
- A job that should have run and did not (for example, a heavy job skipped because the fast tier failed) fails
  the gate as **missing**.
- One exception: `ci-web-e2e` reported as skipped, when `ci-build-once` succeeded and its lane filter
  (`scripts/ci/e2e-lane-filter.sh`) set `reaches-stack`, `reaches-model`, `reaches-handoff` and
  `reaches-sandbox` all to `false`. A chart-only pull request reaches `ci-web-e2e` through `deploy/**` but no
  lane in it, so every lane skips and GitHub reports the calling job as skipped. That skip is the lane
  filter's verdict. If `ci-build-once` did not succeed, or any of those outputs is missing or `true`, the skip
  still counts as missing.
- On a draft, ci-gate is skipped; fast-gate green means mark it ready. A skipped gate lets nothing through: a
  draft cannot be merged, and marking it ready (`ready_for_review`, one of `CI`'s trigger types) runs the
  workflow again on the same head, where ci-gate stays pending until the heavy tier finishes and then passes or
  fails on its result. (`scripts/ci/ci-gate.mjs` still fails a draft if it is ever run for one.)

A required status check can point at `CI / ci-gate` alone. The repository ruleset does not require it today.

### A job GitHub never acquired

GitHub gives a hosted job three tries at a runner, five minutes apart. If no runner takes the job, GitHub ends
it as cancelled about 15 minutes after it was queued. The job has no runner and no steps, and it carries the
annotation "The job was not acquired by Runner of type hosted even after multiple attempts". None of our
workflows cancels a queued job: `cancel-merged-pr-runs.yml` cancels only the runs of a merged pull request,
and its 15-minute `timeout-minutes` limits its own job.

On 2026-11-23 this ended the macOS host job of `ci-desktop.yml` five times (PRs #2947, #2976, #2979, #2934 and
#2980). The cause was GitHub's macOS arm64 capacity, not our concurrency:

- Each of those jobs carried GitHub's notice "Due to capacity constraints, jobs targeting macOS arm64 runners
  may experience longer queue times".
- When each job was queued, none of our macOS jobs was running, and 3 to 17 of our hosted jobs were running,
  under the cap of 20.
- On the same day the macOS jobs that did start waited about 5 or 10 minutes, so they got a runner on the
  second or third try.

Two changes address it:

1. **Fewer macOS jobs.** A web change no longer starts `ci-desktop.yml`. Before, any change under
   `apps/elitea-web/src`, `vite.config.ts` or `libs/ts/client-core` started its four hosted jobs (the macOS
   host, two Windows jobs and one Linux job) just to build the desktop bundle. `ci-web.yml` `build` now builds
   that bundle on the pool for both brands (ELITEA and brand/sample). `ci-desktop` keeps the web paths the host
   itself reads:
   - the desktop entry `src/entries/desktop/**` (the host's tests read its i18n catalogues);
   - the desktop transport `src/shared/desktop/**`;
   - the lockfile, which pins the tsc that compiles the page scripts.

   Over 2026-11-20 to 2026-11-23, 37 of the 87 runs that started the macOS host would not start it now. Four of
   the five jobs GitHub never acquired came from those runs.
2. **One second attempt, and a verdict.** If the macOS host job fails or is cancelled, `host-runner` (on the
   pool) reads the API through `scripts/ci/runner-acquired.mjs`. The job counts as never acquired only when all
   of these hold: it had no runner, it ran no step, and it carries GitHub's annotation. In that case
   `desktop-second-attempt` runs the same steps once on `macos-latest`
   (`.github/actions/desktop-host-checks`). It restores the same cache entry and never writes it. A job that
   ran and failed, timed out, or was cancelled by a newer push gets no second attempt.

   The workflow's own result stays failed, because the first job stays cancelled. So `ci-desktop.yml`'s
   `verdict` job publishes the output `verdict`. The value is `pass` only when every job passed, or when the
   host was never acquired and its second attempt ran and passed. `ci-gate` accepts a failed or cancelled
   `ci-desktop` only when `verdict` is exactly `pass`, and it posts a notice when it does. The gate never goes
   green for a job that did not run.

The trade-off: in the never-acquired case the first job still shows red in the check list. That mark is
GitHub's, and `ci-gate` is the verdict. If the second attempt is not acquired either, `ci-gate` fails and says
so. Push again later. Re-running the jobs does not help, because the capacity is GitHub's.

### Make `ci-gate` required (repository owner)

1. Open **Settings > Rules > Rulesets** and edit the ruleset that targets `main` (or create one).
2. Enable **Require status checks to pass**.
3. Add the check `ci-gate` (the app is GitHub Actions). Its full name is `CI / ci-gate`.
4. Do not add the individual jobs. `ci-gate` covers them and a skipped job would block the merge.

## Rules for a workflow

`scripts/ci/check-tiering.mjs` (run in `no-binaries.yml`) fails the pull request when:

- a heavy job has no `github.event.pull_request.draft == false` in its `if`;
- `ci-gate` does not list every other job of `ci.yml` in `needs`, or does not run with `if: always()`;
- a job of `ci.yml` has no entry in `scripts/ci/tier-paths.json`;
- a workflow starts on `pull_request` and is not a job of `ci.yml` (a new workflow would run untiered);
- a called workflow builds its concurrency group from `github.workflow`. In a called workflow that name is
  `CI`, so all called workflows would cancel each other;
- `ci-governed-conformance` is not started by a change to the worker's egress, governed or MCP code
  (`libs/rust/egress-guard`, `agent-runtime`'s `egress*` and `toolkits/{mcp,governed}*`, the worker's
  `protocol/egress.rs` and `governed*` modules) or to `deploy/docker-compose.governed.yml`
  (`GOVERNED_MUST_REACH`). PR #2976 changed that code and the job was skipped, so the governed stack's test
  hosts were refused as private addresses on main (#2981).

To add a workflow to the pull request pipeline:

1. Give it a `workflow_call` trigger. Keep its `schedule` and `workflow_dispatch` triggers.
2. Add a job to `ci.yml` named like the file, with `uses: ./.github/workflows/<name>.yml`, in the fast or the
   heavy tier. Copy a neighbour: a heavy job needs `[changes, fast-gate]`, and its `if` starts with `!cancelled() && needs.fast-gate.result == 'success'` before the draft condition. Without that, the implicit `success()` is false whenever any fast job was skipped (by its path filter, or by a ready run that reused the fast tier), and every heavy job is skipped with it.
3. Add the job to the `needs` of `ci-gate` (and of `fast-gate` for a fast job).
4. Add its paths and its tier to `scripts/ci/tier-paths.json`.

A change to `ci.yml`, to `tier-paths.json` or to the gate scripts runs every job.

## What else changed with the tiers

- **Folded jobs.** `No Binaries` (9 jobs), `Dependency Scanning` (4) and `Helm lint` (4) are 2, 1 and 3 jobs of
  named steps. Every gate step runs even when an earlier one failed, so one run shows every red gate.
- **Rust.** `ci-rust-lint.yml` runs `cargo fmt` and one `cargo clippy` per Cargo workspace in the fast tier. The
  test jobs no longer run them. Those runs use `--all-features`, which turns on the agent runtime's
  `test-support` and with it switches off its panic guard, so the `libs/rust` leg also lints the agent runtime
  with the worker's exact feature list (`toolkit-sql`, `toolkit-office`, `toolkit-data-analysis`,
  `toolkit-delta-lake`) and with none (the desktop host).
- **The shared crates' tests.** `ci-rust-shared.yml` tests `libs/rust` in four parallel legs, split along the
  dependency graph, each on its own pool pod: `agent hosts` (the agent runtime with every feature, and the local
  tools, workspace runner and local index built on it), `agent runtime (worker features)` (the runtime as the
  worker links it: `toolkit-sql`, serde_json order off), `engines` (every other member) and `office core` (its
  slow suites). It was one job that ran all of it in sequence: 36 minutes on PR #2740 (4 of them cloning two
  git dependencies, 14 compiling 941 crates, 7 running tests, 10 building the agent runtime twice more). The
  runtime's second feature-set run (`test-preserve-order` with the toolkits) is gone: `--all-features` in
  `agent hosts` is that set plus `test-support`, which changes nothing in a test build of the runtime.
- **The worker's feature sets.** The worker (`services/elitea-worker-rust`) is never linted or tested with
  `--all-features`. Its Containerfile builds two binaries: the worker image with no feature flag, and the code
  sandbox supervisor with `--features sandbox-supervisor`. `--all-features` adds the two bench-only features
  and compiles the supervisor's code into every target, so it hid what only the default build sees. The first
  default-set Clippy found ten dead test helpers (unused imports, test fixtures and `test_pg` helpers that only
  supervisor-gated tests call). So `ci-rust-lint.yml` has one leg per image set (`lint (worker)` with no
  features, `lint (worker, sandbox supervisor)`), each with Clippy and rustdoc. They run in parallel, so the fast
  tier gets no longer. `ci-rust.yml`'s `test` job runs every target with `--features sandbox-supervisor`
  (PostgreSQL, NATS and Docker included), then `cargo test --lib` on the default set: the lib that every
  deployment runs, compiled as it ships, with no database. That step shares the job's target cache. It rebuilds
  the dependencies whose features differ between the sets (`tower`, `hyper-rustls`, `jiff` and what links them)
  and the worker. `ci-rust.yml` builds with `CARGO_PROFILE_DEV_DEBUG=0`, as the macOS desktop host does, so the
  cache entry that holds both sets' dependencies is smaller than the one-set entry was (a panic still names its
  file and line; only a backtrace loses its line numbers). The two benches need their bench-only
  features, so `--all-targets` skips them. `lint (worker)` lints them on their own (`cargo clippy --benches
  --features graph-shaping-bench,node-settings-bench`), which is the one place a bench feature is allowed; that
  run never counts as an image set's lint.
  `scripts/ci/check-worker-feature-sets.mjs` (run in `no-binaries.yml`) reads the image sets from the
  Containerfile's `cargo auditable build` lines. It fails when a workflow (or `scripts/runtime/test_sandbox_ledger.py`)
  runs `cargo build|check|clippy|doc|test` on the worker with `--all-features` or with a feature list that no
  image builds, and when an image set has no Clippy or no test run.
- **Go vet once.** `ci-go.yml` runs no standalone `go vet`: the `lint` job's golangci-lint runs `govet` over
  the same `go.work` modules, with the default build tags and the test files, and with golangci-lint's default
  analyzer set, which is `go tool vet help`'s Go 1.26 list less `loopclosure` (a no-op from go1.22, and every
  module is at 1.26). `go vet` also checks generated code, so `.golangci.yml` sets `exclusions.generated:
  disable` and keeps the other linters off the 92 generated files by path. `task vet` still runs `go vet`.
  The gateway (`ci-gateway.yml`, Go 1.27 and golangci-lint 2.15) and the `conformance`-tagged vet in
  `ci-governed-conformance.yml` keep their own `go vet`.
- **Go shards.** `scripts/go/test-shards.json` has a shard `infra-suites` for the slow packages. The fault
  registry test no longer compiles a regexp per code over the whole tree (21 minutes to 4 seconds).
- **Go tests once.** `no-binaries` (no path filter) runs the elitea-main gates `./internal/api/` and
  `./tests/deployedge/...`, and `ci-contract` runs `./tests/contract/`, only when `ci-go` does not run: ci-go's
  shards (api-core, infra-suites, db-storage) run the same packages with `-race`. ci.yml passes `no-binaries`
  ci-go's filter as `go_runs`; `ci-contract` carries `"unless": "ci-go"` in `tier-paths.json`, so the changes
  output itself says it does not run and the gates read its skip as correct. `check-tiering.mjs` holds both.
- **Handoff load gate.** `ci-handoff-loadgate.yml` gates the workspace publish path at a p99 under 50 ms
  (ADR-0034 decision 15), measured on the shared pool, whose pods have no guaranteed CPU. A stall of the pod
  must not read as a regression (#2940), so every measured window runs a stall canary: a goroutine that ticks
  every 1 ms and records its largest lag, plus the largest gap between two `pg_stat_activity` samples beyond
  their 2 ms sleep (the database server's side). Both count only inside the window. A gated window whose
  canary saw more than 25 ms is invalid. It is logged and measured again on a fresh fixture, up to 3 attempts.
  There is one exception: a stalled window whose p99 minus the stall still breaches the gate fails at once,
  because no stall explains it, so a regression that also stalls the process is not re-rolled until a quiet
  window passes. The first clean window decides: a breach there fails as a regression and is never measured
  again. When every attempt stalled, the case fails as `measurement invalid: runner stalled N in every attempt`,
  with each attempt's p99 and stall. That is a different failure from a regression, and it is never a pass.
  The canary of every window is in the log and the job summary (`stall canary`). A full re-measure of every
  gated case adds about nine windows of roughly 5 s each, well inside the 15-minute test timeout.
  The job starts only on changes that can reach the publish path: the workspacestore and handoff packages,
  the `workspace_*` files of the repos package (the test included), the workflow, and the shared migrations
  that name a table of the publish transaction (`workspace_segments`, `workspace_refs`, `workspace_handoffs`
  in `elitea_runtime`; `buckets`, `transfer_grants` in `elitea_storage`). Those migrations are listed one by
  one in `tier-paths.json`. `check-tiering.mjs` reads every shared migration's SQL, comments removed, and
  matches the table names in any form (qualified or not, quoted or not). It fails when a migration that names
  such a table is missing from the list, when a listed one names none or is gone, and when any pattern
  reaches every shared migration (a wildcard). The nightly run on main tests everything regardless of the filter.
- **Web coverage.** One web unit suite per run. On a draft, and in the nightly, the unit shards run without
  coverage in the fast tier (`ci-web.yml` `unit-tests`). On a ready pull request `ci-web-coverage.yml` runs every
  unit test with coverage in the heavy tier and enforces the thresholds, and the plain shards are skipped:
  ci.yml passes `ci-web` the coverage job's own filter as `coverage_runs`. `check-tiering.mjs` holds that
  expression, so no run has neither suite. The coverage shards run on eng-runner: instrumentation makes tests
  2 to 3 times slower, so under `--coverage` `vitest.config.ts` multiplies every test, hook and wait budget by 3
  (`src/test/timeouts.ts`; `VITEST_TIMEOUT_SCALE` overrides it). There are no retries.
- **WebKit.** The WebKit E2E legs run on a pull request only when `apps/elitea-web/src`, the E2E harness, the
  package set or `libs/ts/client-core` change. The nightly run on main runs them always.
- **Image reuse.** The build-once jobs tag each `ci-*` image with a hash of its inputs
  (`ci-<image>:cas-<hash>`, `scripts/ci/stack-image-hash.mjs`) and skip the build when that tag exists. A run on
  main always builds. A fork pull request builds in its own job, as before. The code sandbox supervisor is
  published the same way as `ci-elitea-sandbox-supervisor`.
- **Code runner images.** The code sandbox's runner images (`services/elitea-code-runner/Containerfile`
  targets `verification`, `default` (Deno/Pyodide) and `rust-runtime`) are built once, by the
  `code-runner-images` job in `ci-build-once.yml`, through `.github/actions/code-runner-images`. Each target is
  published as `ci-elitea-code-runner:<target>-cas-<hash>`. The hash comes from `stack-image-hash.mjs`: the
  build spec, the pinned base digests and every COPY source of the target's stages. The job builds a target
  only when that tag is missing, so only a change to `services/elitea-code-runner/` builds it. A run on main
  always builds, because it is the one writer of the three `elitea-code-runner-<target>-linux-amd64` GHA layer
  cache scopes. Pull requests only read those scopes. Two jobs consume the images, and each pulls them by
  digest. `ci-web-e2e.yml`'s `code-sandbox` takes the digests from the `code-runner-image-digests` artifact.
  `ci-rust.yml`'s `test` runs beside `ci-build-once`, not after it. It resolves the same content tags and
  waits up to 20 minutes for them (ci.yml passes `code-runner-producer: true`). A consumer builds a target
  itself only when nothing was published for it: a fork or Dependabot pull request, or a tag that did not
  appear in time. Every build, in the producer or in a consumer, reads the GHA cache first. When that attempt
  fails, it retries once without `cache-from`.
- **Build once.** `ci-build-once.yml` (heavy tier, job `ci-build-once`) builds every stack image once per
  commit. `ci-web-e2e`, `ci-governed-conformance` and `helm-install-smoke` need it and pull its images by
  digest. Each builds in its own job only when nothing was built for it: a fork or Dependabot pull request,
  or its own schedule or dispatch. The job runs when any of the three would run, or when the change touches
  Rust. Its `tier-paths.json` entry is a `union` of those jobs, with no paths of its own.
- **Release builds.** The Rust workflows build no `--release` binaries. The worker, engine, vector and
  doc-extractor images are their release builds, and `ci-build-once` builds them on every pull request that
  touches Rust. The engine and doc-extractor jobs run the Go host tests and the `--version` smoke against the
  debug binary that `cargo test` built.
- **Actions cache.** The cache has a 10 GB cap. `ci-image-cleanup.yml` runs
  `scripts/ci/prune-actions-caches.mjs` every night: it keeps one entry per key family, removes pull-request
  copies, and removes the least recently used entries above 9 GiB. It then prints what main's entries hold by
  key family, warns when that differs from the plan by more than 1 GiB (update the measured sizes), and fails
  when main's entries hold more than the 8 GiB budget. That failure is a real overrun: the next new entry makes
  GitHub evict the least recently used ones, which are the ones pull requests restore. On 2026-11-23 main held
  8.33 GiB against a plan of 7.71, and a measured plan would have failed the check below.
- **The cache plan.** `scripts/ci/check-cache-budget.mjs` derives every cache entry from the workflows, sizes it
  from `scripts/ci/cache-budget.json` (measured with `gh api repos/{owner}/{repo}/actions/caches`), and fails
  when:
  - the plan is over 8 GiB;
  - a cache key has more than one writer (rust-cache and go-cache; every other job restores with
    `save: "false"` or `save-if: false`);
  - a reader's key has no writer on a schedule;
  - a Rust cache is registry only. A registry-only entry measured 0.3 to 0.6 GiB and saves only the seconds
    cargo takes to fetch the crates;
  - a workflow's own `actions/cache` save has no size in the file (each key family is an entry), or a restore
    reads a family that no scheduled workflow saves.

  Each size is looked up as `projected` (an entry a change resized that no nightly has measured yet, with the
  reason in `projectedWhy`), then `measured`, then the estimate. When the janitor prints the new sizes, move
  them to `measured`.

  Three Rust jobs keep a target cache:
  - the worker tests (`ci-rust.yml` `test`);
  - the macOS desktop host (`ci-desktop.yml` `desktop`, shared key `desktop-host`);
  - the macOS package dry run on a pull request, which restores the host's entry.

  Both writers end with `scripts/ci/trim-cargo-cache.mjs` on a run that saves. It cuts cargo's git database
  down to the commits the lockfiles pin, and it deletes the build scripts' `.o` files, which are already
  archived into the static libraries that get linked. `libs/rust/code-parsers` pins two tree-sitter grammars by
  git rev. Cargo clones each repository's whole history to read one commit: 240 MB, 4 minutes of every cold job
  on a pool pod, and 0.23 GiB of each Rust entry. Trimmed, the cpp grammar's database went from 96 MB to 1.8 MB.
  `ci-rust-shared.yml` (`engines` leg) saves the trimmed `libs/rust` database as its own entry,
  `cargo-git-db-v1-<os>-<hash of code-parsers' manifest>`. Its other legs and the `ci-rust-lint.yml` legs that
  lock the grammars restore that entry, so cargo fetches nothing. The trim runs only on a run that saves, and an
  exact hit never saves, so the desktop host's key moved to `prefix-key: v1-rust` (the package dry run reads the
  same) to get a trimmed entry written; the worker's key changed with its debug-info setting.

  The npm cache is Linux only. On macOS the web lockfile's entry was a second copy, 0.19 GiB, so the macOS jobs
  fetch from the registry, as the Windows jobs already did.

  The workspace Go key has one writer, `ci-go.yml` `test-checks`. Its nightly run compiles every `go.work`
  module with `go test -race -run '^$' -exec true`.
- **Path parity.** A cache entry has a key and a version, and a restore matches only when both match. The
  version is a hash of the `path:` lines exactly as written. So `~/.npm` and `/home/runner/.npm` are two
  versions, and a reader with one never restores what a writer with the other saved. Until 2026-11-20 no pull
  request restored main's Go or npm cache for this reason. Every save and restore of one key uses the same
  literal `~/...` lines, in the same order. An expression path is not allowed.
  `scripts/ci/check-cache-path-parity.mjs` (in `No Binaries`) fails when a restore differs from its writer.
- **Compression parity.** The version also hashes the compression: zstd when the `zstd` binary is on `PATH`,
  gzip otherwise. On 2026-11-21 main held the web npm key twice: a zstd entry from the hosted writers, and a
  gzip entry saved by the Storybook job inside the Playwright image, which has no zstd. So a job that runs in
  a job-level `container:` never saves a cache. It restores only, with `save: "false"` or
  `actions/cache/restore`, or it runs on the host. The eng-runner image ships zstd, and `eng-runner-env`
  checks for it. The same parity check fails when a containerised job reaches a save, directly or through
  local actions.
- **One npm slot per job.** `main-write-cache` lets a job use each slot once. A nested action that installs
  `scripts/ci` dependencies, such as `code-runner-images`, calls `ci-scripts-deps` with `npm-cache: "false"`,
  so it never claims the slot its caller has already used.
- **Reading the cache report.** `main-write-cache` ends with one `cache:` line: exact hit, restored from a
  prefix, or MISS. On a writer run, the full `actions/cache` reports only `cache-hit`, which is `true` on an
  exact hit, `false` on a prefix hit and unset on a miss. It never exposes the matched key, so the report
  says "restored from a restore-keys prefix" without naming the key. Its `Cache restored from key` log line
  names it.
- **Home layout.** The Go and npm caches save on a run on main (hosted or eng-runner), and save and restore only wherever `../../..` of the workspace
  is `HOME` (hosted runners and eng-runner): the tar holds paths relative to the workspace, so a runner with
  another home layout would extract them to the wrong directory.
- **Desktop packages.** `desktop-package` (heavy tier) runs on a pull request only when `apps/elitea-desktop/**`,
  `libs/rust/vendor/agent-desktop/**`, the Rust toolchain pin, `desktop-package.yml`, the three packaging actions, the brands
  (`brand/**`, `scripts/brand/**`) or the brand output scan change. The macOS run is an unsigned debug dry run of brand/sample (the app, the
  `.pkg` and the styled `.dmg`, whose Finder layout is read back and checked on the mounted image), and it is the desktop lane of the brand output scan (the macOS host job in `ci-desktop.yml` no longer builds a second binary
  for it; `ci-local-tools-macos.yml` runs the local-index tests that job used to). The Windows run builds the ELITEA MSI and
  MSIX in full, unsigned (`tauri build` in the release profile, the sidecar, makeappx). The Linux run (#3034, on the pool)
  builds the ELITEA AppImage and `.deb` in full, smoke-tests the AppImage under `xvfb-run` (`--version`, and still up
  after 30 s), and keeps both as the `desktop-linux` artifact (7 days). All three stage the release asset names.
  `publish.yml` (the nightly pre-release on main, and every release) runs the release build, macOS signed and verified,
  Windows unsigned until a signer exists, and its `desktop-assets` job (on the pool) attaches the packages and `SHA256SUMS`
  to the release. `check-tiering.mjs` holds the path list, the dry-run inputs, `require_signed_macos: true`, a stated
  `require_signed_windows` and `upload_release_assets: true` in `publish.yml`, and Windows and Linux jobs with no `if:`.
  The macOS app is Apple silicon only.

## Runners

Most jobs run on GitHub-hosted `ubuntu-latest`. The free plan caps hosted jobs at 20 at a time for the whole
organisation, and queueing was most of a pull request's wall time. The organisation also has a self-hosted
pool, `eng-runner`. It is an ARC runner scale set on GKE whose pods have 4 vCPU, 16 GiB, an 80 GiB work
volume and 10 GiB of ephemeral storage. It runs the custom image `ghcr.io/elitea-ng/eng-runner:2.338.2-20261120.10`:
the `actions-runner` image plus build tools such as cc and zstd.

### What the pool can run

A job runs directly in the pod (Ubuntu 24.04):

- **No Docker daemon.** No `container:`, no docker, podman, compose or kind. A job's `services:` run as
  processes instead (below).
- **No root.** No sudo and no apt.
- **Build tools, no toolchains.** The image (`deploy/ci-runners/image/`) has build-essential (cc for cgo and
  the `cc` crate), cmake, pkg-config, zstd, shellcheck, gh, helm, yq and skopeo. node, Go, Python and rustup come
  from the job: `setup-*` actions, or `eng-runner-env` with `node: "true"` or `rust: "true"`. Its `python3` is
  Ubuntu's, whose pip refuses to install (PEP 668), so a job that runs `python`, `pip` or `python3 -m pip
  install` sets up Python. `setup-python` needs no root here: the image points `RUNNER_TOOL_CACHE` and
  `AGENT_TOOLSDIRECTORY` at `/home/runner/_work/_tool`, and the Python installer only copies into that
  directory. The aws CLI is not in the image; `repo-gates` installs it with pip on the pool.
  **Keep `LD_LIBRARY_PATH` in child environments.** Outside `/opt/hostedtoolcache` the setup-python
  interpreter finds `libpython3.12.so.1.0` only through the `LD_LIBRARY_PATH` setup-python exports, so a script
  or test that starts `python3` with a scrubbed env (`{PATH, GITHUB_*}`) must pass it through, as
  `eng-runner-hook.test.mjs` does. This libpython rpath issue is also why the Python jobs first stayed hosted.
- **10 GiB of disk outside `/home/runner/_work`.** That is the container's ephemeral-storage limit. Only
  `_work` is the separate 80 GiB volume, and a pod that writes past the limit is evicted. The default places
  for `~/go/pkg/mod`, `~/.cache/go-build`, `~/.npm`, `~/.cargo`, `~/.cache` and `/tmp` all count against it.

### Action archives come from the image (codeload 429)

In "Set up job" the runner downloads the tarball of every action the job uses from `codeload.github.com`.
Every pool pod leaves the cluster through the same egress address, and codeload limits by address, so a
burst of pool jobs got `429 (Too Many Requests)` three times in a row and failed before its first step
(#3003). The runner already sends the job token with that download, so the requests are authenticated and
another token would not lift the limit.

The runner (2.320 and later) first looks for a local copy under `ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE`, at
`<owner>_<repo>/<commit sha>.tar.gz` (`src/Runner.Worker/ActionManager.cs` in `actions/runner`). The pool
image sets that variable to `/opt/action-archive-cache`, which is read-only to the runner user, and bakes the
archives in:

- `scripts/ci/action-archive-list.mjs` reads every remote `uses:` in `.github/workflows` and
  `.github/actions`. With `--resolve` it resolves each ref to its commit through the GitHub API (a tag such
  as `v7` to the commit it names, as the runner does) and follows the `uses:` of remote composite actions,
  because the runner downloads those too. With `--download <dir>` it writes the archives in the runner's
  layout. A sub-path action such as `actions/cache/restore` shares the `actions_cache` archive.
- `eng-runner-image.yml` runs `--download` on a hosted runner before the build, and its smoke step checks
  the variable, the count, the layout and that no archive is writable.
- **A miss is not a failure.** An action whose commit is not in the image (a Dependabot bump, or a major tag
  that moved) is downloaded as before. The weekly image build bakes the new commits in, and a new image
  reaches the pool when the owner pins its digest in the GitOps values.

To list the set locally: `node scripts/ci/action-archive-list.mjs` (refs, offline) or
`GH_TOKEN=$(gh auth token) node scripts/ci/action-archive-list.mjs --resolve` (one `owner/repo@sha` per
archive). The runner prints "Download action repository" for a hit too; a hit has no codeload warning after
it, and with step debug logging on the log adds `Copied action archive '/opt/action-archive-cache/...'`.

These jobs are routed (when `ENG_RUNNER` is on, on our own pull requests):

| Workflow | Jobs |
|---|---|
| `ci.yml` | `changes`, `fast-gate`, `ci-gate` |
| `ci-web.yml` | `quality`, `unit-tests` (every shard), `gates-static`, `build` |
| `ci-web-coverage.yml` | `unit-tests` (every shard), `coverage-merge` |
| `ci-go.yml` | `proto`, `sqlc`, `env-drift`, `lint`, `test-plan`, `test-checks`, `test` (the aggregator), `test-shard` (every shard, storage included), `integration`, `editor-lifecycle`, `compiled-snapshots-postgres`, `secretstore-openbao` |
| `ci-gateway.yml` | `lint`, `test` |
| `ci-contract.yml` | `compile` |
| `ci-vector.yml` | `vector` |
| `ci-inventory-engine.yml` | `engine` (with the Go host against the binary) |
| `ci-handoff-loadgate.yml` | `loadgate` |
| `ci-governed-conformance.yml` | `unit` |
| `ci-docs.yml`, `ci-mobile.yml` | `docs`, `check` |
| `ci-build-once.yml` | `model-path-changes` |
| `image-matrix.yml` | `gate-selftest`, `issue-script` |
| `no-binaries.yml` | `repo-gates`, `history-gates` |
| `dependency-scanning.yml` | `scan` |
| `helm-lint.yml` | `template` (both charts) |
| `ci-rust-lint.yml` | `lint` (every workspace) |
| `ci-rust-shared.yml` | `shared-crates` (the four test legs) |
| `ci-rust.yml` | `code-runner` |
| `ci-doc-extractor.yml` | `extractor` |
| `ci-deepwiki-engine.yml` | `serde-order`, `engine` (with the Go host against the binary) |
| `ci-mock-llm.yml` | `unit` |
| `ci-deepwiki-conformance.yml` | `fixtures` |
| `ci-web.yml` | `storybook`, `desktop-journeys` (Chromium from the image's `/ms-playwright`; a hosted run installs it) |
| `ci-governed-scale.yml` | `scale` (PostgreSQL and NATS as processes) |
| `ci-web-mutation.yml`, `codeql.yml`, `docs-pages.yml` | `mutation-spotcheck`, `analyze`, `build-and-deploy` |
| `publish.yml`, `release-retention.yml`, `ci-image-cleanup.yml` | `release`, `prune`; `admin`, `prune`; `prune`, `actions-cache` |
| `desktop-package.yml` | `linux` (the AppImage and the `.deb`: the image has the Tauri build libraries and xvfb; no FUSE, so linuxdeploy runs extracted; appimagetool's `file(1)`, which the image lacks, comes from Ubuntu noble's packages pinned by SHA-256 and unpacked in the job) |
| `eng-runner-image.yml` | `runner-pin` |
| `sunset-reminder.yml` | `remind` (the ADR-0059 sunset tracking issue, daily on `main`) |
| `mobile-eas-build.yml`, `mobile-eas-submit.yml` | `build`, `submit` (EAS builds in Expo's cloud) |

Everything else stays hosted, because it needs what the pool does not have (audited 2026-11-21: 46 hosted
job definitions, down from 59; every other job is routed):

- **Docker next to the services.** `ci-rust.yml`'s "Test with PostgreSQL 18" runs the sandbox images in
  Docker (it pulls them from `ci-build-once`, or builds them with buildx when nothing was published), and
  `check-worker-ignored-tests.mjs` credits only unconditional steps, so it cannot gate those steps to hosted
  runners; "Workspace runs on Docker" needs a registry container.
- **Image builds and pushes**: the `ci-build-once.yml` image legs, `publish.yml` (`build`, `scan`,
  `publish-image`, `chart`, `rollback`), `eng-runner-image.yml` (`build`, `publish`), `mirror-buildkit.yml`,
  `nightly-image-scan.yml`, `image-matrix.yml` (`docker buildx bake --print`), the code sandbox's
  supervisor image in `ci-web-e2e.yml`, and its runner images (`code-runner-images` in `ci-build-once.yml`).
- **Docker, compose, kind or gVisor**: the E2E suites (`ci-web-e2e.yml`, `deepwiki-real-engine.yml`,
  `nightly-real-llm.yml`), `ci-governed-conformance.yml` `stack`, `helm-install-smoke`, `ci-browser-session.yml`
  and `browser-bakeoff.yml` (gVisor). helm-lint's `lint` job too: a step it runs calls `docker compose config`.
- **System packages through apt**: the Tauri host on Linux (`ci-desktop.yml` `pane-drivers`, webkit2gtk).
- **The pool's own janitor**: `cancel-merged-pr-runs.yml` (`cancel`, `delete-caches`) cancels the runs of
  merged pull requests to free the pool. Queued on the pool, it would wait behind the very runs it cancels.
- **macOS and Windows**: `ci-desktop.yml` `desktop` (and `desktop-second-attempt`, which runs only when GitHub
  never acquired `desktop`) and `windows-host`, `ci-local-tools-macos.yml`, `desktop-package.yml` (`macos`, `windows`), and
  `ci-desktop-control.yml`, whose matrix has a Windows leg. `ci-desktop.yml` `host-runner` and `verdict` run on
  the pool. A web-only change starts none of `ci-desktop.yml` (see "A job GitHub never acquired").

The routed jobs pin their parallelism to the pod's 4 CPUs, because the node reports 8. `eng-runner-env` sets
`GOMAXPROCS=4`, `CARGO_BUILD_JOBS=4` and `RUST_TEST_THREADS=4` on the pool only, and a job's own `env:` still wins
(`CARGO_BUILD_JOBS=1` in `code-runner`); the web jobs pass `--maxWorkers=4`. A step that
frees hosted-runner disk with `sudo rm` runs on hosted runners only (`if: runner.environment ==
'github-hosted'`): the pool builds on its 80 GiB work volume. Binaries a job downloads (kubeconform,
cargo-deny) go into a directory under `RUNNER_TEMP` on `PATH`, not `/usr/local/bin`.
`scripts/ci/install-golangci-lint.sh` fetches golangci-lint with curl and jq (no gh), on both kinds of runner.

Every routed job runs `./.github/actions/eng-runner-env` straight after its checkout. On a GitHub-hosted runner
it does nothing, so the hosted path is unchanged. On the pool it does these things:

- It fails at once, naming the tool, when the pod's image lacks one the routed jobs take from it (cc, c++,
  make, cmake, pkg-config, zstd, git, jq, curl, unzip, gh, helm, yq, shellcheck, mountpoint), and when
  `~/.npm`, `~/.cache` or `~/go` is not a mount.
- It pins the parallelism (above).
- It keeps `HOME` at `/home/runner`, as on a hosted runner. The scale set's pod template mounts `~/.npm`,
  `~/.cache` and `~/go` from the work volume (`_homecache/*`), so the hosted cache paths are on the 80 GiB
  disk. It points `XDG_CACHE_HOME`, `GOPATH`, `GOMODCACHE`, `GOCACHE`, `GOLANGCI_LINT_CACHE` and
  `npm_config_cache` back at those paths (the image ENV names `_work/_cache`), and sets `TMPDIR` to
  `RUNNER_TEMP`, which covers jest's cache in `os.tmpdir()`; vitest caches in `node_modules`. `CARGO_HOME`,
  `RUSTUP_HOME` and the tool cache stay under `_work`, where the image puts them.
- With `node: "true"`, it installs node, for jobs that run node scripts without a `setup-node` step of their own.
- With `rust: "true"`, it installs rustup (a pinned `rustup-init`, checked against its published SHA-256) with
  no toolchain. The job's `assert-rust-toolchain.sh` step then installs the toolchain `rust-toolchain.toml`
  pins, as it does on a hosted runner, where rustup is preinstalled.

### Services as processes

A routed job keeps its `services:` block for hosted runners, and names every image through
`poolEmptyImage(<image>)` from `scripts/ci/check-runner-routing.mjs`:

```yaml
image: ${{ !(<the routing condition>) && 'postgres:18' || '' }}
```

The condition is the routing condition, word for word, so the image is empty exactly where the job runs on
the pool, and GitHub does not start a service whose image is empty. (The inverted form,
`<condition> && '' || 'postgres:18'`, is always the image: `''` is falsy.) The job then runs
`./.github/actions/native-services` after `eng-runner-env`. On the pool that action starts the same servers
from the image's binaries (PostgreSQL 18, nats-server, Qdrant, OpenBao, and with `storage: "true"` RustFS,
Azurite and fake-gcs-server in place of `deploy/docker-compose.storage-test.yml`). They run as the runner user
under `RUNNER_TEMP`, on 127.0.0.1, at the ports and with the credentials the block maps (the storage ports and
keys are read from the compose file). The binary must be the release the block names, and PostgreSQL is
initialised with the image's locale (`en_US.UTF-8`), so text sorts the same on both legs. The action waits until
each server is healthy and fails with the log's tail when one is not. PostgreSQL runs with `fsync`,
`synchronous_commit` and `full_page_writes` off: it is a throwaway CI database. The job's last step,
`./.github/actions/native-services-logs` with `if: failure()`, uploads the servers' logs whichever step failed. On
a hosted runner both actions do nothing. The few steps that call docker directly (the storage compose file,
copying `nats-server` out of its container) run on hosted runners only; `scripts/nats/ci-secure-test-env.sh` takes
the image's `nats-server` on the pool and checks it is the chart's pin. `index_v2_preflight`'s system test creates
its database on the job's server (`ELITEA_TEST_DATABASE_URL`) instead of starting a container, on both legs.

### The E2E legs on the pool (no containers)

`ci-web-e2e-native.yml` runs the seven E2E legs of `ci-web-e2e.yml` (chromium 1/2 and 2/2, webkit 1/3 to
3/3, `platform-flags`, `platform-flags-webkit`) on the pool as `E2E native (<leg>)`, with the same matrix,
shards and weights (`e2e-journey-shape.test.mjs` holds the two equal). Its `native-build` job builds the Go
binaries and web bundles the two e2e images carry (`scripts/e2e/build-native-bin.sh`, the way their
Containerfiles build them), packs them as one artifact, and the legs unpack it after checking its sha256. They
need no image and no `ci-build-once`. Each leg runs `e2e-stack.sh up` and `seed` with `E2E_STACK=native`, so
`scripts/e2e/native-stack.mjs` runs the compose services as processes from `deploy/native/e2e-standalone.mjs`.
Every service binds the address the pod's hostAliases give its compose name. Playwright then runs on the host,
using the browsers the image bakes in. It runs 4 workers, the config's CI default: the pod has 4 CPUs, as a
hosted runner does, and the weights were measured at 4. On failure a leg uploads every service's log
(`native-stack-logs-<leg>`). `check-native-stack-parity.mjs` (no-binaries) fails when the table drifts from the
compose file: an environment key, a value (beyond the mechanical rewrites: interpolation defaults, mount paths
moved to host paths, listen addresses bound to loopback; anything else is listed in the table's `REWRITTEN_ENV`
with its reason), a `depends_on` edge, a healthcheck or a one-shot.

**Loopback only.** The pool is a shared cluster, so no native service may listen on the pod's network
address. The parity check fails a table whose address, bind-shaped env value (`*_ADDR`, `*_ADDRESS`, `*_HOST`,
`*_LISTEN`, `*_BIND`), argument or config file names `0.0.0.0`, `::`, `*` or a bare `:port`. After `up --wait`,
`native-stack.mjs` also reads `/proc/net/tcp{,6}` and fails the stack when any socket its processes hold listens
on a non-loopback address, which catches a bind a program hard-codes. Each service binds its own `127.0.0.x`
from the name map, except rustfs: it refuses a bind host that is not an interface address, so it binds
`127.0.0.1:9000` and its native clients use `http://127.0.0.1:9000` (`RUSTFS_ENDPOINT`).

Nothing is published on the host's localhost either, so the host-side readers name a service by its compose
name (the pod's hostAliases): the specs read the mock journals from `STANDALONE_MOCK_ORIGIN`,
`STANDALONE_QTEST_ORIGIN` and `STANDALONE_ADO_ORIGIN`. The browser-origin probes in `standalone-stack.sh`
bind the host's own non-loopback address as their source (`stack_browser_source`), so elitea-main sees a
non-loopback peer, as through compose's port forward, and still refuses it `/readyz`.

**Test timeout.** The native E2E legs set `E2E_TIMEOUT_SCALE=1.5` (`playwright.config.ts`, 1 to 3, default 1),
which scales the per-test timeout and nothing else. The pod is a 4-CPU CFS quota on a shared 8-thread node, and a
burst from the browser's threads throttles the whole cgroup, stack included: at 4 workers, 91-96% of periods
were throttled (run 44917909298). The Playwright step prints the cgroup's throttled periods for the run, so the
reason stays measured. Retries are the compose legs', unchanged.

These jobs run only where the routing condition sends them to the pool. Where they run, `ci-web-e2e.yml`'s
compose legs stand down on a pull request to one smoke leg, chromium 1/2. That leg runs only when the change
reaches what only images prove: a Containerfile, a compose file, the web image's nginx or entrypoint, or the
bake and image actions (`e2e-lane-filter.sh` `reaches-images`). A fork's or Dependabot's pull request, a run
with `ENG_RUNNER` off and the nightly run keep the full compose matrix on hosted runners. Each leg therefore
runs under exactly one name. The compose legs are `scripts/ci/e2e-legs.mjs` (`LEGS`), and the matrices of
`e2e` and `chat-stream-rust` are `fromJSON` of the list the run needs, which ci-build-once.yml's lane filter
job computes (`e2e-legs`, `chat-stream-rust-legs`). A stood-down leg is not in the list, so it starts no runner;
it used to start a hosted runner that skipped every step. `visual` and `docs-shots` stay compose on hosted runners, because their pixels
come from the pinned Playwright container.

### The standalone lanes on the pool

The same workflow runs five standalone-stack lanes natively as `E2E native (<lane>)`: chat-stream-rust (three
shards, the compose lane's weights `61:16:33`), deepwiki-stack, index-stream-rust, native-client-conformance
and handoff-journey. They run `deploy/scripts/standalone-stack.sh` with `E2E_STACK=native` over
`deploy/native/standalone-full.mjs`, the standalone-full compose file plus the Rust agent overlay as a native
table. That table also carries the deltas of the conformance and handoff overlays. The lanes need both build
jobs. `native-build` builds the Go binaries and web bundles, now including `elitea-scheduler`,
`elitea-grpc-health` and `grpcurl`. `native-build-rust` (`scripts/e2e/build-native-rust.sh`) builds the five
Rust services (worker, vector, doc-extractor, the DeepWiki and Inventory engines) and the LLM gateway. The
gateway is Go outside `go.work`, so it builds with `GOWORK=off` on its own `go.mod`'s Go. They are built on the
pool and never lifted from an image, because the images link a different glibc from the pool's. That job has
no Rust cache: the cache budget has no room for a release target directory, and the one Rust entry caches the
test profile. It builds cold, in two parallel legs of one pod each (`E2E native build (Rust, worker)` and
`(Rust, engines)`, `NATIVE_RUST_SERVICES`). As one job the five built one after another for 33 minutes, which
was the pull request's critical path. The time went to 3 minutes of fetching git crates, 12 of worker, 2 of
vector, 5 of doc-extractor, 3 of DeepWiki, 6 of Inventory and 1.5 of gateway. The worker leg builds the worker,
vector and the gateway. The engines leg builds Inventory first, then the doc-extractor and the DeepWiki
engine, whose dependency graphs are almost all inside Inventory's. Each leg shares one target directory across
its workspaces and uploads its own artefact (`native-e2e-rust-bin-<leg>`, checked by its own sha256). The lanes
download both. Lifting the binaries from the `ci-build-once` images stays ruled out by the glibc, and the
images also build the full release profile. It keeps the release
semantics (no debug assertions, `panic = "abort"` where set) but drops the whole-program optimisation (thin
LTO, one codegen unit, opt-level 3) through `CARGO_PROFILE_RELEASE_*`. The compose image smoke runs the
shipped profile; `NATIVE_RUST_FULL_RELEASE=1` builds it natively. Neither build job
depends on `ci-build-once`. The feature set is the image's: each Containerfile runs `cargo auditable build
--frozen --release` with no feature flag (default features), and the script runs `cargo build --locked
--release --bin <service>` with none either, so the lanes never run an `--all-features` binary (the worker's
`sandbox-supervisor` and the engines' test-only `loopback-git-http` stay off). `--bin` only skips the
DeepWiki engine's bench and parity binaries; each service is a one-package workspace, so the features resolve
the same.

What the native table does differently from compose, and why:

- The runtime PKI is minted on the host (`standalone-stack.sh certs`), which needs Python's `cryptography`,
  so each lane installs a pinned copy with `actions/setup-python`. The handoff lane adds a pinned `awscli`.
- Host names in `worker-runtime.rust.json` resolve through the pod's `127.0.0.x` name map. The gateway's
  egress allowlist gains `127.0.0.0/8`. The worker's allowlist matches host names, not addresses, so it
  needs no change.
- doc-extractor's `network_mode: none` becomes `PEER_UIDS` set to the runner's uid. It only listens on its
  Unix socket. Its read-only root, dropped capabilities and memory and pid limits have no native equivalent.
- The Python and Go mocks run from source. The migrate and init jobs are one-shots. The OpenTelemetry
  collector is a sink that accepts OTLP/HTTP and discards it.
- No Redis and no new internal HTTP: the services talk gRPC over mTLS or NATS, exactly as in compose.

Pinned parallelism per lane:

| Lane | Runner | Parallelism |
|------|--------|-------------|
| chat-stream-rust ×3, deepwiki-stack | Playwright | 1 worker (`chat-stream-e2e.sh --workers=1`) |
| index-stream-rust | Playwright | 1 worker (`index-stream-e2e.sh --workers 1`) |
| native-client-conformance | `go test` | one run against the one stack |
| handoff-journey | `cargo test` | `--test-threads=1` (`handoff-live.sh`) |

Where these lanes run, `ci-web-e2e.yml`'s compose versions stand down on a pull request. They come back as the
image smoke when `reaches-images` says the change touches what only images prove. That now includes the
compose half of the standalone stack scripts. Only leg 1of3 of chat-stream-rust runs as the smoke (its computed matrix list).
Pushes, the nightly run, forks, Dependabot and runs with `ENG_RUNNER` off keep the full compose versions.

What stays compose or hosted, and why:

- **chat-stream-real and `nightly-real-llm.yml`** stay compose. The model server (`llm-real`) is an image
  holding llama.cpp and two GGUF models, and the pool has neither. The table lists the service as
  `NATIVE_ABSENT` and refuses the `real-llm` profile. Taking them native needs a pinned llama.cpp release
  binary and the models, with their sha256s, in the pool image.
- **`deepwiki-real-engine.yml`** stays compose. Its `engine_image` input is an image by design, its overlay
  replaces llm-mock with an image-only stub, its script builds the images, and it runs weekly, so there is no
  queue to win back.
- **code-sandbox** stays compose: its supervisor runs containers, which the pool cannot.
- **`visual` and `docs-shots`** stay compose, for the rasteriser reason above.
- **`ci-governed-conformance.yml`** stays on hosted runners. Its clients call `graph.microsoft.com` and
  `login.microsoftonline.com`, which the governed mocks answer. Those names are deliberately absent from the
  pod's hostAliases, so a pool job cannot resolve them to the mocks. Also, governed-mocks binds port 443,
  which a non-root process cannot.

### How a job is routed

A routed job's `runs-on` is exactly `ROUTED_RUNS_ON` from `scripts/ci/check-runner-routing.mjs`. That
expression sends the job to `eng-runner` only when **all** of these hold. Otherwise the job goes to
`ubuntu-latest`.

1. The repository variable `ENG_RUNNER` is `on`. It is unset by default.
2. The repository is `elitea-ng/elitea-platform`, so a fork's own push or schedule stays hosted.
3. Neither `github.actor` nor `github.triggering_actor` is `dependabot[bot]`.
4. The event is one of:
   - `pull_request`, when its head repository is this repository (not a fork) and its author is not
     Dependabot;
   - `push` (no workflow starts on one today: `check-no-push-triggers.mjs`);
   - `schedule` and `workflow_dispatch`, the runs on main that write the caches. The Go and npm cache paths are
     the same strings on both kinds of runner and unpack into the same `HOME`, so an entry the pool saves is the
     one a hosted reader would have saved. One writer per key still holds.

These are the events the job-started hook accepts. Each is an `==` arm of an allowlist. A condition written as
"anything but X" would let `pull_request_target`, `workflow_run` and `issue_comment` through, and none of those
may reach the pool. `merge_group` stays hosted: its payload does not say who wrote the queued change, so a
Dependabot bump could reach the pool through it.

`scripts/ci/check-runner-routing.mjs` (run in `no-binaries.yml`) fails the pull request when:

- a `runs-on` can resolve to `eng-runner` without that exact expression, whether as a literal, a list, a group,
  a matrix value or any other expression;
- a routed job has `container:`;
- a routed job has `services:` and an image is not `poolEmptyImage(<image>)`, or it does not run
  `native-services` exactly once, unconditionally, after `eng-runner-env`, with inputs that match the block:
  the same kind of server, version, ports, user, password, database, `postgres -c` settings and NATS
  JetStream; with `storage` set exactly when the hosted compose step runs; and with
  `native-services-logs` on `failure()` after it. A service the action cannot run (a registry, say) keeps the
  job hosted;
- a routed job has a step, or a step inside a local composite action it uses, that runs docker, podman, kind,
  sudo, apt, systemctl or `playwright install` (the image bakes the browsers, read-only), or uses a
  `docker/*` or kind action, unless that step's `if:` starts with
  `runner.environment == 'github-hosted'`;
- a routed job is in a workflow, or is called from a workflow, that can start on an event outside
  `pull_request`, `schedule` and `workflow_dispatch`;
- a routed job runs `node`, `npm`, `npx`, `go`, `cargo`, `rustc`, `rustup` (or `assert-rust-toolchain.sh`),
  `python`, `pip` or `python3 -m pip install` but has no step that installs it (`actions/setup-node`,
  `actions/setup-go`, `actions/setup-python`, or `eng-runner-env` with `node: "true"` or `rust: "true"`);
- a routed job does not run `./.github/actions/eng-runner-env` unconditionally, before every action other than
  the checkout. A sparse checkout must include that action's directory.

Its test evaluates the expression the way GitHub evaluates it. It covers a fork, a same-repository PR,
Dependabot, `merge_group`, `push`, `schedule`, `workflow_dispatch`, `pull_request_target`, `workflow_run`, and
the variable unset or off. `check-no-push-triggers.mjs` refuses `pull_request_target` in every workflow.

**Turn it on or off.** Go to Settings → Secrets and variables → Actions → Variables and set `ENG_RUNNER` to `on`.
Set it to `off`, or delete it, to send every job back to `ubuntu-latest` on its next run. Neither change needs
a pull request.

**When the pool is down.** Routed jobs stay queued, and `ci-gate` waits for them. Set `ENG_RUNNER` to `off`,
then re-run the failed or cancelled jobs (or push again).

**Caches.** `actions/cache` works on the pool through the GitHub cache service. The writers are unchanged, one
per key: the scheduled runs on main write, on the pool when they are routed. Pull requests only read. An
entry is found only when its key AND its version match, and the
version is the SHA-256 of the `path:` strings exactly as written, plus the compression method
(`@actions/cache` `getCacheVersion`; the `version` field of the caches REST API shows it). Three things follow:

- The custom image has `zstd`, so the compression part of the version matches the hosted one.
- The Go and npm caches (`.github/actions/main-write-cache`) spell their paths `~/...`, the same string on both
  kinds of runner, so the version matches.
- The archive holds paths relative to `GITHUB_WORKSPACE`, and `../../..` of the pool's workspace
  (`/home/runner/_work/<repo>/<repo>`) is `/home/runner`, as on a hosted runner, so an entry unpacks into the
  mounted `~/.cache` and `~/go`. The action checks that before it restores, and logs `cache: SKIPPED` on a
  runner where it does not hold.
- A path written from `CARGO_HOME` (under `_work` on the pool) is a different string, so its version differs and
  the entry is never found. `Swatinem/rust-cache` builds its paths that way; none of the jobs routed here uses
  it.

### Security model

The repository is public. A fork's pull request runs the fork's copy of the workflow files, so it can write
`runs-on: eng-runner` itself. The routing above decides where **our** jobs go and keeps fork and Dependabot
jobs on hosted runners, but it is not a security control. The guards are:

1. **The runner group gives access to selected repositories only.** Organisation settings → Actions → Runner
   groups.
2. **Fork pull-request runs need approval for every external contributor.**
3. **The job-started hook (deployed 2026-11-20).** `deploy/ci-runners/job-started.sh` runs on the
   runner before any step. It refuses every job that is not from an allowed repository, and every event outside
   `pull_request`, `schedule`, `workflow_dispatch` and `push`. It also refuses a pull request whose head is a
   fork, and Dependabot. The owner's GitOps chart deploys it; the repository copy is the tested reference.
   The runbook is `deploy/ci-runners/README.md`.

The hook is the only layer that refuses a fork job whose own YAML targets `eng-runner`, so keep approving fork
runs with care. The close-time run of `cancel-merged-pr-runs.yml`, which a fork's close also queues for
approval, needs no approval: its hourly sweep covers that pull request.

`cancel-merged-pr-runs.yml` uses `pull_request: [closed]` and an hourly schedule, not `pull_request_target`.
GitHub blocks `pull_request_target` by default from 2026-12-02. A fork or Dependabot close gets a read-only
token, so the close-time pass logs a notice and does nothing. The hourly sweep then cancels the unfinished
runs of every pull request merged in the last 2 hours and deletes closed pull requests' caches.

## Security scanners

| Scanner | Where | When |
|---|---|---|
| `govulncheck` | `dependency-scanning.yml`, step in the folded job. Covers every `go.work` module and `services/elitea-llm-gateway` with `GOWORK=off` | A pull request that changes `go.mod`, `go.sum` or `go.work`; every night |
| `cargo-deny` | The same job, `deny.toml`, every tracked `Cargo.lock` | A pull request that changes a `Cargo.toml` or `Cargo.lock`; every night |
| CodeQL | `codeql.yml` (Go, JavaScript/TypeScript, Actions) | Every night. It opens or updates one issue per language |
| Secret scanning and push protection | GitHub repository settings | Always on |

An advisory that cannot be fixed now goes in `deny.toml` (`ignore`) or `scripts/ci/govulncheck-allowlist.json`
with a reason and an expiry date. An entry with no date, or a date in the past, fails the job.

## Services on the self-hosted pool

The `eng-runner` image ([`deploy/ci-runners/image`](../../deploy/ci-runners/image/README.md)) carries the
servers that jobs otherwise get from `services:` or compose. These are PostgreSQL 18, nats-server and the nats CLI,
Qdrant, OpenBao, RustFS and `rc`, fake-gcs-server, oidc-provider-mock and nginx. It also carries the Playwright
1.62.1 browsers with their system libraries. The pool has no container engine, so a job there runs them as
processes, as the runner user, with their data under `_work`. Their versions are the ones the workflows and the
compose files pin. A change to one of those pins needs the same change in the Containerfile.
`check-playwright-image-tag.mjs` enforces this for Playwright. A native stack reaches its services by their
compose names through the pod's per-service loopback addresses
([name map](../../deploy/ci-runners/README.md#native-stack-name-map)).
