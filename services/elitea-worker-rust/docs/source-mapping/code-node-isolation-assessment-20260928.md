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

## Exact upstream inspection

The inspected ADK revision is `3b946c1949b28545f992c631ad167441a36e4e66`.
This inspection does not change the worker dependency lockfile.

| Source | Verified behavior | Required integration work |
| --- | --- | --- |
| [`adk-code/src/rust_executor.rs`](https://github.com/zavora-ai/adk-rust/blob/3b946c1949b28545f992c631ad167441a36e4e66/adk-code/src/rust_executor.rs) | Checks and builds Rust before sandbox execution. Check and build launch `rustc` with `tokio::process::Command`. | Isolate compilation as well as the resulting program. Bound compiler output and terminate descendant processes. |
| [`adk-code/src/container.rs`](https://github.com/zavora-ai/adk-rust/blob/3b946c1949b28545f992c631ad167441a36e4e66/adk-code/src/container.rs) | Provides Python and Node images, package setup, and container execution. Inspected `DockerConfig` lacks CPU, memory, and process limits. | Enforce these limits before setup starts. Do not accept the default container configuration as sufficient isolation. |
| Same container source | Package helpers compose shell setup commands. | Validate dependency declarations. Keep credentials outside command text and diagnostic output. |
| [`adk-code/README.md`](https://github.com/zavora-ai/adk-rust/blob/3b946c1949b28545f992c631ad167441a36e4e66/adk-code/README.md) | Describes typed executor and policy contracts. Its WASM guest executor remains a placeholder. | Reuse established contracts where compatible. Verify each backend implementation before advertising its capability. |
| [Monty limitations](https://pydantic.dev/docs/monty/limitations/) | Monty does not support third-party Python packages. | Do not select Monty as the only Python backend for package-capable Code nodes. |

The Rust graph compiler currently dispatches supported node types in `src/agents/graph/compiler.rs::parse_pipeline_node`.
The inspected dispatch has no Code-node branch.
The new implementation must map the SDK input selection and output mapping behavior at this boundary.
No existing Code-node implementation is implied by this assessment.

## Resource enforcement candidate

[bwrapbox](https://github.com/edubart/bwrapbox) matches the requested bubblewrap wrapper with cgroup v2 limits.
Its README describes memory, CPU time, elapsed time, and process-count controls.
The separate [BubbleBox project](https://github.com/RalfJung/bubblebox) targets application sandbox profiles.
Do not treat these project names as interchangeable.

Evaluate bwrapbox as an existing implementation before writing equivalent process supervision.
Verify controller delegation, process-tree termination, exit classification, maintenance, and licensing before dependency selection.
Reject sandbox admission when required limits cannot be installed or verified.
Do not silently downgrade to unrestricted execution inside Docker or Kubernetes.
Keep CPU rate limits, total CPU time, and wall-clock deadlines distinct.
Keep aggregate writable-storage limits separate from per-file limits.

Prefer an implementation of the existing ADK sandbox trait over a replacement trait or parallel execution framework.
Verify that dependency preparation and Rust compilation use this implementation, not only final program execution.
Package the launcher and required runtimes in a dedicated sandbox image with pinned versions.
Keep cgroup administration in the trusted supervisor. Do not grant that authority to executed code.
Verify deployment permissions on Linux before enabling the capability in the worker catalog.

## Proposed lifecycle and ownership

The Rust worker owns graph state, authorization, invocation identity, and result validation.
A separately bounded sandbox owns language processes, dependency setup, compilation, and temporary files.
Python package support remains required. Rust-only execution does not close this compatibility gap.
JavaScript support requires an explicit runtime and package contract.

1. Validate the immutable node definition, input selection, dependency declarations, and authorized execution policy.
2. Persist invocation intent using the existing execution durability mechanism.
3. Admit the sandbox against project and host capacity before allocating its workspace.
4. Install dependencies inside enforced resource limits, with authorized registry access.
5. Compile when necessary, within the same isolation boundary and a bounded preparation deadline.
6. Execute with a separate deadline, bounded output, and the permitted network policy.
7. Validate output variables and artifact ownership before publishing graph state.
8. Persist the terminal receipt, then remove temporary resources through an idempotent cleanup operation.

Dependency installation is executable work. Python build hooks, Node install scripts, and Rust build scripts require isolation.
Use resolved dependency versions and a runtime image digest for reproducible environments.
Cache immutable dependency content by dependency digest, runtime, architecture, and policy.
Scope private dependencies to their authorized tenant or project.
Never share writable execution environments between unrelated invocations.
Keep package acquisition access separate from execution network access.

## Recovery and acceptance boundaries

Worker recovery must inspect the existing invocation before launching another sandbox.
Completed receipts permit result recovery without repeated execution.
Lost contact with effectful code produces an explicit uncertain outcome unless the effect supports deduplication.
A failed Code node must not publish partially validated output variables to downstream nodes.
Future node resilience controls must not retry authorization rejection automatically.

The first backend proof must install a real library and execute code that imports it.
Run resource-exhaustion tests during installation, compilation, and execution.
Verify limits across all descendants, not only the initial process.
Verify worker restart, supervisor restart, cleanup, and durable result recovery separately.
Measure admission and resource use under concurrent execution before claiming platform capacity.
This document records the implementation assessment. Backend selection and runtime acceptance remain open.

## Non-root deployment inspection

Read-only Docker inspection on 2026-09-28 confirms the rehearsal worker runs as `10001:10001`.
It uses a private cgroup namespace, without privileged mode or added capabilities.
Docker reports `Memory=0` and `NanoCpus=0` for this container.
These values mean no explicit limits at this container boundary. They do not describe host or ancestor limits.
This deployment does not prove per-Code-node resource enforcement.

The worker `Containerfile` uses a distroless runtime and sets `USER 10001:10001`.
The Helm worker deployment takes its UID and GID from worker values.
Do not add compilers or package managers to this orchestration image as an incidental sandbox change.

The upstream [`SandboxBackend`](https://github.com/zavora-ai/adk-rust/blob/main/adk-sandbox/src/backend.rs) inspection on 2026-09-28 confirms three methods:
`name`, `capabilities`, and asynchronous `execute`.
The trait has no start, stop, or recovery methods.
Its enforcement flags cover timeout, memory, network, filesystem reads and writes, and environment isolation.
They do not describe CPU, process-count, or storage quotas.
The platform must verify these additional requirements independently of the upstream capability flags.
Pin the selected interface revision before implementation.

Two deployment paths require separate proof:

- A trusted non-root supervisor receives a delegated cgroup v2 subtree. Executed code cannot access that subtree.
- An external container runtime enforces per-job resources when subtree delegation is unavailable.

Container UID configuration alone does not establish either path.
Do not mount the host cgroup tree writable into the general worker or user-code environment.
Do not enable a fallback that discards required resource limits.
The initial runtime probe must verify controllers, namespace support, enforcement, and descendant cleanup on the actual deployment target.
