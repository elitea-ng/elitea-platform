# Remote OpenAPI specification retrieval

## Current platform source mapping

| Current source | Replacement source | Behavior |
| --- | --- | --- |
| `projects/elitea-sdk/elitea_sdk/tools/openapi/__init__.py` | `src/toolkits/families/openapi/config.rs` | Accept toolkit settings containing a specification URL, inline JSON, or inline YAML. |
| `projects/elitea-sdk/elitea_sdk/tools/openapi/api_wrapper.py`: `client.load_spec(self.spec)` | `src/toolkits/families/openapi/source.rs` and `spec.rs` | Retrieve the configured document and derive selected operation schemas. |
| `projects/EliteaUI/src/hooks/toolkit/useGetSelectedToolSchema.js` | Web `useGetSelectedToolSchema.ts` and Rust `src/toolkits/direct_runtime.rs` | Display the actual saved toolkit argument schema in Toolkit Test. |
| Current SDK configured-toolkit assembly | `src/toolkits/materialize.rs`, `src/agents/{ordinary,application_tools,pipeline}.rs` | Use the same configured toolkit in direct tests and model-owned calls. |

Paths beginning with `projects/` refer to the source umbrella workspace.
Rust paths start at `services/elitea-worker-rust/`.
The web hook lives under `apps/elitea-web/src/features/toolkits/ui/test-tools/`.
The current platform defines the required behavior. The implementation retains native Rust ownership and typed failures.

## Implementation

The admitted toolkit settings select the source URL.
Tool invocation arguments cannot change that URL.
The materializer retrieves remote documents asynchronously before creating operation tools.
All existing callers await materialization, including direct tests, ordinary agents, saved applications, and pipelines.
Inline documents keep the existing parser path.

Retrieval requires HTTPS and verified certificates.
URLs cannot contain user information or fragments.
The client follows no redirects and sends no toolkit credentials or cookies.
Connection timeout is five seconds. The total request timeout is fifteen seconds.
The URL limit is eight KiB. The response limit is one MiB.
The response limit applies with or without a declared content length.
Deployment network controls still own destination access. This loader does not grant new network access.

The existing parser validates JSON/YAML, depth, node counts, references, and selected operations.
Relative server URLs resolve against the document URL.
An absent server list defaults to the document origin root.
Explicit absolute servers remain subject to the existing parser rules.
Documents containing unresolved relative server variables remain unsupported.
External references and OpenAPI versions outside the existing parser contract remain unsupported.

Retrieval failure maps to a typed dependency failure.
Invalid source URLs map to invalid configuration. Oversized responses map to resource exhaustion.
These failures do not silently remove an otherwise supported toolkit.
The credential remains separate from specification retrieval and is used only by the operation client.

No application schema, migration, dependency, or public route changes are required.
The source URL is frozen in the admitted settings. Remote document bytes are external input at materialization time.
A later assembly can observe a changed document. This change does not establish immutable remote-document replay.

## Verification

The isolated commit passes 25 focused OpenAPI tests.
Coverage includes anonymous retrieval, redirect refusal, declared and streamed body limits, deadlines, inline fallback, and relative server resolution.
Existing OpenAPI argument, credential, delegated authorization, and pipeline tests also pass.
The isolated commit passes strict Clippy across the library and tests.
Whole-worktree Clippy still has unrelated failures in pending instruction-authority tests.

The deployed fixture uses synthetic OAuth client credentials and verified local TLS.
Internal MCP creates credential 14 and toolkit 31 in project 2.
A later internal MCP update changes the credential label and retains its secret reference.
The same update restores the toolkit specification URL and retains the credential reference and selected operation.

Toolkit Test discovers the URL-backed `echo_marker` schema and displays its required marker field.
Execution `486245d46e8988cce59d3cd9ddaf5486` returns HTTP 200 and reaches durable `SUCCEEDED` state.
Its result contains marker `RUST_GATE3_REMOTE_SPEC_20260911`, mode `stored`, and generation `1`.

Internal MCP attaches toolkit 31 to application 19, version 20.
The persisted relations retain GitHub toolkit 30 and skill version 5.
Saved-agent chat 543 invokes `echo_marker` with marker `RUST_GATE3_AGENT_OAUTH_20260911`.
Trace step 7313, message group 5840, records that exact input and the authenticated result.
The trace has `is_error=false` and `finish_reason=stop`.
The model returns the result and follows the linked skill's joke instruction.

These checks prove the synthetic credential and toolkit lifecycle through chat, Toolkit Test, and a saved agent.
They do not close all point 3 providers, authorization modes, or replacement and replay requirements.
