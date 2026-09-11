# Toolkit Test delegated reference binding

## Functional reference

Current Core `api/v2/mcp_oauth_proxy.py` resolves credentials from the saved toolkit when its ID is supplied.
The optional provider OAuth resource parameter remains distinct from the platform's saved toolkit identity.
Current SDK OpenAPI execution consumes a bearer token for the configured API resource.
The replatform keeps credentials in Main and supplies only a scoped reference to the browser retry.

## Implementation boundary

Main `internal/api/v2/eliteacore/mcp_delegated_tokens.go` resolves the saved toolkit resource under the authenticated actor and project.
OpenAPI exchanges can omit the optional provider resource parameter.
An explicit resource must still match. MCP resource validation remains unchanged.
The response adds `authorization_resource`, alongside the opaque reference and expiry.
Web `features/mcps/lib/oauthFlow.ts` stores the reference under that authoritative resource.
The provider request does not gain a resource parameter from this change.

Main `internal/infra/storage/toolkit_runtime_context.go` redeems the reference only after live claim authorization.
It projects bearer tokens into `access_token` and optional `session_id` fields.
Rust `src/toolkits/direct_request.rs::valid_materialized_token` owns that exact accepted token shape.
Stored `token_type` metadata does not enter the execution object. Unsupported token types are refused.
No plaintext token enters the browser retry or immutable runtime context.
No database schema changes are required by these repairs.

## Verification history

The initial consent fails with `oauth_resource_mismatch` before reference storage.
After the binding repair, browser consent succeeds and the Test retry contains a reference and the original marker argument.
Execution `1f895a0ede09122df41c5b99ee6d5470` then fails with invalid input.
Main includes `token_type` in the materialized token; Rust's strict parser rejects the extra field.
The projection repair removes that cross-language mismatch.

Focused Main tests cover authoritative binding, omitted OpenAPI resources, explicit mismatches, unchanged MCP refusal, and exact token projection.
They also cover actor/project isolation, changed revisions, revoked grants, and unsupported token types.
Forty OAuth UI tests pass. TypeScript and focused lint pass.
These tests use in-process fixtures. The following section records the separate deployed browser evidence.

## Deployed acceptance, 2026-09-11

The repaired Toolkit Test consent flow succeeds for saved delegated OpenAPI toolkit 27.
The retry carries `mcp_authorization_reference`, the selected `echo_marker`, and the original marker arguments.
It carries no `mcp_tokens` browser payload.
HTTP 200 returns marker `RUST_GATE3_REFERENCE_BINDING_20260911`, mode `stored`, and generation 1.
Execution `d7899b38658fb99e08aca25ab3f3f0d1` is durably `SUCCEEDED`.
The previous invalid-input execution is durably `FAILED`.
The fixture's protected-call count increases from 15 to 16 across the failed and successful attempts.

Main image: `sha256:1b9f9256ab8688070e998782d094d1e1d5a21a12fcc92bed9f049c18e825ce2b`.
Web image: `sha256:11f01a5a095ec7ee8a4e213f6368c7b9863bcec7e12e8a9b1b83fbb6358a6544`.
Rust image remains `sha256:39875097e5f14efdaa55678bc5f09db091d0083d445124b1b4f183d6eb0ebeca`.

This proves the local stored-client delegated Test flow. It does not prove cancellation, replacement, remote-spec authorization, or all MCP variants.

## Browser reload and Skip check: 2026-09-11

The deployed toolkit 27 Test pane returns a safe authorization challenge for execution `5f6d4a3ba638895900584ec9e0806ccd`.
Reload clears the pending action, selected operation, and argument controls.
The page contains no stale Authorize button after reload.
A deliberate new Test run returns a fresh challenge for execution `61ea9554e848599186763ea4488ad1a2`.
Skip displays `Tool run skipped.` and performs no protected invocation.

The synthetic provider's protected-call counter stays at 16 across both checks.
Consent and token counters also remain unchanged.
Both jobs settle as `SUCCEEDED` because the authorization challenge is a valid terminal protocol result.
That state does not mean the protected operation ran.

This check proves pending-authorization invalidation on reload and local Skip behavior.
It does not prove reload recovery of an active invocation, explicit cancellation, or worker replacement.
