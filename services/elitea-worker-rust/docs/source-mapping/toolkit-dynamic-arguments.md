# Dynamic Toolkit Test arguments

## Current platform evidence

The current UI resolves static argument schemas, saved MCP schemas, then instance discovery schemas.
OpenAPI operations use instance discovery because each specification defines different operations.
The saved credential remains a reference throughout this UI flow.

| Current source | Replacement source | Behavior |
| --- | --- | --- |
| `projects/EliteaUI/src/hooks/toolkit/useGetSelectedToolSchema.js` | `apps/elitea-web/src/features/toolkits/ui/test-tools/useGetSelectedToolSchema.ts` | Resolve `args_schemas` through saved toolkit discovery. |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py` | Main `internal/api/v2/toolkits/handler.go:AvailableTools` | Authorize discovery for the saved toolkit. |
| `projects/elitea-sdk/elitea_sdk/tools/openapi/__init__.py` | Rust `src/toolkits/direct_runtime.rs` and `src/toolkits/families/openapi/` | Derive selected operation schemas and invoke the configured API. |

Paths beginning with `projects/` refer to the source umbrella workspace.
Main paths start at `services/elitea-main/`. Rust paths start at `services/elitea-worker-rust/`.

## Implementation

The UI previously omitted dynamic schemas because an old endpoint returned only tool names.
The Rust discovery contract now returns tool names and `args_schemas`.
Both Toolkit Test panes pass the saved toolkit ID to the schema hook.
An explicit project ID takes precedence over the selected project.
The existing query cache shares the instance discovery result.
Static and saved MCP schemas retain priority.
A failed discovery shows an error and retry control.
A successful empty-object schema represents a tool without arguments.

No database schema changes are required.

## Verification

Nine hook tests cover static schemas, MCP schemas, dynamic schemas, failure, and retry.
Six pane tests cover argument rendering, required fields, execution input, and refusal handling.
TypeScript and focused lint checks pass.
The deployed browser shows discovery errors after selecting the URL-backed OpenAPI operation.
After an internal MCP update supplies valid inline JSON, the browser renders the required marker input.
Toolkit Test returns HTTP 200 for execution `bf93d865715549997a1a4a1248d7cce1`.
The protected fixture returns marker `RUST_GATE3_SAVED_OAUTH_20260911`, mode `stored`, and generation `1`.
The toolkit retains its saved credential reference and selected `echo_marker` operation.
Credential creation and toolkit creation occurred through internal MCP in chat 545.
The credential stores a secret reference. The protected read proves runtime redemption and OAuth client authentication.
The model initially adds an extra JSON brace during the update. A second internal MCP update corrects it.
This fixture uses synthetic credentials and no external account.

## Related runtime failure

Main omitted `toolkit.call_tool.v1` and `toolkit.available_tools.v1` from its failure projector capability list.
The worker failure was rejected, so the synchronous endpoint timed out.
The repaired projector accepts both admitted capabilities and retains existing fencing and replay checks.
Focused repository tests cover failure insertion and replay without index side effects.
The deployed repair changes both previously stuck discovery executions to `FAILED`.
Those execution IDs are `4a6c939f1471f2574709313a3a079b98` and `d42cc4fe583f3123c0d9ea4bac7b4821`.
The capability name is shared with the pending discovery implementation.

## Remaining boundary

Remote OpenAPI specification loading remains unsupported by the current Rust parser.
It requires an authorized, bounded fetch path before parsing.
Inline specification testing does not close this URL-source gap.
Full point 3 completion requires additional credential, toolkit, and external MCP acceptance evidence.
