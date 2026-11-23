# Re-enter the SMTP password or MCP client secret when its destination changes (#2944, ADR-0059)

**What changed.** Two administration saves now refuse to keep a stored
credential when the save changes where that credential is sent:

- `PUT /admin/email/administration`: a save that changes the stored SMTP
  `host` or `port` while a password is in force (sealed in the platform vault,
  or supplied by `SMTP_PASSWORD`) and that has no `password` field answers
  `400` with `{"field": "password", "error": "re-enter the password when the
  relay changes: …"}`. Nothing is written. The same save with the password, or
  with `"password": ""` to clear it, still lands.
- `PUT /admin/mcp_prebuilt_servers/administration/{key}`: a save that moves the
  origin (scheme, host and port) of `url`, `base_url` or `token_url` while the
  entry has a stored client secret and that has no `client_secret` field
  answers `400` with `{"field": "client_secret", "error": "re-enter the client
  secret when the server origin changes: …"}`. Nothing is sealed or written. The
  same save with the secret, or with `"client_secret": ""`, still lands.

Saves that keep the host and port (e-mail) or every origin (MCP), for example a
new sender address, a new path or a new display name, keep the stored
credential as before.

The web Admin › E-mail editor and the MCP servers dialog show the reason under
the password or client secret field.

**Why.** `platform.email.manage` and `mcp.catalogue.manage` (#2942) are meant to
be granted on their own. A holder of only one of them was never shown the
credential, and could otherwise read it out by pointing the relay or the server
at a host it runs.

**Who is affected.** Administrators and scripts that change the SMTP relay or a
pre-built MCP server's URLs without sending the credential. No migration, no
configuration change.

**What to do.** When you change the relay host or port, or move a pre-built MCP
server to another origin, send the password or client secret in the same save.
A script that relied on the old behaviour gets a `400` naming the field.
