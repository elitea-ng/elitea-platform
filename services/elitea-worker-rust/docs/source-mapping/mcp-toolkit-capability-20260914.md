# MCP toolkit admission correction

## Failure and ownership

A fresh browser opens temporary MCP toolkits 52 through 54 in Toolkit Test.
The tool list fails before execution. The discovery API returns HTTP 422 with `reason: unsupported_toolkit`.
All temporary toolkits are removed with HTTP 204.
This failure prevents the planned OAuth reload test from reaching authorization.

Current Core `api/v2/test_toolkit_tool.py` and SDK MCP tools provide the functional reference.
The replatform uses its own durable discovery and invocation paths.
Rust `toolkits/direct_runtime.rs` already materializes `FrozenToolKind::Mcp` through the shared ADK MCP connector.
That path preserves token references, toolkit policy, and authorization requirements.
Main's `runtimecomposition/toolkit_call_tool_runtime.go` checks the pinned worker capability before dispatch.
Its Rust capability snapshot includes only configured families from `toolkits/materialize.rs`.
MCP uses a separate materialization branch, so the snapshot incorrectly excludes it.

## Correction and verification

Regenerate the supported-type set from configured families and the existing direct MCP branch.
Include `mcp` in the pinned Main snapshot.
Extend the source consistency test to check both materialization paths.
Add an explicit MCP capability verdict assertion.
The focused capability and pinned-snapshot load tests pass.
No Rust protocol, product schema, or worker execution code changes are required.

Deployment and browser OAuth reload acceptance remain pending at this commit.
Do not treat capability admission as proof of the complete authorization flow.

## Deployment and next failure

Main image `sha256:80ca2030a1555a6cf9046871e156bd4496599e2261cc03907e370e84779867e5` contains the correction.
The rehearsal replacement retains its environment, six mounts, networks, and resource limits.
Temporary toolkit 55 now reaches Rust discovery instead of the unsupported-type gate.
Executions `2f35b4cfdf5ec21a9faf83221e374e54` and `94f0e404b5f2b58dcfecb140a5694029` retire through the toolkit delivery path.
The API returns HTTP 500, and the browser still shows Retry.
Toolkit 55 is removed with HTTP 204.

The source shows a separate authorization-discovery gap.
`toolkits/delegated_auth.rs::requirement_for_direct` includes discovery requirements for the exact toolkit.
`toolkits/direct_runtime.rs::materialize` returns this requirement as an authorization error.
`execution/toolkit_delivery_processor.rs::execute_operation` projects authorization metadata only for call-tool commands.
Its discovery branch converts all errors into runtime failures.
Main's discovery service accepts only successful result artifacts, so it cannot return that authorization challenge to the editor.
The browser OAuth reload gate remains open.
Preserve the authorization challenge through discovery and reuse the existing browser authorization controls before repeating acceptance.
Evidence is `elitea-oauth-reload-discovery-fixed.log` and the inspected runtime branches.
