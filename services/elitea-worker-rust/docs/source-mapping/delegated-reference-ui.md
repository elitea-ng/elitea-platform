# Browser delegated authorization references

Current Core's OAuth proxy and current UI consent flow define the credential and consent behavior.
The replatform uses Main's scoped grant references for Toolkit Test retries.
Rust receives transient token material through the claim-authorized content endpoint.

Web `features/mcps/api/mcpOAuthClient.ts` accepts a reference-only exchange request and its grant response.
`features/mcps/lib/oauthFlow.ts` validates reference responses separately from browser-token responses.
`authorizationReference.ts` stores only bounded, expiring references keyed by project, toolkit, and resource.
An explicit reference-only request never falls back to storing provider access or refresh tokens.
`McpAuthModal.tsx` and `authModalHelpers.ts` pass the mode through the existing consent flow.

Main returns `authorization_resource` from its saved toolkit settings.
The OpenAPI flow uses this resource for local reference lookup without adding an OAuth resource parameter to the provider request.
MCP keeps its existing resource indicator.
The existing browser-token flow remains available to callers that do not request reference-only mode.

Focused OAuth flow and modal-helper tests pass with 40 cases in the working tree.
The isolated candidate passes OAuth flow, modal-helper, and reference-storage tests, plus TypeScript and focused lint.
The deployed Toolkit Test proof is recorded in the separate `toolkit-test-reference-binding.md` mapping.
