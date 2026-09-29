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
to the code. Otherwise missing imports are prepared through micropip; import names differing
from distribution names need an explicit installation before the corresponding
import. Package availability and egress are deployment policy. Network remains
disabled in the default Code container. Download authorization and a package
proxy are not implemented by this adapter.

For the offline compatibility tests, first prepare the cache using the existing
`scripts/runtime/probe_deno_pyodide.mjs` workflow. From the repository root:

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
Scoped platform-client access, the Rust adapter, graph binding and browser
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
