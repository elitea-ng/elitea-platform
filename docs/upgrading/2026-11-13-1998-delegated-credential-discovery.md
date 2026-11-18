# Credentials: delegated sign-in reads the identity provider's metadata (#1998)

The check of a credential that signs in per user (SharePoint with an
`oauth_discovery_endpoint`) now answers 401 `requires_authorization` with the
identity provider's OAuth metadata, which is what opens the sign-in window in
the web, desktop and mobile apps. To build that answer Main fetches the
provider's discovery document (`<endpoint>/v2.0/.well-known/openid-configuration`,
then the endpoint as written) through the same SSRF guard as the MCP OAuth
proxies.

Who is affected: deployments whose identity provider is on a private or
internal address (an on-premises ADFS or Keycloak). Public providers such as
`login.microsoftonline.com` need nothing.

What to do: add the provider's host to `ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST` on
`elitea-main`. Without it the check answers 400 `toolkit.oauth_discovery_failed` and the
credential form says the discovery endpoint could not be read.

Also in this change (#2020): a refused chat turn (422
`unsupported_agent_execution`) now carries the reason in `message`. Nothing to
configure; the same reason is in the `current agent execution start failed`
log line.
