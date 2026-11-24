# eng-runner image

The container image for the org self-hosted runner pool `eng-runner` (ARC
runner scale set on GKE, namespace `saas-eng-runner`). It is the stock
`ghcr.io/actions/actions-runner` plus the native tools our jobs need and cannot
install themselves.

- Source: [`Containerfile`](Containerfile)
- Built, smoke-tested and published by
  [`.github/workflows/eng-runner-image.yml`](../../../.github/workflows/eng-runner-image.yml),
  on GitHub-hosted runners only
- Published as `ghcr.io/elitea-ng/eng-runner`

## Why a custom image

The probe of 2026-11-20 found that jobs run bare in the runner pod. There are
no container hooks and no Docker daemon, so `container:` and `services:` fail.
There is also no root: `NoNewPrivs=1` and `CapEff=0`. That means `sudo
apt-get install` in a job fails. The stock image has `python3`, `git`, `jq`,
`curl` and `unzip`, and nothing else a build needs. Whatever a job needs that
a setup action cannot download into `$HOME` therefore has to be in the image.

The toolchains themselves are left out on purpose. Go, Node, Python and Rust
versions still come from `actions/setup-go`, `setup-node`, `setup-python` and
`dtolnay/rust-toolchain`, exactly as on `ubuntu-latest`. So a version bump
stays a workflow change and never needs an image rebuild.

## What is in it, and why

| Package | Why | Jobs it unlocks |
|---|---|---|
| `build-essential` (gcc, g++, make, libc dev) | cgo for `go test -race`. The `cc` crate builds C and C++ for `ring`, `aws-lc-sys`, `libsqlite3-sys` (rusqlite `bundled`), `onig_sys`, `zstd-sys` and `libfuzzer-sys`. | The Go race jobs (`ci-go`, `ci-gateway`, `ci-deepwiki-engine`, `ci-inventory-engine`). Every Rust build and clippy job. |
| `cmake` | `aws-lc-sys` builds with cmake. It is in `libs/rust`, `elitea-worker-rust`, `elitea-vector`, `elitea-deepwiki-engine`, `elitea-inventory-engine` and the desktop `src-tauri` lockfiles. | The Rust jobs. |
| `pkg-config` | The `pkg-config` crate is in the same graphs. Build scripts probe with it before they fall back to a vendored build. | The Rust jobs. |
| `zstd` | Without `zstd` on PATH, `actions/cache` falls back to gzip, which is slower. It also writes under a different cache version, so a self-hosted run would never hit the entries a hosted run wrote. | Every job that restores a cache. |
| `python3-venv`, `python3-pip` | `python3 -m venv` and `pip` against the system Python, for helper scripts. The stock image has `python3` but cannot create a venv. | `pip install` steps in `helm-lint`, `ci-go`, `no-binaries`, `ci-deepwiki-conformance` and `helm-install-smoke`. |
| `python3-yaml` | PyYAML for the system `python3`, which refuses `pip install` (PEP 668). `scripts/nats/render-secure-conf.sh` imports it, and `ci-secure-test-env.sh` otherwise tries to pip-install it. | The secured NATS tests in the ci-go shards and ci-gateway. |
| `shellcheck` | Shell lint. | `helm-lint`, `no-binaries`. |
| `unzip`, `xz-utils` | Archives that setup actions and scripts unpack. `unzip` is already in the base, so listing it only states the dependency. | Browser and toolchain downloads (`ci-browser-session`, `ci-web-e2e` helpers). |
| `skopeo` | Registry copy and inspect without a daemon. | Steps that copy or inspect images, such as `mirror-buildkit.yml`. |
| `gh` 2.105.0 | The GitHub CLI. It is preinstalled on hosted runners, so steps call it without installing it. | Any `run:` step that calls `gh`. |
| `helm` v4.3.2 | Helm 4, the latest stable release. | A fallback only. Jobs that run `azure/setup-helm` get the pinned version from `check-helm-version.mjs` first on PATH. |
| `yq` v4.54.3 | YAML edits in shell steps. | `helm-lint`'s yq step: it uses a `yq` already on PATH and otherwise needs `sudo`, which the pool lacks. Also `publish` and `nightly-image-scan`. |
| `ca-certificates` | TLS trust for every download above. | Everything that downloads. |
| `locales` (`en_US.UTF-8` generated) | The locale the `postgres:18` service image initialises its cluster with (`LANG=en_US.utf8`). A job that runs its PostgreSQL as a process on the pool must pass `initdb --locale=en_US.UTF-8` to sort and compare text as on a hosted runner: `LANG` itself is not set (so other jobs see no change), and a plain `initdb` gets the C locale. The smoke test initialises its cluster that way and checks the collation. | Every job whose `services:` run as processes on the pool. |
| `gnupg`, `openssl` | `gnupg` checks the PGDG key's fingerprint at build time. `openssl` is the CLI the cert scripts (`gen-*-certs.sh`) call. | The native E2E stacks' cert generation. |

**Not needed, checked against every tracked `Cargo.lock`.** Nothing links
`openssl-sys`: TLS is rustls on `ring` or `aws-lc-rs`. Nothing uses `bindgen`,
so there is no libclang. `protoc` is vendored (`protoc-bin-vendored`).
(`libssl-dev` arrives with the Tauri libraries below, not for these crates.)

### Service binaries and browsers

The pool cannot start a container, so `services:` and compose do not work
there. A job runs these as plain processes instead, as the runner user, with
their data under `_work`. None of them is started, and no data directory is created,
in the image.

| What | Version, source | Replaces | Jobs it unlocks |
|---|---|---|---|
| PostgreSQL server and client (`postgres`, `initdb`, `pg_ctl`, `pg_isready`, `psql`, …) | 18 (latest 18.x at build), PGDG apt repo, key checked by fingerprint | the `postgres:18` service | the ci-go shards, integration, snapshots and editor lifecycle; ci-gateway; ci-contract; ci-rust's PostgreSQL test; the deepwiki and inventory engines; handoff loadgate; governed scale |
| `pgbouncer` | PGDG | the standalone stack's pgbouncer | the standalone E2E lanes |
| `nats-server`, `nats` | 2.12.0, natscli 0.4.0 (the nats-box 0.19.7 CLI), GitHub releases | the `nats:2.12.0` service and nats-box bootstrap | ci-go, ci-gateway (the secured NATS tests read `ELITEA_TEST_NATS_SERVER_BIN`), governed scale, the E2E stacks |
| `qdrant` | v1.19.2, GitHub release (glibc build) | the `qdrant/qdrant:v1.19.2` service | ci-vector, the standalone lanes |
| `bao` | OpenBao 2.4.1, GitHub release | the `openbao:2.4.1` service (`-dev`, no `IPC_LOCK` needed) | ci-go's SecretStore test, governed conformance |
| `rustfs`, `rc` | rustfs 1.0.0 (musl build), rc v0.1.38 (`rustfs/cli`), GitHub releases | the rustfs and `rustfs/rc` containers | the db-storage shard, ci-contract, the E2E stacks |
| `fake-gcs-server` | 1.56.1, GitHub release | the storage-test GCS emulator | the db-storage shard, ci-contract |
| `oidc-provider-mock` | 0.4.8, PyPI, a venv in `/opt/oidc-provider-mock`, every wheel pinned by hash | the `oidc-provider-mock` container | the E2E stacks |
| `nginx` (`nginx-light`) | Ubuntu noble; `nginx-light` is `nginx` plus the echo module there | the elitea-web container's nginx | the E2E stacks |
| Playwright browsers in `/ms-playwright` (`PLAYWRIGHT_BROWSERS_PATH`) | 1.62.1: chromium, its headless shell, webkit and ffmpeg, copied out of `mcr.microsoft.com/playwright:v1.62.1-noble` by digest; firefox left out | the Playwright container of the E2E legs | the native E2E legs |
| The browsers' system libraries | Playwright's own `install-deps chromium webkit`, run at build time | the same container | the same |
| Tauri Linux build libraries (`libwebkit2gtk-4.1-dev`, `libgtk-3-dev`, `libsoup-3.0-dev`, `libayatana-appindicator3-dev`, `librsvg2-dev`, `libssl-dev`, `libxdo-dev`) | Ubuntu noble | `sudo apt-get install` in the handoff journey | the handoff journey's host test |

**Not here, and why.**
- **Azurite** is a Node program, and the image ships no Node (the toolchain
  rule above). A job that needs it runs it with the Node that `setup-node`
  installs.
- **llama-server and the OpenTelemetry collector** are optional in the native
  stack. The real-model lanes move later with their own model files, and the
  collector is dropped natively.
- **The sandbox, gVisor, kind and egress-proof lanes** need isolation the pool
  cannot give. They stay on hosted runners.

**The browsers are read-only.** `/ms-playwright` belongs to root and nobody
else can write it. On the pool, never run `npx playwright install` (it writes
its link file there and fails) or `install-deps` (it needs root). The browsers
are the ones `@playwright/test` 1.62.1 drives. `PLAYWRIGHT_BROWSERS_PATH` is
set for every job on the pool. A routed job that needs another Playwright
release must stay hosted. The build refuses a Playwright stage whose browser
revisions are not the ones that release's `browsers.json` names.
`apps/elitea-web/scripts/check-playwright-image-tag.mjs` fails when
`ARG PLAYWRIGHT_VERSION` and the web app's pin differ, so a library bump
needs an image rebuild and a pin bump on the pool first.

**Sizes.** The base `actions-runner` image is about 549 MB compressed. With
the build tools, the services and the browsers, this image measured
**about 1.7 GB compressed** (its saved archive was 1,786,194,432 bytes) and
**about 4.9 GB unpacked** (4,648 MiB of uncompressed layers) on the PR that
added the services (#2722). Every build prints both in its summary, and publish prints the exact sum of the pushed
layers. The pool pulls
it through an Artifact Registry remote with image streaming, so a node does
not wait for the whole image before a pod starts.

**Removed from the base.** The upstream image ships Docker's static bundle.
The daemon side of it (`dockerd`, `containerd*`, `ctr`, `runc`, `docker-init`
and `docker-proxy`) is deleted, because the pool has no privileges to run it.
The `docker` CLI and the buildx plugin stay, for a future in-cluster BuildKit
reached over TCP.

**Pinning.**
- The base is pinned by tag and by manifest-list digest.
- `gh`, `helm` and `yq` are pinned by version and SHA-256. The build checks
  each download against the pinned hash and against the checksum file of the
  same release (`gh_<v>_checksums.txt`, `helm-<v>-linux-amd64.tar.gz.sha256sum`,
  and yq's `checksums` read through `checksums_hashes_order`). A mismatch with
  either fails the build.
- The service binaries are pinned the same way, by version and SHA-256,
  checked against the release's checksum file (`SHA256SUMS`,
  `checksums-linux.txt`, `checksums.txt`, `<asset>.sha256`). Qdrant publishes
  no checksum file, so its pin is the only check.
- The PGDG repository is trusted through its key, with `signed-by` for that
  repository only. The build refuses a key whose fingerprint is not
  `B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8`. The PostgreSQL major is pinned
  (18), and the minor follows PGDG.
- The Playwright stage is pinned by tag and index digest. The
  `playwright-core` tarball that runs `install-deps` is pinned by SHA-256 and
  checked against the npm registry's published integrity.
- `oidc-provider-mock` and its dependency closure are pinned by hash in
  `oidc-provider-mock.requirements.txt`. pip installs them with
  `--require-hashes --only-binary=:all:`.
- apt packages are not version-pinned. The weekly rebuild runs `apt-get
  upgrade`, so they track Ubuntu 24.04 security updates.

**Version skew with the workflows.** The image takes the latest stable `helm`
and `yq`. The workflows pin their own: `HELM_VERSION` in
`scripts/ci/check-helm-version.mjs` (v4.3.1 on 2026-11-20), and `YQ_VERSION`
in `helm-lint.yml`. That checker reads workflows only, not this file.

- A job that installs its own version keeps it, because `setup-helm` puts the
  pinned helm first on PATH.
- A job that falls back to the image's binary runs the image's version, so a
  render can differ between the pools.

The routed helm and yq jobs keep their pins or never had one: helm-lint
`template` installs its pinned Helm 4 and Helm 3 with `setup-helm`, and ci-go
`env-drift` runs the image's yq on the pool (its hosted step installs `latest`
when the runner lacks one, so it was never pinned). helm-lint `lint`, the job
that pins `YQ_VERSION`, stays hosted. A job routed later that falls back to the
image's helm or yq should either bump its pin to the image's version or keep
its own install step.

### Disk: everything on `_work` (1Gi root limit)

On the pool, the runner container has an **`ephemeral-storage` limit of
1Gi**. Only `/home/runner/_work` is the separate 40 GB volume. Anything a job
writes elsewhere counts against the 1Gi: a toolchain, a module cache or a
temp file. Going over gets the pod evicted mid-job. So the image points every
cache and the temp dir at `_work` by default:

| Variable | Default | Used by |
|---|---|---|
| `RUNNER_TOOL_CACHE`, `AGENT_TOOLSDIRECTORY` | `/home/runner/_work/_tool` | The runner's tool directory (`HostContext.cs`, `WellKnownDirectory.Tools`), so setup-go, setup-node and setup-python. setup-python's Ubuntu installer reads `AGENT_TOOLSDIRECTORY` directly. The Ubuntu Python builds are relocatable, and only macOS needs `hostedtoolcache`. |
| `TMPDIR` | `/home/runner/_work/_temp` | `mktemp`, compilers, pip and npm. This is the runner's own per-job temp dir (`RUNNER_TEMP`). |
| `XDG_CACHE_HOME` | `/home/runner/_work/_cache` | Anything that follows XDG. |
| `GOMODCACHE`, `GOCACHE` | `_work/_cache/go-mod`, `_work/_cache/go-build` | go. |
| `CARGO_HOME`, `RUSTUP_HOME` | `_work/_cache/cargo`, `_work/_cache/rustup` | rustup and cargo. `dtolnay/rust-toolchain` puts `$CARGO_HOME/bin` on PATH. |
| `npm_config_cache` | `_work/_cache/npm` | npm. |
| `PIP_CACHE_DIR` | `_work/_cache/pip` | pip. |

**`_work` is a mounted volume, so it is empty when the pod starts.**
- Nothing is or can be pre-created under it in the image, because the mount
  would hide it.
- Each tool creates its own directory on first use. setup-go, setup-node and
  setup-python create the tool cache. go, rustup, pip and npm create their
  homes.
- The runner creates and empties `_work/_temp` at job start, before the first
  step.
- The smoke test checks this. It mounts an empty `_work`, then every variable
  must point under it and be creatable.

Two caveats:

1. **A step that sets its own value wins over these defaults.** The repo's
   `.github/actions/go-cache` and `npm-cache` actions export
   `GOCACHE=$HOME/.cache/go-build`, `GOMODCACHE=$HOME/go/pkg/mod` and
   `NPM_CONFIG_CACHE=$HOME/.npm`, because `main-write-cache` saves those
   literal paths. On this pool `$HOME` is the 1Gi root fs. The PR that routes
   Go and Node jobs here has to make those actions pool-aware, for example by
   using `_work` paths when `runner.environment == 'self-hosted'`, keyed apart
   from the hosted entries. Routing them unchanged gets the pods evicted.
2. **`$HOME` itself stays on the root fs.** git config and small dotfiles are
   fine. Anything large is not.

**Optional (values):** raise the runner container's limit to 8 to 10Gi. That
gives headroom for whatever still writes outside `_work`, such as `$HOME`
dotfiles and a stray `/tmp`:

```yaml
template:
  spec:
    containers:
      - name: runner
        resources:
          limits:
            ephemeral-storage: 10Gi
          requests:
            ephemeral-storage: 8Gi
```

### Action archives (`/opt/action-archive-cache`)

Every remote action the workflows use, as the tarball the runner would fetch
from codeload, at `<owner>_<repo>/<commit sha>.tar.gz`, with
`ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE` pointing there. The pool shares one
egress address and codeload answered 429 to it (#3003). The build workflow
fills `action-archive/` with `scripts/ci/action-archive-list.mjs --download`
before the build; the directory is never committed. A commit that is not in
the image is downloaded by the runner as before, so the cache can lag a
Dependabot bump by up to a week without breaking anything. About 50 MB.
docs/contributing/ci.md, "Action archives come from the image", has the
details. The scale-set values set the variable explicitly too (pod env wins
over the image), so both must name the same directory;
`scripts/ci/action-archive-list.test.mjs` holds the Containerfile and
`../values-eng-runner.yaml` to it.

### Why Ubuntu

The Debian 13 single-OS-family rule covers the images we build and ship.
This image is CI infrastructure, and it extends `actions-runner`, which is
Ubuntu 24.04 upstream (`ImageOS=ubuntu24`). It also has to stay
binary-compatible with what the setup actions download: their Python and Node
builds are made for the hosted Ubuntu images. No checker in `scripts/ci`
scans this file for the OS family.

## Switching the pool to this image (owner)

The scale-set values are not in this repository. They live in the owner's
gitops (Argo CD) repository, in the `saas-eng-runner` application, and Argo
CD applies them. The steps below edit those values. In a gitops setup,
"apply" means committing the change there and letting Argo CD sync it.

1. **Make the package readable by the cluster.** GitHub creates
   `ghcr.io/elitea-ng/eng-runner` as PRIVATE on its first push. Do one of
   these:
   - Make it public: the package settings, then *Change visibility*, then
     Public. It holds nothing secret, only the stock runner plus public
     tools.
   - Keep it private and give the scale set an `imagePullSecret`. That is a
     `docker-registry` secret in `saas-eng-runner` holding a token with
     `read:packages`, listed under `template.spec.imagePullSecrets`.
2. **Point the runner container at a dated tag** in the scale-set values (the
   `gha-runner-scale-set` chart):

   ```yaml
   template:
     spec:
       containers:
         - name: runner
           image: ghcr.io/elitea-ng/eng-runner:2.338.2-YYYYMMDD
           command: ["/home/runner/run.sh"]
   ```

   Use the dated tag the publish run printed in its summary. It does not move
   under you. `:2.338.2` moves with every weekly rebuild.

   Keep `command` as it is in the current values. The image's entrypoint is
   unchanged from upstream.

3. **Apply it.** Commit the values change in the gitops repository and let
   Argo CD sync `saas-eng-runner`. That is the `helm upgrade` of the
   `gha-runner-scale-set` release. A manual `helm upgrade` would be reverted
   by the next sync.

   New runner pods use the image, and running jobs finish on the old one.
4. **Check it.** Dispatch any job on `eng-runner` that runs `cmake --version`
   and `cc --version`, or watch the next routed job.

**Rollback.** Set the previous dated tag, or the stock
`ghcr.io/actions/actions-runner:2.338.2`, in the same values, and sync again. Dated
tags are kept and never overwritten. A second build on the same UTC day is
published as `<version>-<date>.<run number>`, and publish refuses a tag that
already exists.

## Rebuild cadence

| Trigger | What it does |
|---|---|
| Pull request touching `deploy/ci-runners/image/**` or the workflow | Builds and smoke-tests. Nothing is pushed. |
| Weekly, Mondays 05:17 UTC (`schedule`, main) | Builds fresh with no layer cache, so `apt-get upgrade` picks up security updates. Smoke-tests, then pushes `2.338.2-<date>`, checks that the pushed config digest is the smoke-tested image ID, and moves `2.338.2` to it. A separate `runner-pin` job fails if `actions/runner` has a newer release than the pin. So a red `publish` means the image did not ship, and a red `runner-pin` means only that the bump is due. GitHub stops accepting old runner versions some weeks after a release. |
| `workflow_dispatch` from main | The same as the weekly run, on demand. Use it for the first publish. |

The pool does not follow the moving tag by itself, because the values pin a
dated tag. Moving the pool to a new dated tag is a deliberate values change.

The smoke step runs inside the built image as `runner`. It runs every tool's
version command. It checks that `_work` is not baked into the image. Then,
with an empty runner-owned `_work` mounted the way the pool mounts it, it
checks that every cache variable and `TMPDIR` point under `_work` and can be
created there, and that `mktemp` lands there. It also creates a venv, checks
that `$HOME` is writable, compiles and runs a C and a C++ file, checks the CA bundle, and
asserts that no container engine and no toolchain (go, cargo, rustc, node) is
in the image. Every package in the table above is exercised there. Change the
two together. On every run, PRs included, the build job also saves the image
and reads the archive back with skopeo. That proves the path publish depends
on before a merge.

A second smoke step covers the service binaries and the browsers, again as
`runner` (uid 1001) with an empty `_work` and no extra capabilities, the way
the pool runs them. Every binary prints its version. PostgreSQL gets an
`initdb` under `_work`, starts, answers over TCP and its socket, and stops.
nats-server (JetStream), qdrant, `bao server -dev` (without `IPC_LOCK`),
fake-gcs-server and a non-root nginx each start, pass their health probe and
are stopped. The Tauri libraries resolve through pkg-config. Chromium (the
headless shell and the full browser) and WebKit launch and render a page.
They run under the hosted runner's node and the pinned `playwright-core`,
which are mounted outside PATH, since the image has no node. The step also
checks that `/ms-playwright` is read-only to the runner and holds no firefox.

## Bumping

**The runner.** Set `RUNNER_VERSION` and `RUNNER_DIGEST` in the Containerfile
together. Get the digest with:

```sh
skopeo inspect --raw docker://ghcr.io/actions/actions-runner:<version> | sha256sum
```

Then open a PR. CI builds it, and the merge plus a dispatch publishes it.

**gh, helm and yq.** Set the version and the SHA-256 of the linux amd64 asset,
taken from the release's own checksum file. The build refuses a hash the
release does not publish.

**The service binaries** work the same way: `*_VERSION` and `*_SHA256` in the
Containerfile. Keep them at the versions the workflows' `services:` and the
compose files run (the comment above the ARGs lists where), so a job behaves
the same on both pools.

**Playwright** moves with `@playwright/test` in `apps/elitea-web/package.json`.
Set `PLAYWRIGHT_VERSION`, `PLAYWRIGHT_DIGEST` (the index digest of
`mcr.microsoft.com/playwright:v<version>-noble`, from `skopeo inspect --raw … |
sha256sum`) and `PLAYWRIGHT_CORE_SHA256` (the SHA-256 of
`playwright-core-<version>.tgz` from the npm registry) together.

**oidc-provider-mock**: regenerate the whole
`oidc-provider-mock.requirements.txt` with the command in its header.
