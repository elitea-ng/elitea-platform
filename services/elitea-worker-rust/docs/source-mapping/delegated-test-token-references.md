# Delegated OAuth references for toolkit Test

## Source and ownership

The current OAuth proxy returns access tokens to browser consumers.
Main now also stores an immutable encrypted grant for saved MCP and OpenAPI toolkits.
The Test retry carries an opaque reference.
It does not copy browser token maps into durable execution input.

| Current source | Target source | Contract |
| --- | --- | --- |
| Main `internal/api/v2/eliteacore/mcp_oauth_proxy.go` | `mcp_delegated_tokens.go` | Save a grant only after a successful provider exchange. |
| Main `internal/mcpoauth/clients.go` | `internal/mcpoauth/tokens.go` | Keep registered client secrets separate from delegated access grants. |
| Main `internal/api/v2/secrets/handler.go` | Main `internal/api/mcp_oauth_clients.go` | Reuse the deployment master key. |
| Rust `src/toolkits/mcp.rs` | Main `internal/mcpoauth/token_resource.go` | Bind MCP grants to the resolved `url`. |
| Rust `src/toolkits/families/openapi/config.rs` | Main `internal/mcpoauth/token_resource.go` | Match top-level base URL precedence and delegated URL formatting. |
| Rust `src/toolkits/families/openapi/spec.rs` | Main `internal/mcpoauth/token_resource.go` | Resolve the first root server and its default variables. |
| Main `internal/application/toolkitcalltool/resolver.go` | Main `internal/infra/storage/toolkit_runtime_context.go` | Validate references at admission. Redeem them after live claim authorization. |

## Storage boundary

Shared migration `0130_mcp_oauth_tokens.sql` owns the delegated grant records.
The existing product vault exposes regular and hidden names through project-scoped secret resolution.
It cannot enforce actor and toolkit ownership for these grants.
The existing DCR store owns client registration secrets and renews their idle lifetime.
It does not own delegated grant revision, revocation, or toolkit identity.

Each grant has a random 256-bit reference and revision 1.
A new exchange creates a new reference.
No operation renews or overwrites an existing grant.
AES-GCM authenticates the reference, revision, actor, project, toolkit, resource, and expiry.
HKDF derives a separate encryption key from the existing deployment master key.
There is no unencrypted storage fallback.

The grant stores the access token and optional MCP session identifier.
It does not store refresh tokens or registration secrets.
PostgreSQL controls expiry and revocation across Main replicas.
Expiry follows the provider duration, with a one-day maximum.
An absent provider duration gives the grant a five-minute lifetime.
Each exchange removes at most 128 expired grants.

## Resource authority

MCP uses the resolved saved toolkit `url`.
Main resolves prebuilt templates before it derives the resource.
OpenAPI uses the first non-empty top-level `base_url` or `base_url_override`.
Otherwise, it reads the first present `spec`, `schema_settings`, or `openapi_spec`.
The specification can contain an inline object, JSON, or YAML.
Its first root server supplies the resource URL.
Server variables use their string defaults.
The native adapter does not use operation or path server overrides.

The OAuth request resource must match this saved authority after canonical formatting.
Main never treats a specification download URL as the resource.
It refuses user information, query strings, fragments, insecure transport, and ambiguous URL encodings.
It also refuses dot segments and international hostnames in this grant path.
These inputs need explicit normalization parity before this path can accept them.

## API and claim behavior

A successful saved-toolkit exchange adds `authorization_reference`, `authorization_revision`, and `authorization_expires_at`.
Toolkit Test sends `authorization_reference_only: true`.
This mode returns no access token, refresh token, identity token, or session credential.
It fails closed when token storage is unavailable.
Other browser OAuth consumers retain their existing response fields.
The Test request submits `mcp_authorization_reference`.
The immutable runtime context stores the reference and exact toolkit resource binding.
It contains no access token, refresh token, or MCP session credential.

Admission checks the authenticated actor, project, saved toolkit, resource, expiry, and revocation state.
The content endpoint checks the active command claim before it calls the materializer.
Only this authorized materialization path loads the access token.
The worker receives credentials through the authenticated transient content response.
Redis carries no token values.

A reference can support multiple authorized executions before expiry or revocation.
It is not a single-use OAuth code.
Claim fencing controls execution replay.
Reference substitution across actors, projects, toolkits, resources, or ciphertext rows fails closed.

## Verification history

On 2026-09-09, isolated PostgreSQL tests passed for encryption, replacement, scope substitution, ciphertext replay, expiry, revocation, and cancellation.
The migration ran twice in a new test database.
The test removed that database after completion.
The application database and running services did not change.

Resource tests cover MCP URL formatting and OpenAPI override, alias, inline JSON, YAML, and server-variable behavior.
OAuth handler tests cover successful binding, foreign resources, hidden toolkits, provider rejection, malformed tokens, expired tokens, and storage failure.
Reference-only tests prove that credentials stay outside the browser response.
The Go package tests and focused `go vet` checks passed.
Claim tests belong to the toolkit Test producer and materializer change.
This document does not claim deployed browser or provider proof.
