# eng-runner: the self-hosted CI runner pool

`eng-runner` is the org's ARC runner scale set on GKE (namespace `saas-eng-runner`).

- **Pod size:** 4 vCPU, 16 GiB of memory, an 80 GiB `_work` volume and 10 GiB of ephemeral storage outside it.
  `minRunners` is 0.
- **Image:** the owner's custom image `ghcr.io/elitea-ng/eng-runner:2.338.2-20261120`. It is the
  `actions-runner` image (Ubuntu 24.04, runner 2.338.2) plus build tools: cc, zstd and others.
- **Limits:** jobs run directly in the pod. There is no Docker daemon, so no `container:` or `services:`, and
  no root, so no sudo or apt.

How CI routes jobs to it is in [`docs/contributing/ci.md`](../../docs/contributing/ci.md#runners).

The repository is public, so a fork pull request can write `runs-on: eng-runner` into its own copy of a
workflow. YAML cannot stop that. The job-started hook in this directory can: it runs on the runner, before any
step, and refuses every job that is not ours.

> **Status: the hook is DEPLOYED** (2026-11-20, through the owner's GitOps chart; see below). A probe job
> passed: the hook accepted it, and its caches landed under `_work`. It works together with:
>
> - the runner group, which gives access to selected repositories only;
> - approval of fork workflow runs for every external contributor;
> - the YAML routing (`docs/contributing/ci.md`), which keeps fork and Dependabot jobs on GitHub-hosted runners.
>
> Keep approving fork runs with care. The hook refuses a fork job that targets `eng-runner`, but it is the
> only layer that does.

| File | What it is |
|---|---|
| `job-started.sh` | The hook. It allows only the listed repositories (built in: `elitea-platform`, `elitea-docs`, `elitea-brand-template`; an `allowed-repos` key in the same ConfigMap, read from `/etc/eng-runner/allowed-repos`, replaces the list, and no environment variable can; the log names the list it used) and the events `pull_request`, `workflow_dispatch`, `schedule` and `push` (not `merge_group`: its payload does not say who wrote the queued change). It refuses a pull request whose head is another repository (a fork, or a deleted one) and any Dependabot job. Any other case fails closed. Tests: `scripts/ci/eng-runner-hook.test.mjs`. |
| `values-eng-runner.yaml` | The part of the scale-set values that mounts the hook and sets `ACTIONS_RUNNER_HOOK_JOB_STARTED`. |

## Where the hook is deployed

The owner's GitOps chart for the runner scale set is the deployment that counts. It holds the ConfigMap
`eng-runner-hooks`, with the keys `job-started.sh` and `allowed-repos`, mounts it at `/etc/eng-runner`, and sets
`ACTIONS_RUNNER_HOOK_JOB_STARTED`. Its `allowed-repos` list includes `elitea-ng/elitea-e2e-llm`.

The files here are the reference copy, with tests. Keep the two in sync: change `job-started.sh` here, with
its tests, then copy it unchanged into the chart.

The manual steps below do the same thing by hand.

## Runbook (repository owner)

### 1. Deploy the hook

```sh
kubectl -n saas-eng-runner create configmap eng-runner-hooks \
  --from-file=job-started.sh=deploy/ci-runners/job-started.sh \
  --dry-run=client -o yaml | kubectl apply -f -
```

Merge `values-eng-runner.yaml` into the scale set's values and upgrade the release with
`helm upgrade <release> oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set -n <ns> -f <your values>`.
Runner pods pick the hook up when they are recreated. They are ephemeral, so new jobs get it.

The runner group also gives `elitea-ng/elitea-e2e-llm` (public) access, but the built-in allowlist does not
name it: its jobs on `eng-runner` are refused until you add it. To add it, put every allowed repository in a
file, one per line, and add it to the ConfigMap with
`--from-file=allowed-repos=<file>` next to `--from-file=job-started.sh=…`. The file replaces the built-in list.

### 2. Prove it refuses a fork

1. Keep Settings → Actions → General on **Require approval for all external contributors**, so nothing from a
   fork runs without your click.
2. From a test account that is **not** an org member, fork `elitea-ng/elitea-platform`. Open a pull request
   that adds a workflow with one job, `runs-on: eng-runner`, `steps: [{run: echo hi}]`, and a
   `pull_request` trigger.
3. Approve the run. The job must fail at **Set up runner** with
   `eng-runner refuses this job: pull request from <test account>/elitea-platform, not from elitea-ng/elitea-platform`.
   The `echo hi` step must not appear.
4. Close the pull request and delete the fork.

If the job runs `echo hi`, the hook is not active: stop here and check the pod's env
(`kubectl -n saas-eng-runner exec <runner pod> -- env | grep HOOK`) and the mount.

### 3. Turn routing on

Settings → Secrets and variables → Actions → **Variables** → New repository variable:
`ENG_RUNNER` = `on`.

This step does not depend on steps 1 and 2, but turn routing on only with the hook deployed (it is, as of
2026-11-20).

The next run of a same-repo pull request sends the toolchain-only jobs to `eng-runner`. Their "Set up job" log
shows `Runner name: eng-runner-…`, and "Set up runner" shows
`eng-runner: accepted pull_request job of …`.

### 4. Roll back

Set `ENG_RUNNER` to `off` (or delete it). Every job goes back to `ubuntu-latest` on its next run. No pull
request is needed. To cut the pool off from the repository entirely, also untick it under the runner group's
**Repository access**.

## Next step: move the jobs the custom image unlocks

The routed set in `docs/contributing/ci.md` was chosen for the plain `actions-runner` image. The custom image
adds tools that let more jobs move. Each tool is listed with what it unlocks; check that it is in the image
before moving a job:

| Tool | Unlocks |
|---|---|
| A C toolchain (cc, make), `pkg-config`, `libssl-dev` | The Rust jobs (rust-lint, rust-shared, `code-runner`, `quality`, doc-extractor, `serde-order`), and Go with cgo: `go test -race` (`test-checks`, the shards that need no services) |
| `zstd` | The cache client's zstd version. Hosted entries become readable once the cache paths match too (see below) |
| `python3-venv`, `python3-pip` | The Python jobs (ci-go `proto`, mock-llm `unit`, deepwiki-conformance `fixtures`) |
| `shellcheck`, `gh`, `helm`, `yq`, `protoc` | no-binaries `repo-gates`, helm-lint `lint` and `template`, ci-go `env-drift`, dependency-scanning `scan` |

To move a job, switch its `runs-on` to `ROUTED_RUNS_ON`, add `./.github/actions/eng-runner-env` after its
checkout, and let `check-runner-routing.mjs` approve it. The guard's "runs go or node without installing it"
rule and its docker and sudo rules still apply.

**Cache paths.** `.github/actions/eng-runner-env` moves `HOME` and every cache under `_work`. Outside `_work`
the pod has only 10 GiB of ephemeral storage, and a pod that writes past it is evicted. A side effect is that
the cache paths differ from the hosted ones, so hosted entries stay unreadable. Mounting the work volume at
`/home/runner` would let `HOME` stay where it is. The cache paths would then match, and main's cache writers
could move too.

Jobs that need Docker, `services:` or kind stay hosted until there is a daemon option, such as an in-cluster
BuildKit or a separate dind scale set.

## Changing the hook

1. Edit `job-started.sh` here.
2. Run `node --test scripts/ci/eng-runner-hook.test.mjs`.
3. Merge.
4. Copy the file unchanged into the owner's GitOps chart.

The chart's copy is the one that counts; the repository copy is the tested reference.
