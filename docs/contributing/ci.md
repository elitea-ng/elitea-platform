# How CI runs a pull request

A pull request runs the workflow `CI` (`.github/workflows/ci.yml`). It runs in two tiers and ends in one
verdict job, `ci-gate`.

## The two tiers

| Tier | Starts | What runs | Signal |
|---|---|---|---|
| Fast | Every push to the pull request | Static gates, Go vet, lint and unit shards, web unit, typecheck and lint, Rust format and Clippy, the small contract suites, the dependency scanners | About 5 minutes to the first result |
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
- While the pull request is a draft, the gate fails with the word `draft`. The heavy tier has not run, so the
  pull request has no verdict yet.

A required status check can point at `CI / ci-gate` alone. The repository ruleset does not require it today.

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
  `CI`, so all called workflows would cancel each other.

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
  test jobs no longer run them.
- **Go shards.** `scripts/go/test-shards.json` has a shard `infra-suites` for the slow packages. The fault
  registry test no longer compiles a regexp per code over the whole tree (21 minutes to 4 seconds).
- **Web coverage.** The unit shards run without coverage in the fast tier. `ci-web-coverage.yml` runs them with
  coverage in the heavy tier and enforces the thresholds.
- **WebKit.** The WebKit E2E legs run on a pull request only when `apps/elitea-web/src`, the E2E harness, the
  package set or `libs/ts/client-core` change. The nightly run on main runs them always.
- **Image reuse.** The build-once jobs tag each `ci-*` image with a hash of its inputs
  (`ci-<image>:cas-<hash>`, `scripts/ci/stack-image-hash.mjs`) and skip the build when that tag exists. A run on
  main always builds. A fork pull request builds in its own job, as before. The code sandbox supervisor is
  published the same way as `ci-elitea-sandbox-supervisor`.
- **Actions cache.** The cache has a 10 GB cap. `ci-image-cleanup.yml` runs
  `scripts/ci/prune-actions-caches.mjs` every night: it keeps one entry per key family, removes pull-request
  copies, and removes the least recently used entries above 9 GiB. It then compares what main's entries hold
  with the plan and warns when they differ by more than 1 GiB.
- **The cache plan.** `scripts/ci/check-cache-budget.mjs` derives every cache entry from the workflows, sizes it
  from `scripts/ci/cache-budget.json` (measured with `gh api repos/{owner}/{repo}/actions/caches`), and fails
  when:
  - the plan is over 8 GiB;
  - a cache key has more than one writer (rust-cache and go-cache; every other job restores with
    `save: "false"` or `save-if: false`);
  - a reader's key has no writer on a schedule;
  - a Rust cache is registry only. A registry-only entry measured 0.3 to 0.6 GiB and saves only the seconds
    cargo takes to fetch the crates.

  Three Rust jobs keep a target cache:
  - the worker tests (`ci-rust.yml` `test`);
  - the macOS desktop host (`ci-desktop.yml` `desktop`, shared key `desktop-host`);
  - the macOS package dry run on a pull request, which restores the host's entry.

  The workspace Go key has one writer, `ci-go.yml` `test-checks`. Its nightly run compiles every `go.work`
  module with `go test -race -run '^$' -exec true`.
- **macOS package.** `desktop-package` (heavy tier) runs on a pull request only when `apps/elitea-desktop/**`,
  `libs/rust/vendor/agent-desktop/**`, the Rust toolchain pin, `desktop-package.yml` or the two packaging actions change. The run is an
  unsigned debug dry run. `publish.yml` (the nightly pre-release on main, and every release) runs the signed
  release build and verifies it. `check-tiering.mjs` holds the path list, the dry-run inputs and
  `require_signed: true` in `publish.yml`. The app is Apple silicon only.

## Runners

Most jobs run on GitHub-hosted `ubuntu-latest`. The free plan caps hosted jobs at 20 at a time for the whole
organisation, and queueing was most of a pull request's wall time. The organisation also has a self-hosted
pool, `eng-runner`. It is an ARC runner scale set on GKE whose pods have 4 vCPU, 16 GiB, an 80 GiB work
volume and 10 GiB of ephemeral storage. It runs the custom image `ghcr.io/elitea-ng/eng-runner:2.338.2-20261120`:
the `actions-runner` image plus build tools such as cc and zstd.

### What the pool can run

A job runs directly in the pod (Ubuntu 24.04):

- **No Docker daemon.** No `container:`, no `services:`, no docker, podman, compose or kind.
- **No root.** No sudo and no apt.
- **Build tools, no toolchains.** The image (`deploy/ci-runners/image/`) has build-essential (cc for cgo and
  the `cc` crate), cmake, pkg-config, zstd, shellcheck, gh, helm, yq and skopeo. node, Go, Python and rustup come
  from the job: `setup-*` actions, or `eng-runner-env` with `node: "true"` or `rust: "true"`. Its `python3` is
  Ubuntu's, whose pip refuses to install (PEP 668), so a job that runs `python`, `pip` or `python3 -m pip
  install` sets up Python.
- **10 GiB of disk outside `/home/runner/_work`.** That is the container's ephemeral-storage limit. Only
  `_work` is the separate 80 GiB volume, and a pod that writes past the limit is evicted. The default places
  for `~/go/pkg/mod`, `~/.cache/go-build`, `~/.npm`, `~/.cargo`, `~/.cache` and `/tmp` all count against it.

These jobs are routed (when `ENG_RUNNER` is on, on our own pull requests):

| Workflow | Jobs |
|---|---|
| `ci.yml` | `changes`, `fast-gate`, `ci-gate` |
| `ci-web.yml` | `quality`, `unit-tests` (every shard), `gates-static`, `build` |
| `ci-web-coverage.yml` | `coverage-merge` |
| `ci-go.yml` | `proto`, `sqlc`, `env-drift`, `lint`, `test-plan`, `test-checks`, `test` (the aggregator) |
| `ci-gateway.yml` | `lint` |
| `ci-governed-conformance.yml` | `unit` |
| `ci-docs.yml`, `ci-mobile.yml` | `docs`, `check` |
| `ci-web-e2e.yml` | `model-path-changes` |
| `image-matrix.yml` | `gate-selftest`, `issue-script` |
| `no-binaries.yml` | `repo-gates`, `history-gates` |
| `dependency-scanning.yml` | `scan` |
| `helm-lint.yml` | `template` (both charts) |
| `ci-rust-lint.yml` | `lint` (every workspace) |
| `ci-rust-shared.yml` | `shared-crates` (rustdoc and tests) |
| `ci-rust.yml` | `code-runner`, `quality` (docs and release build) |
| `ci-doc-extractor.yml` | `extractor` |
| `ci-deepwiki-engine.yml` | `serde-order` |
| `ci-mock-llm.yml` | `unit` |
| `ci-deepwiki-conformance.yml` | `fixtures` |

Everything else stays hosted:

- **The coverage unit shards** (`ci-web-coverage.yml` `unit-tests`): under coverage instrumentation the pod
  ran them 1.5 to 2 times slower, and the heaviest test files hit their per-test timeouts.
- **`services:` or `container:`.** The Go test shards, `integration`, `editor-lifecycle`,
  `compiled-snapshots-postgres` and `secretstore-openbao` in `ci-go.yml`; the gateway's `test`; the engine jobs
  of `ci-deepwiki-engine.yml` and `ci-inventory-engine.yml` (Postgres, so their "Go host against the binary"
  steps stay with them); `ci-vector.yml`; `ci-contract.yml`; and `ci-rust.yml`'s "Test with PostgreSQL 18" and
  "Workspace runs on Docker".
- **Docker, compose, kind or gVisor**: image builds and `docker buildx bake --print` (`image-matrix.yml`'s list
  check), `helm-install-smoke`, the E2E suites, `ci-browser-session.yml`. helm-lint's `lint` job too: a step it runs calls `docker compose config`.
- **System packages through apt**: the Tauri host on Linux (`ci-desktop.yml` `pane-drivers`, webkit2gtk).
- **macOS and Windows**, including `ci-desktop-control.yml`, whose matrix has a Windows leg.
- **Every event but `pull_request`**, so the scheduled runs on main, which write the caches, stay hosted.

The routed jobs pin their parallelism to the pod's 4 CPUs, because the node reports 8: `GOMAXPROCS=4`,
`--maxWorkers=4`, `CARGO_BUILD_JOBS=4` (`1` in `code-runner`, as before) and `RUST_TEST_THREADS=4`. A step that
frees hosted-runner disk with `sudo rm` runs on hosted runners only (`if: runner.environment ==
'github-hosted'`): the pool builds on its 80 GiB work volume. Binaries a job downloads (kubeconform,
cargo-deny) go into a directory under `RUNNER_TEMP` on `PATH`, not `/usr/local/bin`.
`scripts/ci/install-golangci-lint.sh` fetches golangci-lint with curl and jq (no gh), on both kinds of runner.

Every routed job runs `./.github/actions/eng-runner-env` straight after its checkout. On a GitHub-hosted runner
it does nothing, so the hosted path is unchanged. On the pool it does three things:

- It moves `HOME` to `/home/runner/_work/_home`, and sets `XDG_CACHE_HOME`, `GOPATH`, `GOMODCACHE`,
  `GOCACHE`, `GOLANGCI_LINT_CACHE`, `npm_config_cache`, `CARGO_HOME` and `RUSTUP_HOME` under it. It also sets
  `TMPDIR` to `RUNNER_TEMP`, which covers jest's cache in `os.tmpdir()`; vitest caches in `node_modules`. Because
  `HOME` itself moves, the `go-cache`, `npm-cache` and `main-write-cache` actions follow it: they resolve their
  paths from `$HOME`.
- With `node: "true"`, it installs node, for jobs that run node scripts without a `setup-node` step of their own.
- With `rust: "true"`, it installs rustup (a pinned `rustup-init`, checked against its published SHA-256) with
  no toolchain. The job's `assert-rust-toolchain.sh` step then installs the toolchain `rust-toolchain.toml`
  pins, as it does on a hosted runner, where rustup is preinstalled.

### How a job is routed

A routed job's `runs-on` is exactly `ROUTED_RUNS_ON` from `scripts/ci/check-runner-routing.mjs`. That
expression sends the job to `eng-runner` only when **all** of these hold. Otherwise the job goes to
`ubuntu-latest`.

1. The repository variable `ENG_RUNNER` is `on`. It is unset by default.
2. The event is `pull_request`.
3. The pull request's head repository is this repository, so it is not a fork.
4. Its author is not Dependabot.
5. Neither `github.actor` nor `github.triggering_actor` is `dependabot[bot]`.

It is an allowlist of one event. A condition written as "anything but `pull_request`" would let
`pull_request_target`, `workflow_run` and `issue_comment` through, and none of those may reach the pool.

These events stay hosted:

- **`schedule` and `workflow_dispatch`** (the runs on main). They write the caches. An entry saved on the pool
  would be saved under the moved `HOME` paths, and the cache version hashes the path strings, so every hosted
  reader would miss it.
- **`merge_group`.** Its payload does not say who wrote the queued change, so a Dependabot bump could reach the
  pool through it.

`scripts/ci/check-runner-routing.mjs` (run in `no-binaries.yml`) fails the pull request when:

- a `runs-on` can resolve to `eng-runner` without that exact expression, whether as a literal, a list, a group,
  a matrix value or any other expression;
- a routed job has `container:` or `services:`;
- a routed job has a step, or a step inside a local composite action it uses, that runs docker, podman, kind,
  sudo, apt or systemctl, or uses a `docker/*` or kind action, unless that step's `if:` starts with
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
per key: the scheduled runs on main write, and they stay hosted. Pull requests only read. Expect a routed job
to miss the hosted entries and run cold. An entry is found only when its key AND its version match, and the
version is the SHA-256 of the `path:` strings exactly as written, plus the compression method
(`@actions/cache` `getCacheVersion`; the `version` field of the caches REST API shows it). Three things follow:

- The custom image has `zstd`, so the compression part of the version matches the hosted one.
- A path written from `$HOME`, `CARGO_HOME` or the workspace is a different string on the pool
  (`/home/runner/_work/_home/...`), so its version differs and the entry is never found. `Swatinem/rust-cache`
  builds its paths that way; none of the jobs routed here uses it.
- A `~/...` path string is the same on both, but the archive holds paths relative to `GITHUB_WORKSPACE`, so on
  the pool it would unpack under `/home/runner` (the 10 GiB ephemeral disk), not under the moved `HOME`.

Mounting the work volume at `/home/runner` in the scale set would keep `HOME` unmoved, and then a `~/...`
string would match and unpack in the right place (`deploy/ci-runners/README.md`).

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
