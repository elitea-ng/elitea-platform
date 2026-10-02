# Toolkit Test authorization UI

## Source mapping

| Current platform source | New platform owner | Preserved behavior |
| --- | --- | --- |
| `projects/EliteaUI/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js` | Web `features/toolkits/lib/hooks/useToolkitChatDispatch.hooks.ts` | Forward the authorization challenge to the existing callback. |
| `projects/EliteaUI/src/[fsd]/features/mcp/lib/hooks/useMcpAuthModal.hooks.js` | Web `features/mcps/model/useMcpLogin.ts`, `McpAuthModal` | Reuse the existing OAuth dialog and completion callback. |
| `projects/elitea-sdk/elitea_sdk/runtime/toolkits/tools.py`, authorization exception enrichment | Rust direct execution and Main toolkit result projection | Keep the toolkit identity with the authorization challenge. |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/test_toolkit_tool.py` | Main toolkit Test route; Web `features/toolkits/api/toolkitTestRun.ts` | Run one named operation with explicit arguments. |

Web paths are relative to `apps/elitea-web/src`.
Rust and Main own the typed challenge and invocation admission. The browser does not plan another operation.

## Implementation history

The September 9, 2026 follow-up adds the HTTP 409 authorization outcome to the Test API adapter.
The adapter requires a bounded task identifier, valid resource URL, optional metadata object, and matching saved toolkit ID.
Missing or foreign challenges produce a failure instead of opening an authorization dialog.

The active editor renders `TestToolPane` through `ConfigurationTab`. It does not currently mount the older `TestTools` transcript surface.
The page supplies `renderTestAuthorization`. This renderer composes the existing `useMcpLogin` and `McpAuthModal` components.
The older transcript callback also receives the typed challenge and explicit retry and Skip callbacks.

The pending action retains a copy of the operation name and arguments.
OAuth completion selects a saved `authorization_reference` for the exact project, toolkit, and resource.
Expired or missing references cannot retry the operation. Main also verifies actor and scope during redemption.
The transcript preserves the selected model name and nonsecret temperature, maximum-token, and reasoning-effort controls.
These values remain explicit inputs; they do not add a model-planning request.
Retry sends `mcp_authorization_reference`; it does not send a browser access-token map, settings, or a model request.
The Test dialog requests `authorization_reference_only: true`. Its exchange response contains no access, refresh, identity, or session token.
The browser stores a dedicated reference and server expiry record, without a placeholder access token.
Other consumers retain their existing OAuth response contract.

Skip clears the pending action locally. It does not dispatch an invocation or store an authorization-ignore preference.
A project change, toolkit change, new run, reset, or unmount invalidates an older pending completion.
A consumed completion cannot retry twice. A subsequent authorization challenge requires another explicit authorization action.

## Verification

Focused tests cover frozen arguments, reference-only retry, duplicate completion, local Skip, and project changes.
Challenge tests reject missing, malformed, and foreign toolkit identities.
Reference tests enforce project, toolkit, resource, and expiry matching.
Page tests verify existing OAuth action wiring, missing references, and local Skip.
OAuth tests verify persistence of the server reference from code exchange.
The Web typecheck and focused Test, transcript, OAuth, and storage suites pass.
The deployed browser OAuth exchange now passes through Rust.
See [reference binding evidence](toolkit-test-reference-binding.md) for the execution identifier and persistence check.
Cancellation, reload, and replacement proof remain separate gates.

The September 11 follow-up updates three test expectations for request identities.
An authorization retry uses a new request identity with the same frozen operation and arguments.
Tests retain exact request-body assertions and reject duplicate completion dispatch.
