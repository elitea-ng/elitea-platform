# Code language adapters

`python.mjs` executes the prepared Python request through pinned Pyodide 0.29.0
and Deno. It is an image component, not a host execution service. The runner and
outer container must enforce CPU, memory, PID, output and time limits during
interpreter startup, package preparation and user execution alike.

Input is the supervisor-owned prepared-job JSON file (revision 1), containing
`language`, `source` and already-selected `input`. The CLI takes the request path
and a per-job writable package-cache directory. Runtime assets and the Deno
cache are preloaded into the immutable image. Do not mount shared writable
package caches between jobs. No platform credentials are supplied here.

Only one JSON envelope is written to stdout: `{"revision":1,"result":...}`.
Python prints and package messages stream to stderr, bounded by the parent
runner. The last Python expression is the result; a Python exception exits
unsuccessfully. Results must be finite JSON values and at most 256 KiB; the
worker must still apply its typed output/state boundary. Output is untrusted.

`elitea_state` contains selected input, and `alita_state` is a shallow copy for
legacy compatibility. Top-level await and explicit micropip installation are
supported. An explicit micropip import leaves installation order and versions
to the code. Otherwise Pyodide loads prepared packages through the frozen import-name mapping.
Remaining missing imports use micropip and require available package content.
Package availability and egress are deployment policy. Network remains
disabled in the default Code container. Download authorization and a package
proxy are not implemented by this adapter.

For the offline compatibility tests, first prepare the cache using the existing
`preload.mjs` preparation path with a JSON package profile containing
`["idna==3.10", "python-slugify==8.0.4"]`. For example:

```sh
DENO_DIR=/path/to/deno-cache deno run --frozen \
  --lock=services/elitea-code-runner/adapters/deno.lock \
  --allow-read --allow-write=/path/to/wheels --allow-env=NODE_DEBUG \
  --allow-net=cdn.jsdelivr.net,pypi.org,files.pythonhosted.org \
  services/elitea-code-runner/adapters/preload.mjs /path/to/wheels /path/to/profile.json
```

The preparation process never executes user source. From the repository root:

```sh
DENO_DIR=/path/to/deno-cache ELITEA_TEST_WHEEL_CACHE=/path/to/wheels \
  deno test --frozen --lock=services/elitea-code-runner/adapters/deno.lock \
  --cached-only --no-prompt --deny-net \
  --allow-read=/path/to/deno-cache,/path/to/wheels \
  --allow-write=/path/to/wheels \
  --allow-env=NODE_DEBUG,ELITEA_TEST_WHEEL_CACHE \
  services/elitea-code-runner/adapters/python_test.mjs
```

These interpreter tests do not prove Linux isolation or deployment. The
`deno-runtime` image target supplies the fixed `elitea-code-execute` launcher.
Scoped platform-client access, runtime-profile selection, graph binding and browser
acceptance remain integration work. Do not enable Code nodes based only on these tests.


## JavaScript and TypeScript

`javascript.mjs` accepts `javascript` or `typescript` and the same prepared-job
input. The second CLI argument is a job-local writable scratch directory. Deno
loads a uniquely named module from that directory, transpiling TypeScript.
Source is a module exporting a default JSON result, promise, or function accepting
selected state and returning the result. Top-level await and module imports are
supported by Deno, subject to the image's package cache and deployment permissions.
For example:

```typescript
export default async (state: { count: number }) => ({ count: state.count + 1 });
```

`elitea_state` and `alita_state` are also available as global input values.
Host input is cloned before exposure. `console.log`/`console.info` diagnostics go
to stderr; stdout carries the same revisioned result envelope. All output remains
untrusted: user code can access Deno APIs permitted to that job. Cycles,
non-finite numbers, undefined/function/symbol/bigint values and oversized results
fail instead of silently deleting fields. No VM security claim is made.

The local tests need only scratch access, no network or package downloads:

```sh
deno test --no-lock --no-prompt --deny-net \
  --allow-read=/tmp,/private/tmp --allow-write=/tmp,/private/tmp \
  services/elitea-code-runner/adapters/javascript_test.mjs
```


## Preloaded Linux image

Build `services/elitea-code-runner/Containerfile` with target `deno-runtime` and
context `services/elitea-code-runner`. Deno 2.5.4 is pinned by its multi-platform
image digest. The Deno lockfile pins Pyodide and npm dependencies. Build time
preloads the interpreter and micropip wheel; execution uses cached-only/frozen
resolution with network, subprocess and FFI permissions denied. The fixed Rust
launcher clears inherited environment variables and copies immutable base wheels
into job-local writable storage. Additional package preparation remains subject
to an explicit future egress/profile policy; this image does not promise arbitrary
online pip/npm installs.

Use the resulting local immutable image ID with
`scripts/runtime/probe_code_adapters_container.py --image sha256:...` for Linux
container checks. These include all three languages, failure reporting, denied
network/subprocess calls and timeout termination. Tests create only fresh named
containers with read-only fixture mounts, UID 10001, 512 MiB memory/swap ceiling,
one CPU, 64 PIDs, read-only root, dropped capabilities and no network. They clean
up their containers afterward. This does not deploy or enable product Code nodes.

## Rust

The `rust-runtime` image target contains the pinned Rust toolchain, fixed Cargo
project and vendored dependencies. User source supplies this module function:

```rust
pub fn run(state: serde_json::Value)
    -> Result<serde_json::Value, Box<dyn std::error::Error>>
{
    Ok(serde_json::json!({"count": state["count"].as_i64().unwrap() + 1}))
}
```

The image-owned adapter stages only that module and selected input, compiles with
`cargo build --locked --offline -j 2`, and runs the resulting binary. Compilation,
build scripts and execution all consume the same container resources and runner
deadline. Compiler/program output streams to stderr. A bounded result file is
parsed before the adapter emits the standard result envelope on stdout. A result
file is untrusted input, not authority to modify graph state.

The initial dependency profile contains pinned serde_json only. Arbitrary Cargo
manifest changes and downloads are not enabled. Additional dependencies require
a separately prepared immutable runtime profile. The immutable source cache is
shared through image layers; compiled outputs are job-local and are currently
rebuilt for each job. Cross-job compiled-artifact caching is not implemented.

Rust needs an executable job-local workspace for build scripts and binaries;
Deno jobs do not. The container probe selects this explicitly with `--runtime
rust`. The Docker supervisor now requires an explicit Rust-only compilation profile
for this permission. Service configuration and worker routing to profiles remain open.
The root filesystem remains read-only, UID remains non-root, network is disabled,
and CPU/memory/PID/time limits cover both compilation and execution.


## Approved Python package profiles

`python-packages.json` is an operator-owned build input. The default list is empty.
Use exact requirements, such as `python-slugify==8.0.4`, to prepare an approved image.
Preparation rejects requirement URLs, version ranges, and environment markers.
It freezes transitive versions and caches their verified wheels in the image.
The immutable image digest binds this package set to sandbox dispatch and recovery.
Runtime code can use ordinary imports or `await micropip.install(...)` offline.
Unprepared packages and incompatible versions fail without enabling network access.

The package tests use a separate profile with `idna==3.10` and `python-slugify==8.0.4`.
Run `probe_code_adapters_container.py --image sha256:... --python-packages` against that image.
The check includes a transitive dependency, an import alias, and an unavailable version.
On-demand preparation and Cargo package expansion remain separate work.

## Frozen npm dependency profiles

`javascript-packages.json` supplies operator-owned image requirements. Its default list remains empty.
Use exact references, such as `npm:csv-parse@5.6.0`, including scoped names when needed.
`prepare_javascript.mjs` rejects ranges, tags, URLs, local paths, and more than 128 references.
It generates a module graph. Preparation does not execute the dependency modules.
The image build uses native Deno cache resolution and records transitive versions and integrity values in the image lockfile.
Preparation does not enable npm lifecycle scripts or native addons.
Only the image build can update this lockfile. Runtime execution uses frozen, cached-only resolution.
The launcher disables project configuration discovery and local `node_modules` generation.
Network, subprocess, and FFI permissions remain denied.
Changing package content changes the admitted runtime image identity.
This does not implement package acquisition during a Code request.

The optional test profile contains `npm:strip-ansi@7.1.0`, `npm:slugify@1.6.6`, and `npm:csv-parse@5.6.0`.
It verifies a transitive dependency, CommonJS interoperability, and a package export subpath.
Run `probe_code_adapters_container.py --image sha256:... --python-packages --javascript-packages` against the combined test image.
The thirteen checks include both languages and rejection of an unprepared npm version.
Native preparation uses build-system network and resource policy. Per-download acquisition limits remain future work.
