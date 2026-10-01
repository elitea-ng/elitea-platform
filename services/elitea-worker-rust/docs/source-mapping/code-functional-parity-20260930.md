# Code-node functional parity audit

This audit follows the live Docker and Kubernetes resource-limit checks.
Resource isolation does not establish package, platform-client, or artifact compatibility.
The current SDK is the behavior reference. It is not a source-code port target.

## Source mapping

| Current SDK source | Current behavior | Rust destination and status |
| --- | --- | --- |
| `elitea_sdk/runtime/tools/function.py::_prepare_pyodide_input` | Empty selection or only `messages` selects all other state values. Explicit names select matching values. | `src/agents/graph/code_state.rs::input_json` preserves selection rules for declared user state. Undeclared runtime metadata remains excluded. |
| Same method | Defines `elitea_state` and the shallow `alita_state` compatibility copy. | `services/elitea-code-runner/adapters/python.mjs` and `javascript.mjs` provide these names. Rust receives explicit JSON state. |
| `function.py::_handle_pyodide_output` | Maps results into selected outputs and supports structured state updates. | `code_result.rs` and `CodeStateBoundary::validate_updates` validate the complete typed patch before publication. |
| `infra/data/sandbox/main.ts` package preparation | Detects missing imports and installs packages through micropip. Explicit package instructions control installation order. | `adapters/python.mjs` supports prepared imports and explicit micropip calls. Approved image profiles preload their frozen dependency closure. On-demand downloads remain unimplemented. |
| `runtime/tools/sandbox.py::_prepare_pyodide_input` | Supplies `SandboxClient` with project context and a token or session reference when code requires it. | The Code runner receives no platform authentication material. A scoped client bridge remains unimplemented. |
| `runtime/clients/sandbox_client.py` | Exposes application reads, MCP calls, secret access, and artifact operations. | These require operation-specific authorization and integration with the existing platform contracts. Admission to execute code does not grant these operations. |
| `function.py::_save_code_to_artifact` | Saves executable debug source and state preamble into an artifact bucket. The exported client uses a token placeholder. | The YAML parser accepts `debug`, but the Rust path does not yet produce the artifact. Gate 7 artifact authority remains a dependency. |
| `src/agents/graph/code_remote.rs::execute` in the new worker | No legacy counterpart is assumed for admission policy. | Saved literal source can execute. State-variable source is rejected until its approval contract exists. Parsing variable source is not execution acceptance. |

The live four-language state chain verifies fixed-source sorting and selected-state transfer.
It does not verify arbitrary dependency installation, platform-client methods, debug artifacts, or variable-source approval.

## Implementation boundaries

Keep package acquisition separate from execution admission and ordinary provider egress.
Resolve requested packages into an immutable manifest before dispatch.
Bind that manifest and the runtime image to the prepared request identity.
Retain package versions and content digests across retries and recovery.
Use bounded downloads and an authorized registry route; do not grant general network access to the execution container.
Keep writable installation state private to each execution.
Use immutable shared package content for cache reuse.
Test both automatic imports and explicit micropip installation, including incompatible wheels and unavailable versions.
Preserve JavaScript/TypeScript and Rust dependency support as separate language contracts.
Do not claim Python wheel preparation implements npm or Cargo package resolution.

Route platform operations through the existing actor and project authorization boundary.
Do not copy a broad user token or session into the source preamble or runtime environment.
Bind any delegated operation to the admitted execution and requested resource.
Preserve authorized secret and artifact outcomes through explicit operation contracts.
Effectful calls also depend on gate 6 effect receipts and reconciliation.
Workspace snapshots and debug exports depend on gate 7 artifact grants and path containment.
Gate 7a must reuse these boundaries for the ordinary-agent Code tool.

## Acceptance still required

- Install an allowed dependency and use it from a persistent-chat Code run.
- Reuse its immutable content without a download on the next run.
- Reject unauthorized sources and oversized package content before execution.
- Recover preparation and execution without changing the package manifest.
- Prove user/project isolation for each supported platform-client operation.
- Export debug source without authentication material.
- Approve or reject state-variable source with durable decision binding.
- Verify these behaviors on both runtime backends and in the UI where applicable.

No database schema change or runtime permission expansion is made by this audit.

## Variable-source approval integration

`CodeNodeDefinition::resolve_source` preserves fixed-source versus state-variable provenance.
`code_remote.rs::RemoteCodeRuntime::execute` currently refuses every state-variable invocation before sandbox dispatch.
This is an admission policy restriction, not a language-runtime limitation.

The existing direct-tool approval path provides the following reusable contracts:

- `direct_tool.rs::SensitiveResumeDecision::parse` checks configuration, call, and argument digests before accepting a decision.
- `direct_tool.rs::sensitive_decision` creates the existing `elitea.graph.tool-confirmation.v1` interrupt.
- `resume.rs::PipelineToolDecision::resolve` binds browser decisions to the latest durable checkpoint and pending node.
- `events.rs::project_pipeline_tool_confirmation` publishes the existing approval controls.
- Direct-tool rejection sets the blocked pipeline state and stops downstream execution.

Reusing this path requires binding the exact source, selected input, language, and execution policy.
Approval of a variable name alone cannot authorize subsequently changed source.
Preserve activation identity across resume and recovery, including repeated visits to a looped Code node.
Never move approval into the supervisor as a substitute for graph checkpoint ownership.

The existing confirmation argument limit is 40 KiB, while Code source can reach 256 KiB.
Do not silently truncate source review or reduce the supported Code source limit to fit that event.
Large-source review requires an explicit bounded presentation contract before activation.

The user is asked whether state-variable source needs separate approval or follows saved-pipeline authorization.
Keep the current refusal until that policy is settled and the selected execution contract is implemented.
Fixed-source execution remains unchanged.

## Frozen Python dependency preparation

The current SDK `infra/data/sandbox/main.ts::install_imports` resolves import names and installs missing packages through micropip.
The new image preparation uses native `micropip.freeze()` and Pyodide lockfile loading.
`services/elitea-code-runner/adapters/preload.mjs` accepts an operator-owned list of exact package requirements.
The image embeds the resolved lockfile and wheel content, including transitive dependencies.
The runtime image digest already participates in `sandbox/request.rs::PreparedJob` identity and authorization.
Changing the package set therefore changes the admitted runtime identity.

Pyodide 0.29 incorrectly joins frozen external wheel URLs to its CDN base during package loading.
`adapters/python_wheels.mjs` materializes those references and replaces them with local wheel filenames.
It preserves and verifies each frozen SHA-256 digest.
Downloads accept only HTTPS PyPI wheel storage, reject redirects, and cap each external wheel at 32 MiB.
The external wheel set has a 128 MiB bound. Each download has a 60-second timeout.
These bounds cover wheel materialization, not micropip's preceding build-time metadata resolution.
Existing cached content requires digest verification before reuse.

`adapters/python.mjs` loads the frozen lockfile from the private execution cache.
Native `loadPackagesFromImports` handles different import and distribution names.
Explicit micropip calls retain their original order and version constraints.
No execution-container network access is added.
The Rust launcher copies immutable image assets into each job's private cache.

Local tests pass with network denied: automatic imports, explicit installation, transitive dependencies, state handling, exceptions, and result bounds.
Two cache tests verify source restrictions, offline reuse, and tamper rejection.
All ten real Docker adapter checks pass with UID 10001, network disabled, and read-only root.
They include automatic and explicit package installation, unavailable versions, subprocess denial, timeout, and identical repeated receipt reads.
The verified image identity is `sha256:09bc755806d705f5251d2d059a8c4e82a38ceca213807922295da75d541b6f79`.
The first container check exposes build-directory paths in the frozen lockfile.
Preparation now normalizes verified local references and retains only the installed dependency closure.
Browser and Kubernetes acceptance pass in persistent chat 767. A repeat run uses fresh isolated jobs and returns the same result.
See the [dependency profile record](code-dependency-profiles-20261001.md) for deployment identities and npm verification.

This is an immutable image-preparation path, not completed on-demand dependency acquisition.
The [on-demand preparation component](code-python-demand-preparation-20261001.md) adds source discovery, native resolution, and verified frozen-content reuse.
Its component and offline-container tests pass. Production dispatch, shared storage, recovery, and browser integration remain open.
The default package profile remains empty. Operators can supply approved requirements when building a runtime profile.
Approved npm image profiles now pass frozen offline resolution through Deno.
On-demand preparation, Cargo profile expansion, and compiled-artifact caching remain open.

Native API references: [micropip freeze](https://micropip.pyodide.org/en/latest/project/api.html) and [Pyodide lockfiles](https://pyodide.org/en/0.29.0/usage/api/js-api.html).
