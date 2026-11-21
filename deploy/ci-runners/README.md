# eng-runner: the self-hosted CI runner pool

`eng-runner` is the org's ARC runner scale set on GKE (namespace `saas-eng-runner`).

- **Pod size:** 4 vCPU, 16 GiB of memory, an 80 GiB `_work` volume and 10 GiB of ephemeral storage outside it.
  `minRunners` is 0.
- **Image:** `ghcr.io/elitea-ng/eng-runner:2.338.2-20261120.10`, built from [`image/Containerfile`](image/Containerfile).
  It is the `actions-runner` image (Ubuntu 24.04, runner 2.338.2) plus build tools (cc, zstd and others) and
  the native services described in [`image/README.md`](image/README.md).
- **Cache mounts:** the pod template mounts `~/.npm`, `~/.cache` and `~/go` from the work volume
  (`_homecache/*`; `values-eng-runner.yaml` shows them). `HOME` stays `/home/runner`, so the Go and npm caches
  sit at the hosted paths and restore main's entries.
- **Limits:** jobs run directly in the pod. There is no Docker daemon, so no `container:` (a job's `services:`
  run as processes through `.github/actions/native-services`), and no root, so no sudo or apt.

How CI routes jobs to it is in [`docs/contributing/ci.md`](../../docs/contributing/ci.md#runners).

The runner image is described in [`image/README.md`](image/README.md).

The repository is public, so a fork pull request can write `runs-on: eng-runner` into its own copy of a
workflow. YAML cannot stop that. The job-started hook in this directory can: it runs on the runner, before any
step, and refuses every job that is not ours.

> **Status: the hook is DEPLOYED** (2026-11-20, through the owner's GitOps chart; see below). A probe job
> passed: the hook accepted it, and its caches landed under `_work`. It works together with:
>
> - the runner group, which gives access to selected repositories only;
> - approval of fork workflow runs for every external contributor;
> - the YAML routing (`docs/contributing/ci.md`), which keeps fork and Dependabot jobs on GitHub-hosted runners
>   and sends this repository's pull requests, schedules and dispatches to the pool.
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

The next same-repo pull request, schedule or dispatch sends the routed jobs to `eng-runner`. Their "Set up job" log
shows `Runner name: eng-runner-…`, and "Set up runner" shows
`eng-runner: accepted pull_request job of …`.

### 4. Roll back

Set `ENG_RUNNER` to `off` (or delete it). Every job goes back to `ubuntu-latest` on its next run. No pull
request is needed. To cut the pool off from the repository entirely, also untick it under the runner group's
**Repository access**.

## Moving a job to the pool

The jobs the custom image ([`image/Containerfile`](image/Containerfile)) unlocked (the Rust builds and lints,
`test-checks`, the Python jobs, `repo-gates`, helm-lint `template`, `env-drift` and `scan`) are routed;
`docs/contributing/ci.md` ("Runners") lists every routed job and why the rest stay hosted.

To move another job, switch its `runs-on` to `ROUTED_RUNS_ON`, add `./.github/actions/eng-runner-env` after
its checkout (`node: "true"` or `rust: "true"` when it needs them), and let `check-runner-routing.mjs` approve
it. Its toolchain rules (node, go, rust, python) and its docker and sudo rules apply.

**Cache paths.** `HOME` stays `/home/runner`; the pod mounts `~/.npm`, `~/.cache` and `~/go` from the work
volume, and `.github/actions/eng-runner-env` checks the mounts and keeps every other cache under `_work`.
Outside `_work` the pod has only 10 GiB of ephemeral storage, and a pod that writes past it is evicted. The Go
and npm cache paths match a hosted runner's, so main's cache writers run on the pool too.

Jobs that need Docker, `services:` or kind stay hosted until there is a daemon option, such as an in-cluster
BuildKit or a separate dind scale set.

## Changing the hook

1. Edit `job-started.sh` here.
2. Run `node --test scripts/ci/eng-runner-hook.test.mjs`.
3. Merge.
4. Copy the file unchanged into the owner's GitOps chart.

The chart's copy is the one that counts; the repository copy is the tested reference.

## Native stack name map

The pool cannot run containers, so a native stack runs each compose service
as a process on the pod's loopback. The owner's scale-set values give the
runner pod `hostAliases`, and each compose service name gets its own
`127.0.0.x` address. That way:

- every process binds its own address at its compose port, so two services
  that both listen on 9443 (or 8443) never collide;
- the hostnames in configs, seeded rows, mTLS SANs and specs stay exactly as
  compose writes them.

Linux answers the whole of `127.0.0.0/8` on `lo`, so no privilege is needed.

Every native service binds loopback only, never a wildcard: the pool is a shared cluster, and a CI service with
dev credentials must not listen on the pod's network address (`check-native-stack-parity.mjs` and the
supervisor's socket check enforce it). One exception to the map: rustfs refuses a bind host that is not an
interface address, so natively it binds `127.0.0.1:9000` and its clients use that endpoint; `rustfs` stays in
the map for the names compose-shaped configs carry.

| Address | Names |
|---|---|
| 127.0.0.1 | `elitea-main`, `oidc-mock`, `oidc.localhost` |
| 127.0.0.2 | `postgres` |
| 127.0.0.3 | `nats` |
| 127.0.0.4 | `rustfs` |
| 127.0.0.5 | `elitea-deepwiki` |
| 127.0.0.6 | `elitea-inventory` |
| 127.0.0.7 | `elitea-web` |
| 127.0.0.8 | `pgbouncer` |
| 127.0.0.9 | `qdrant` |
| 127.0.0.10 | `elitea-llm-gateway` |
| 127.0.0.11 | `elitea-scheduler` |
| 127.0.0.12 | `elitea-vector` |
| 127.0.0.13 | `elitea-worker` |
| 127.0.0.14 | `elitea-validate-worker` |
| 127.0.0.15 | `elitea-deepwiki-engine` |
| 127.0.0.16 | `elitea-inventory-engine` |
| 127.0.0.17 | `llm-mock` |
| 127.0.0.18 | `llm-real` |
| 127.0.0.19 | `mcp-mock` |
| 127.0.0.20 | `mcp-mock-trust` |
| 127.0.0.21 | `qtest-mock` |
| 127.0.0.22 | `ado-mock` |
| 127.0.0.23 | `otel-collector` |
| 127.0.0.24 | `openbao` |
| 127.0.0.25 | `azurite` |
| 127.0.0.26 | `fake-gcs-server` |
| 127.0.0.27 | `egress-proxy` |
| 127.0.0.28 | `governed-mocks`, `entra.governed.test`, `jira.governed.test`, `sharepoint.governed.test` |
| 127.0.0.29 | `governed-nats-tap` |

**Why 127.0.0.1 is shared.** `elitea-main` and the OIDC mock are where the
browser goes. Chromium resolves every `*.localhost` name to loopback by
itself, so `oidc.localhost` has to be on 127.0.0.1.

**Deliberately not mapped:** `graph.microsoft.com` and
`login.microsoftonline.com`. The pod's `/etc/hosts` applies to every job on
the pool, so mapping them would break real access to Microsoft for all of
them. A native governed stack has to point its clients at
`governed-mocks` explicitly instead.

**The `services:` jobs do not use the map.** They reach PostgreSQL, NATS,
Qdrant and OpenBao on `127.0.0.1:<port>`, the same as the port mappings of a
`services:` block on a hosted runner.

**The map lives in the owner's values, not here.** The kubelet writes the
pod's `/etc/hosts`, so an image cannot carry it. A new compose service needs
its own address there before a native stack can run it, and this table needs
the same change.
