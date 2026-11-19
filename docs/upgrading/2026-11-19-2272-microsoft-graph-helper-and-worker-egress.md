# Worker egress: Microsoft Graph and Dataverse hosts (issue 2272, ADR-0057)

**What changed.** Three delegated Microsoft toolkit types now have worker egress rules, ahead of the
toolkits themselves (epic 2254, stories 2257 to 2260):

- `outlook`, `teams` and `ms_graph` toolkits open `graph.microsoft.com` for the run, like `sharepoint` does.
- A `dataverse` toolkit opens the single host of its credential's `url`. The URL must be an
  `https://<org>.<region>.dynamics.com` origin (no path, no port, no user info); a sovereign cloud
  (`.dynamics.cn`, `.microsoftdynamics.us`) or any other host refuses the run with `EGRESS_POLICY_INVALID`.
- `login.microsoftonline.com` is not a worker host for these types. The credential's discovery endpoint adds
  no host, because a worker never calls a token endpoint.

The worker also stops receiving the credential fields that could mint a token. For the four delegated-only
types (`outlook`, `teams`, `ms_graph`, `dataverse`) the claim and the frozen run input carry no
`client_secret` and no `refresh_token`: a worker holds an access token only. SharePoint credentials are
unchanged. The shared Rust Microsoft Graph helper (token lookup, 401 then one refresh request to Main then one
retry, `Retry-After` backoff, paging on the same host, `$batch`) ships with no tool using it yet.

**Who is affected.** Operators who restrict worker egress with an allowlist. The four types are still hidden
from every create picker, so no run uses these rules yet. No database migration, no contract bump, no new
configuration.

**What to do.** Nothing today. When the Outlook, Teams, Microsoft Graph and Dataverse toolkits ship, a
deployment with a strict platform allowlist must keep `graph.microsoft.com` and the organisation's Dataverse
host reachable from workers.
