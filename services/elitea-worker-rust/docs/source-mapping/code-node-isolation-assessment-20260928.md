# Code-node isolation assessment

Status: investigation and proposed direction. No Code-node runtime is enabled by this document.

## Current-platform behavior

The SDK reference revision is `966526e`.
`elitea_sdk/runtime/tools/function.py` prepares selected graph inputs as `elitea_state` and maps declared output variables.
The same file handles debug artifacts and propagates structured tool outcomes.
`runtime/langchain/pyodide_sandbox.py` runs Python through Deno and Pyodide.
It sets a WASM heap limit when the caller supplies `memory_limit_mb`.
`runtime/langchain/remote_sandbox.py` also supports remote execution.
Its `execute` signature accepts a memory limit, but the inspected HTTP request does not forward that field.
These paths provide behavior references. They do not establish the new isolation boundary.

## ADK evidence

The vendored `adk-runner` exposes a sandbox lifecycle under `src/sandbox_runner`.
Its optional `adk-sandbox` dependency names version 2.2.0. The worker lockfile does not currently include that dependency.
The runner manages provisioning, tool binding, snapshots, and cleanup. That wrapper alone does not enforce resource limits.

The [upstream sandbox README](https://github.com/zavora-ai/adk-rust/tree/main/adk-sandbox) describes process and WASM backends.
The default process backend does not enforce memory limits or filesystem and network isolation.
Optional native enforcement adds OS isolation; Linux uses bubblewrap.
The WASM backend describes memory limits and restricted host access, but accepts WASM rather than arbitrary native Python or Node applications.
This upstream overview is not proof of the exact dependency version's implementation.

## Proposed execution boundary

Keep orchestration in the Rust worker. Place language runtimes in separately bounded execution environments.
Do not add Python and Node to the orchestration image merely to launch untrusted child processes.
Reuse an ADK backend or workspace client when its verified behavior satisfies the execution contract.
Inspect exact dependency sources before selecting a backend or adding a dependency.

Enforce memory, CPU, process count, wall time, output size, writable storage, and network policy per execution.
An OS namespace tool does not replace CPU and memory accounting.
Container resource limits apply to the sandbox workload, not only the shared orchestration worker.
Do not expose a host container socket to user code.

The node receives bounded JSON inputs and returns typed output variables, diagnostics, and authorized artifact references.
Commit graph outputs only after successful execution and output validation.
Failure or rejection stops the pipeline unless an explicit future recovery policy defines another route.
Stable invocation identities and receipts must prevent blind replay of completed external effects after worker loss.
Pure computations and effectful code need distinct retry rules.

## Required implementation proof

- Map current Python state and output behavior to the Rust graph compiler and node implementation.
- Specify JavaScript support explicitly; do not infer it from the Python compatibility path.
- Test CPU loops, memory exhaustion, child processes, output floods, disk exhaustion, and denied network access.
- Test cancellation, deadline expiry, worker loss, orphan cleanup, and completed-result recovery.
- Verify cross-project isolation and scoped artifact access.
- Verify real Code nodes through main chat and periodic ephemeral editor tests.
- Measure concurrent sandbox admission against the platform capacity target.

Bubblewrap, container isolation, WASM, and other backends remain candidates. No backend is selected or accepted yet.
