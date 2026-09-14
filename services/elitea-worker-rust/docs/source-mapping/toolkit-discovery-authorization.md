# Toolkit discovery authorization

Status: deployed discovery authorization and invocation pass headed-browser acceptance on 2026-09-14. The separate pending-call reload check remains open.

## Source mapping

Current Core references are `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py` and `api/v2/test_toolkit_tool.py`.
They establish saved-toolkit discovery, caller-scoped configuration expansion, and direct tool invocation.
Current SDK `projects/elitea-sdk/elitea_sdk/runtime/utils/mcp_adapter.py::_preflight_auth_check` extracts OAuth metadata from an initialization refusal.
Current UI `projects/EliteaUI/src/[fsd]/features/mcp/ui/modal/McpAuthModal.jsx` supplies the authorization dialog behavior.
The replatform reuses the shared ADK MCP connector and the existing browser OAuth dialog.

Rust `toolkits/direct_runtime.rs` returns the scoped discovery authorization requirement.
`execution/toolkit_delivery_processor.rs` resolves its public metadata with the same helper used by tool calls.
Discovery publishes an immutable result artifact containing empty `tools` and `args_schemas`, plus `authorization_required`.
A challenge is an explicit discovery outcome, not a successful provider call or an empty catalogue.
No credential value enters this artifact or Redis.

Main `application/toolkitdiscovery/service.go` validates the existing authorization domain type.
The toolkit ID and type must match the admitted request; a challenge cannot include tool results.
The existing signed artifact and settlement checks remain in place.
`X-MCP-Authorization-Reference` carries only the server-issued opaque reference on an explicit retry.
The shared call-tool resolver validates its actor, project, toolkit, resource, and immutable revision.
The content materializer redeems it only through the existing claim-authorized token path.

The browser uses `ToolkitTestAuthorization` for discovery as well as tool execution.
The tool list appears after the authorized discovery retry.
The next Toolkit Test request reuses that reference in memory.
Changing project or toolkit clears the test-run reference.
No external MCP approval or interrupt-resume operation is added.

## Typed identity correction

The earlier discovery command contains type and settings-entry identity only.
Rust consequently constructs a synthetic toolkit ID of 1.
`ToolkitAvailableToolsCommandV1.toolkit_id`, field 16, now carries the actual saved toolkit identity.
Fields 3 through 15 remain reserved.
Main takes this ID from the admitted request, never from browser-supplied authorization metadata.
Rust requires a positive canonical ID for MCP discovery.
Older non-MCP discovery commands retain their previous identity fallback for compatibility.
Bindings are regenerated through `scripts/contract/generate_proto.sh` with the pinned tool versions.
No database migration is required.

## Verification scope

Focused Main tests cover public challenge validation, foreign toolkit/type refusal, malformed references, and signed command identity.
Shared content-materialization tests cover token redemption for both discovery and calls, including foreign actors and changed or revoked grants.
The focused browser-component suites pass 31 tests, including challenge-to-tool-list transition through the reference header.
Rust standalone discovery and public challenge projection have focused tests.
These checks do not replace deployed OAuth, reload, or provider execution evidence.

## Deployment correction

The first deployed browser request exposes a missing strict-wire rule for command field 16.
Rust rejects that command before execution with `execution_delivery.incompatible_version`.
`protocol/wire.rs` now admits field 16 and retains the rejection of reserved fields.
All 12 shared-toolkit protocol tests pass with the real toolkit identity in their signed fixture.

A focused UI regression covers authorization lifetime across tool selection and toolkit changes.
Selecting a tool retains the discovery reference; changing the toolkit clears it.
All nine test-run hook tests pass after this correction.

## Headed-browser acceptance, 2026-09-14

A fresh Chrome session creates temporary MCP toolkit 62 from the isolated DCR-public fixture.
The browser receives the discovery challenge and completes real client registration, PKCE consent, and token exchange.
The tool selector shows `Echo marker`; its argument form shows the required `marker` field.
One browser Run Tool request returns HTTP 200 with `ok: true`.
Rust execution `0f3b6e728ac75bd793a992bb9662dbe6` returns `RUST_DISCOVERY_AUTH_BROWSER_20260914`, mode `dcr-public`, and generation 1.
The rendered result contains that marker and no remaining Authorize control.
Toolkit cleanup returns HTTP 204.
Evidence files are `elitea-discovery-auth-invoke.log` and `elitea-discovery-auth-success.png`.

Main image: `sha256:4dd4a04ee22828ed20311bb8a0aa0b24a9ec7d581909cf6a90059baca8fbea5c`.
Rust image: `sha256:5d024c10addbb14f58be732f3e48059df1abaf1ad5dc4d46fcca5588c04ce78d`.
Web image: `sha256:943247c4cc19d28a29df37a6609d7d4bbeefab7eca941822b77f1af6d05d7a7a`.

The browser check also exposes an empty temporary certificate mount in Main.
DCR fails because the configured CA bundle no longer exists.
Main now mounts the existing worker public-trust volume read-only and selects its combined CA bundle.
The fixture certificate verifies against this bundle, which contains 143 public certificates.
Main retains its image, credentials, other mounts, and databases.
The OAuth emulator is not restarted.
Chrome accepts the verified isolated fixture certificate only within its temporary test context.
Main and Rust retain TLS certificate verification.

The schema-read regression confirms that the authorized argument request carries the same opaque reference.
The selected-schema suite passes 10 tests; the test-run hook suite passes nine tests.
The earlier combined picker, schema, and run suites pass 37 tests before the final schema-reference assertion is added.
Full production, replacement-soak, and point 4 or point 5 acceptance are not claimed here.
