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
