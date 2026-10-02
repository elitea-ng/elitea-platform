# Main-owned delegated token references

## Ownership and source mapping

Current Core `api/v2/mcp_oauth_proxy.py` resolves stored toolkit credentials for OAuth exchange.
Current SDK receives a prepared token mapping when it constructs toolkit clients.
Main keeps the replacement token authority. Rust receives credentials only through claim-authorized input materialization.

`internal/mcpoauth/tokens.go` stores encrypted, immutable, expiring access grants.
References bind project, actor, toolkit, and canonical resource. New grants receive new random references.
`internal/mcpoauth/token_resource.go` resolves supported resources from authoritative toolkit settings.
`internal/api/v2/eliteacore/mcp_delegated_tokens.go` exposes reference-only token exchange without browser access or refresh tokens.
The API composition root supplies the store using the existing deployment master key.
An independent derived encryption key separates access grants from confidential DCR registration secrets.

## Storage requirement

Migration `0130_mcp_oauth_tokens.sql` adds an auth-owned ephemeral grant table.
Existing toolkit and credential rows remain unchanged.
The existing DCR client table owns registration secrets, not user access grants or their expiry and revocation.
A separate grant ledger permits another Main replica to validate and redeem the same scoped reference.
Browser storage and process memory cannot provide this ownership across replicas.
The migration already exists in the rehearsal deployment; this delivery records its source.
It does not add another table during the resource-binding repair.

## Failure contract

Explicit resource mismatches, hidden toolkits, provider failures, and failed storage do not produce usable references.
OpenAPI exchanges may omit the optional provider resource parameter; Main derives their stored binding from toolkit settings.
MCP exchanges still require their resource indicator.
The response includes `authorization_resource` so the UI can key the reference independently of provider parameters.
Remote OpenAPI documents without an explicit authoritative base URL remain outside this resource resolver.

## Verification

The isolated commit candidate passes focused token-resource and API exchange tests. Go vet passes for both owning packages.
`TestDelegatedTokenReferencesScopeExpiryRevocationAndReplacement` passes against a disposable PostgreSQL database, without skips.
It verifies encrypted storage, another store instance, foreign bindings, wrong keys, ciphertext swaps, expiry, revocation, and cancellation.
Toolkit Test's separate materializer and browser integration are recorded in `toolkit-test-reference-binding.md`.
This foundation alone does not prove end-to-end Test authorization.
