# External Elitea-as-MCP source mapping

Status: capability-disabled catalogue and direct execution are implemented for
saved agent and pipeline versions and for selected read-only toolkit
operations. The UI now gives agent and pipeline versions an explicit MCP
access control that writes the existing exact `mcp` tag contract. A local
full-stack proof covered toolkit discovery, one OpenAPI call, durable
settlement, result return, and immediate opt-out. Saved-agent and pipeline
proofs also reach two independent nested authorization pauses. Authorization
resume and terminal completion remain live-test gates. Effectful toolkit
operations remain closed.

This surface lets an external MCP client, such as Codex, Claude, Cursor, or VS
Code, use project capabilities. It is distinct from:

- the operator-managed prebuilt MCP catalogue;
- internal Elitea MCP endpoints that let an Elitea chat call Main operations;
- configured remote MCP toolkits that the Rust agent runtime consumes.

## Current-platform contract

Current-platform evidence was rechecked on 2026-09-04 against `elitea_core`
revision `6a036d777ca909fac377ceaec05719f0fa611b6d`. The current release includes
issue 6273 through merge `6598f8951b0c8bda0969319168b778e51c0fe5b7` and
security follow-up `88d2dfa12bdf2a668363a66f93b5ca6c5cd4892b`.

| Current source | Observable contract | Replatform owner |
| --- | --- | --- |
| `utils/application_tools.py::toolkits_listing` | `filter_mcp=None` includes ordinary toolkits and connected remote-MCP toolkits. The default UI listing behavior remains unchanged. | Main external MCP catalogue query |
| `utils/mcp_service.py::__get_all_tools` and `__get_toolkit_tools` | Publish only selected operations from rows with `meta.mcp_options.available_by_mcp=true`. Use sanitized MCP tool names and saved toolkit schemas. | Main `internal/api/v2/mcp/catalog.go` |
| `utils/mcp_service.py::__get_toolkit_by_name` | Reapply the opt-in at call time and refuse an ambiguous sanitized name. Issue 6273 prevents connected MCP rows from disappearing before this gate. | Main catalogue target identity plus `internal/application/toolkitexecution` re-read |
| `methods/runtool.py` and SDK `runtime/clients/client.py::test_toolkit_tool` | Invoke the exact saved toolkit operation directly. Do not add an LLM planning turn. | Main durable direct-tool service and Rust direct execution kernel |
| `utils/mcp_handler.py` and `utils/mcp_service.py::__get_all_tools` | Resolve the literal `mcp` tag and expose application versions selected by that tag as one-task MCP tools. Agents and pipelines share application-version storage. | Main `agentTools`; web agent and pipeline MCP access controls |
| `utils/mcp_service.py::__get_application_tools` | A resource-scoped agent, pipeline, or application endpoint exposes the exact named version without requiring the project-wide tag. | Main `agentToolForVersion` and aliased resource scopes |
| `utils/mcp_service.py::__handle_call_tool_request` | Run the selected version through `do_predict` with the caller task and return the terminal chat response. | Main durable application execution bridge; Rust execution style selected by the stored version discriminator |
| EliteaUI `ApplicationEditForm.jsx` and `TagEditor.jsx` | The current UI can edit the generic version tag list. The literal `mcp` tag is the exposure switch but is not a dedicated agent or pipeline control. | Web `ApplicationMcpAccessToggle`, backed by the same tag list |

The replatform deliberately strengthens collision handling. The current Python
project-wide list can retain the first colliding name while its dispatcher
refuses the ambiguous call. Main omits every colliding project-wide descriptor,
so discovery cannot advertise an unusable target. A resource-scoped toolkit
endpoint remains unambiguous because its URL pins one row.

The replatform makes application and pipeline exposure explicit in the UI but
does not introduce a second persistence flag. Enabling the control adds the
exact `mcp` tag. Disabling it removes only that tag and preserves all other
version tags. The project-wide catalogue selects the newest tagged version for
an application deterministically. A resource-scoped endpoint stays pinned to
the version ID in its URL and does not require the tag, matching the current
platform.

## Catalogue ownership

The project-wide catalogue selects application versions tagged `mcp` and
toolkit rows opted into `available_by_mcp`. It publishes only saved selected
operations. Built-in toolkit operations use the exact argument schema from
Main's digest-pinned SDK snapshot. Dynamic `mcp`, `mcp_config`, and `openapi`
operations use an explicit open-object fallback until their per-instance schema
projection is available.

The catalogue applies the current `folder_exclusion_clause` behavior. It drops
objects in actor-specific `no_access` folders before discovery. Project RBAC
remains authoritative when the optional access projection is absent. A partial
or invalid projection fails closed. Resource-scoped endpoints apply the same
rule, so an object ID cannot bypass visibility.

The catalogue query is a bounded translation of the current SQLAlchemy reads.
It must select `p_<project>` at runtime. PostgreSQL cannot bind a schema
identifier as a query value, so this one repository validates and quotes that
identifier before it runs the query. Stable shared-schema execution, outbox,
result, settlement, and current-row reads use named sqlc queries. No raw SQL is
owned by the MCP handler or application service.

## Direct execution boundary

The following section describes the Rust read path. Main also supports
`toolkit.call_tool.v1` for toolkit Test and Python external-MCP execution.
Rust must implement that broader contract; see [`toolkit-test.md`](toolkit-test.md).
The read-only restriction is a staged safety gate, not final functional parity.

Each descriptor keeps its exact toolkit row ID and original operation name in
private server fields. `tools/call` passes that target to Main. Main then:

1. authorizes the actor with the current chat-run permission;
2. re-reads the row through `GetCurrentMCPToolkitVisibleToActor`;
3. rechecks folder visibility, `available_by_mcp`, selection, and guardrails;
4. resolves the saved configuration and freezes one bounded snapshot;
5. admits a signed `toolkit.execute.read.v1` command and waits for its fenced
   terminal result.

The second read closes the discovery/call race. Removing the opt-in or selected
operation after `tools/list` refuses the call before dispatch.

Rust accepts exactly one materialized toolkit and one original operation. It
enumerates the toolset once and invokes the exact target outside an LLM loop.
It refuses missing or duplicate tools, invalid arguments, oversized results,
unprojected ADK user scopes, and any operation whose runtime
`Tool::is_read_only()` value is false. Delegated authorization remains a typed,
redacted failure. Main accepts only a generation-fenced result whose toolkit
type, toolkit name, and operation match the admitted snapshot.

Agent and pipeline descriptors keep the exact application and version IDs in
private server fields. A call creates a visible conversation with source
`mcp`, addresses the stored version, and enters the same durable application
start use case as the chat surface. Main waits for the projected terminal
response and returns it as the MCP result. A projected failure or a durable
HITL or delegated-authorization pause is explicit. The bridge does not wait
until its deadline. The stored `agent_type` chooses the Rust execution style,
so an agent loop and a pipeline graph share the bridge without losing their
runtime distinction.

Configuration expansion opens a project vault only when it encounters an
actual `{{secret.NAME}}` placeholder. Clear-text and credential-reference-only
configurations do not depend on a vault key. A real placeholder still opens the
owning vault and fails closed if the vault is unavailable. This is an
intentional availability hardening over the current eager `VaultClient`
construction; it does not bypass secret redemption.

Migration `0122_toolkit_execute_read.sql` extends both earlier allowlists. It
admits the direct-tool protobuf media type through
`input_bundle_entries_content_size` and admits
`TOOLKIT_EXECUTE_READ_RESULT` through `output_inbox_payload_type`. Its isolated
PostgreSQL upgrade test proves both constraints before it exercises admission.

## Verification

The focused Main corpus covers agent and pipeline tag opt-in, project and exact
resource scopes, actor folder access, schema projection, name collisions,
exact application/version and toolkit targets, permission refusal, projected
failure and pause handling, redacted admission errors, cancellation,
empty-result refusal, durable reference-only admission, Redis envelope replay,
strict claim input, terminal result identity, and the migration upgrade path.

The focused UI corpus covers the explicit agent and pipeline controls, stored
state, read-only behavior, Zod form preservation, create and update bodies, and
the empty-tag removal operation. The generated version request remains the
single wire contract.

The focused Rust corpus covers strict command parsing, exact input media type,
claim-bound materialization, immutable toolkit and argument identity, runtime
read-only enforcement, bounded invocation, typed authorization failure, output
binding, Redis retirement, and failure settlement.

The local standalone stack proved the full external path on 2026-09-04. An
external `tools/list` discovered one temporarily opted-in OpenAPI operation.
`tools/call` traversed Main admission, signed Redis delivery, Rust claim and
materialization, the OpenAPI HTTP call, durable output, Main result ingestion,
and the MCP response. The returned marker matched the request. After the test
removed `available_by_mcp`, a new `tools/list` omitted the operation without a
restart. The test restored the original row and retained no credential or
temporary client file.

The current-platform issue 6273 dispatcher tests pass in the checked checkout.
Its older listing fixture currently needs a `VaultClient` import stub after a
later configuration change. The production source, the replatform catalogue
tests, and the live opt-in/opt-out proof cover the listing rule used here.

The saved-pipeline proof uses the current `Resolve Name` pipeline and its
`Full Name Resolver` Agent node. Execution
`b13c0d177a1da5f35b6c689e88ed8661` starts the two leaf model requests
386 microseconds apart. The session stores separate application branches and
provider call IDs. Its graph checkpoint binds two distinct authorization
interrupt IDs. Main returns the authorization pause to the external client.
The outer execution commits a successful settlement and releases its claim.

The proof restores the temporary MCP exposure and selected child-version
reference. The checkpoint remains durable evidence and consumes no active
worker claim. This proves the pause path, not authorization resume or final
pipeline completion.

Verification on 2026-09-06 passes 849 Rust library tests and 83 integration or
contract tests. The two PostgreSQL state tests run with isolated databases.
Rust formatting, Clippy, and documentation checks pass. Focused Main tests and
Go vet pass. MCP and repository service tests also pass with PostgreSQL.
Regenerated SQL, OpenAPI, and protobuf bindings produce no changes.

The UI passes 128 focused tests, type checking, and lint for changed files.
Full UI lint reports nine errors in unchanged authorization files. These
errors remain a separate UI cleanup item. The live evidence above dates from
2026-09-04; these later checks do not repeat the provider calls.

## Remaining gates

- Keep effectful and sensitive toolkit operations closed until durable effect
  identity, receipt, replay, and authorization policy are complete.
- Project live schemas for dynamic MCP, prebuilt MCP, and OpenAPI instances.
- Complete live external agent and pipeline success, failure, HITL,
  authorization-resume, and replay proofs. Parallel authorization pause is
  proved for the saved-agent and pipeline entrypoints.
- Add broader external-client, restart, load, and Kubernetes evidence before
  production capability registration.
- Prove the implemented Main and UI OAuth/DCR flow with a live external MCP
  provider. Rust receives only claim-scoped credentials and safe metadata.
- Use the separate
  [`delegated-oauth-dcr.md`](delegated-oauth-dcr.md) ledger for this flow.
