# Toolkit discovery authorization

Status: deployed discovery authorization, invocation, and pending-call reload pass headed-browser acceptance on 2026-09-14.

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

## Pending-call reload correction

A browser fault injection omits one opaque reference after successful discovery authorization.
The server returns the expected HTTP 409 challenge; it does not invoke the protected tool.
Reload recovers the original call but initially shows both discovery and call authorization controls.
The pending call now owns the single control until its authorization resolves.
Its immutable arguments remain in the existing server result contract.
A successful retry retains the new reference for subsequent discovery and argument reads.
An authorized remount refreshes discovery despite the production 30-second cache period.
Eight pane tests pass, including the duplicate-control and cache regression with production cache timing.
This change does not alter external MCP, HITL graphs, or sensitive-tool decisions.

## Completed pending-call browser acceptance

A fresh headed Chrome session creates temporary toolkit 67 and completes discovery OAuth.
The test omits one opaque reference from the first Run Tool request to trigger the real authorization boundary.
Execution `8bf9682a0c85465d1bc497716f522dab` returns HTTP 409 and the expected public challenge.
Reload reads the saved execution and displays exactly one Authorize control.
The user completes fixture consent again without selecting a tool or re-entering its arguments.
Execution `a5f8c215b61f52d6f773b79b970da060` returns HTTP 200, `ok: true`, and the original marker.
The result is visible, the tool selector returns, and no Authorize control remains.
Toolkit cleanup returns HTTP 204.
Evidence files are `elitea-native-auth-reload-final.log` and `elitea-native-auth-reload-success.png`.
Web image: `sha256:d854a9c8a0daaf50606e9acd2783c10b4ed04c03da28346ac746974377b5d428`.
Main and Rust retain the images recorded above.
TypeScript checking, focused lint, and all eight pane tests pass.

This proof covers a received challenge followed by reload.
It does not cover connection loss before the browser receives an execution identity.
