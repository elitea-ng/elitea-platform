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
pool, `eng-runner`. It is an ARC runner scale set on GKE whose pods have 4 vCPU, 16 GiB and 40 GB of work
disk.

### What the pool can run

A job runs directly in the plain `actions-runner` pod (Ubuntu 24.04):

- **No Docker daemon.** No `container:`, no `services:`, no docker, podman, compose or kind.
- **No root.** No sudo and no apt.
- **No C compiler.** Rust cannot link, `go test -race` cannot build, and cgo is off.
- **No node, Go, gh, yq or Helm.** The image has bash, git, curl, jq and python3 without pip or venv.
  Toolchains come from `setup-*` actions or from a download into the job's own directories.

So only toolchain-only jobs are routed to it:

| Workflow | Jobs |
|---|---|
| `ci.yml` | `changes`, `fast-gate`, `ci-gate` |
| `ci-web.yml` | `quality`, `unit-tests` (every shard), `gates-static`, `build` |
| `ci-web-coverage.yml` | `unit-tests` (every shard), `coverage-merge` |
| `ci-go.yml` | `sqlc`, `lint`, `test-plan`, `test` (the aggregator) |
| `ci-gateway.yml` | `lint` |
| `ci-governed-conformance.yml` | `unit` |
| `ci-docs.yml`, `ci-mobile.yml` | `docs`, `check` |
| `ci-web-e2e.yml` | `model-path-changes` |
| `image-matrix.yml` | `gate-selftest`, `issue-script` |
| `no-binaries.yml` | `history-gates` |

Everything else stays hosted. That covers the jobs that need Docker, services or kind, every Rust job, the Go
test shards and the race build, and the Python jobs (`setup-python` cannot install into the hosted tool cache
path without root). It also covers jobs that rely on tools the hosted image ships (gh, shellcheck, Helm, yq,
pip), macOS, Windows, and every job outside the pull-request pipeline.

The routed jobs set `GOMAXPROCS=4` and `--maxWorkers=4`. The pod's cgroup limit is 4 CPUs, but the node reports
8. A step a routed job runs only for node (`actions/setup-node`) has `if: runner.environment != 'github-hosted'`,
so the hosted path is unchanged.

### How a job is routed

A routed job's `runs-on` is exactly `ROUTED_RUNS_ON` from `scripts/ci/check-runner-routing.mjs`. That
expression sends the job to `eng-runner` only when **all** of these hold. Otherwise the job goes to
`ubuntu-latest`.

1. The repository variable `ENG_RUNNER` is `on`. It is unset by default.
2. Neither `github.actor` nor `github.triggering_actor` is `dependabot[bot]`.
3. The event is `schedule`, `workflow_dispatch` or `merge_group`, or it is a `pull_request` that meets both of
   these:
   - its head repository is this repository;
   - its author is not Dependabot.

It is an allowlist of events. A condition written as "anything but `pull_request`" would let
`pull_request_target`, `workflow_run` and `issue_comment` through, and none of those may reach the pool.

`scripts/ci/check-runner-routing.mjs` (run in `no-binaries.yml`) fails the pull request when:

- a `runs-on` can resolve to `eng-runner` without that exact expression, whether as a literal, a list, a group,
  a matrix value or any other expression;
- a routed job has `container:` or `services:`;
- a routed job has a step, or a step inside a local composite action it uses, that runs docker, podman, kind,
  sudo, apt or systemctl, or uses a `docker/*` or kind action, unless that step's `if:` starts with
  `runner.environment == 'github-hosted'`;
- a routed job is in a workflow, or is called from a workflow, that can start on an event outside
  `pull_request`, `merge_group`, `schedule` and `workflow_dispatch`.

Its test evaluates the expression the way GitHub evaluates it. It covers a fork, a same-repository PR,
Dependabot, `merge_group`, `push`, `schedule`, `workflow_dispatch`, `pull_request_target`, `workflow_run`, and
the variable unset or off. `check-no-push-triggers.mjs` refuses `pull_request_target` in every workflow.

**Turn it on or off.** Go to Settings → Secrets and variables → Actions → Variables and set `ENG_RUNNER` to `on`.
Set it to `off`, or delete it, to send every job back to `ubuntu-latest` on its next run. Neither change needs
a pull request.

**Caches.** `actions/cache` works on the pool through the GitHub cache service. The writers are unchanged, one
per key: the scheduled runs on main write, and pull requests only read. The image has no `zstd`, so a routed
job may miss entries that a hosted writer saved with zstd. It then runs cold until a routed writer saves its
own. The Go jobs build with cgo off, so the hosted Go build cache only partly applies to them.

### Security model

The repository is public. A fork's pull request runs the fork's copy of the workflow files, so it can write
`runs-on: eng-runner` itself. The routing above decides where **our** jobs go and keeps fork and Dependabot
jobs on hosted runners, but it is not a security control. The guards are:

1. **The runner group gives access to selected repositories only.** Organisation settings → Actions → Runner
   groups.
2. **Fork pull-request runs need approval for every external contributor.**
3. **The job-started hook (recommended; not deployed yet).** `deploy/ci-runners/job-started.sh` runs on the
   runner before any step. It refuses every job that is not from an allowed repository, every event outside the
   allowlist, a pull request whose head is a fork, and Dependabot. The runbook is
   `deploy/ci-runners/README.md`.

Until the hook is deployed, one risk remains: a maintainer approves a fork pull request whose own YAML targets
`eng-runner`. **Never approve workflow runs on a fork pull request until the hook is deployed.** This includes
the close-time run of `cancel-merged-pr-runs.yml`, which a fork's close also queues for approval. Its hourly
sweep covers that pull request, so the run needs no approval.

`cancel-merged-pr-runs.yml` uses `pull_request: [closed]` and an hourly schedule, not `pull_request_target`.
GitHub blocks `pull_request_target` by default from 2026-12-02. A fork or Dependabot close gets a read-only
token, so the close-time pass logs a notice and does nothing. The hourly sweep then cancels the unfinished
runs of every pull request merged in the last 6 hours and deletes closed pull requests' caches.

## Security scanners

| Scanner | Where | When |
|---|---|---|
| `govulncheck` | `dependency-scanning.yml`, step in the folded job. Covers every `go.work` module and `services/elitea-llm-gateway` with `GOWORK=off` | A pull request that changes `go.mod`, `go.sum` or `go.work`; every night |
| `cargo-deny` | The same job, `deny.toml`, every tracked `Cargo.lock` | A pull request that changes a `Cargo.toml` or `Cargo.lock`; every night |
| CodeQL | `codeql.yml` (Go, JavaScript/TypeScript, Actions) | Every night. It opens or updates one issue per language |
| Secret scanning and push protection | GitHub repository settings | Always on |

An advisory that cannot be fixed now goes in `deny.toml` (`ignore`) or `scripts/ci/govulncheck-allowlist.json`
with a reason and an expiry date. An entry with no date, or a date in the past, fails the job.
