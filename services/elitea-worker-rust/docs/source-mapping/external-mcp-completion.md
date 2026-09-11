# External MCP instance schemas and terminal results

This slice connects external MCP catalogues to the same saved-instance discovery
used by toolkit settings and makes a projected output-limit pause explicit.
It does not establish deployed browser or provider completion.

## Current business source mapping

| Current source under the umbrella workspace | Behavior | Native owner |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/utils/mcp_service.py`, `__get_toolkit_tools` and `__get_all_tools` | Saved opt-in rows, selected operations, and argument schemas become external descriptors. | Main `internal/api/v2/mcp/catalog.go` and `external_toolkit_schemas.go` |
| Same file, `__get_application_tools` and `__handle_call_tool_request` | Saved application/version identity enters the common prediction path. | Main `mcp/execute.go`, application start, and existing Rust agent/pipeline dispatch |
| `projects/elitea-sdk/elitea_sdk/tools/__init__.py`, `get_toolkit_available_tools` | Instance settings choose live tool enumeration and argument schemas. | Shared Main `application/toolkitdiscovery` and Rust `toolkit.available_tools.v1` execution |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py`, `PromptLibAPI.get` | Project/folder authorization and saved settings precede discovery. | The same shared discovery admission use case |

Inspected Core revision: `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
Inspected SDK revision: `ecf49dfac73cd096da4c2297f3d91d13e526395a`.
The current Python external catalogue reads type-registry schemas. The native
adapter uses live saved-instance discovery for dynamic families, preserving the
same selected-operation and private target identity contract while filling that
schema limitation.

## Schema ownership and limits

The main external MCP route receives the existing `toolkitdiscovery.UseCase`.
For `openapi`, `mcp`, `mcp_config`, and `mcp_*`, each visible saved row supplies
its exact project, actor, and toolkit ID to that use case. Other families retain
the static projection. A runtime-disabled handler retains its existing static
fallback. A configured discovery service failure is returned; it does not
silently replace a live schema with an open object.

The adapter preserves the discovered JSON schema, including nested references,
required fields, and exact JSON numbers. Selected operations withdrawn from live
discovery are omitted. Missing schemas, duplicate operation names, and non-object
argument schemas fail closed. Existing opt-in, folder visibility, exact resource
scope, and duplicate exported-name checks still apply.

Catalogue reads collect at most 512 toolkit rows and release the PostgreSQL rows
before awaiting discovery. One 60-second context bounds instance discovery for
the catalogue. The result is limited to 4096 tools and 4 MiB of argument schemas.
There is no new type catalogue or process-local discovery cache.

`PostgresContentRepository.AuthorizeContent` obtains the admitted toolkit type
from the prepared command in the existing `command_outbox`, bound to the same
execution and input bundle. The shared `PreparedToolkitType` decoder verifies
both envelope and inner-command digests plus exact tenant/project/command/input
identity. Output acceptance uses the same decoder; no discovery job table is
required.
`toolkit_discovery_materializer.go` wraps raw discovery settings temporarily and
reuses `materializeCurrentAgentTools`, the trusted prebuilt MCP resolver, and the
existing frozen-configuration walker. It then returns the settings-only shape
required by the protocol. Prebuilt endpoint/headers come from trusted resolution;
frozen secrets use their configuration owner. Caller settings cannot choose the
admitted toolkit type or overwrite the trusted prebuilt endpoint.

## Terminal, resume, and replay boundary

Saved agents and pipelines retain the existing exact application/version target
and visible `mcp` conversation. Stored `agent_type` determines execution style.
The bridge observes the common chat projection: terminal failure, HITL, delegated
authorization, and now `output_limit_reached` are explicit MCP errors. A partial
answer at a continuation boundary cannot be returned as successful final output.
Output-limit responses direct the user to the conversation's existing Continue
flow. Deadline responses report that no final answer was observed and direct the
caller to current conversation state; they do not assert an unobserved running
status after cancellation.

External MCP has no separate resume protocol in this implementation. Existing
conversation continuation owns resume identity, checkpoints, and authorization.
A repeated external `tools/call` is a new invocation because the bridge does not
accept an idempotency key. Durable command/output replay remains owned by the
existing execution and settlement machinery. This slice does not claim that
client retries deduplicate separate calls or prove the browser resume round trip.

## Verification evidence

Focused unit tests cover exact schema preservation, dynamic family selection,
identity/cancellation, invalid schemas, trusted prebuilt resolution, and frozen
configuration ownership. PostgreSQL tests ran without skipping before the schema
freeze: live saved-instance schema projection for all four dynamic families;
opt-in and folder filtering; exact saved application/pipeline versions; projected
success, failure and pause; and rejection of partial output for both execution
styles. Discovery and execution providers in these tests are controlled doubles.
They prove adapter, repository, and projection behavior, not a live provider run.

Test owners are `external_toolkit_schemas_test.go`,
`external_toolkit_schemas_postgres_integration_test.go`,
`external_terminal_postgres_integration_test.go`, existing `execute_test.go`, and
`toolkit_discovery_prebuilt_test.go` in Main. The prepared-command identity/integrity check has focused unit coverage. The
replacement artifact test passed with the complete migration corpus and existing
ledger, with no discovery artifact or job table.
Deployed dynamic/prebuilt/OpenAPI enumeration, saved-agent/pipeline final output,
authorization resume, and replacement/replay remain browser/runtime proof gates
coordinated by the root task.
