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
| `shellcheck` | Shell lint. | `helm-lint`, `no-binaries`. |
| `unzip`, `xz-utils` | Archives that setup actions and scripts unpack. `unzip` is already in the base, so listing it only states the dependency. | Browser and toolchain downloads (`ci-browser-session`, `ci-web-e2e` helpers). |
| `skopeo` | Registry copies and inspects without a daemon. | `mirror-buildkit`-style jobs, image digest checks. |
| `gh` 2.105.0 | Scripts that call the GitHub API. | PR and release housekeeping scripts. |
| `helm` v4.3.2 | Helm 4, the latest stable release. | Chart render and lint jobs that do not run `azure/setup-helm`. |
| `yq` v4.54.3 | YAML edits in shell steps. | `helm-lint`, `publish`, `nightly-image-scan`. |
| `ca-certificates` | TLS trust for every download above. | Everything that downloads. |

**Not needed, checked against every tracked `Cargo.lock`.** Nothing links
`openssl-sys`: TLS is rustls on `ring` or `aws-lc-rs`. Nothing uses `bindgen`,
so there is no libclang. `protoc` is vendored (`protoc-bin-vendored`).

**Out of scope.** The GTK and WebKit `-sys` crates of
`apps/elitea-desktop/src-tauri` need `libwebkit2gtk-4.1-dev` and friends. The
desktop jobs stay on hosted runners, so those libraries are not here.

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
- apt packages are not version-pinned. The weekly rebuild runs `apt-get
  upgrade`, so they track Ubuntu 24.04 security updates.

### Tool cache

`RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` are both
`/home/runner/_tool`. The directory is created in the image and owned by
`runner`.

- The runner resolves its tool directory from `RUNNER_TOOL_CACHE` first
  (`HostContext.cs`, `WellKnownDirectory.Tools`). Without it, the runner uses
  `_work/_tool`.
- `setup-go`, `setup-node` and `setup-python` install into what the runner
  reports.
- setup-python's Ubuntu installer also reads `AGENT_TOOLSDIRECTORY` directly.
  The Ubuntu Python builds are relocatable, so a non-default path works.
  Only macOS needs the fixed `hostedtoolcache` path.
- `dtolnay/rust-toolchain` uses rustup under `$HOME` (`~/.rustup`,
  `~/.cargo`), which is writable.

The cache starts empty in every pod, because pods are ephemeral. It sits on
the pod's root overlay, not on the 40 GB `_work` volume. To put it on the
volume instead, unset both variables in the scale-set values, and the runner
falls back to `_work/_tool`.

### Why Ubuntu

The Debian 13 single-OS-family rule covers the images we build and ship.
This image is CI infrastructure, and it extends `actions-runner`, which is
Ubuntu 24.04 upstream (`ImageOS=ubuntu24`). It also has to stay
binary-compatible with what the setup actions download: their Python and Node
builds are made for the hosted Ubuntu images. No checker in `scripts/ci`
scans this file for the OS family.

## Switching the pool to this image (owner)

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

3. **Apply it:**

   ```sh
   helm upgrade <release> oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
     -n saas-eng-runner --version <the installed chart version> -f <values.yaml>
   ```

   New runner pods use the image, and running jobs finish on the old one.
4. **Check it.** Dispatch any job on `eng-runner` that runs `cmake --version`
   and `cc --version`, or watch the next routed job.

**Rollback.** Set the previous dated tag, or the stock
`ghcr.io/actions/actions-runner:2.338.2`, and run `helm upgrade` again. Dated
tags are kept. A bad rebuild never replaces an existing dated tag, except
another build on the same UTC day.

## Rebuild cadence

| Trigger | What it does |
|---|---|
| Pull request touching `deploy/ci-runners/image/**` or the workflow | Builds and smoke-tests. Nothing is pushed. |
| Weekly, Mondays 05:17 UTC (`schedule`, main) | Builds fresh with no layer cache, so `apt-get upgrade` picks up security updates. Smoke-tests, then pushes `2.338.2-<date>` and `2.338.2`. It then fails if `actions/runner` has a newer release than the pin, so the bump is not missed. GitHub stops accepting old runner versions some weeks after a release. |
| `workflow_dispatch` from main | The same as the weekly run, on demand. Use it for the first publish. |

The pool does not follow the moving tag by itself, because the values pin a
dated tag. Moving the pool to a new dated tag is a deliberate values change.

The smoke step runs inside the built image as `runner`. It runs every tool's
version command, creates a venv, checks that the tool cache and `$HOME` are
writable, compiles and runs a C and a C++ file, and asserts that no container
engine and no toolchain (go, cargo, rustc, node) is in the image.

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
