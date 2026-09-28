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

## Platform client and credential compatibility

The SDK revision `966526e` supplies more than state transformation inside Code nodes.
`elitea_sdk/runtime/tools/sandbox.py::_prepare_pyodide_input` injects the simplified platform client and token or session authentication.
`elitea_sdk/runtime/clients/sandbox_client.py::SandboxClient` exposes these behaviors:

| Current client operation | Required target behavior |
| --- | --- |
| `unsecret` | Read an authorized secret in the execution project. Keep plaintext outside durable execution metadata. |
| `get_private_project_secret` | Resolve the executing user's personal project. Enforce external-sharing permission. Never substitute a same-named project secret. |
| `get_mcp_toolkits`, `mcp_tool_call` | Preserve authorized discovery and calls through existing tool admission. Retain toolkit identity and guard decisions. |
| Application and version reads | Preserve project visibility and exact version selection. |
| Artifact and bucket operations | Preserve scoped reads and writes through existing artifact services. Record effect outcomes for recovery. |

Personal-secret failures distinguish missing personal project, missing secret, and denied external access.
An explicit caller default is separate from fallback to another secret scope.
Unattended execution must not invent a human identity to resolve a personal secret.
The target Code-node client is not implemented yet. These rows define required compatibility, not completed source mappings.

The source also contains direct HTTP calls with disabled TLS verification.
Do not preserve that transport behavior. Use the platform's verified transport and execution authorization contracts.
Preserve legitimate secret access without copying a broad user token into generated source.
Keep any sandbox credential scoped to the admitted execution and unavailable during dependency installation.
Inspect existing grants before adding another credential mechanism.

`tools/function.py::_build_client_preamble` uses a token placeholder for exported debug code.
Preserve this separation between executable authentication and debug artifacts.
Do not put live credentials in generated source artifacts, package caches, compiler diagnostics, or persisted graph state.
Authorized code can use a returned secret. Redaction alone cannot make arbitrary code unable to disclose a secret it receives.
Apply the granted network and artifact policy to that execution, and document this trust boundary explicitly.

Code-node acceptance must include two users resolving distinct personal values, denied sharing, and no cross-project fallback.
Also test artifact writes and tool calls across worker loss before enabling automatic retries for effectful code.

## Graph state access

`tools/function.py::_prepare_pyodide_input` copies state before constructing `elitea_state`.
It removes `messages` and applies the configured input-variable selection.
An absent selection, an empty selection, or a selection containing only `messages` supplies all remaining state.
The legacy alias `alita_state` receives another copy.
`_handle_pyodide_output` maps declared output variables and can merge structured results into graph state.
This is input and output transformation, not evidence that sandbox code directly owns the checkpoint database.

The target must separate user graph variables from runtime control metadata before serialization.
Expose selected data values, not checkpoint handles, grants, claims, resume tokens, or internal execution fields.
Validate returned keys, types, size, and declared destinations before committing outputs atomically.
Reject attempts to write reserved runtime fields, including through structured-result merging.
Input copying prevents shared-memory mutation. It does not protect secrets already included in the copied data.
Legacy all-variable selection requires explicit compatibility handling and reserved-field filtering.
Keep code-side changes local until successful output validation. Failure must leave the prior graph state intact.

Acceptance must cover undeclared output keys, reserved keys, nested oversized values, failed execution, and exact selected-input behavior.
Verify that authorized user variables survive execution while internal runtime state remains inaccessible.

Use one language-neutral JSON input and result contract for Python, JavaScript, and Rust.
Language adapters expose dictionaries, objects, or JSON values without exposing the supervisor's memory or checkpoint client.
Validate every result in the worker, regardless of the language adapter's checks.
Use explicit artifact references for binary data. Define numeric precision before exposing large integers to JavaScript.
Language-specific platform clients must use the same execution authority and operation contracts.
No language bypasses preparation limits, execution limits, output validation, or effect recovery rules.

## YAML compatibility decision

Keep existing Code-node YAML valid. An omitted `language` means Python.
Add an optional `language` field for the editor dropdown: `python`, `javascript`, or `rust`.
Keep `code`, `input`, `output`, `structured_output`, `debug`, and existing graph transition fields unchanged.
Do not require users to convert existing definitions to a new request envelope.
Keep deployment limits and resource enforcement outside node YAML unless a specific user control requires them.
Language support must remain capability-gated until its backend passes execution acceptance.

The editor now supplies `CodeLanguageSelect` beside the existing code mapping.
It displays Python for omitted language without writing a default into legacy YAML.
Selecting JavaScript or Rust updates only the language field.
Focused Code-node tests pass: 14 tests, with the pre-existing Debug-toggle case still marked as an expected failure.
TypeScript validation passes.
A fresh headed browser verifies the legacy Python default and the Rust selection in the real development editor.
The YAML view retains source, inputs, outputs, and transition after selection.
The test changes an unsaved fixture only. It does not save or execute the unsupported Code node.
The existing admission controls still block save and execution for this node family.
This verifies editor behavior, not backend language support.

The current SDK constructs these nodes in `runtime/langchain/langraph_agent.py`, in the `node_type == 'code'` branch.
It maps `code` through `FunctionTool.input_mapping`, defaults `input` to `['messages']`, and supplies the platform client.
It disables the sandbox tool's sensitive-action middleware for saved Code nodes.
Model-generated sandbox calls use a separate guarded path.
Preserve this distinction without bypassing downstream authorization or private-secret permissions.
Do not assume all saved nodes contain fixed source: the existing `code` mapping can select variable content.
Resolve source provenance before applying any policy exception intended for fixed, editor-authored code.

## bwrapbox dependency assessment

The inspected source revision is `236cca9a29b551335444a1e902012e8b0e55293f`.
GitHub reports no detected license and a last push date of 2024-04-03.
The inspected `bwrapbox.nelua` and generated `bwrapbox.c` contain no license or copyright declaration.
No code from this repository is copied into the product.
Its implementation rejects failed cgroup creation, limit writes, and process migration.
That source inspection does not prove non-root deployment or descendant cleanup on our target.
Resolve reuse permission and runtime acceptance before selecting this dependency.
An OCI runtime with enforced job limits remains an alternative; it does not require copying the wrapper.

## Updated sandbox shortlist

The 2024 date above applies to bwrapbox, not Bubblewrap.
The upstream latest release resolves to [Bubblewrap 0.13.0](https://github.com/containers/bubblewrap/releases/tag/v0.13.0) during this assessment.
Its [versioned command reference](https://github.com/containers/bubblewrap/blob/v0.13.0/bwrap.xml) includes cgroup namespace isolation, but no CPU, memory, or process-count quota controls.
A cgroup namespace does not allocate or enforce these quotas.
[Kernel documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html) requires suitable delegation for unprivileged cgroup management.
An updated Bubblewrap binary therefore does not remove the deployment requirement.

[NsJail](https://github.com/google/nsjail/blob/master/README.md) provides namespace isolation, seccomp, and cgroup resource controls in one existing implementation.
It remains a candidate, not a selected backend.
Its documented privileged Docker example does not prove compatibility with our non-root deployment.
Test delegation, namespace availability, descendant cleanup, and resource enforcement before selection.

Prefer an existing resource supervisor over adding a custom cgroup wrapper.
Continue the bounded OCI execution probe, and assess NsJail where delegated cgroups are available.
Keep Bubblewrap as an isolation option when an external supervisor owns resource limits.
Do not select bwrapbox until reuse permission and deployment support are established.
These choices do not change the Code-node YAML contract or its language selector.

### Products built on Bubblewrap

Bubblewrap explicitly delegates security policy to its caller in its [upstream README](https://github.com/containers/bubblewrap/blob/main/README.md).
The absence of integrated quota controls does not disqualify this composition model.
[Flatpak](https://github.com/flatpak/flatpak) supplies a desktop application runtime and distribution system around sandboxing.
Its desktop scope does not match the worker's job lifecycle directly.
[Anthropic sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime) uses Bubblewrap on Linux with filesystem policies and filtered network proxies.
This is a closer policy-layer candidate for agent code execution.
Its reviewed README does not establish per-job cgroup quotas or durable recovery after supervisor failure.
Do not infer those guarantees from its filesystem and network controls.
Evaluate it with an external resource supervisor before treating it as a complete worker backend.

### Deno and Python package compatibility

The current SDK's `infra/data/sandbox/main.ts` hosts `npm:pyodide@0.29.0` in Deno.
Its `install_imports` function finds missing Python imports and calls `micropip.install` before `runPythonAsync` executes user code.
It also supports explicit `import micropip` and top-level `await micropip.install(...)` within user code.
Package preparation can therefore occur both before and during execution.
The current wrapper reports unavailable packages before user execution when automatic preparation fails.
Preserve this behavior in compatibility tests instead of assuming a native CPython environment is equivalent.

Assess Deno/Pyodide as the Python compatibility backend before replacing it.
Deno can also execute JavaScript, but each language requires its own state and result adapter.
The [Pyodide package documentation](https://pyodide.org/en/stable/usage/loading-packages.html) defines which packages can run in its WebAssembly environment.
Do not promise arbitrary native CPython extension compatibility.
The [Deno permission model](https://docs.deno.com/runtime/fundamentals/security/) restricts I/O; it does not replace the outer job resource supervisor.
Apply resource limits during package installation as well as execution.
Keep package downloads within approved egress and isolate writable package caches between untrusted jobs.
Use preloaded immutable runtime assets to avoid downloading the Python runtime for every invocation.
Do not carry the current SDK's serialized interpreter session into authoritative graph checkpoints.

#### Executed compatibility evidence

`scripts/runtime/probe_deno_pyodide.mjs` exercises the real Pyodide interpreter through Deno.
The local run uses Deno 2.5.4 and Pyodide 0.29.0, matching the current SDK's Pyodide version.
It installs `idna==3.10` through top-level `await micropip.install(...)` and verifies the encoded domain result.
It also verifies literal escapes, 160 KiB source, and Python exception propagation.
These checks pass again with Deno network access denied after the first package download.

The first attempt fails because Pyodide tries to write wheels beside its runtime assets.
Setting `packageCacheDir` to a separate writable directory resolves that failure.
The successful run grants write access only to that wheel directory.
This confirms the need to separate immutable runtime assets from mutable package storage.

Example invocation after preparing a temporary cache directory:

```sh
DENO_DIR=/tmp/elitea-code-cache deno run --no-prompt \
  --allow-read=/tmp/elitea-code-cache,/tmp/elitea-code-wheels \
  --allow-write=/tmp/elitea-code-wheels \
  --allow-net=cdn.jsdelivr.net,pypi.org,files.pythonhosted.org \
  --allow-env=NODE_DEBUG scripts/runtime/probe_deno_pyodide.mjs /tmp/elitea-code-wheels
```

For the offline repeat, add `--cached-only` and replace `--allow-net=...` with `--deny-net`.
This macOS compatibility probe does not prove Linux isolation or worker integration.
The large-source test covers the interpreter boundary, not operating-system stdin transport.

#### Pinned ADK integration contract

The worker pins ADK 2.2.0. Inspection of published `adk-sandbox` 2.2.0 confirms `SandboxBackend::execute(ExecRequest)` returns `ExecResult`.
Its language enum already includes Python, JavaScript, TypeScript, and Rust.
The request includes source, optional stdin, timeout, memory limit, and an explicit environment map.
The trait does not own durable lifecycle or restart recovery.
Its capability report has no CPU or process-count enforcement fields.
Reuse this execution interface, but retain explicit deployment checks for those required controls.
Do not substitute the plain process backend when the configured sandbox cannot enforce them.
No ADK dependency or lockfile change is made by this assessment.

## Disposable OCI resource proof

Run the reproducible deployment probe from the repository root:

```sh
python3 scripts/runtime/probe_code_sandbox_limits.py --image python:3.12-slim-bookworm
```

The probe resolves an existing local image to its immutable ID. It does not pull images.
The verified image ID is `sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c`.
Each case uses a separate container with UID/GID 10001, no network, a read-only root, and no capabilities.
The supervisor assigns 64 MiB memory, no additional swap, 0.25 CPU, 16 processes, and an 8 MiB temporary filesystem.
No host files, credentials, or runtime socket enter the sandbox.

The Linux container probe passes these checks:

- Python executes a JSON state transformation as UID 10001.
- The sandbox reads the expected cgroup limits and cannot write the cgroup root.
- Process creation fails after 15 child processes.
- Memory exhaustion exits with status 137 and a confirmed runtime OOM flag.
- Consuming 0.75 CPU seconds takes 3.000 wall seconds and records 30 throttled periods.
- The supervisor times out a live workload with a child process and removes its container.
- Every case verifies that its uniquely named container no longer exists after cleanup.

This proves local runtime enforcement, not the production sandbox contract.
The host test harness controls Docker. The Rust worker receives no Docker socket.
The test does not prove rootless runtime deployment, Kubernetes delegation, package installation, or recovery after supervisor failure.
It does not enable Code nodes or verify their platform-client authorization.
Connect the selected supervisor through the ADK execution boundary only after its deployment and lifecycle contracts are defined.
