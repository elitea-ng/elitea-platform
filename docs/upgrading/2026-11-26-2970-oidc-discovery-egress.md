# Authentication: OIDC discovery, JWKS and token exchange go through the egress guard (#2970, ADR-0011)

**What changed.** elitea-main now sends every request to an OpenID Connect
identity provider through its egress guard: discovery, the JWKS (signing key)
fetch and the authorization-code token exchange. This covers the provider from
the environment (`OIDC_ISSUER_URL`) and every provider authored on the admin
Authentication page, Entra mode included. The guard re-resolves the host on each
connection, connects only to the address it checked, uses no proxy, and refuses
link-local, cloud-metadata, multicast and reserved addresses whatever the
configuration says. SAML is unchanged: its metadata is pasted, never fetched.

**Who is affected.** Deployments whose identity provider is on a **private
address** (RFC 1918, ULA, CGNAT): an in-cluster Keycloak reached through its
Service, an on-premises AD FS. Identity providers on public addresses (Entra ID,
Google, Okta, Auth0, a public Keycloak) need nothing.

**What to do.**

- Name the identity provider's range or address in the new elitea-main
  variable `ELITEA_IDENTITY_PROVIDER_EGRESS_ALLOWLIST` (egresslib grammar: a CIDR
  permits its block, an IP literal that address, either optionally pinned to one
  port), for example `10.20.0.0/16` or `10.20.0.5:8443`. A host name entry does
  not permit a private address.
  - Helm: `main.env.ELITEA_IDENTITY_PROVIDER_EGRESS_ALLOWLIST` (default `""`,
    which refuses every private address).
- Without it, a private environment provider stops elitea-main at startup with
  `OIDC discovery failed ... destination refused`, and a private provider from
  the admin page makes sign-in unavailable with the same log line.
- The guard ignores `HTTPS_PROXY`. If elitea-main reached its identity
  provider only through a forward proxy, allow it direct egress to the identity
  provider (as webhook and MCP traffic already needs).
- Loopback identity providers are a development affordance:
  `ELITEA_IDENTITY_PROVIDER_ALLOW_LOOPBACK=true` admits them, and a loopback entry
  in the allowlist is ignored without it. A block that covers loopback and more,
  such as `0.0.0.0/0` or `::/0`, fails startup without the flag: list the
  private ranges instead (`10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`). The compose and native development
  stacks set both variables for the bundled OIDC mock. The chart does not expose
  the loopback flag.
