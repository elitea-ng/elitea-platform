# Credentials: Microsoft delegated types and a descriptor-driven sign-in check (issue 2266, ADR-0057)

**What changed.** The credential catalogue gains four types: `outlook`, `teams`, `ms_graph` and
`dataverse`. They are delegated per-user OAuth credentials (Microsoft Entra authorization code), defined
natively in `application/configurations/native_types_microsoft.go`. SharePoint's delegated mode and the
four share one descriptor table, `internal/microsoftauth`, and the delegated connection check reads it:
no token answers `401 requires_authorization` with the Entra metadata, a token that comes with the check
is verified by an identity read (Graph `GET /me`, Dataverse `WhoAmI`). The new types accept only
`https://login.microsoftonline.com/<tenant>` as the authority (Dataverse builds it from `tenant_id`) and
only `https://<org>.<region>.dynamics.com` as the environment. A refresh token is never stored in a
credential's data: fields a type does not define, `refresh_token` and `access_token` among them, are
dropped on save.

**Who is affected.** Operators. The four types are registered `Hidden`, so no client's create picker lists them yet. No
database migration, no contract bump, no new configuration.

**What to do.** Nothing. Main reads `login.microsoftonline.com` (sign-in discovery) and, for an identity
read, `graph.microsoft.com` or the credential's own `*.dynamics.com` host through its existing egress
guard; a deployment that blocks outbound access from Main to those hosts will see the check fail with
"discovery could not be read". The sign-in window, the Default app registry and the toolkits that use
these credentials ship in later stories of the same programme (epic 2254); until then the types stay hidden. SharePoint credentials behave as before.
