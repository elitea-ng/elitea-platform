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
| `infra/data/sandbox/main.ts` package preparation | Detects missing imports and installs packages through micropip. Explicit package instructions control installation order. | `adapters/python.mjs` contains import preparation and supports explicit micropip calls. The deployed offline image preloads only the interpreter and base micropip assets. Additional downloads remain unimplemented. |
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
