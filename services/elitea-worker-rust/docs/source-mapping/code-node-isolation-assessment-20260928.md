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


### Authorized submission and fixed image adapter boundary

`src/protocol/sandbox_grant.rs::AuthorizedJob` retains the verified request
fingerprint and expiry. `src/sandbox/docker_supervisor.rs::submit_authorized`
checks both again and binds the prepared image digest and policy to the trusted
supervisor configuration before reserving a ledger row. Admission is bounded.
A claimed lease is renewed throughout preparation, dispatch and observation.
Existing terminal receipts are returned without another execution. Existing
partial preparation is observed, never overwritten; expired incomplete
preparation is terminated before recording failure. Dispatch is recorded before
signaling the existing runtime. Lost dispatch acknowledgement remains a
reconciliation case, not permission to create another workload.

`src/sandbox/request.rs::manifest` maps the bounded source, language and selected
state to `.elitea-code.json`. The fixed runner command invokes the image-owned
`/usr/local/bin/elitea-code-execute`; callers cannot choose executable arguments.
This replaces the legacy SDK sandbox launch boundary described above, while the
worker retains graph state ownership. It does not yet implement the legacy
Python/Pyodide language behavior or platform-client integration.

Verification: the disposable PostgreSQL/Docker test
`sandbox_supervisor_submits_only_the_authorized_request_once` passed. Missing
admission policy and altered request are rejected before ledger insertion; an
authorized request executes, persists its receipt, removes its stopped runtime,
and a repeat returns the identical saved result. The helper obtains the fixture
image identity through Docker image inspection, not commit command output.
`services/elitea-code-runner/tests/fixtures/code-execute.py` is strictly a test
adapter using native Python, not the product Python implementation. Nine focused
worker tests and worker-library Clippy passed; three live tests are intentionally
excluded from the default test invocation (the submission test ran separately).

Docker deployment image preparation and Kubernetes digest-pinned node warming
remain distinct from job execution. Cached layers are reused across isolated
jobs; user workspaces and installed untrusted dependencies must not be reused
between jobs. Kubernetes execution, production language adapters, live service
mTLS admission, cancellation, graph integration and browser acceptance remain
open. These checks do not claim service-process crash or live-cluster proof.


### Recovery of a committed dispatch without its signal (2026-09-29)

`src/sandbox/docker_supervisor.rs::run_owned` now reconciles the boundary where
the ledger dispatch commit succeeded but the supervisor stopped before sending
the Docker signal. After checking for a terminal receipt and the original job
deadline, the new lease owner renews its lease and signals the same immutable
container. The existing ADK `dispatch_code_job` contract consumes the signal
once through container PID 1; it does not restart or create a container. A stopped
container is read for its receipt, and an expired job is terminated before any
recovery signal. This supersedes the earlier limitation that an unsignaled
committed dispatch could only wait until its deadline.

The live disposable PostgreSQL/Docker recovery test passed for both an already
signaled job and a prepared job with committed dispatch but no signal. It also
verified persisted deadline failure and cleanup for an expired job, and rejection
of the previous owner's lease. Worker-library Clippy passed after extracting the
shared receipt persistence operation. This tests replacement ownership at the
specific crash boundary, not a killed production supervisor service or Kubernetes
execution. No current-platform remote-sandbox replay behavior was copied; its
lack of durable job receipts is the business/runtime gap this boundary closes.


### Python image adapter contract (2026-09-29)

Current SDK `infra/data/sandbox/main.ts::runPython` uses Pyodide's last expression
as the result and prepares missing imports. `runtime/tools/function.py::_prepare_pyodide_input`
provides selected state and the legacy `alita_state` copy. The new
`services/elitea-code-runner/adapters/python.mjs::executePython` preserves those
behaviors using pinned Pyodide 0.29.0 and a Deno lockfile. State is passed as JSON
data, never interpolated into executable source. Interpreter sessions are fresh
per job; serialized Python sessions do not enter graph checkpoints.

Prints and package diagnostics stream to stderr under the parent runner's bound.
Stdout contains a single revisioned JSON result envelope. Non-finite/non-JSON and
oversized results fail instead of silently truncating state. The worker must
still validate typed output destinations. The adapter has no platform token or
client injected; scoped platform access remains separate integration work.
Automatic package preparation uses import names; an explicit micropip import
delegates installation ordering and versions to the code, so preprocessing does
not defeat a pinned install. Distributions with different names require explicit
micropip installation. The default network-isolated
container can only use prepared assets until approved package egress is wired.

Four real, offline Pyodide tests passed: state/final-expression/top-level-await
and escaping, exception propagation, inline micropip installation of cached
idna 3.10, and invalid/oversized result rejection. A CLI subprocess check verified
stdout parses as exactly one result envelope while diagnostic/package messages
appear only on stderr. Deno lint passed. These are local interpreter/transport
checks, not an enabled product Code node, Linux image integration, Kubernetes or
browser acceptance. Image assembly and JS/TS/Rust adapters remain open.


### Repository workspace access requirement (2026-09-29)

Code execution against a codebase requires an authorized repository/workspace
reference, not a caller-selected host path. The target container layout is a
fixed `/workspace/repo` plus job-local writable scratch/output. Kubernetes may
use a PVC-backed cache or an explicitly persistent workspace; Docker may use a
supervisor-managed volume or staged copy. A mutable checkout must not be shared
between concurrent untrusted jobs. Default access should use a pinned revision
and read-only source, with an isolated writable checkout when modifications are
required. Changes return as patches/artifacts unless publishing is separately
authorized. HostPath mounts and Docker socket access are not code capabilities.

This requirement is not implemented by the current two-file manifest. Before
adding mounts, extend admission to bind repository identity, immutable revision,
access mode and workspace lifecycle to the request fingerprint. Recovery must
reuse that identity rather than silently checking out a newer branch head.
Persistent workspace ownership, locking/cleanup, size limits and tenant access
must be resolved alongside both deployment backends. Image warming remains
separate from repository preparation and never grants repository access.


### JavaScript/TypeScript adapter contract (2026-09-29)

`services/elitea-code-runner/adapters/javascript.mjs` adds the requested languages
without changing existing Python YAML. Unlike the current Python-only SDK path,
new JS/TS source is a module with a default exported value or function of selected
state. Deno supplies TypeScript transpilation, top-level await and module import
semantics; the container supplies security/resource enforcement. State is cloned
as data and never interpolated into source. Each call stages a uniquely named
module in job-local scratch; diagnostics and structured output use separate
streams. The adapter rejects invalid JSON values and oversized results. Worker
state projection remains mandatory and is not replaced by these checks.

Three local Deno tests passed for JS state isolation/async execution, TypeScript
transpilation, and explicit failures (missing export, nonfinite/undefined/cyclic
values, oversized output and thrown exception). Deno lint passed. The tests use
no network. Runtime-image packaging, imported-package policy, Rust adapter,
worker binding, Kubernetes and browser acceptance remain open.


### Preloaded Deno/Pyodide image and fixed launcher (2026-09-29)

`services/elitea-code-runner/src/execute.rs` now supplies the image-owned
`elitea-code-execute` entrypoint used by `PreparedJob::manifest`. It accepts only
the fixed prepared request path, bounds file input, selects a fixed language
adapter, clears the inherited environment and replaces itself with Deno. Deno
runs cached-only/frozen with no network, subprocess or FFI permission. The outer
PID-1 runner continues to own captured output and the overall deadline. Python
gets its own copy of the image's immutable base wheels; writable package caches
are never shared between jobs. Unsupported languages fail explicitly.

The Containerfile `deno-runtime` target pins Deno 2.5.4 using multi-platform digest
`sha256:1245e9856180be858aebeefdff2c3b98dc01e8ea667b48f4fb64647dc56bb61d`.
The locked npm dependencies and micropip wheel are populated during image build.
Normal job startup requires no interpreter/wheel download. Additional dependency
installation still needs the separate approved egress/profile design. Docker
layer caching and Kubernetes image warming can use this same immutable image.
The old minimal `verification` target remains available for runner-only tests.

Local Linux image `sha256:f8edde63731d794261528fdd4fc4cb77b6d401ebec665f1f200911bd7e36da57`
passed all seven `probe_code_adapters_container.py` cases: Python, JavaScript,
TypeScript, Python exception, denied network, denied subprocess, and timeout.
Tests configured non-root UID, 512 MiB memory/swap ceiling, one CPU, 64 PIDs,
read-only root, dropped capabilities and no network. Five runner/launcher tests
and Clippy also passed. This is actual Linux adapter execution, not the native
Python fixture used in earlier lifecycle tests.

`test_sandbox_ledger.py --test-filter sandbox_supervisor_submits --adapter-image`
uses the prebuilt image unchanged with disposable PostgreSQL. The prepared source
returns a generated marker in its structured result; the test checks persisted
receipt/result content and byte-identical repeat retrieval after cleanup.
Production service mTLS wiring, graph projection, cancellation, Rust execution,
Kubernetes job execution and browser acceptance remain open. No rehearsal
containers or existing product database were changed.

ADK `process.rs::execute_rust` was inspected for reuse: it writes source, compiles
with rustc and executes under the same resource enforcer, including compilation.
Its direct rustc path does not itself provide the requested Cargo dependency and
Elitea state/result contract. Keep compilation inside the outer sandbox when
completing that adapter; do not compile user source on the worker host.


### Offline Rust language image (2026-09-29)

`services/elitea-code-runner/src/rust_execute.rs` and `adapters/rust` now implement
the requested Rust extension to the current Python Code-node behavior. User code
supplies `pub fn run(serde_json::Value) -> Result<serde_json::Value,
Box<dyn std::error::Error>>`. The fixed image-owned Cargo project supplies input
and a bounded result wrapper. The adapter stages selected state separately from
source, compiles with locked offline dependencies and two build jobs, streams
diagnostics to stderr, then validates a bounded result file before emitting the
same envelope used by the other language adapters. Result content is untrusted;
worker typed state projection remains mandatory.

The Containerfile `rust-runtime` target uses the existing pinned Rust 1.97.1
base and vendored serde_json dependencies. This extends the ADK compile-then-run
pattern with the product state/result and dependency contracts; it relies on the
existing outer ADK/Docker resource boundary and PID-1 runner deadline rather than
adding a second resource enforcer. Compilation and user execution both remain
inside the container. No arbitrary manifest, Cargo download or host compiler is
exposed. Additional dependencies need immutable runtime profiles. Compiled
artifacts are currently rebuilt per job; no cross-job compiled cache is claimed.

Live image `sha256:1817e5f46c373a29e7a3830df3216290b451b8bc7d4eb1b1169c41253c5516af`
passed successful structured execution, compile failure, runtime failure and
timeout after the user-code entry marker. Initial verification found two concrete
requirements: compiler temporary files must use job-local writable storage, and
Rust build scripts/binaries require an executable workspace. The Rust probe
explicitly enables that mount option while retaining UID 10001, read-only root,
no network, dropped capabilities and CPU/memory/PID/time limits. Deno workspaces
can remain non-executable. Supervisor profile selection for executable workspace
is still pending; do not globally relax workspace permissions for all languages.
Five runner/launcher tests and Clippy passed. These are Linux image proofs, not
service integration, Kubernetes execution or browser acceptance.


### Supervisor language/resource profile binding (2026-09-29)

The ADK Docker extension now defaults Code workspace mounts explicitly to
`noexec`. `with_code_compilation` opts a trusted runtime into executable scratch
only after finite Code resource policy validation. `/tmp` stays non-executable.
`DockerSupervisor::with_admission_policy` requires an explicit, nonempty,
duplicate-free language list. Rust must use a Rust-only list and the compilation
flag; interpreted lists cannot use executable scratch. Prepared request admission
matches language, immutable image digest and policy revision before reserving
work. Signed content fingerprints already include language and policy revision.
This replaces the previously unbound language selection at this layer.

The real Rust image passed authorized supervisor submission through this profile,
structured-result persistence, cleanup and exact repeat receipt retrieval using
disposable PostgreSQL. Ten focused worker tests passed, including incorrect
language/image/revision rejection; three separately gated live tests are not part
of that component invocation. Worker-library Clippy passed. Production service
configuration and worker routing to these profiles remain open. These changes do
not make a user-supplied runtime flag authoritative or grant filesystem mounts.

The real Pyodide image also passed the same live submission/result-reuse test
with the explicit default `noexec` workspace policy.

### Supervisor certificate identity boundary (2026-09-29)

The current SDK remote sandbox uses configured HTTP headers. It does not supply
an authenticated workload certificate identity or a durable execution grant.
The new supervisor instead uses the platform workload identity contract from
`services/elitea-main/internal/auth/workloadidentity/identity.go`.

`src/sandbox/peer_identity.rs` extracts the leaf certificate from tonic TLS
connection data. Request headers cannot supply this identity. The parser accepts
one DNS SAN or one SPIFFE URI SAN. It rejects missing, mixed, and multiple SANs.
Common Name is never a fallback. DNS identities use lowercase without a final dot.
The optional supervisor feature enables pinned `x509-parser` 0.18.1 for DER parsing.
Existing dependencies retain their locked versions.

The supervisor rejects escaped or URL-normalized SPIFFE forms rather than
changing the bytes used for grant identity comparison. It also rejects other SAN
types instead of ignoring them. These restrictions are stricter than Main's
parser. Deployment certificates must use the shared, unambiguous subset.

Certificate parsing does not prove trust. The service listener must require
client certificates and verify their chain before calling this boundary.
The listener, RPC routing, and live mTLS admission tests remain pending.
Test certificates contain public fixture data only. Their temporary private keys
were discarded. Certificate parser tests do not check TLS trust or expiry.

Three focused tests passed. They cover metadata spoofing, DNS/SPIFFE validation,
real DER fixtures, mixed SANs, missing SANs, email SANs, and trailing DER data.
Worker-library Clippy passed with `sandbox-supervisor` enabled and warnings denied.
No runtime deployment or browser acceptance is claimed for this boundary alone.

### Supervisor RPC and bounded transport (2026-09-29)

`libs/proto/elitea/runtime/v1/sandbox.proto` now defines `SandboxSupervisorService`.
`SubmitSandboxJob` carries the signed grant and bounded prepared-job JSON.
Retries use the same activation and inputs with a fresh Main grant.
A retry reconciles the durable job through `DockerSupervisor::submit_authorized`.
It does not create a replacement activation after an uncertain response.

`src/sandbox/request.rs` decodes transport data through the existing constructor.
It rejects unknown and duplicate top-level fields, unsupported revisions,
invalid runtime fields, and requests larger than one MiB.
Nested state receives the existing shape, size, and normalization checks.
Transport serialization preserves the fingerprint used by the signed grant.

`src/sandbox/service.rs` combines verified TLS identity, grant verification,
and supervisor submission. Its listener requires a server identity and client CA.
It has no plaintext serving method. Request and response decoding limits include
bounded protocol overhead. Connection concurrency and RPC duration are bounded.
The RPC returns pending or a terminal receipt. Output remains untrusted and
requires worker state projection. Cleanup failure does not discard a receipt.
Errors distinguish capacity, conflicting inputs, lost ownership, unavailable
persistence, and invalid receipts. Responses never include raw dependency errors.

This maps the current SDK remote-sandbox request/response boundary to an
authenticated, durable protocol. It does not port remote-sandbox static headers
or session bytes. The prior current-platform source references remain applicable.
Rust server bindings are generated only with the supervisor feature enabled.
Go and Python bindings use the repository generation script.

Production configuration, Main grant issuer composition, worker dispatch,
live mTLS submission, Kubernetes execution, cancellation, and UI acceptance remain open.
The service composition alone does not prove these deployment gates.

Validation: 14 focused sandbox tests passed. The two service tests also passed
after the Clippy ownership correction. Worker-library Clippy passed with warnings denied.
Protocol generation, Buf lint, and Buf compatibility against the preceding commit passed.
The generated Go runtime package compiled; it contains no tests.
These checks do not establish a live TLS handshake or deployed job submission.

### Live supervisor transport verification (2026-09-29)

The supervisor now serves from a bound Tokio listener. Tests reserve an ephemeral
port without a bind/release race. The TLS handshake has a ten-second timeout.
The service calls the existing worker TLS provider initializer before construction.
The first live test exposed a panic from ambiguous Rustls provider features.
Reusing `diagnostics::install_tls_crypto_provider` removes that startup failure.
Initialization and transport failures return typed errors.

`scripts/runtime/test_sandbox_ledger.py --test-filter sandbox_supervisor_mtls
--adapter-image` creates temporary CA/client/server certificates and disposable
PostgreSQL. It uses an existing cached adapter image. It never reads deployment
credentials. Temporary certificate keys and database resources are removed afterward.

The test sends gRPC requests over TLS to the real service. It checks missing
client credentials, a trusted but incorrect workload identity, and modified job inputs.
It then executes code through the Docker supervisor and compares repeated receipts.
This verifies supervisor transport and execution together. Main issues no grant in
this fixture; the test signs its own grant. Production issuer composition, worker
routing, deployment, and mandatory browser acceptance remain required.

The live mTLS test passed with both cached images: Pyodide and compiled Rust.
Each run verified caller rejection, real execution, persisted receipt reuse,
and successful cleanup. Test execution took 2.62 and 2.56 seconds respectively,
excluding infrastructure setup and compilation. These are local measurements.
Worker-library Clippy passed with warnings denied. No rehearsal deployment occurred.

### Main production grant issuer composition (2026-09-29)

`services/elitea-main/internal/runtimecomposition/config.go` now accepts
`ELITEA_RUNTIME_SANDBOX_AUDIENCES`. The comma-separated list contains exact
supervisor audience identities. Empty configuration disables grant issuance.
Validation rejects empty entries, duplicates, wildcards, whitespace, oversized
identities, and lists longer than sixteen entries.

`runtimecomposition/composition.go` creates `SandboxGrantIssuer` with the existing
validated active signing key and injects it into the control server.
No new private key store, database table, or product schema is introduced.
The existing handler still verifies the worker session, active execution fence,
signed command scope, request fingerprint, and configured audience.

Docker Compose exposes the optional environment setting. Helm exposes
`main.runtime.sandboxAudiences`, which defaults to an empty list.
The chart emits exact comma-separated values into Main's configuration.
This completes issuer startup composition, not worker dispatch or deployment.

Focused runtime configuration and sandbox grant-handler tests passed.
Helm rendering passed for disabled defaults and two exact configured audiences.
The standalone supervisor process, worker RPC client, graph integration,
rehearsal deployment, and browser acceptance remain pending.

### Dedicated supervisor process and image target (2026-09-29)

The optional `elitea-sandbox-supervisor` binary now composes the verified service.
`src/sandbox/process.rs` reads a bounded configuration and file-backed secrets.
It reuses the worker secure-file reader, TLS identity validation, public keyring
loader, crypto initialization, and telemetry setup. No worker signing secret or
graph checkpoint ownership moves into this process.

The receipt pool requires certificate-verified PostgreSQL TLS and bounded connections.
Startup checks the receipt table and cached immutable runtime image.
Deployment owns migrations. Startup performs no schema writes or image pulls.
The configured language set selects interpreted or Rust-only compilation policy.
Admission also checks the request timeout against the deployment timeout.
This closes a gap where the prepared request could exceed that profile bound.

The process handles SIGINT and SIGTERM. It drains RPCs for fifteen seconds,
then drops unfinished handlers. Durable leases and runtime identities remain
available for reconciliation. This implementation is not a process-kill recovery proof.

`deploy/runtime/sandbox-supervisor.example.json` documents the required settings.
Replace its illustrative digest and replica identity before deployment.
Private material must satisfy the existing regular-file permission policy.
Copy projected Kubernetes secrets into suitable regular files when required.

The worker Containerfile has a `supervisor` target with the optional feature.
The default target still builds the ordinary worker without Docker access.
The supervisor image needs deployment-owned access to its Docker backend.
Code containers and worker containers must never receive that access.
The Kubernetes execution backend remains separate pending work.

Binary checking and worker-library/binary Clippy passed. Two configuration tests
passed, including invalid resource profiles and the deployment timeout boundary.
The new image target has not been built or deployed in this step.
Worker dispatch wiring and mandatory deployed UI verification remain open.
The final focused sandbox suite passed all sixteen tests after composition refactoring.

### Worker sandbox client boundary (2026-09-29)

The prepared request is now shared with the default worker build.
Docker runtime, receipt database, and supervisor service modules remain feature-gated.
The default dependency graph excludes `adk-sandbox`.

`src/transport/control_grpc.rs` requests sandbox grants with the existing workload
session metadata, producer identity, response bounds, and control deadline.
Authorization rejection remains distinct from transient service unavailability.
`src/sandbox/client.rs` derives the fingerprint and configured audience itself.
It requests a fresh grant before one bounded submission attempt.
The caller owns durable retry policy and must retain the activation identity.
Transport failure never authorizes a replacement activation.

The client returns typed pending, completed, failed, cancelled, or uncertain outcomes.
Completed receipts require a successful runner envelope and bounded JSON.
Code output remains untrusted and still requires graph state projection.
No Docker socket or receipt database is exposed to the worker client.

The live mTLS fixture now uses this client for successful submission and replay.
It still supplies a fixture grant; it does not exercise Main grant composition.
Worker production channel configuration, graph execution, deployment, and browser
acceptance remain pending. This transport is not yet reachable from a Code node.

The default worker library check passed. Its dependency graph excludes the
Docker sandbox crate. The live Pyodide mTLS test passed through `SandboxClient`,
including repeat retrieval of the identical persisted receipt.
This fixture uses a signed test grant, not a running Main issuer.
Default-worker Clippy passed. The final focused suite passed twenty-one tests.
Four infrastructure tests were ignored in that invocation; the mTLS test ran
separately and passed. No deployed UI acceptance is claimed.

### Code receipt to graph-state projection (2026-09-29)

Rechecked the current SDK at `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`:
`elitea_sdk/runtime/tools/function.py::_handle_pyodide_output` maps the adapter's
`result` to selected output variables, produces assistant messages when requested
(or when no outputs are selected), and merges named structured results into state.

`src/agents/graph/code_result.rs::project_code_receipt` implements that boundary
for all four adapters. It accepts only a successful revisioned runner receipt
containing one revisioned adapter JSON envelope. Diagnostic stderr is never state.
Ordinary destinations receive the result value; structured objects and JSON object
strings supply named updates. The existing `CodeStateBoundary` validates every
selected destination and every value before returning any update. Reserved
control fields, message injection, malformed output, and type mismatches fail
atomically. Assistant roles are constructed by trusted Rust code.

Legacy structured list output maps to `result`; the new graph declares this
built-in channel as a string, so this projection writes its JSON text explicitly.
Arbitrary structured objects cannot write that reserved channel. Output fan-out
is bounded before cloning result values. This module is not yet reachable from
a production Code node; admission, execution binding, deployment, and browser
acceptance remain required. Focused verification results are recorded below.

All 23 focused Code definition/state/result tests passed, including oversized
fan-out rejection before cloning. The initial test fixture incorrectly declared
the reserved `result` field; it was corrected to exercise trusted built-in
projection separately from user state. No production gate was relaxed.

The dedicated supervisor container target built successfully from `0b3cca1cb`
(before the worker-only result projection changes), tagged locally
`elitea-sandbox-supervisor:gate5`, image manifest list
`sha256:d280dc971ada74b4ac5218eb655259447f64d6549bd1aa1e24901dcc6b3cf858`.
An isolated, network-disabled/read-only startup smoke test verified UID/GID
10001 and the expected missing-configuration failure. This proves executable
packaging only; it does not prove configured service startup or deployment.
Default-worker Clippy (`--lib -- -D warnings`) passed after the projection change.

### ADK graph binding for admitted Code runtimes (2026-09-29)

`src/agents/graph/compiler.rs` now parses Code definitions, includes their
configuration in the pipeline definition digest, and binds their explicit
transition and terminal output selection. Compilation still fails when its
invocation-owned `CodeSandboxRuntime` is absent. Production does not yet supply
this runtime, so parsing a saved Code definition does not enable execution.

`src/agents/graph/code_runtime.rs::CodeNode` implements the existing ADK `Node`
interface. It selects and validates input before dispatch, preserves fixed versus
state-variable source provenance, awaits the runtime, validates the complete
receipt through `project_code_receipt`, and returns one `NodeOutput`. Runtime or
projection errors return no update and stop graph execution.

The activation digest is domain-separated and length-framed over graph thread,
node ID, durable graph step, and validated Code configuration. Repeating the same
visit gives the same identity; another graph thread, loop step, or configuration
gives a different identity. The sandbox request digest separately binds the actual
source and input. This identity test does not prove restart recovery: the runtime
must still request a Main grant and reconcile the supervisor's existing receipt.
Code is not yet added to the production recovery-frontier allowlist.

Business reference remains SDK `FunctionTool._prepare_pyodide_input` and
`_handle_pyodide_output` at the revision recorded above. ADK owns node scheduling
and checkpoint application; Elitea owns admission and typed state projection.
The compiler/runtime tests use an injected fixture, not a deployed sandbox.
Production channel composition, dynamic-source admission, debug artifacts,
cancellation, deployment and mandatory UI/restart acceptance remain open.

Verification: all 108 graph tests passed. After extracting the Code binding helper
for the compiler lint, all four Code-runtime tests passed again and default-worker
Clippy passed with warnings denied. These include real ADK graph invocation using
a fixture runtime, not a live supervisor or a browser test.

### Claim-bound remote Code submission (2026-09-29)

`src/protocol/sandbox_authority.rs` narrows the existing post-authorization
runtime-context authority into a one-time sandbox binding. It checks the exact
signed-command digest and complete execution identity, retains the original
fence, and zeroizes its copied fence token on drop. Graph code cannot construct
this authority or choose another tenant/project/execution. Each remote submission
also checks the control client's workload and producer before requesting a fresh
Main grant. This does not bypass Main's live claim and desired-state checks.

`execution/agent_preparation.rs` attaches that authority inside the authorized,
cancellation-safe assembly phase. `agents/graph/code_remote.rs` prepares the
immutable source/input request from deployment-selected image/policy settings.
It reconciles pending jobs and transient transport loss under the same activation
and fingerprint with a fresh grant per attempt. Waiting is bounded by the runtime
timeout plus lease-recovery allowance, not an extension of code execution time.
Failed, cancelled, malformed, or uncertain results stop graph execution; none
becomes a successful state update. Dropping the caller drops its request future;
supervisor-side job cancellation remains a separate unfinished integration.

Dynamic state-supplied code remains explicitly rejected by the remote binding
until its approval contract is available. The production assembler still needs
to carry this authority into a configured remote-runtime factory (including
nested pipeline contexts), and startup must create verified mTLS channels.
This implementation alone does not enable Code execution in the deployed UI.

An initial full-library run exposed a debug-test stack overflow after adding the
inline claim binding to nested async frames. The claim payload was moved behind
an owned Box (with the same token-zeroizing drop), and the affected lifecycle
test then passed. A subsequent run completed with 1,270 passes, seven localhost
socket-binding failures under the tool sandbox, and one ignored infrastructure
test; the network-enabled rerun is recorded below.

Network-enabled full default-library verification passed: 1,278 tests, zero
failures, one ignored infrastructure test. The new authority tests cover exact
command/fence preservation, one-time extraction, changed binding/identity
rejection, and another workload being rejected before network submission.
Default-worker Clippy passed with warnings denied after simplifying equivalent
retry match patterns. No deployment or live Main-to-supervisor grant test is
claimed by the library suite.


## Production runtime selection and direct pipeline assembly — 2026-09-29

The current platform selects its local or remote code backend through deployment configuration.
The behavioral reference remains `elitea_sdk/runtime/tools/function.py`, described above.
Pipeline YAML supplies code and state selections, not infrastructure authority.

The Rust mapping now connects these boundaries:

- `src/config.rs` accepts up to four optional `sandbox_runtimes`, with one profile per language.
- Each profile requires a private gRPC target, audience, immutable image digest, policy revision, and bounded timeout.
- `src/bootstrap.rs` creates supervisor channels with the existing private CA and worker certificate.
- `src/agents/graph/code_remote.rs` shares immutable profiles through `CodeRuntimeFactory`.
- `src/agents/runtime.rs` exposes the already admitted sandbox authority to pipeline assembly.
- `src/agents/pipeline.rs` binds that authority before compiling a direct pipeline.
- `src/agents/native_runtime.rs` and `src/execution/production.rs` carry the factory through production composition.

An absent profile list preserves disabled Code execution. Duplicate language profiles fail startup validation.
Model output and graph state cannot select a supervisor, image, audience, or policy.
Each invocation receives its own authority; the shared factory does not retain invocation claims.

This change does not finish nested pipeline binding, supervisor cancellation, or Code checkpoint recovery.
It does not prove deployment, live Main grant issuance, Kubernetes execution, or browser acceptance.
Those gates remain open. The product database schema does not change.

Verification: default-library compilation passes. Seven configuration tests and 108 graph tests pass.
Default-library Clippy passes with warnings denied. These checks do not prove live supervisor execution.


## Saved child pipeline sandbox binding — 2026-09-29

The current SDK code tool executes within the calling graph's selected state.
The Rust implementation keeps that behavior through the existing saved-child admission path.

`runtime.rs` now preserves sandbox authority through ordinary admission and runtime-context redemption.
`ordinary.rs` binds one invocation-scoped Code runtime after admission.
`application_tools.rs` carries that runtime to the saved pipeline tool materializer.
`pipeline.rs` supplies it to both ordinary-parent and pipeline-parent saved children.
`code_remote.rs` shares deployment profiles but binds the authority separately for each invocation.

The existing child project, frozen-version, toolkit-policy, and depth checks still run before binding.
Code activation identity includes the child graph thread, node, step, and definition digest.
Sharing the runtime does not replace those child identities with the parent's graph identity.
The existing one-level saved-pipeline depth restriction remains. This change does not claim arbitrary recursive pipeline support.

The agent library suite passes: 523 tests, zero failures.
These tests provide regression evidence, not live nested sandbox acceptance.
Live Main grants, nested execution, cancellation, crash recovery, and browser acceptance still require deployed verification.

Default-library Clippy also passes with warnings denied.


## Agent tool reuse and recovery investigation — 2026-09-29

The sandbox is shared infrastructure for Code nodes and the future ordinary-agent code tool.
Gate 7a owns that tool, including the main chat agent module. Gate 5 does not close the module requirement.
The tool needs tool-call activation identity, model-generated-source admission, and bounded tool-result projection.
It reuses deployment profiles, Main grants, supervisor receipts, and execution limits.

Recovery inspection finds that `PipelineDefinition::recovery_frontier_supported` still rejects Code nodes.
ADK restores the saved graph step and pending nodes, which preserves Code activation identity.
ADK itself does not save an initial frontier before executing the first graph node.
The direct-pipeline compiler already adds this through `with_initial_checkpoint`; the first investigation missed that wrapper.
The separate child-subgraph path still requires its own entry-frontier audit.
Do not treat a replay allowlist change alone as complete crash recovery.
A focused graph test covers a lost response after a simulated remote effect and before graph state commitment.
It uses a saved frontier and a receipt fixture; it does not simulate a process or database restart.

The focused lost-response test passes. Library and test Clippy pass with warnings denied.
The receipt test helper now borrows its JSON value, removing an unnecessary clone.


## Direct Code checkpoint admission — 2026-09-29

`compiler.rs::with_initial_checkpoint` already persists initialized state before the direct graph calls any node.
The Code recovery allowlist now permits reconciliation through the invocation-bound sandbox runtime.
Unknown node identities and other unsupported frontiers remain rejected.

The new direct-graph test inspects the persisted frontier from inside the sandbox runtime fixture.
It checks execution metadata, step zero, pending Code node, and initial user input before returning a receipt.
Together with the lost-response test, this covers graph initialization and stable receipt reconciliation boundaries.
It does not prove a deployed worker restart or child-subgraph recovery.

Verification passes: 110 graph tests, plus library and test Clippy with warnings denied.


## Child-subgraph initial frontier — 2026-09-29

`compile_subgraph_with_runtime` now inserts the internal `__elitea_subgraph_entry_v1` node before the stored entry point.
This node performs no external operation and changes no state.
ADK checkpoints the initialized child state after this step, before any stored node executes.
Checkpoint-save failure stops execution before the business node can dispatch.
The compiler reserves the internal node name against user node and state declarations.

Existing checkpoints still restore their recorded frontier and step. They do not execute the new entry node.
Fresh child runs spend one graph step on this persistence boundary.
The stored YAML and product database schema remain unchanged.

The lost-response test now starts with an empty checkpoint store.
It drops execution after the simulated remote effect, reconstructs the graph, and verifies the same activation identity.
It also verifies one simulated effect and no dispatch after the terminal checkpoint.
This remains an in-process receipt test; deployed process-loss acceptance remains open.

Verification passes: 525 agent tests, plus library and test Clippy with warnings denied.


## Docker supervisor packaging — 2026-09-29

`deploy/docker-compose.sandbox.yml` composes separate Deno and Rust supervisor services.
Only supervisors mount the Docker socket. They use nonroot UID 10001, dropped capabilities, and a read-only root filesystem.
Their own CPU, memory, and process limits are separate from each sandbox job's limits.
The overlay sets the two Main grant audiences and replaces only the worker runtime configuration mount.
Required material paths fail interpolation when missing; bind mounts cannot create missing source directories.

`deploy/runtime/sandbox-deployment.md` documents certificate identities, file ownership, cached images, and the separate agentstate ledger.
`scripts/runtime/test_sandbox_compose.py` renders the overlay and checks its isolation properties without starting containers.
The render test passes. Material provisioning, merged deployment, live execution, and browser verification remain open.
This Docker packaging does not implement Kubernetes sandbox execution.


## Rehearsal trust provisioning — 2026-09-29

The running rehearsal still uses the earlier worker and Main images. No supervisor is running yet.
Read-only inspection confirms PostgreSQL has TLS disabled; its TLS settings support configuration reload.
The supervisor keeps `verify-full` database verification. Deployment must supply a verified database server certificate.

`deploy/scripts/gen-sandbox-certs.sh` issues Deno, Rust, and PostgreSQL server certificates under the existing runtime CA.
It preserves existing material and checks CA, hostname, expiration, and key pairing before reuse.
Disposable-CA issuance, hostname verification, and rerun preservation tests pass. Shell syntax validation passes.
Rehearsal certificates are prepared in the ignored certificate directory; no secret enters tracked files.
No database setting, service image, or running workload changes during this provisioning step.


## Rehearsal database TLS activation — 2026-09-29

The deployed worker CA matches the local runtime CA byte-for-byte.
The PostgreSQL server certificate verifies against that CA with hostname `postgres`.
The prior PostgreSQL auto configuration is preserved before changing TLS settings.
Server key ownership and permissions are restricted to the PostgreSQL account.
TLS is enabled through configuration reload; PostgreSQL is not restarted.

A live PostgreSQL-protocol handshake negotiates TLS 1.3 and verifies the server hostname and certificate chain.
PostgreSQL reports TLS enabled and nine active client backends immediately after the reload.
The rehearsal Main and PostgreSQL health checks remain healthy, with their existing uptime preserved.
No product tables, runtime identities, or service images change in this step.

The rehearsal agentstate database has a migration ledger but no `sandbox_jobs` table yet.
Apply the existing agentstate migration through the migration runner before starting supervisors.
Supervisor startup, full Main grant execution, and browser acceptance remain open.

## Rehearsal supervisor startup — 2026-09-29

The existing Go migration runner applies agentstate migration `0004_sandbox_jobs` successfully.
The migration ledger records versions 1 through 4. Product tables remain unchanged.
A dedicated supervisor account receives database connection, schema usage, and receipt-table SELECT, INSERT, and UPDATE permissions.
The account has no receipt-table DELETE permission or administrative role attributes.

Separate private Docker volumes hold each supervisor's material, owned by UID 10001 with restricted permissions.
Both supervisors run as UID 10001 with read-only roots, dropped capabilities, and CPU, memory, and process limits.
Only these trusted supervisors receive the Docker socket. Neither service publishes a host port.
Cached Deno and Rust images pass startup validation without pulls.
Both authenticated listeners start and report zero container restarts.
PostgreSQL confirms two TLS 1.3 connections for the dedicated supervisor account.

This validates live supervisor startup and receipt-database connectivity.
Main and worker deployment, authorized Code execution, cancellation, recovery, and browser acceptance remain open.

## Rehearsal Main and worker deployment — 2026-09-29

Both production images build successfully. The worker image retains release line tables and symbols.
Main receives the two supervisor audiences while retaining its existing environment, mounts, networks, and limits.
Main reports healthy after replacement.
The worker receives four language profiles through a dedicated private configuration volume.
The initial startup fails because a nested toolkit-security mount lacks a mount point in the read-only volume.
Creating that mount point permits the existing security-file mount without changing its contents.
The same replacement container then starts successfully and reports `production_startup_admitted` and `production_intake_started`.
The worker reports zero restarts. No Docker socket is added to the worker.

A fresh Chrome tab completes rehearsal sign-in and opens the Private project.
This proves navigation after Main replacement, not sandbox execution acceptance.
Code execution, cancellation, process recovery, and UI result verification remain open.

## Code authoring contract correction — 2026-09-29

Fresh Chrome acceptance creates an isolated Python Code pipeline through the standard creation form.
The saved editor rejects its Code node because `CompilerAdmittedNodeTypes` still excludes `code`.
Rust `agents/graph/compiler.rs::parse_pipeline_node` now admits this type through `CodeNodeDefinition`.
The editor allow-list now includes Code. Unsupported custom nodes remain excluded.
The existing `CodeLanguageSelect` gains TypeScript alongside Python, JavaScript, and Rust.
The legacy Python default and existing YAML fields remain unchanged.

The UI source mapping is `features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts` to the Rust compiler admission arm.
Language selection maps `features/pipelines/ui/nodes/CodeLanguageSelect.tsx` to `CodeLanguage` in `agents/graph/code.rs`.
Three focused UI suites report 68 passing tests and one pre-existing expected failure.
The expected failure tracks the missing Debug artifact-capture toggle, issue 5203; it is not a passing feature check.
The acceptance fixture is saved as pipeline 131. Execution remains unverified while the updated UI image builds.

## Python Code browser execution — 2026-09-29

The updated UI image builds and replaces the rehearsal UI with its existing configuration preserved.
Reloading pipeline 131 removes the obsolete Code-type refusal and displays the Python language selector.
The first test returns null because its source ends with an assignment.
The Pyodide adapter returns the final expression value; the corrected fixture ends with the marker expression.
The pipeline test chat displays `GATE5_CODE_PYTHON_20260929: 42` and reports completion.
The agentstate ledger records two completed receipts, corresponding to these two deliberate runs.
The first execution settles through Main with terminal sequence 9 and `agent_delivery.executed_retired`.

This verifies live Python execution through the editor test chat, worker, authenticated supervisor, and receipt ledger.
It does not prove restart recovery, cancellation, other languages, or main-chat execution.

## Four-language and persistent-chat acceptance — 2026-09-29

The same isolated fixture is edited and saved through the language selector and Code field.
JavaScript returns `GATE5_CODE_JAVASCRIPT_20260929: 42` through the pipeline test chat.
TypeScript declares a `number` variable and returns `GATE5_CODE_TYPESCRIPT_20260929: 42`.
Rust compiles its `run(serde_json::Value)` function and returns `GATE5_CODE_RUST_20260929: 42` through the separate Rust supervisor.
Each test displays completion. The receipt ledger then contains five completed jobs, including the initial Python null-result fixture.

The pipeline's Chat action opens a persistent conversation with the pipeline as a direct participant.
Chat 754 returns the Rust marker and creates the sixth completed receipt.
Reloading that chat preserves the user request and the exact result.
This verifies persistent-chat execution and history reload, not process-restart recovery.

Browser interaction also exposes inconsistent mouse activation of the language selector inside the flow canvas.
Keyboard selection succeeds; an arrow key also moves the selected graph node.
Inspect canvas event propagation before closing editor interaction acceptance.
Cancellation, process recovery, Kubernetes, and the remaining gate requirements stay open.

## Worker interruption during Code execution — 2026-09-29

The Rust fixture sleeps for 35 seconds before returning `GATE5_WORKER_RECOVERY_20260929`.
Persistent chat 755 starts the run through the UI.
The receipt ledger confirms dispatch before an immediate worker restart.
Job `b5a68f1355ad3918ea4620f0e0f115ab9535d68c5cffe431246ac27d22ca62d7` subsequently completes under lease epoch 2.
The worker settles execution `259a50a88cf520368934c7ceb0e063f6` with terminal sequence 10.
The open chat receives the expected marker without manual reload.

This verifies worker interruption and receipt reconciliation through the deployed stack.
Docker returns no creation events for the queried interval, so that history cannot prove dispatch count.
The durable job identity remains unchanged. Separate supervisor crash and cancellation checks remain required.

## Supervisor crash acceptance failure — 2026-09-29

A second slow run in chat 755 confirms dispatch before `SIGKILL` stops the Rust supervisor.
The supervisor restarts while the isolated Code container remains intact.
Container `036e62a32907` exits successfully, but job `fadf48390036f6da1a1b4c8cd2ce3a069c264eabb1295eacea9d88cc44c67749` remains dispatched.
The worker prematurely settles execution `2f7dbb26afacbf6c29bb4b1186e07b53` at terminal sequence 2.
Its error reaches the UI through the generic `agent.legacy` failure classification.

This is a failed acceptance check, not a successful recovery.
Inspect the interrupted RPC status before changing retry classification.
Preserve the original activation and fingerprint during reconciliation; do not rerun code under a replacement identity.
Retain the completed container and receipt row for diagnosis until reconciliation is verified.

## Interrupted supervisor transport classification — 2026-09-29

A localhost TCP proxy drops the connection after the supervisor receives the submission.
Tonic reports `Unknown` with a local Hyper error source, rather than `Unavailable`.
The previous worker retry match treats this transport loss as a terminal graph failure.

`src/sandbox/client.rs` maps local Hyper error chains to `Unavailable`.
Server rejection statuses retain their original codes. No response message or user payload enters this classification.
`src/agents/graph/code_remote.rs` also accepts `Aborted` during bounded reconciliation.
The supervisor uses this status when another lease still owns the same job.
Each retry keeps the activation and request fingerprint and obtains a fresh Main grant.

`src/sandbox/client_disconnect_tests.rs` verifies connection loss after request receipt through real localhost sockets.
The regression fails before the change and passes after the change.
This test proves transport classification, not deployed supervisor recovery.
Repeat the UI crash test after worker deployment before accepting recovery.
This recovery behavior extends the legacy Code execution contract; it does not copy a legacy restart mechanism.

## Deployed supervisor crash recovery — 2026-09-29

Worker image `elitea-worker-rust:sandbox-gate5-a78fe82a7` includes the transport classification fix.
Its release build passes the shipped debug-line and symbol-table checks.
The deployment preserves the worker's environment, five mounts, networks, and resource limits.

Persistent chat 755 starts the slow Rust Code fixture through the browser.
The receipt confirms dispatch before `SIGKILL` stops the Rust supervisor.
The supervisor restarts. The worker keeps waiting instead of publishing a terminal error.
Job `0e435fbaa8ad3096fd63038996d837ca54b2d078cd5e3ee3fe25f4d93c821855` completes under lease epoch 2 at 18:17:43 UTC.
Execution `31108c22032b0e407a60540373080c64` settles with terminal sequence 9.
The open chat displays `GATE5_WORKER_RECOVERY_20260929` without a reload.

This verifies supervisor process recovery with the same durable job identity through the deployed UI.
It does not verify Kubernetes, cancellation, host loss, or simultaneous worker and supervisor loss.
The earlier failed run remains historical evidence; its receipt is not silently changed by this test.

## Stop cancellation acceptance failure — 2026-09-29

Persistent chat 755 starts the 35-second Rust fixture and clicks Stop after confirmed dispatch.
Execution `0c492324152d99d3b4e66a7ae58a1f58` settles at terminal sequence 2 at 18:20:11 UTC.
Container `065c905b1b7b` remains running after Stop and exits normally at 18:20:29 UTC.
Job `78cdbe5c2fbdef093be8f6fc6281321bcdb071210299b2dade0e61e6aa644e45` remains dispatched under lease epoch 1.
This is a failed cancellation acceptance check. Chat cancellation does not yet stop the external sandbox.

### Source evidence and required ownership

`src/execution/native_agent_lifecycle.rs::drive_native_stream` converts authoritative claim cancellation into `native.request_stop()`.
`src/agents/graph/code_runtime.rs::CodeSandboxRuntime` currently exposes execution only.
`src/sandbox/client.rs` has no cancellation operation, and `sandbox.proto` exposes submission only.
`src/sandbox/docker_supervisor.rs::run_owned` intentionally preserves containers when its future drops.
That behavior permits crash recovery and must not become implicit cancellation.

The current SDK reference is `elitea_sdk/runtime/langchain/pyodide_sandbox.py::PyodideSandbox.execute` in `projects/elitea-sdk`.
Its timeout handler kills and waits for the subprocess, but its `CancelledError` handler only passes.
The remote implementation posts an execution request in `elitea_sdk/runtime/langchain/remote_sandbox.py`.
These references define execution behavior, not proof of durable remote cancellation.

Implement an explicit authenticated stop operation with durable intent before terminating a runtime.
Keep stop authority separate from submit authority. Main currently authorizes submission only while desired state is Running.
Do not permit a cancellation grant to dispatch code or bypass project and workload scope.
Do not report sandbox cancellation complete before the runtime stops.
Preserve completed receipts when completion wins the race with cancellation.
Make stop delivery recoverable when either worker or supervisor fails during cancellation.
Cover cancellation before dispatch, during execution, after completion, and after restart.
Use the same contract for Docker and Kubernetes; keep graph checkpoint ownership in the worker.
No product database schema change is justified by this defect.

## Durable cancellation intent foundation — 2026-09-29

Agentstate migration `0005_sandbox_cancellation.sql` adds one Boolean to the existing sandbox receipt table.
The field separates a requested stop from confirmed termination. No product table changes.
`src/sandbox/ledger.rs::request_cancellation` preserves immutable terminal receipts and reserves missing activations before recording stop intent.
The dispatch update rejects stopped activations. Completion cannot replace a stop intent that commits first.
The current lease owner remains responsible for terminating its exact runtime identity.

`src/sandbox/docker_supervisor.rs::stop_if_requested` renews ownership, confirms runtime termination, and then persists the cancelled receipt.
Preparation, execution polling, and recovered reserved activations check durable stop intent.
A restart between intent and termination retains the request for the next owner.
The existing ADK Docker termination method targets the inspected immutable container ID and verifies its stopped state.

The isolated PostgreSQL receipt test passes cancellation-before-dispatch, persisted intent, immutable completion, and concurrent completion-versus-stop checks.
The real-container recovery test passes persisted stop before and after dispatch, with replacement lease ownership and confirmed container removal.
The authenticated cancellation transport and worker stop-delivery integration remain pending.
Do not enable stop delivery until the migration and all supervisor owners support this field.
Do not call this foundation a passing UI cancellation fix.

## Purpose-bound cancellation transport — 2026-09-29

`sandbox.proto` adds `CancelSandboxJob` with signed identity only; cancellation sends no code or state.
Main's grant request adds `cancel_only` outside reserved field numbers.
Stop grants use revision 2 and bind their purpose inside the signed claims.
Revision 1 submission grants retain their existing behavior.

Main accepts a stop-grant request under a valid Running or Cancelled execution fence.
It still checks the verified workload, signed command, tenant, project, capability, and configured supervisor audience.
A stale fence cannot authorize cancellation.
The Rust verifier creates a separate `AuthorizedCancellation` type.
Submission rejects stop grants, and cancellation rejects submission grants.
Expiry and exact mTLS peer checks apply to both operations.

The supervisor records stop intent before checking dispatch capacity.
An active owner observes that intent; an unavailable owner leaves it for lease recovery.
The response reports Pending until runtime termination is confirmed.
This contract does not yet wire the worker's Stop lifecycle or durable delivery after worker loss.
Generated Go and Python contracts use the repository's pinned generator.

Focused Main grant tests and three Rust grant tests pass.
These tests cover purpose separation, cancelled executions, stale fences, peer binding, expiry, and signature validation.
Live cancellation RPC and UI acceptance remain required.

## Cancellation transport mTLS verification — 2026-09-29

`src/sandbox/client.rs` requests stop-only Main grants and calls the cancellation endpoint without code or state.
It validates the returned status and distinguishes durable Pending from a terminal result.
The submission method always requests submission authority, regardless of the caller's input flag.

The isolated `sandbox_supervisor_mtls_submission` test passes with real certificates, PostgreSQL, and the cached Deno/Pyodide runner.
The worker first executes code and reads its saved receipt through mTLS.
The test then rejects cancellation with a submission grant and rejects a different certificate peer.
It also rejects submission with a stop grant.
Authorized cancellation before dispatch returns Cancelled; repeating it returns the same terminal status.
This is component transport evidence, not live Main grant issuance or deployed chat Stop acceptance.
The worker lifecycle and durable pending-stop delivery remain open.


### 2026-09-29: Supervisor-owned stop reconciliation

The current SDK reference remains `elitea_sdk/runtime/langchain/pyodide_sandbox.py::PyodideSandbox.execute`:
local timeout handling kills and waits for its process. It does not establish durable remote cleanup after owner loss.
The new implementation extends the durable supervisor receipt rather than adopting process-local cancellation.

- `sandbox/ledger.rs::request_cancellation` persists the accepting supervisor's stable owner with stop intent.
  A retry cannot redirect existing intent. Completed receipts remain immutable.
- Agentstate migration `0006_sandbox_stop_reconciliation.sql` adds the reconciliation owner and partial pending-stop index.
  Product tables are unchanged. Existing owned stop intents are backfilled; any old intent with no owner needs an authenticated retry.
- `JobLedger::pending_cancellations` selects at most 32 pending stops for this owner, ordered by update time and identity.
  Live leases and terminal records are excluded. Stop-before-dispatch is discoverable even without a dispatch owner.
- `DockerSupervisor::reconcile_cancellations` uses existing fenced termination and receipt handling.
  `SupervisorService::serve` owns a two-second recovery loop alongside the TLS listener; no detached cleanup task is introduced.
  A process replacement with the same owner and runtime daemon can finish a previously authorized stop without a new client RPC or grant.
- Discovery does not grant execution or treat disconnect as cancellation. Worker-side delivery of stop intent remains a separate open integration requirement.

Verification: the disposable PostgreSQL receipt test passes, including owner partitioning, immutable stop routing,
lease fencing, terminal immutability, and the completion/stop race. The real Docker recovery test passes:
a replacement supervisor discovers persisted intent, terminates the existing runtime, persists Cancelled, and removes the container.
The mTLS listener test also passes, including automatic recovery of a pre-existing stop without a cancellation RPC,
wrong-peer rejection, grant-purpose isolation, and repeated-stop idempotency.
Commands: `test_sandbox_ledger.py --test-filter sandbox_receipts_fence`,
`--test-filter sandbox_supervisor_recovers`, and `--test-filter sandbox_supervisor_mtls --adapter-image`.
All use disposable PostgreSQL; the latter two use cached test/runtime images.
This is not deployed UI Stop acceptance and does not close Kubernetes execution.


### 2026-09-29: Worker dispatch identities survive process loss

`graph/code_remote.rs::RemoteCodeRuntime::execute` now commits an execution-scoped dispatch identity before
requesting a Main grant. The scope comes from `ClaimBoundSandboxAuthority`, not YAML or model input.
`sandbox/dispatch.rs::DispatchJournal` stores tenant/project, execution/generation, activation, request digest,
and configured supervisor audience. It does not store source, input state, credentials, grants, or outputs.
Agentstate migration `0007_sandbox_dispatch_journal.sql` owns this worker delivery metadata; supervisor job receipts
remain authoritative for execution results and graph checkpoints remain worker-owned.

A repeated activation must match both content and supervisor identity. Confirmed Completed/Failed/Cancelled
receipts resolve the delivery. Transport loss, timeout, and uncertain completion retain the original identity.
Re-registration cannot reopen resolved delivery. This provides the missing durable lookup needed before root
cancellation settlement, including replacement-worker recovery. It does not itself deliver Stop yet.

Verification: `scripts/runtime/test_sandbox_ledger.py --test-filter sandbox_dispatch_journal` passes against
disposable PostgreSQL. Coverage includes process-instance replacement, idempotent registration, changed digest/target
rejection, exact resolution, tenant/generation isolation, and racing conflicting registrations.
The worker Code path and bootstrap compile with the new journal. Deployment and browser Stop verification remain open.


### 2026-09-29: Stop delivery gates cancellation settlement

Legacy reference: the SDK local Pyodide timeout path kills/waits for its process. Durable remote Stop and
replacement-worker delivery are new runtime requirements; local future cancellation cannot provide them.

`SandboxStopAuthority` is a separate sealed protocol type. Fresh invocation authority validates exact signed
command binding; recovery authority requires the matching cancelled execution and replacement fence.
`AgentControlClient::stop_sandbox_job` verifies local workload/session ownership before requesting Main's stop-only grant.
`CodeRuntimeFactory::stop` reads at most 32 pending dispatch identities, routes to the recorded configured supervisor,
and resolves delivery only for a confirmed Completed/Failed/Cancelled receipt. Missing targets, transport errors,
pending or uncertain status retain delivery. `BoundSandboxStop` bounds a delivery pass to ten seconds.

`CursorBoundAuthorizedAgentRun::finish_terminal` performs this check before emitting cancellation, settlement,
or Redis retirement. `AgentDeliveryProcessor::process_output_recovery` applies the same check before cancelled
running-execution recovery. No runtime connection loss is interpreted as a user stop. This does not retroactively
repair executions already settled by older worker versions without a dispatch journal.

Supervisor Stop uses separate bounded admission capacity. Once authenticated stop intent is durable,
`JobLedger::claim_cancellation` can fence a live Dispatched lease; the previous owner cannot publish completion.
A live Reserved/preparation lease is not preempted because its provision request may still create a container.
Termination remains confirmed before the Cancelled receipt is persisted.

Five focused `durable_stop_` tests pass on the normal test-thread stack, including confirmed-stop ordering and
pending-stop suppression of terminal output, settlement, and Redis acknowledgement. During development the added
unboxed failure-close branch enlarged `execute_owned` enough to overflow the debug stack at terminal binding;
boxing that phase boundary and keeping the stop binding boxed removes the failure without a stack-size override.
The PostgreSQL fencing test passes. A stop replaces a live dispatch lease and fences its previous owner.
A live preparation lease remains protected. A request without durable stop intent cannot claim cancellation.

The authenticated supervisor transport test passes with the real Deno/Pyodide image.
It starts a sleeping Python job, waits for dispatch, and sends a separately signed stop grant.
Cancellation completes within the five-second test deadline. The ledger records Cancelled and the execution container is absent.
The original submit call returns cancellation or a fenced-owner response; it cannot publish success.
The recovery-authority test also passes: only the exact cancelled execution can receive replacement-worker stop authority.

These checks use a disposable PostgreSQL database and real Docker execution containers.
Deployment and browser Stop/restart acceptance remain open. Kubernetes execution remains a separate required gate.

All 67 output-delivery tests pass on the normal test-thread stack.
The broader suite finds a second oversized async phase during terminal-spool reopening.
Boxing the terminal operation fixes this failure without changing thread limits or output behavior.
Supervisor-feature Clippy passes across all targets with warnings denied.
Request preparation and expired-job termination now use small helpers; ordered authority and integration-test lifecycles remain together.

### 2026-09-29: deployed Stop and replacement-worker acceptance

Deploy commit `b9a31a4d0` to Main, the worker, and both supervisors.
Apply agentstate migrations 0005 through 0007 with the migration runner. The product database schema remains unchanged.
Preserve supervisor material volumes, stable owners, cached runtime images, nonroot users, limits, and network aliases.
The old supervisor containers remain stopped as rollback artifacts.

| Component | Verified image digest |
| --- | --- |
| Main | `sha256:15d1b08e6fef2ca5f193064ceb82afacdec680be6c61938ad0e5ce0ff6360efb` |
| Worker | `sha256:e7804c16da6e629015e3d2be8757701e0832920b85833ba8f5b65d7f19b4954d` |
| Both supervisors | `sha256:eb6884198dbe3741617235e4874dd1e3fc5a18c0a9c59b13e7a68b0cf6552bb9` |

Persistent chat 755 starts saved pipeline 131 with its slow Rust Code node.
The browser Stop creates cancellation for execution `b78d2a0f5f05f53f64a4bafda63dadb1`.
Job `644fcbdd55e3b3adcd8f692b01bc21a26b7104b1c4d36d07af294ca10dc84df3` becomes Cancelled at 19:24:21.165783 UTC.
The container is absent and the original dispatch resolves.
Terminal output starts at 19:24:21.212915; settlement and Redis retirement follow. Termination therefore precedes terminal publication.

The second browser run tests execution `c9267dc50e69375701d8a25afa1cd557`.
Pause the Rust supervisor after dispatch, request Stop through chat, restart the worker, then resume the supervisor.
The stopped worker does not publish a terminal event before replacement.
The replacement claim uses lease epoch 2 and the original dispatch journal.
Job `f8279a4e5f5f8e078f99a568294de73ba64e5938821d5f61ae25c287af7fe282` becomes Cancelled at 19:27:05.658881 UTC.
The terminal event follows at 19:27:05.717552; settlement and Redis retirement follow again.
The journal resolves and the original container is absent. No replacement Code execution starts.
This proves pending-stop recovery; it does not prove that execution stops while the supervisor is unavailable.

Browser acceptance exposes a separate presentation gap.
`apps/elitea-web/src/features/chat-messages/model/useChatStreamTransport.ts::stop` detaches before confirming the server outcome.
It settles the local message and ignores Stop request errors. The stopped turn disappears from chat history after reload.
The local screenshot `elitea-sandbox-stop-recovery-20260929.png` records the empty assistant entry.
Keep cancellation-status rendering, retained observation, and durable history verification open. Kubernetes execution also remains open.


### UI Stop observer correction (2026-09-29)

The deployed cancellation checks exposed premature client detachment. `apps/elitea-web/src/features/chat-messages/model/useChatStreamTransport.ts` now keeps observing after Stop admission, reconnects during pending cancellation, deduplicates concurrent Stop requests, and reports request failures with retry guidance. Only the canonical terminal event settles the response. Completion racing with Stop (HTTP 409) leaves the terminal observer in control.

The existing Main policy in `internal/infra/db/repos/agent_cancel.go` and `configuration_validation_results.go` intentionally removes empty cancelled question/answer pairs. Their disappearance after reload is not evidence of lost runtime receipts. No product schema or history-policy change is required. The worker-to-supervisor mapping above remains unchanged.

Validation: 44 focused transport tests pass, including confirmed cancellation, pending-cancellation reconnect, and failed Stop retry. Typechecking and the production image build also pass.

Deployed image: `sha256:cf2428daaa0827ac436a83d76d707a4639c60ec1e9f5ce7d0f59e245e6a6b205`. Browser acceptance passes in persistent chat 755. Stop remains available while cancellation awaits confirmation. The terminal event displays “Execution was cancelled” and releases the composer. Receipt `e3d0ec1f6e25f5ba8c1e637441e35f4e5304fcdf8d4d9c938ecd3614f13ad403` becomes cancelled at `2026-09-29T19:39:26.075030Z`.

### Kubernetes runtime boundary (2026-09-29)

`src/sandbox/runtime.rs` defines the runtime boundary below the durable supervisor.
The Docker adapter delegates to the existing ADK extension without changing dispatch, receipt, or cancellation semantics.
`docker_supervisor.rs` now consumes that boundary. Its existing name remains until the Kubernetes adapter is connected.
The supervisor still owns database leases and receipts. The worker still owns graph checkpoints.

`src/sandbox/kubernetes/mod.rs` adds the execution Pod policy and termination identity checks.
Each durable job uses one Pod with `restartPolicy: Never`, without a Job or Deployment retry controller.
Controller retries could execute code again after an uncertain result. The supervisor must reconcile the original identity instead.
The Pod retains a receipt finalizer. Cleanup must remove it only after receipt persistence.
Deletion acknowledgement and Pod phase alone do not confirm runtime termination.
The runtime must check the original UID, full job fingerprint, request fingerprint, and terminated container status.
A missing Pod after dispatch remains uncertain. Never recreate it automatically.

The policy pins a registry digest and uses `imagePullPolicy: Never`.
Deployment warmup must install that digest on eligible sandbox nodes before execution.
The policy also requires explicit node selection, a RuntimeClass, CPU limits, memory limits, and a deadline.
Execution Pods have no service-account token, host namespaces, privilege escalation, or writable root filesystem.
Code and input must use authenticated exec stdin. They must not enter Pod metadata, environment variables, or ConfigMaps.
The deployment still needs enforced network isolation and node-level process limits.

This extends the remote-sandbox behavior mapped earlier. The current platform does not provide this durable Kubernetes lifecycle.
The Kubernetes API adapter, UID persistence, deployment RBAC, and live acceptance remain open.
The configured local minikube API refuses connections during this check. No cluster configuration changes occur.

Kubernetes references:
- [Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
- [Finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)

Validation: all 22 focused sandbox tests pass. The local minikube profile reports all control-plane components stopped.
The first restricted test run cannot bind its loopback listener. The authorized rerun passes without ignored tests.
Library Clippy passes with warnings denied. These checks do not prove Kubernetes execution or deployment.

### Persisted runtime identity (2026-09-29)

Agentstate migration `0008_sandbox_runtime_binding.sql` adds a bounded runtime identity to the existing supervisor receipt row.
No product schema changes occur. An additive column is required because a workload name can identify a replacement instance.
`JobLedger::bind_runtime` binds once under the current preparation lease. Repeating the same binding succeeds; replacement identities fail.
The binding survives lease takeover and supervisor replacement. Terminal receipt immutability remains unchanged.

The supervisor saves the binding before dispatch and restores it for receipt reads, cancellation, and cleanup.
A missing bound workload does not authorize provisioning another workload.
The ADK `CodeJobIdentity` extension carries the binding. Docker observation compares it with the immutable container ID.
Existing dispatched Docker receipts without bindings remain readable during upgrade. New preparations always save a binding.
The supervisor startup check requires migration 0008 before accepting work.

Kubernetes will store its cluster, namespace, and Pod UID in this binding.
Its adapter and deployment integration remain open. Minikube platform deployment follows implementation readiness, as requested.

Validation: 23 focused sandbox tests pass. The isolated PostgreSQL binding test passes without skips.
The real Docker/Rust supervisor submission regression passes, including receipt reuse and cancellation checks.
All-target Clippy passes with warnings denied. No rehearsal migration or supervisor rollout occurs in this step.

## Docker sorting and state acceptance, 2026-09-29

The supervisor image now includes immutable runtime binding from `e9e4b1de8`.
Its image ID is `sha256:b8da3f02bc8098a792f067d69533918d4a7910c39a84a7dfc13c86f3763e4891`.
Both deployed supervisors retain their private volumes, verified trust, nonroot UID, and resource limits.
The worker has no Docker socket. The committed Compose overlay passes its render and isolation checks.

Migration 0008 is applied only to rehearsal agentstate.
The SQL is applied directly; its SHA-256 and version are then recorded in the existing migration ledger.
The deployment checks the previous head before recording version 8. Product tables remain unchanged.
Use the normal migration runner for subsequent deployments.

The browser creates pipeline 132 with four sequential Code nodes.
The exact instructions are saved in `scripts/runtime/fixtures/code-multilanguage-state.yaml`.
The adjacent input and expected JSON files make this acceptance case reproducible.

| Stage | Actual operation | State boundary |
| --- | --- | --- |
| Python | Parse input and sort records by amount and ID | Write `sorted_data` |
| JavaScript | Group category totals and preserve sorted records | Read `sorted_data`; write `grouped_data` |
| TypeScript | Check sorting and reconcile independent totals | Read `grouped_data`; write `checked_data` |
| Rust | Compile, verify IDs and total, return the final report | Read `checked_data`; write `messages` |

The test includes a negative amount and non-sorted input.
Expected IDs are `e,b,d,a,c`; the total is 31, with fruit 24 and veg 7.
Both the pipeline test chat and persistent chat 757 return this exact report.
Persistent chat retains one final report after reload.
These runs execute actual code without model-generated results or browser response mocks.
The model picker value is not used because this pipeline contains no model node.

The first run creates four completed receipts between 20:06:31 and 20:06:37 UTC.
The persistent run creates four completed receipts between 20:06:58 and 20:07:04 UTC.
Every receipt contains its original runtime ID.
These timings describe one local fixture, not a concurrency or production throughput benchmark.

Input `[]` fails the Python assertion at 20:08:00 UTC.
Only one failed receipt is created: `e71919ba3cb43d2f67f55a5983fb8c25014badbc09cbee5b2e174467337e3827`.
No downstream language dispatch occurs. The UI initially shows an unhelpful generic runtime error.

### Code failure mapping correction

`agents/graph/code_runtime.rs` now sends failures through the invocation-owned pipeline event channel.
`agents/graph/compiler.rs` supplies that channel when binding Code nodes.
`agents/graph/node_events.rs` shares execution-failure delivery with direct toolkit nodes.
This preserves graph failure and does not convert failure into a successful state update.

`libs/proto/elitea/runtime/v1/errors.proto` adds error code 32, `PIPELINE_CODE_FAILED`.
Rust `protocol/output.rs` and Main `internal/transport/runtimegrpc/output/server.go` register the same safe message.
The message identifies the Code node, suggests source/input/limit checks, and states that later nodes did not run.
Detailed causes remain in protected diagnostics. User source and state do not enter the public error message.
The worker logs the node identity and safe sandbox cause at ERROR severity.

The legacy Code state and sandbox references remain those mapped earlier in this assessment.
This change corrects new-platform failure delivery; it does not change the legacy YAML or product schema.

## One supervisor process, 2026-09-30

The Docker overlay now defines one supervisor container with two bounded runtime profiles.
`src/sandbox_main.rs` accepts repeated configuration paths.
`src/sandbox/process.rs::run_profiles` validates distinct owners and listener ports before startup.
Each profile retains its authenticated audience, runtime image, resource limits, and durable recovery owner.
Both listeners share the process lifecycle. A profile failure terminates that process; durable receipts remain available after restart.
The Deno listener uses port 9446. The Rust listener uses port 9447.
Network aliases preserve certificate hostnames and grant audiences.
This change does not combine execution containers or expose supervisor material to user code.
The legacy reference remains the remote sandbox execution boundary described above.
Compose rendering, three profile configuration tests, and all-target Clippy pass.
The consolidated supervisor image is 23,293,166 bytes. It excludes language runtimes and compilers.
Main was rebuilt without cached layers. The rebuilt image starts and passes its healthcheck.
Runtime Redis consumer groups were absent after the crash. The existing idempotent bootstrap recreated them from `0-0`.
The worker now consumes commands.
Browser chat 757 passes all four Code stages through the consolidated supervisor.
Four completed receipts retain immutable container identities. Docker inspection confirms all four execution containers are removed.
A failed Python assertion also retains its receipt and removes its container.
The browser retest exposed an event-channel bypass for Code-only pipelines in `agents/pipeline.rs::bind_node_runtimes`.
The correction retains the node-event channel without requiring an LLM runtime.
The deployed error-message acceptance passes in chat 757, including after reload.
Support details show `PIPELINE_CODE_FAILED` and message reference `fcbdbc26-4aef-5c89-8b56-e0cca83c98e2`.
The worker image is `sha256:cb21aaf32eee933739ef2796566c2361b234ead9ae12d0ec67810a6ee6828cc6`.
The supervisor image is `sha256:68c90b27e166eaab80077a257e4e50db5a94c62917cc281924b1185184b7663d`.
All 51 pipeline tests and all-target Clippy pass after the assembly correction.

The rehearsal host exhausted disk space on September 29. Docker then stopped multiple dependencies.
The local direct-tool regression passed after disposable incremental compiler cache removal.
The worker image rebuild passed and was deployed on September 30.
The previous Main image exited with code 139 without deployment configuration. Its uncached replacement passes startup.
Existing product and agentstate volumes remain unchanged. No database restore was required.

## Kubernetes client and namespace boundary, 2026-09-30

The supervisor uses `kube` 4.2.0 and `k8s-openapi` 0.28.0 behind its existing Cargo feature.
The default worker does not enable these dependencies. The client enables TLS, ring, and WebSocket support.
It does not enable the operator runtime or CRD generation.
The existing receipt ledger owns reconciliation. Kubernetes controllers must not retry Code execution automatically.

`src/sandbox/kubernetes/client.rs` provides typed Pod creation, observation, and graceful stop requests.
Observation checks namespace, Pod UID, job fingerprint, and request fingerprint.
Only Kubernetes `NotFound` means absence. Authorization failures and server errors do not permit recreation.
Creation conflicts require observation of the original name.
Delete requests include UID and resource-version preconditions.
A deletion response does not prove termination. The original Pod must report a terminated container.
API operations have a 15-second timeout. An expired mutation requires reconciliation because its outcome is unknown.

`deploy/helm/elitea/templates/sandbox/kubernetes-boundary.yaml` defines the optional execution namespace boundary.
The namespace must differ from the platform namespace and built-in namespaces.
Restricted Pod Security Admission applies. A namespace-wide NetworkPolicy denies ingress and egress.
The supervisor service account has Pod, exec, and log permissions only in the execution namespace.
Execution Pods use a separate service account without automatic credential mounting.
A Pod-count quota bounds workloads, including terminal Pods awaiting receipt cleanup.
The namespace, network policy, and quota remain after Helm uninstall to protect retained workloads.
Operators must drain receipts and confirm termination before they remove these retained resources.
Network isolation requires an enforcing CNI. No rendered manifest proves that the cluster enforces this policy.

Legacy references remain the local and remote Code sandbox paths mapped above.
These paths supply Code input, output, and state behavior. They do not supply Kubernetes lifecycle semantics.
The new client extends `src/sandbox/runtime.rs`; graph checkpoints remain worker-owned.
Pod preparation, dispatch, receipt reads, finalizer cleanup, and process selection remain to be connected.
Kubernetes deployment and browser acceptance remain open. This boundary does not enable execution by itself.

References: [kube feature contract](https://docs.rs/crate/kube/4.2.0/features),
[Kubernetes NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/),
and [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/).

Validation: six Kubernetes component tests pass, including mocked HTTP status and conditional-delete checks.
Supervisor library/test Clippy and Rust formatting checks pass.
Helm lint, boundary rendering, and existing image-warmup rendering checks pass.
The default worker dependency tree excludes `kube` and `k8s-openapi`.
These checks do not prove cluster enforcement, workload recovery, or browser execution.

## Kubernetes lifecycle adapter, 2026-09-30

`src/sandbox/kubernetes/runtime.rs` implements the existing `CodeJobRuntime` interface.
`src/sandbox/process.rs` selects Docker by default. An explicit Kubernetes backend uses in-cluster credentials and the same receipt lifecycle.
`deploy/runtime/sandbox-supervisor.kubernetes.example.json` documents the backend configuration.
The configured registry digest must match the signed job image digest.
The runtime binding includes cluster identity, namespace, and Pod UID.
The worker still owns graph checkpoints. No product table or checkpoint ownership changes.

`services/elitea-code-runner/src/lifecycle.rs` supplies inert preparation, readiness, and dispatch commands.
The Kubernetes downward API supplies the Pod UID and request fingerprint.
The helper compares both values before writing input or signaling execution.
This check protects the interval between supervisor observation and Kubernetes exec, which addresses Pods by name.
The input travels through exec stdin. Command arguments and Pod metadata do not contain code or user input.
Only two fixed input files are accepted. The helper writes readiness last.
A readiness marker prevents retries from rewriting prepared input. Dispatch uses one persistent marker consumed by PID 1.
The existing Docker runner entry point remains unchanged when no lifecycle command is supplied.

Exec input, stdout, stderr, duration, and receipt reads have explicit bounds.
A dropped attachment aborts its transport task. It does not prove that a remote operation failed.
The adapter reads receipts only after the original container reports termination.
Receipt validation requires the runner envelope and a zero exit code for successful completion.
Cleanup first requests graceful deletion while the receipt finalizer retains the Pod.
It then removes only Elitea's finalizer with UID and resource-version conditions.
Other finalizers remain. Missing bound Pods do not prove termination.

The legacy business references remain the Code sandbox and state mappings above.
Kubernetes placement and receipt retention are new runtime concerns, not a Python port.
The adapter requires rebuilt execution images containing the lifecycle helper.
The deployed Docker rehearsal still uses the previously verified images.
Projected Kubernetes Secrets need an owner-private material copy before supervisor startup.
Kubernetes deployment, network enforcement, cancellation races, restart recovery, and browser acceptance remain open gates.

Validation: all 32 sandbox tests pass with localhost sockets enabled for disconnect testing.
The ten Kubernetes tests include conditional cleanup and preservation of unrelated finalizers.
All six runner tests pass, including preparation, repeated dispatch, UID rejection, output bounds, and timeout handling.
Worker supervisor library/test Clippy, runner all-target Clippy, and formatting checks pass.
No new runtime images or Kubernetes workloads are deployed in this checkpoint.

## Kubernetes supervisor deployment material, 2026-09-30

`src/sandbox/material.rs` copies one projected Secret revision into canonical owner-private files.
The init mode reads at most 32 files, one MiB each, and eight MiB total.
It rejects nested symlinks, escaped projection directories, and invalid filenames.
Private buffers use zeroizing storage. Temporary destination files use mode `0600`.
`src/sandbox_main.rs --prepare-material` runs this operation before the supervisor starts.
This preserves `config.rs::read_regular_file` checks instead of relaxing them for Kubernetes symlinks.

`deploy/helm/elitea/templates/sandbox/supervisor.yaml` defines one non-root supervisor and one init container using the same image.
The init container mounts the projected Secret. The supervisor mounts only the private memory-backed copy, read-only.
The Pod has no Docker socket or host filesystem mount.
One internal Service exposes the configured runtime profile ports.
Images require registry digests. Configuration filenames and ports must be unique and bounded.
The supervisor remains optional and requires the separate execution namespace boundary.

Two material-copy tests and all-target supervisor Clippy pass.
The expanded Helm checks validate the deployment, permissions, mounts, service, and invalid configurations.
Live image and cluster verification are pending.
A separate `elitea-sandbox-rehearsal` Minikube profile uses Calico for network-policy tests.
The existing stopped `minikube` profile remains unchanged.

The isolated Minikube node is Ready and Calico is running.
Live RBAC checks allow supervisor Pod, exec, and log operations only in the execution namespace.
They deny Secret reads, platform-namespace Pod deletion, and execution-account Pod creation.
An HTTP baseline succeeds from the platform namespace. The same request times out from the execution namespace.
All three disposable network probes are deleted after this check.
These checks prove the tested RBAC and Pod-to-Pod network boundary, not complete runtime acceptance.
Material-copy retries remove files absent from the selected Secret revision.
The supervisor build stage now branches before the worker binary build to avoid an unrelated release compilation.

## Live Kubernetes language fixture, 2026-09-30

`src/sandbox/kubernetes/live_tests.rs` exercises the production runtime adapter against an explicit isolated Kubernetes context.
It reuses `scripts/runtime/fixtures/code-multilanguage-state.yaml` and its input and expected-result fixtures.
Python sorts five records. JavaScript aggregates categories. TypeScript checks totals. Rust verifies the final state.
Each language runs in a separate Pod with an immutable image reference and resource limits.
The test verifies original Pod identity, preparation, dispatch, terminal receipt, repeated terminal dispatch, and cleanup.
All four nodes pass. The full fixture takes 20.22 seconds in local Minikube.
All four execution Pods are removed after receipt inspection.
The native `runc` RuntimeClass is used; this check does not establish VM isolation.

The first test attempt stopped before dispatch because its TLS provider was not initialized.
The corrected harness selects the same ring provider used by supervisor startup.
This is runtime-adapter evidence only. It does not prove ledger durability, authenticated supervisor admission, or browser execution.
Supervisor restart, worker recovery, cancellation races, and full Kubernetes UI acceptance remain open.

## Kubernetes supervisor startup, 2026-09-30

The Helm supervisor deployment is running in the isolated rehearsal cluster.
One non-root Pod serves the Deno and Rust profiles on separate authenticated listeners.
The projected-Secret init container completes successfully with the deployed image.
Both profiles connect to the retained rehearsal receipt database through verified PostgreSQL TLS.
The Kubernetes owners differ from the running Docker owners, so they do not reclaim Docker work.
A namespace-local Service and EndpointSlice route the original database hostname to the retained rehearsal database.
No application database schema or stored data changes are required.

The deployed supervisor manifest digest is `sha256:0a31cec3ef1876569ef4124b05ca464322783d5e566f6b682c9383b67520f7ff`.
Both authenticated listeners report startup. The deployment has one Ready replica with no restarts.
This proves supervisor startup and private configuration preparation, not authenticated job submission or restart recovery.
The application and worker still run in Docker. Full Kubernetes application deployment and UI verification remain open.

## Live Kubernetes supervisor recovery, 2026-09-30

A disposable supervisor runs the production image with a separate owner and fixture verification key.
The client uses the existing worker SPIFFE certificate over verified mTLS.
A valid signature bound to another peer is rejected with `PERMISSION_DENIED`.
The admitted JavaScript code generates a unique marker and waits 45 seconds before returning it.
After the dispatch marker appears, the test replaces the supervisor Pod with a one-second termination grace period.
The execution Pod retains its original UID and continues running.
After lease expiry, the replacement supervisor reconciles the same activation and persists its completed receipt.
A fresh signed retry returns identical result bytes. No replacement execution Pod is created.
The execution Pod is removed after durable completion. The disposable supervisor and fixture Secret are then removed.
The normal supervisor remains Ready with zero restarts.

`scripts/runtime/test_kubernetes_supervisor_recovery.py` retains the verification procedure with explicit cluster, namespace, and certificate-directory arguments.
The fixture signer is separate from Main. This proves supervisor authorization enforcement and receipt recovery, not Main grant issuance.
The first harness attempt stopped before submission because it expected a DNS certificate instead of SPIFFE.
No production behavior change was needed for that harness correction.
Full Kubernetes worker/application deployment and browser acceptance remain open.

## Helm worker backend configuration, 2026-09-30

The Kubernetes application preparation exposed a deployment configuration gap.
`templates/worker/configmap-runtime.yaml` omitted `sandbox_runtimes` and `agent_model_checkpoint_recovery` from the Rust configuration.
The Docker rehearsal already supplies these fields through its private runtime configuration.
Without the Helm fields, Code nodes have no configured backend and model-checkpoint recovery retains its disabled default.

`worker.runtime.sandboxRuntimes` now maps exactly to `config.rs::SandboxRuntimeConfig` entries.
`worker.runtime.agentModelCheckpointRecovery` maps to the existing recovery flag.
Both options are Rust-only. Helm rejects their use with the Python worker.
Empty profiles and disabled recovery preserve the existing default configuration for both implementations.
The target hostname must match the supervisor TLS certificate. Runtime image digests and policies must match the supervisor profile.

The new `render-worker-sandbox.sh` verifies field preservation and Python rejection.
The existing eight worker rendering checks still pass. The Helm workflow now runs both checks.
This closes configuration delivery, not live worker startup or UI acceptance.

## Isolated Kubernetes application dependencies, 2026-09-30

Application preparation uses dedicated Kubernetes services instead of joining the retained Docker network.
The network attachment proposal was rejected by automatic approval review. No Docker network membership changed.
The existing TLS database endpoint remains the only configured retained data-service route.

The chart deploys a separate runtime Redis with its own persistent volume and the existing authenticated TLS configuration.
The default `ceph-rbd` storage class is unavailable in Minikube.
The initial claim remained Pending without a bound volume or stored data.
Only that empty claim was replaced with a `standard` claim; the final claim is Bound with eight GiB capacity.
Redis starts with TLS on port 6380 and AOF enabled. Its authenticated readiness probe passes.
The chart bootstrap Job completes and creates the isolated consumer groups.
The retained Docker Redis and application are unchanged.
Main, worker, and UI images are cached in Minikube, but application ownership has not switched.

### Kubernetes platform edge verification

The existing Helm platform-edge template now runs in the isolated Kubernetes rehearsal namespace.
It uses the retained certificate identity through a Kubernetes TLS Secret.
The container runs as user 65532 with a read-only root filesystem and all capabilities removed.
The Deployment becomes Ready without restarts.
A temporary localhost port forward verifies the runtime CA chain and the `elitea-platform-edge` hostname with TLS 1.3.
The verification closes the port forward after the handshake.
This proves the TLS transport dependency only. Main, worker, and browser execution remain unverified in Kubernetes.
No Docker network membership or application ownership changes.


## Kubernetes application and browser acceptance, 2026-09-30

The rehearsal now runs Main, Rust worker, supervisor, web, browser edge, platform edge, runtime Redis, cache Redis, and object storage in Kubernetes.
PostgreSQL, the LLM gateway, OIDC mock, and trace collector remain external rehearsal dependencies through their existing host-published endpoints.
No Docker network attachment is added. This is not a claim of a fully self-contained Kubernetes deployment.

Main and worker first deploy with zero replicas. Their isolated material initialization Jobs both complete.
The object-store snapshot follows stopped admission and a check for zero live execution claims.
Its 923136-byte archive transfers with an identical SHA-256 digest before extraction into a separate Kubernetes volume.
The original object-store volume remains unchanged. Docker services restart after the snapshot.
The final cutover checks zero pending commands and zero consumer-group lag before changing ownership.
The 23 expired historical claims and two historical dispatched rows remain unchanged.

The first Main startup fails TLS verification because the retained Redis certificate names `runtime-redis`.
The chart example uses `elitea-runtime-redis`. Automatic rollback restores Docker Main and worker.
A certificate-matching Kubernetes Service and configured endpoint correct the mismatch without disabling TLS verification.
The second startup succeeds. Kubernetes Main and worker become Ready without restarts.
Docker Main, worker, and browser edge stop and remain available for rollback.
The rehearsal browser endpoint uses a localhost-only Kubernetes port forward.

A fresh Chrome tab opens persistent chat 757 and submits the existing four-language state fixture.
The production Main grant issuer authorizes the normal supervisor profiles; no fixture signer participates.
Execution `5e3cbcf7391fe0e31c6cdb800c9e6eb8` reaches `SUCCEEDED`.
Python sorts five records. JavaScript groups them. TypeScript checks the state. Rust verifies the final result.
The UI shows count 5, total 31, fruit 24, veg 7, and all four completed steps.
Reload preserves the exact final result once for this turn.
The supervisor ledger contains four completed receipts with Kubernetes runtime identities.
All execution Pods are removed after durable completion.

A second browser submission uses an empty array and fails the Python assertion.
The ledger adds one failed Kubernetes receipt. No later-language receipt is created for that run.
The UI shows `PIPELINE_CODE_FAILED`, recovery guidance, and operator-only diagnostic instructions.
Its support message reference is `dde18897-121c-50c0-a92a-0cf90ee13059`.
This verifies direct pipeline failure propagation through the Kubernetes backend.

Existing source mappings remain authoritative: `sandbox/dispatch.rs` owns durable worker dispatch intent.
`sandbox/ledger.rs` owns supervisor receipts; `sandbox/kubernetes/runtime.rs` owns Pod execution and identity checks.
Main owns grant issuance, while the worker owns graph checkpoints and result propagation.
The Kubernetes paths reuse the Docker Code-node graph and UI contracts rather than a second implementation.

Kubernetes resource exhaustion remains open. The following section records cancellation, worker recovery, and ephemeral-editor acceptance.
The browser connection badge remains disconnected while the execution-event stream delivers the result; that indicator needs separate investigation.
No load, soak, external provider, or package-installation claim follows from these Code-only tests.


## Kubernetes Stop, worker recovery, and editor acceptance, 2026-09-30

Persistent chat 755 starts the slow Rust Code fixture through the normal Main grant issuer.
The browser Stop action requests cancellation after sandbox dispatch.
Execution `f4b7aee45aca971221c5a59ba206aa29` reaches `CANCELLED`.
The supervisor persists the cancelled receipt before Main settles the execution.
The browser shows the cancellation message and releases the composer. The execution Pod is removed.

A second request starts the same fixture. The test removes the worker Pod after sandbox dispatch.
The replacement worker resumes execution `06cca078a5a84e4ecce27cf26570bbe4`, which reaches `SUCCEEDED`.
The completed receipt retains sandbox Pod UID `ba8eb44a-87ac-47b0-9ae9-34f91191f13d`.
The browser receives `GATE5_WORKER_RECOVERY_20260929` without resubmission.
The execution Pod is removed after completion.
This verifies worker recovery with a replacement Pod and the original sandbox runtime identity.
It does not prove exactly-once effects against external systems.

The pipeline 132 editor test chat executes the four-language state fixture.
Its displayed result has status `PASS`, count 5, total 31, fruit 24, and veg 7.
The ledger records four completed receipts. No execution Pods remain.
The editor shows a completed run and releases its controls.
Ephemeral chat survival across browser reload is not part of this check.

These checks use the existing `sandbox/dispatch.rs`, `sandbox/ledger.rs`, and `sandbox/kubernetes/runtime.rs` mappings.
They add deployment evidence without changing application schemas or execution contracts.
Resource exhaustion, package/workspace authorization, and performance acceptance remain open.


## Kubernetes memory-kill receipt correction, 2026-09-30

The live resource test allocates and touches up to 512 MiB inside a 256 MiB sandbox.
Kubernetes terminates the original container with `OOMKilled` and exit code 137.
Before this correction, the adapter reads empty runner logs and reports a receipt validation error.
That error cannot establish a durable terminal receipt for the supervisor.

`sandbox/kubernetes/client.rs` now checks the identity-bound terminal container status before reading runner logs.
An `OOMKilled` status produces a bounded `memory_limit` receipt.
A Pod deadline produces a timeout receipt. Other nonzero container exits produce failed receipts.
A successful container exit still requires the complete, validated runner envelope.
Missing Pods, replacement UIDs, and API failures do not authorize this fallback.
`sandbox/docker_supervisor.rs` maps the memory receipt to failed phase and `sandbox.memory_limit`.
The existing ledger persists that failure before runtime cleanup.

The current-platform reference remains `elitea-sdk/runtime/langchain/pyodide_sandbox.py` for bounded subprocess execution.
The new adapter handles a container-wide kernel termination that prevents the subprocess runner from writing its envelope.
This correction extends the existing sandbox mapping without application-schema changes.

The original live test fails on the missing receipt. The corrected live test passes in 1.91 seconds.
It receives exit code 137 and `memory_limit`, verifies an identical repeated receipt, and requests cleanup.
Six Kubernetes client tests and two supervisor classification tests pass. Clippy passes with warnings denied.
This is live adapter evidence. Deployment and browser acceptance for this memory failure remain pending.
CPU throttling, deadline exhaustion, and performance acceptance retain their separate gates.


## Shared build-host pressure, 2026-09-30

The rehearsal shares an 8 GiB Docker VM with the current platform and release compilation.
During supervisor compilation, the VM exhausts swap and the isolated Minikube container records OOM events.
The Kubernetes API stops responding. This event is separate from the bounded Code memory-exhaustion test.
The build is cancelled through its identified client. BuildKit records a terminal error.
The existing Minikube profile restarts without recreating volumes. Its services recover after Redis becomes ready.
The localhost browser port forward is restored.

The supervisor Containerfile accepts an optional `CARGO_BUILD_JOBS` build argument.
A one-job build still overlaps enough resident memory to produce another node OOM event.
Build concurrency alone is therefore not a sufficient host memory guarantee.
Minikube confirms that the isolated rehearsal node stops while the build continues. Current-platform containers remain unchanged.
The single-job build completes after the rehearsal node stops. The existing node then restarts with its retained volumes.
The supervisor rolls out with the memory receipt fix. Browser memory-failure acceptance remains pending.
The Rust test targets compile after the deadline test helper uses the declared request timeout type.
Do not interpret this shared build-host event as production runtime capacity evidence.

## Kubernetes resource failure acceptance, 2026-09-30

The supervisor deploys the memory receipt correction to the existing rehearsal.
The cached image digest is `sha256:91449fff17c17fe84ea519c61398431839329c7c78ca4981e0c886428ee95234`.

The live deadline test passes in 5.77 seconds. The live memory test passes in 4.45 seconds.
Both tests verify repeated receipt retrieval and final Pod removal.
The memory receipt reports `memory_limit` and exit code 137.
The test helper uses the request contract's `u32` timeout.

Persistent chat 759 runs pipeline 133 after the fixture replaces reserved state key `result` with `memory_probe`.
The first attempt fails admission and does not test resource enforcement.
The corrected execution is `57a83998b93acaba18cd1d6541065051`.
Its only sandbox job reaches `failed` with `sandbox.memory_limit`.
The job key is `ccb974eaa2af8a66475cdf9827c5bdf62a700fa293f0e54ee36e5e2aeeee1eb3`.
The downstream sentinel does not run. No sandbox Pods remain.
The browser shows Code-node failure guidance and restores the composer.
The same failure remains visible after reload.
Screenshot evidence: `elitea-kubernetes-memory-failure-20260930.png`.

The fixture is `scripts/runtime/fixtures/code-memory-limit.yaml`.
The live adapter checks are in `src/sandbox/kubernetes/live_tests.rs`.
These checks extend the current-to-new mapping above. They do not prove production load capacity.
The existing disconnected sidebar indicator remains a separate UI issue.
