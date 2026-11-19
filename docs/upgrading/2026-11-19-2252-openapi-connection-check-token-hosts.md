# Connection checks for OpenAPI client credentials and SharePoint tenants (issues 2251, 2252)

**What changed.** `Test connection` now runs for an `openapi` credential that holds a client ID, a client
secret and an https `token_url`: it requests a client-credentials token from that URL, the way the SDK
does, and answers `auth_failed` when the provider refuses the client. A `sharepoint` client-secret
credential may now carry its own `tenant_id` (a tenant ID or domain) and then needs no site: the check
signs in as the app and reports success on that alone. Before, both answered `unsupported_type`.
No migration, no contract change.

**Who is affected.** Operators who set `ELITEA_TOOLKIT_CHECK_ALLOWLIST`, which REPLACES the built-in list.
A token URL is typed by the tenant, so its host is on no built-in list: until it is named, the OpenAPI
check answers `unreachable` ("refused by the egress policy of the toolkit connection check"). It is a
warning beside Save for no one, as `unreachable` never warns.

**What to do.** If you want OpenAPI checks for your tenants, add the identity-provider hosts their token
URLs use (for example `login.microsoftonline.com`, `oauth2.googleapis.com`, your Auth0 domain). Otherwise
nothing: the check stays harmless and Save is never blocked.
