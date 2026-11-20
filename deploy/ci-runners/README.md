# eng-runner: the self-hosted CI runner pool

`eng-runner` is the org's ARC runner scale set on GKE (namespace `saas-eng-runner`). Each pod has 4 vCPU,
16 GiB and a 40 GB `_work` volume, and runs the plain `actions-runner` image (Ubuntu 24.04). Jobs run directly
in the pod: there is no Docker daemon, no `container:` or `services:`, no sudo, no apt and no C compiler. How CI routes jobs
to it is in [`docs/contributing/ci.md`](../../docs/contributing/ci.md#runners).

The repository is public, so a fork pull request can write `runs-on: eng-runner` into its own copy of a
workflow. YAML cannot stop that. The job-started hook in this directory can: it runs on the runner, before any
step, and refuses every job that is not ours.

> **Status: the hook is NOT deployed yet.** It is the recommended hardening. Until it is deployed, these are the
> guards:
>
> - The runner group gives access to selected repositories only.
> - A fork pull request's workflow runs wait for approval for every external contributor.
> - The YAML routing (`docs/contributing/ci.md`) keeps fork and Dependabot jobs on GitHub-hosted runners.
>
> **The remaining risk:** a maintainer approves a fork pull request whose own workflow YAML targets
> `eng-runner`. That job would run in the cluster.
>
> **The rule: never approve workflow runs on a fork pull request until the hook is deployed.** Review the
> fork's code in the diff instead, or push its commits to a branch of this repository yourself.

| File | What it is |
|---|---|
| `job-started.sh` | The hook. It allows only the listed repositories (built in: `elitea-platform`, `elitea-docs`, `elitea-brand-template`; an `allowed-repos` key in the same ConfigMap, read from `/etc/eng-runner/allowed-repos`, replaces the list, and no environment variable can; the log names the list it used) and the events `pull_request`, `workflow_dispatch`, `schedule` and `push` (not `merge_group`: its payload does not say who wrote the queued change). It refuses a pull request whose head is another repository (a fork, or a deleted one) and any Dependabot job. Any other case fails closed. Tests: `scripts/ci/eng-runner-hook.test.mjs`. |
| `values-eng-runner.yaml` | The part of the scale-set values that mounts the hook and sets `ACTIONS_RUNNER_HOOK_JOB_STARTED`. |

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

This step does not depend on steps 1 and 2. The owner decided on 2026-11-20 to turn routing on before the hook
is deployed. In that case the rule in the status box above applies.

The next run of a same-repo pull request sends the toolchain-only jobs to `eng-runner`. Their "Set up job" log
shows `Runner name: eng-runner-…`. With the hook deployed, "Set up runner" also shows
`eng-runner: accepted pull_request job of …`.

### 4. Roll back

Set `ENG_RUNNER` to `off` (or delete it). Every job goes back to `ubuntu-latest` on its next run. No pull
request is needed. To cut the pool off from the repository entirely, also untick it under the runner group's
**Repository access**.

## Next step: a custom runner image (not implemented)

Most hosted-only jobs are hosted because the plain `actions-runner` image lacks tools, not because of Docker. The
proposal is an image at `deploy/ci-runners/Containerfile`: `FROM ghcr.io/actions/actions-runner:<pinned digest>`
plus apt installs at build time. It would be published by a hosted workflow to ghcr and pinned by digest in the
scale-set values.

| Add | Unlocks |
|---|---|
| `build-essential` (gcc, make), `pkg-config`, `libssl-dev` | Rust jobs (rust-lint, rust-shared, `code-runner`, `quality`, doc-extractor, `serde-order`), and Go with cgo: `go test -race` (`test-checks`, the shards that need no services) |
| `zstd` | The same cache version as hosted runners, so routed jobs read the zstd entries main writes, and main's writers could move too |
| `python3-venv`, `python3-pip` | The Python jobs (ci-go `proto`, mock-llm `unit`, deepwiki-conformance `fixtures`) with a venv, or `setup-python` with `AGENT_TOOLSDIRECTORY` pre-created |
| `shellcheck`, `gh`, `helm`, `yq`, `protoc` | no-binaries `repo-gates`, helm-lint `lint` and `template`, ci-go `env-drift`, dependency-scanning `scan` |

Also mount the 40 GB work volume at `/home/runner`, not only at `/home/runner/_work`. The container's
ephemeral-storage limit is 1 GiB, so today `.github/actions/eng-runner-env` moves `HOME` and every cache under
`_work` to keep a job from being evicted. With the volume at `/home/runner`, `HOME` stays where it is, and the
cache paths match the hosted ones.

Jobs that need Docker, `services:` or kind stay hosted until there is a daemon option (an in-cluster BuildKit or a
separate dind scale set). Once the image lands, move each job by switching its `runs-on` to `ROUTED_RUNS_ON` and
letting `check-runner-routing.mjs` approve it.

## Changing the hook

Edit `job-started.sh` here, run `node --test scripts/ci/eng-runner-hook.test.mjs`, merge, then repeat step 1.
The cluster copy is the one that counts: the repository copy is a reference.
