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
   heavy tier. Copy a neighbour: a heavy job needs `[changes, fast-gate]` and the draft condition.
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

## Security scanners

| Scanner | Where | When |
|---|---|---|
| `govulncheck` | `dependency-scanning.yml`, step in the folded job. Covers every `go.work` module and `services/elitea-llm-gateway` with `GOWORK=off` | A pull request that changes `go.mod`, `go.sum` or `go.work`; every night |
| `cargo-deny` | The same job, `deny.toml`, every tracked `Cargo.lock` | A pull request that changes a `Cargo.toml` or `Cargo.lock`; every night |
| CodeQL | `codeql.yml` (Go, JavaScript/TypeScript, Actions) | Every night. It opens or updates one issue per language |
| Secret scanning and push protection | GitHub repository settings | Always on |

An advisory that cannot be fixed now goes in `deny.toml` (`ignore`) or `scripts/ci/govulncheck-allowlist.json`
with a reason and an expiry date. An entry with no date, or a date in the past, fails the job.
