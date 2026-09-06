# Delegated OAuth and DCR source mapping

Status: Main's OAuth and DCR proxies are implemented. The existing UI flow can
use them. Live provider proof remains required.

This flow supports remote MCP servers and delegated toolkit authentication.
SharePoint and OpenAPI use the same authorization contract.

## Ownership

| Layer | Responsibility |
| --- | --- |
| UI | Discover authorization metadata, open the popup, create PKCE material, call DCR, exchange codes, store tokens, and refresh tokens. |
| Main | Authorize the caller, resolve stored toolkit credentials, proxy token and DCR requests, and return bounded safe responses. |
| Rust worker | Emit durable authorization interrupts and consume claim-scoped tokens after approval. |

Rust never receives a stored OAuth client secret from this browser proxy.
Main expands the secret only for one outbound token request.

## Current-platform evidence

Current-platform evidence was checked on 2026-09-04. The `elitea_core`
revision was `6a036d777ca909fac377ceaec05719f0fa611b6d`.

| Current source | Observable contract | Replatform source |
| --- | --- | --- |
| `models/pd/mcp_oauth.py::McpOAuthTokenRequest` | Accept authorization-code and refresh grants. Preserve `used_dcr` for both grants. | `internal/api/v2/eliteacore/mcp_oauth_proxy.go` |
| `api/v2/mcp_oauth_proxy.py::ProjectAPI.post` | Resolve omitted toolkit credentials. Read SharePoint and OpenAPI nested configuration. | Main resolver and proxy |
| `utils/mcp_oauth.py::{exchange_token,refresh_token}` | Send form data. Accept JSON or form token responses. Apply a 30-second timeout. | Main proxy HTTP boundary |
| `models/pd/mcp_oauth.py::McpDynamicClientRegistrationRequest` | Accept RFC 7591 client metadata. | Main DCR request model |
| `utils/mcp_oauth.py::register_dynamic_client` | Supply current defaults and forward optional registration fields. | Main DCR proxy |
| EliteaUI `mcpAuthFlow.helpers.js` | Run popup, PKCE, DCR, exchange, persistence, and refresh flows. | `apps/elitea-web/src/features/mcps/` |

The current platform prevents stored credentials from replacing DCR-issued
credentials. Main keeps the same `used_dcr` rule.

Main can still resolve an omitted toolkit scope for a DCR request. It never
loads that toolkit's client identifier or secret for the request.

## Main credential boundary

Main reads one actor-visible toolkit through the generated tenant repository.
The repository applies the current folder-access overlay.

Main then uses the existing claim-mode configuration resolver. This resolver
expands configuration references and vault references.

The OAuth handler owns no raw SQL. It also creates no second vault or
configuration expander.

Stored settings can use top-level fields. They can also use
`sharepoint_configuration` or `openapi_configuration`.

Prebuilt MCP settings use the existing catalogue resolver. Main validates the
stored toolkit type before it reads those settings.

## Endpoint safety

Caller-supplied and DCR-issued credentials can use any validated HTTPS token
endpoint. Loopback HTTP remains available for local development.

Main sends a stored toolkit secret only to a bound endpoint. An exact stored
`token_endpoint`, `token_url`, or `oauth_token_endpoint` creates that binding.

An `oauth_discovery_endpoint` also creates a same-origin path binding. This
supports the configured Azure tenant base URL used by SharePoint.

Main rejects a stored secret for an unbound endpoint before transport. This is
an intentional security correction over the current unrestricted proxy.

Redirects must keep the original origin. TLS verification remains enabled.

## Wire and failure contract

OAuth requests are limited to 64 KiB. Provider responses are limited to
512 KiB. Both proxies use a 30-second request context.

The token proxy supports `authorization_code` and `refresh_token`. It forwards
PKCE, scope, client data, and the required grant value.

The token proxy accepts JSON and form-encoded provider responses. It returns
only token fields used by the UI.

The proxy never returns a stored `client_secret`. Provider errors contain only
bounded sanitized descriptions.

The DCR proxy forwards RFC 7591 fields. It supplies these defaults:

- `grant_types`: `authorization_code`, `refresh_token`;
- `response_types`: `code`;
- `token_endpoint_auth_method`: `none`;
- `application_type`: `web`.

DCR can return a provider-issued client secret. The UI needs that secret for a
later token exchange and refresh.

## Verification

Main tests cover nested OpenAPI credentials, SharePoint scopes, DCR isolation,
bound endpoints, unbound endpoints, and actor identity.

Main tests also cover JSON and form responses, RFC 7591 fields, redirects,
request bounds, response bounds, and redacted failures.

Application tests prove claim-mode resolution and absent-row behavior. The
runtime composition reuses the sqlc repository and existing resolver.

The replatform UI already has focused DCR, exchange, refresh, and `used_dcr`
tests. A live provider test remains required before production activation.

## Remaining gates

- Prove one configured remote MCP DCR flow through the browser and Rust resume.
- Prove one stored SharePoint or OpenAPI delegated flow through both grants.
- Prove logout and concurrent-tab behavior against the replatform stack.
- Publish both proxy schemas in Main's OpenAPI document.
- Add load and Kubernetes evidence before production capability registration.
