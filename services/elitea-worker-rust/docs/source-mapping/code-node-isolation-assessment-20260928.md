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

## JavaScript, TypeScript, and Rust package proof

`scripts/runtime/probe_code_packages.py` checks trusted fixtures with actual language runtimes and public packages.
It creates disposable source files. It keeps dependency and compiler caches outside the repository.
Run preparation once, then repeat with cached packages:

```sh
python3 scripts/runtime/probe_code_packages.py --cache /tmp/elitea-code-package-cache --prepare
python3 scripts/runtime/probe_code_packages.py --cache /tmp/elitea-code-package-cache
```

Both runs pass locally on 2026-09-28 with Deno 2.5.4 and Cargo/Rust 1.97.1.

- JavaScript imports `npm:semver@7.7.2` and produces the expected JSON result.
- TypeScript executes a type-annotated version through the same Deno runtime.
- Rust uses a normal Cargo manifest with `serde_json = "=1.0.151"`.
- Cargo generates a lockfile, compiles, and executes the Rust fixture.
- A second Rust execution uses `--frozen` and produces the same result.
- The cached probe uses Deno `--cached-only` and Cargo `--offline`.

These results prove package compatibility on macOS. They do not prove Linux isolation or worker integration.
The fixture's two compiler jobs limit local test pressure. They do not constitute production CPU enforcement.
The probe owns its subprocess groups and terminates them after a timeout.

### Proposed package contract

Use standard Deno package imports for JavaScript and TypeScript.
The [Deno package documentation](https://docs.deno.com/runtime/fundamentals/node/) describes npm imports and compatibility limits.
Native addons and package lifecycle scripts need separate policy and image support.
Do not enable them implicitly for every Code node.

Deno can fetch static imports despite `--deny-net`. This flag does not isolate package acquisition.
Resolve packages under the preparation egress policy. Execute the resolved package graph with acquisition disabled.
Retain the resolved dependency identity for recovery. Do not treat an import's version range as immutable execution intent.

Use stable Cargo manifests for the first Rust adapter.
The inspected ADK Rust executor invokes `rustc` directly and does not supply general Cargo dependency resolution.
Run dependency preparation, build scripts, procedural macros, compilation, and execution inside the outer sandbox.
Store generated source files and build outputs in its private filesystem.
Record the lockfile and toolchain identity before an effectful execution.

[Cargo single-file scripts](https://doc.rust-lang.org/cargo/reference/unstable.html#script) remain documented as an unstable feature at this assessment.
They use `cargo +nightly -Zscript file.rs`, not ordinary `cargo run file.rs`.
Do not add a nightly requirement solely to hide manifest preparation.
The UI can retain one code editor while the adapter prepares the required files.
The user-facing Rust dependency declaration remains open; no custom inline manifest parser is introduced.

Python retains the verified Pyodide and inline `micropip` behavior described above.
All languages require the same outer resource, authority, result-validation, and recovery contracts.
No production adapter or Code capability is enabled by these probes.

## Rust user-state boundary

`src/agents/graph/code_state.rs` implements the first language-neutral data boundary.
It is not connected to sandbox execution or pipeline admission yet.

| Current SDK behavior | Rust implementation |
| --- | --- |
| `_prepare_pyodide_input` selects input variables and removes messages. | `CodeStateBoundary::input_json` serializes selected declared user values. |
| Empty input or only `messages` selects remaining state. | The same selection exports declared user state and the user input channel. |
| Runtime state can accompany legacy all-variable selection. | A declaration allowlist excludes undeclared runtime fields, including future metadata. |
| `_handle_pyodide_output` merges structured results. | `validate_updates` accepts only declared user destinations with matching types. |
| Declared outputs constrain ordinary result mapping. | Non-structured patches must target the configured output selection. |

The boundary reuses the compiler's reserved-field and normalized-type checks.
It borrows input values during serialization instead of copying the checkpoint.
The input writer stops at 512 KiB, including JSON escaping.
Result parsing checks the byte bound first and retains the JSON parser's recursion limit.
Validation returns one complete patch. An invalid field returns an error without a partial patch or checkpoint mutation.
Errors contain no source values, identifiers, or raw parser diagnostics.

Assistant messages and built-in result channels require separate trusted projection.
The legacy result envelope, source mapping, platform client, and sandbox adapter remain unfinished.
The graph compiler continues to reject Code execution until those contracts are connected and verified.

Verification: `cargo test --locked --lib agents::graph:: -j 2` passes all 92 graph tests, including 11 Code-state tests.
These are component tests. They do not prove sandbox execution, deployed UI behavior, or worker recovery.

## Optional systemd resource supervisor

The user supplied a `systemd-run --user --scope` wrapper around Bubblewrap.
This is a valid composition to assess on hosts with an available systemd user manager and delegated controllers.
The wrapper creates a transient scope. Systemd supplies cgroup resource controls; Bubblewrap supplies its configured isolation.
`CPUQuota=50%` means half of one CPU, not half of all host CPUs.
`MemoryMax` sets the memory boundary. Swap and task-count limits require their own settings.
The [upstream resource-control documentation](https://github.com/systemd/systemd/blob/main/man/systemd.resource-control.xml) describes these controls and delegation requirements.
The [systemd-run documentation](https://github.com/systemd/systemd/blob/main/man/systemd-run.xml) describes transient scopes and user-manager selection.

This option does not require adding systemd to the Rust worker image.
Do not assume that a non-root application container provides a user manager or delegated controllers.
Select this supervisor only when the deployment supports it and the enforcement probes pass.
The ADK adapter still needs execution identity, cancellation, cleanup, diagnostics, and durable terminal-result handling.
No systemd service, host configuration, or sandbox dependency is installed by this assessment.

## Existing Pyodide resource controls

Source inspection confirms existing SDK protections at revision `966526e`.
Do not describe the current Python runner as unlimited.

| Source | Existing control | Scope |
| --- | --- | --- |
| `runtime/tools/sandbox.py::_read_sandbox_limits_from_env` | Default timeout: 55 seconds. Default WASM memory cap: 512 MiB. | Configured invocation limits. |
| `runtime/langchain/pyodide_sandbox.py::_build_command` | Converts MiB into WASM pages for `--wasm-max-mem-pages`. | WASM linear memory, not total Deno process memory. |
| Async and synchronous execution methods | Timeout handling terminates the subprocess. | Wall-clock duration, not a CPU bandwidth quota. |
| `runtime/tools/sandbox.py` admission checks | Default concurrent-execution threshold: 16. | Process-count observation before starting work. |
| `_cgroup_memory_pressure_pct` and admission checks | Default pressure threshold: 85 percent. | Existing container memory pressure; the probe can fail open. |

Configuration can disable the concurrency and pressure gates with zero.
These are source defaults, not verified values in every deployed container.
No per-invocation CPU cgroup quota appears in the inspected Python execution path.
Preserve the useful runtime limits while adding whole-job enforcement for package preparation, host bindings, and native compilation.
WASM isolation does not exempt Python from the outer execution boundary.

Indexer plugin revision `c048daa` confirms the configuration wiring.
`pylon_indexer/plugins/indexer_worker/config.yml` declares the four `SANDBOX_*` limits under `env_vars`.
`module.py` copies configured environment values into `os.environ` during startup.
The SDK reads those values through `_read_sandbox_limits_from_env`.
This verifies the configuration-to-enforcement source path without claiming live container settings were inspected.

## Code YAML admission and source provenance

`src/agents/graph/code.rs` parses the stored node contract before the compiler rejects unavailable sandbox execution.
The parser preserves omitted-language Python behavior, fixed mappings, variable mappings, and the UI's legacy bare-source strings.
It recognizes explicit Python, JavaScript, TypeScript, and Rust values without advertising backend availability.
It retains input, output, structured-output, debug, and transition settings in the configuration digest.
Equivalent explicit and implicit Python settings produce the same digest.
Language changes and source mapping changes produce different digests.

Variable source resolution requires a declared string variable or the user input channel.
Missing, non-string, empty, null-containing, and oversized source values fail before execution.
Resolved source retains its origin: saved literal or state variable.
Admission must not treat a variable's current value as trusted saved source merely because its mapping is saved.
The parser permits up to 256 KiB source within a 512 KiB node document.
It borrows resolved source instead of copying source from checkpoint state.
Malformed-field errors do not echo source or parser payloads.

The compiler still returns an unsupported-capability error for valid Code nodes.
No Code node executes through an unrestricted process fallback.
Backend admission, dependency declarations, trusted result projection, platform-client calls, and runtime verification remain open.

Verification: all 99 graph component tests pass, including seven Code-definition tests and 11 Code-state tests.
The test command is `cargo test --locked --lib agents::graph:: -j 2`.
No deployment or browser execution is claimed for this parser change.

## ADK workspace container backend and supervisor placement

The exact `adk-sandbox` 2.2.0 package also contains `workspace/docker.rs`.
This is separate from the previously inspected `adk-code` executor and the minimal `SandboxBackend` interface.
Its `DockerClient::with_resource_limits` sets container memory and fractional CPU limits.
The `SandboxClient` interface supplies provision, start, stop, snapshot, and resume operations.
Reuse these existing lifecycle concepts before introducing another sandbox framework.

`DockerClient::new` calls Bollard's `connect_with_local_defaults` and pings a Docker daemon.
It requires daemon access. It does not require nested Docker daemons.
Keep daemon access in a dedicated trusted sandbox executor, not the general worker or user-code container.
A Kubernetes deployment must not assume that its nodes expose Docker sockets.
Its supervisor needs a native workload backend with the same execution and result contracts.

The current SDK already demonstrates a remote execution boundary in `runtime/langchain/remote_sandbox.py`.
It delegates Python execution to a sandbox service and classifies admission and transport failures.
Use that separation as a behavior reference, without copying its static-token or serialized-session contracts.

The upstream workspace backend needs these changes before production activation:

- Enforce non-root identity, process limits, swap policy, read-only root, and explicit writable mounts.
- Enforce network policy and keep dependency preparation distinct from user execution.
- Bound captured output before appending it to strings.
- Terminate the workload on timeout; the inspected `exec_command` timeout only stops waiting for its output.
- Clean up containers after partial provisioning failures.
- Preserve invocation-to-container identity outside the client's in-memory session map.
- Make cleanup recoverable when container removal fails after the session-map entry is removed.

CPU and memory support alone does not prove these requirements.
Use separate bounded admission and per-job limits so concurrent pipelines cannot consume unbounded executor resources.
Reuse pinned images and immutable dependency/build artifacts. Do not reuse another invocation's mutable interpreter or workspace.
Measure cold and warm execution independently before claiming performance or platform capacity.

## Required Docker and Kubernetes deployment support

Both deployments are required for Gate 5 acceptance, not alternative future options.
The trusted supervisor uses Docker containers on Docker deployments and native Kubernetes
Jobs on Kubernetes. Neither the worker nor user-code containers receive a runtime socket.
The same durable invocation identity, cancellation, bounded output, and terminal receipt
contract must apply to both backends. Kubernetes automatic retry of user code must be
disabled by default: reconciliation inspects the existing invocation before deciding
whether a new attempt is safe. A missing workload without a terminal receipt is not proof
that its external effects did not happen.

Runtime image preparation is a deployment operation, not a per-invocation build:

- Publish separate digest-pinned language runtime images with their standard dependencies
  already installed. Share base layers where practical; the worker image stays independent.
- Docker preparation pulls and verifies configured digests before the supervisor admits
  that language. Normal job creation uses the local image; missing images return the
  executor to preparation rather than starting unrestricted fallback execution.
- Kubernetes uses explicit `imagePullPolicy: IfNotPresent` with immutable digests.
  An optional pre-pull DaemonSet warms the configured images on sandbox nodes, including
  newly added nodes. Image caches are node-local and subject to eviction; cache misses
  must remain a supported, bounded preparation path with visible status.
- Air-gapped installations may use preloaded images and `Never`, but must verify image
  availability on every eligible node. This is a deployment mode, not the default.
- Keep image/preparation deadlines separate from the user-code execution deadline, with
  an overall job deadline. Distinguish preparation failure from code timeout in results.
- Cache dependency/build artifacts by runtime digest, architecture, and resolved dependency
  identity. Publish artifacts atomically and mount completed shared artifacts read-only;
  mutable environments and output directories remain invocation-local.

Image caching avoids repeated downloads, not container/process startup cost. Warm-pool
complexity is deferred until measurements establish a need. Verify cold pull, warm spawn,
cache eviction, registry outage, new-node admission, and supervisor restart for both
deployment paths before claiming support. These are design requirements; the supervisor,
Kubernetes backend, image preparation, and deployment acceptance remain unimplemented.

References: [Docker pull policy](https://docs.docker.com/reference/cli/docker/container/run/#set-the-pull-policy---pull)
and [Kubernetes images and pull policy](https://kubernetes.io/docs/concepts/containers/images/).


## Initial optional ADK Docker hardening implementation

The worker manifest now patches optional `adk-sandbox = 2.2.0` to the vendored
source, enabled only by `sandbox-supervisor`. Provenance is in `vendor/README.md`.
`vendor/adk-sandbox/src/workspace/docker.rs` implements the explicit offline
non-root policy, combined 1 MiB decoded output cap, stream/input failure handling,
command-error/timeout termination, bounded preparation with attempted cleanup,
and cleanup-handle retention after removal failure. Policy is revalidated at
provisioning because upstream resource fields remain mutable. Code-policy
snapshot/resume is rejected rather than silently losing tmpfs state.

Verification: the vendored crate's 85 library tests pass with
`--no-default-features --features workspace-docker`, including output boundary
and invalid UTF-8 expansion coverage. These are component tests, not Docker
termination/recovery proof. Pending: cancellation-safe ownership, durable job
registry/receipts, lost-acknowledgement reconciliation, Kubernetes backend,
real-container tests, and UI execution acceptance. Code nodes remain gated.

## ADK backend real-container verification

Two opt-in tests in `vendor/adk-sandbox/src/workspace/docker_live_tests.rs` passed
against Docker Desktop's Linux engine using cached immutable Python image
`sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c`.
No image pull was performed.

The tests exercise the Rust ADK backend itself, not equivalent Docker CLI flags:
UID 10001, 64 MiB memory with no extra swap, 0.25 CPU, 128 PIDs, network disabled,
read-only root configuration, and no Docker socket. In-container cgroup reads
confirm the memory and CPU settings. Quoted file paths round-trip literally.
Timeout and combined-output overflow stop the actual container. A 256 MiB
allocation fails with exit 137 under the memory bound. Two concurrent containers
retain different values for the same state filename. All test-owned containers
are removed through the backend after each case, including assertion failures
inside the verification body.

Run with a cached immutable image in `ELITEA_SANDBOX_TEST_IMAGE`:
`cargo test --manifest-path services/elitea-worker-rust/vendor/adk-sandbox/Cargo.toml --no-default-features --features workspace-docker --lib live_tests -- --ignored --nocapture`.

Both tests passed in 13.32 seconds including several isolated container lifecycles.
This is functional local-container evidence, not a throughput benchmark, Kubernetes
verification, cancellation/restart recovery, or deployed UI Code-node acceptance.

## Pinned-image readiness and Kubernetes warming

`DockerClient::check_code_image_ready` inspects only the local image cache,
requires a complete SHA-256 identity, and bounds the daemon lookup to ten seconds.
Code provisioning rechecks readiness and uses the returned immutable local image
ID. It does not perform registry pulls; missing images require deployment
preparation. This closes mutable-tag drift and makes readiness available to the
future supervisor. It is not yet a supervisor health endpoint.

The optional `sandboxImageWarmup` Helm values and
`deploy/helm/elitea/templates/sandbox/image-warmup.yaml` preload digest-pinned
runtime images on explicitly selected sandbox nodes. Each image must provide
`/bin/sleep`. The DaemonSet keeps a minimal non-root idle process in each image;
it is not a pool of reusable user environments. Its pods mount no runtime socket
or service-account token and have deny-all ingress/egress NetworkPolicy.
Registry downloads are performed by the node runtime, outside pod networking.
Use Linux sandbox nodes and the same node selection for eventual execution Jobs.

`deploy/helm/tests/render-sandbox-images.sh` verifies disabled-by-default behavior,
rendered pod isolation, cache policy, node selection, and rejection of mutable
images/missing node selection. It requires Helm, ripgrep, Python and PyYAML.
Rendering passed. Kubernetes scheduling, cache eviction, new-node warming, and
runtime behavior remain unverified.

The vendored crate's 86 component tests pass (two live tests ignored by default).
Both opt-in real Docker tests also pass after the readiness change. No product
database, worker deployment, or UI execution was changed.

## Stable runtime identity for supervisor reconciliation

The existing graph durability boundary remains
`src/state/postgres_checkpointer.rs` and its execution-bound writer authority.
Current SDK `runtime/langchain/remote_sandbox.py` supplies the remote-execution
behavior reference, but its request/session transport is not a durable job ledger.

The ADK extension `vendor/adk-sandbox/src/workspace/docker_code_jobs.rs` adds:
`CodeJobIdentity`, `provision_code_job`, and `observe_code_job`.
The trusted caller supplies a validated opaque job key and request fingerprint;
the supervisor must derive these from its authorized persisted invocation,
including tenant/project scope, activation, code/input, runtime, policy and
dependency revisions. Docker names contain only the opaque key, and labels
bind the request fingerprint. Concurrent creates are arbitrated by Docker's
unique-name constraint.

Observation is bounded and never starts commands or reconstructs an executable
session. Only an actual Docker 404 means the container is absent; transport
failure means unknown. A request fingerprint mismatch is a hard conflict.
Existing names require reconciliation, not reexecution. Named preparation
failure terminates the container and retains its name for reconciliation;
anonymous legacy workspaces retain cleanup-on-failure behavior.

This is runtime identity persistence, not complete supervisor durability.
The caller must retain durable attempt/terminal receipts across container
deletion and daemon/node loss. An absent container does not establish that
code had no effects. Execution claim fencing, cancellation ownership,
receipt persistence, and the supervisor service/API remain open. No graph
checkpoint schema or product database was changed.

Verification: four real Docker tests pass (13.41 seconds). They cover
concurrent same-identity creation with exactly one winner, new-client
observation of the same container, no reexecution, fingerprint conflict,
failed preparation retaining a stopped identity, and the previous limits/
termination/concurrent-state cases. All test-owned workloads were removed.
A worker library check with `sandbox-supervisor` also passed before the final
preparation-termination refinement; the live suite compiled that refinement.
No process-kill/failover service test or Kubernetes recovery claim is made.

## Supervisor receipt ledger in agent-state storage

The existing native graph/session tables were inspected before adding storage.
They bind graph state to checkpoint/session writer authority and have separate
retention semantics; reusing them as a remote sandbox job queue would mix owners
and allow graph-history pruning to erase replay protection. The current SDK
remote-sandbox request/session API supplies no durable job receipt equivalent.

`services/elitea-main/migrations/agentstate/0004_sandbox_jobs.sql` therefore adds
one table in the existing separate agent-state database. It changes no product,
tenant, or legacy public tables. Main's migration corpus owns schema creation;
the optional sandbox supervisor owns the records. The migration is not applied
to rehearsal by this implementation.

`src/sandbox/ledger.rs` supplies a concrete PostgreSQL ledger with:
tenant/project/job scope and immutable request fingerprint; idempotent reservation;
database-timed leases and increasing ownership epochs; a one-time reserved-to-
dispatched transition; and immutable completed/failed/cancelled/uncertain receipts.
Only a current unexpired owner can mutate a job. Reclaiming dispatched work grants
reconciliation only. Terminal receipt retention must cover the parent execution's
replay horizon; there is intentionally no automatic deletion API.

Result JSON is bounded to 512 KiB and validated before persistence. Failure
receipts carry bounded machine codes, not arbitrary error text. Results and
scoped identities have no Debug implementation. Docker identity derives from a
domain-separated tenant/project/job hash so execution-local keys cannot collide
across tenants in the shared runtime namespace. The request fingerprint remains
separate so a changed request cannot reuse an existing runtime job.

This ledger is not yet an authenticated supervisor service. The caller must bind
scope to the execution authority, persist dispatch before code launch, coordinate
leases with runtime observation, and store a verified receipt before cleanup.
Runtime disappearance after dispatch remains uncertain, never automatic replay.
RPC integration, workload reconciliation, bounded admission and Kubernetes remain
open.

Verification helper `scripts/runtime/test_sandbox_ledger.py` starts only a
disposable resource-bounded PostgreSQL container, generates fresh test-only
credentials, runs the explicitly ignored integration test and removes that test
container/volumes. It does not inspect or reuse deployment credentials.

Verified: the isolated PostgreSQL integration test selected and passed one test,
covering concurrent ownership, epoch fencing, single dispatch, request conflict,
tenant isolation, terminal-result recovery and terminal uncertain-state behavior.
Go migration manifest/history checks passed with agent-state head 4. The worker
library compiled with the optional supervisor feature. This remains component
persistence evidence, not end-to-end crash recovery or UI acceptance.

## In-container terminal receipt runner

Connection-attached Docker exec output is not recoverable merely because the
supervisor has a durable database row. The execution process must outlive that
connection and publish a bounded terminal receipt independently.

`src/bin/elitea-code-runner.rs` is the initial container-main-process runner,
built explicitly with `sandbox-runner`. Default worker builds do not include this
binary. It reads a bounded supervisor-written request file, validates argument/
timeout limits, launches the selected runtime with separate captured pipes,
enforces a combined 512 KiB raw output cap and wall-clock deadline, and emits one
revisioned JSON receipt with status, exit code and captured stdout/stderr.
The supervisor, not ordinary user input, must select runtime arguments.
Captured content remains untrusted and needs the Code-state boundary validation
before graph-state projection.

Current-platform reference remains
`elitea-sdk/runtime/langchain/pyodide_sandbox.py` for subprocess timeouts and
output behavior; this is a language-independent outer runner, not a literal
port of the Deno/Pyodide implementation. Python compatibility, dependency
preparation and platform-client bridging still have their separate requirements.

The runner uses a single-thread Tokio runtime. Child pipe output is bounded
before retention; JSON escaping can expand the final receipt, so the eventual
container-log reader must bound its envelope separately (and state receipts still
have the ledger's own 512 KiB validation boundary). CPU, memory, PIDs, network and
filesystem limits are supplied by the admitted container, not this executable.
As the container's main process exits, the container PID namespace must terminate
remaining descendants. That behavior still requires Linux-container verification;
local subprocess tests do not prove it.

Three focused tests verify exit-status/output retention, output-limit handling,
timeout handling, and rejection before launch. Pending: Linux image packaging,
main-process named dispatch, bounded durable-log receipt recovery, supervisor
ownership/heartbeat/cancellation, and both deployment acceptance paths. The
runner is not yet wired to Code-node execution or deployed.

## Linux runner image and named-container receipt recovery

The runner moved from the worker binary directory into the independent
`services/elitea-code-runner` crate. This supersedes the initial
`sandbox-runner` worker feature above. Its locked dependency set contains only
the runner's serde/JSON/Tokio requirements; it imports no worker, database,
Docker client, credentials, or toolkit code. The existing worker CLI/build remains
unchanged. The new Containerfile uses pinned base digests and cargo-auditable.

The initial verification image contains Python plus the runner. It does not
claim Deno/Pyodide compatibility or complete JS/Rust language images. Build:
`docker build -f services/elitea-code-runner/Containerfile -t elitea-code-runner:gate5-receipts services/elitea-code-runner`.
Verified local image identity:
`sha256:c5248d3c1d855d274979059549c2789bb6cbe4e10cfc514134a649616b19ae7d`.

Named ADK Docker jobs now wait for a reserved dispatch marker, then exec the
runner as container PID 1 exactly once. The supervisor writes the request during
preparation and must persist dispatch in the ledger before calling
`dispatch_code_job`. User manifests cannot supply the dispatch marker.
Repeated signals cannot restart an exited container or reexecute its main process.
Preparation still runs inside the resource-limited non-root container.

Named jobs retain one bounded local Docker log file (8 MiB, compression disabled).
`read_code_job_receipt` waits for terminal container state, reads at most 4 MiB
within ten seconds, parses one revisioned receipt and validates its vocabulary/
output bounds. The envelope and user output remain untrusted; graph-state
projection must validate them before mutation. Missing/malformed logs, missing
runtime identity and transport failure require reconciliation. No result is
fabricated and no code is replayed.

`scripts/runtime/probe_code_runner_container.py` passed five Linux PID-1 cases:
success, nonzero exit, output overflow, timeout, and child exit with a background
process holding an inherited output pipe. Each container terminated (no running
PID) and a fresh Docker client read the identical terminal receipt from logs.
The named ADK API test also passed: request provisioning, repeated dispatch,
new-client terminal result recovery and rejection of dispatch after completion.
These tests used fresh test-owned containers and cleaned them up.

Still open: authenticated supervisor/API, database receipt commit and cleanup
ordering, renewal/reconciliation loop, cancellation and service-process crash
tests, Kubernetes Jobs, language/dependency integration and UI acceptance.
Docker logs surviving a client reconnect are not a substitute for the PostgreSQL
receipt when the container is removed or its node is lost.

### Reconnected supervisor termination and cleanup

The ADK Docker extension now exposes `terminate_code_job` and `remove_code_job`
by the same durable job identity used for dispatch and receipt retrieval. These
operations do not require the original client's session map. Both verify the
job and request labels, then act on the immutable container ID from inspection.
Termination confirms that this container stopped or no longer exists. An
unconfirmed result remains an error that requires reconciliation. Cleanup is
non-forced and rejects a running container. Repeated cleanup of an absent
container is safe. Each Docker operation has a bounded timeout.

The supervisor must hold the current ledger lease before termination and must
persist the terminal receipt before removal. These methods do not implement
that ordering by themselves. The current-platform remote sandbox provides the
separate execution boundary described above; this extension adds runtime
identity recovery needed by the new worker's durable job contract.

Implementation: `vendor/adk-sandbox/src/workspace/docker_code_jobs.rs`.
Verification: `live_recovered_client_terminates_and_removes_named_job` used the
cached Linux runner image and a disposable Docker container. It verified new
client recovery, rejection of conflicting identity, refusal to remove a live
job, confirmed termination, and repeated cleanup. The focused live test passed;
86 component tests passed, with six live tests excluded from the component run.
This is not proof of service-process crash recovery, Kubernetes execution, or
Code-node browser execution. Those integration gates remain open.

Library Clippy passed with `workspace-docker` and default features disabled.
The broader all-target Clippy command did not compile: upstream integration
fixtures import `ProcessBackend` without gating on the disabled `process`
feature. No all-target Clippy pass is claimed for this feature combination.

### Docker supervisor receipt reconciliation

`src/sandbox/docker_supervisor.rs` composes the Docker lifecycle and PostgreSQL
receipt ledger for already-dispatched jobs. It does not dispatch or replay code.
Reserved jobs return `NeedsDispatch`; a job with a current owner returns
`OwnedElsewhere`. A semaphore bounds concurrent reconciliation and rejects excess
admission instead of building an unbounded wait queue. Ownership has a 60-second
lease, renewed every 20 seconds in the same future. Dropping the future leaves
the runtime available for recovery after lease expiry; no detached renewal task
survives the request.

A fresh supervisor reads the named container's terminal envelope, saves its
terminal state under the lease fence, then removes the stopped runtime. Failed
cleanup leaves the saved receipt intact and returns an explicit cleanup-pending
flag. A later reconciliation returns the saved result and retries cleanup. The
worker must still validate the result against declared graph outputs before it
updates graph state. This module never writes graph checkpoints.

`JobLedger::age_seconds` uses database time and the existing `created_at` column.
The supervisor's outer 3660-second age bound allows the runner's maximum
3600-second execution plus preparation, and does not reset after reconnect.
After that bound it renews ownership, confirms runtime termination, and writes
a deadline failure. A runtime or database outage leaves the receipt unresolved
for later reconciliation. A malformed or missing runtime receipt never causes
a second execution. No new migration is needed for this age check.

The existing ledger's 512-KiB result bound is enforced before persistence. An
oversized envelope becomes an explicit `sandbox.receipt_limit` failure, not a
truncated JSON result. Runner failure statuses map to separate stable failure
codes. Rich failed-output artifact retention and user-facing messages still
need the execution service contract; this is not the final Code-node UI flow.

Current-platform reference remains the remote-sandbox execution boundary and
`runtime/tools/sandbox.py` result/error interpretation documented above. The new
supervisor adds durable receipt and ownership semantics rather than reproducing
the legacy synchronous HTTP session contract. Authenticated service admission,
initial preparation/dispatch, cancellation, Kubernetes Jobs, dependency/runtime
images, and browser execution remain required integration work.

Verification used `scripts/runtime/test_sandbox_ledger.py --test-filter
sandbox_supervisor_recovers` with a cached runner image. The helper creates its
own PostgreSQL container with generated test-only credentials and removes it
on exit; it does not read deployment credentials. The integration test passed
against real PostgreSQL and Docker. It verified expired-owner recovery, durable
result read-back after runtime removal, repeated terminal reconciliation, stale
owner fencing, and termination/cleanup of a prepared job whose dispatch signal
was lost and whose persisted deadline had expired. The test changes timestamps
only in its isolated database. Two focused receipt-classification tests passed.
This is a component integration test with a new supervisor object and an expired
lease, not a service-process kill test or Kubernetes/browser acceptance.
The worker library also passed Clippy with `sandbox-supervisor` enabled and
warnings denied. This check exposed missing public error-contract documentation
in the optional ledger module; those contracts are now documented. Runtime
identity hex encoding now writes into one allocated string without per-byte
format allocations, preserving the same digest representation.

### Prepared request fingerprint and admission boundary

`src/sandbox/request.rs` adds a typed prepared-job contract. Its domain-separated
SHA-256 fingerprint binds the protocol revision, language, exact source,
normalized input object, immutable runtime image digest, policy revision, and
execution timeout. The job activation key remains stable across reconnects;
changed execution material changes its request fingerprint and therefore
conflicts with a receipt for the original request. Input object insertion order
does not change identity. The constructor sorts nested objects before hashing.
The runtime image and policy are selected by trusted deployment composition,
not by the code or a model-generated command. Runtime wrappers and fixed
dependencies must be covered by those revisions.

Source is limited to 256 KiB. Input has at most 256 selected fields, 64 nested
levels and 100,000 visited values. A capped JSON writer rejects a serialized
request above 1 MiB before allocating an unbounded output buffer. Timeouts must
be 1..3600 seconds and mutable image tags are rejected. Source/input do not have
a Debug implementation. The new constructor derives the content fingerprint
itself rather than accepting a caller-provided digest.

This contract is not an authorization grant. Inspection of Main's
`internal/transport/runtimegrpc/control/server.go::ObserveDesiredState` and
`internal/transport/workloadauth/authorizer.go` confirms that an execution fence
is bound to the original worker's verified mTLS identity and persisted workload
session. A separate supervisor cannot forward that fence as if it were the
worker. The future admission RPC must authorize that delegation explicitly;
no network service or permissive fallback has been enabled by this change.
Current-platform source/input selection remains the `_prepare_pyodide_input`
reference described above; neither the legacy static remote headers nor full
worker credentials are passed to the sandbox.

Four focused request tests passed: changed execution material changes identity;
map insertion order does not; mutable image tags and invalid timeouts are
rejected; oversized/deep input is rejected before fingerprinting. This proves
request construction and replay-conflict inputs, not network admission or
end-to-end Code execution.

### Explicit Main-to-supervisor admission grant

`libs/proto/elitea/runtime/v1/sandbox.proto` now defines job admission and signed
grant claims. `RuntimeControlService.AuthorizeSandboxJob` is additive. Generated
Go and Python bindings were regenerated with the repository's pinned generation
script; Rust includes the new schema through its build script.

Main's `internal/transport/runtimegrpc/control/sandbox_grant.go` reuses verified
workload-session authentication, the active execution fence and desired-state
check, and the existing signed-command verifier. This last check matters:
`fenceDomain` binds execution/claim ownership but does not carry tenant/project
scope. The handler derives tenant/project from Main's verified signed command,
checks the request scope matches it, and accepts only agent execution commands.
The configured supervisor audience must match exactly. An optional issuer in
`ServerConfig` keeps the endpoint disabled by default. No production composition
or deployment is enabled yet.

Grants contain no fence token, code, input, or credentials. Ed25519 signs a
sandbox-specific domain, explicit byte length and the exact protobuf claims
bytes. Lifetime is at most 30 seconds. The grant binds verified submitting
workload identity, supervisor audience, request fingerprint, tenant/project,
execution, activation, and generation. Key and audience configuration are copied
at construction. The sandbox receives neither the grant nor signing material.

`src/protocol/sandbox_grant.rs` verifies with the existing exact-key-ID resolver.
It verifies the signature before parsing and uses the protocol wire scanner to
reject unknown fields and duplicate singular fields. Admission also checks the
verified mTLS peer, local audience, request fingerprint, positive project and
generation, field bounds and expiry. The resulting job scope derives its stable
activation key from execution plus activation identity. Worker generation is
not part of that key, so recovery cannot accidentally create a new job solely
because a lease/generation changed. Tenant/project remain bound in the runtime
identity. A valid grant never bypasses the durable receipt's replay fence.

This is a new delegation contract for the separate supervisor. The legacy
remote-sandbox static-header/session transport is only a business-boundary
reference. Production issuer configuration, the supervisor's authenticated RPC
listener, dispatch and cancellation wiring, and deployed acceptance remain
open. No independent sandbox bearer credential or worker-identity impersonation
fallback was introduced.

Verification: the Main control-package tests passed, including valid signature
and scope bindings plus rejection of mismatched scope/audience, stale fence,
cancelled execution, invalid command signature, non-agent capability and disabled
issuer. Two Rust verifier tests passed for peer/request/audience/time binding,
stable activation identity across generation changes, invalid signatures and
unknown/duplicate signed fields. Worker-library Clippy passed with the optional
supervisor feature. Buf lint and breaking-change comparison against the prior
HEAD passed. These are component tests; a live Main-to-supervisor mTLS exchange
has not yet been implemented or verified.

### Final preparation marker before dispatch

The ADK Docker extension now records `.elitea-ready` only after every manifest
entry has been prepared successfully. The marker contains the exact request
fingerprint. Both lifecycle marker names are reserved against manifest input.
`code_job_prepared` lets a replacement supervisor inspect completion without
repeating preparation. `dispatch_code_job` requires both the runner request file
and the matching final marker before it writes the dispatch signal. A request
file alone was insufficient: it could exist while source/input copying was
still incomplete. A partial or absent marker is not dispatch authority.

This supports the durable reserved-to-dispatched boundary without introducing
a product database schema change. The supervisor still must own the ledger
lease and persist dispatch before signaling. Preparation recovery and the
admission/dispatch service are not complete solely because this marker exists.

Named jobs also use their stable opaque name as the local session-map key.
Removal clears that key, including an already-absent runtime after a lost
cleanup acknowledgement. This prevents the long-lived supervisor from retaining
one local session entry per completed named job. Anonymous ADK sessions retain
their original handle behavior.

The focused Linux Docker receipt test passed with a new client observing the
marker, an incomplete marker rejecting dispatch despite request-file presence,
and successful execution/receipt recovery after the valid marker was restored.
It also rejected manifest attempts to supply lifecycle paths. The cleanup test
passed for a new client removing the runtime followed by the original client
clearing its stale local mapping. The component suite passed 86 tests (six live
tests excluded), and ADK library Clippy passed. No production Code runtime,
Kubernetes execution or browser acceptance is claimed by these checks.
