# Microsoft Default app registry (issue 2267, ADR-0057, epic 2254 story S1b)

**What changed.** The admin console gains **Configuration › Microsoft apps**, the registry of
Microsoft Entra apps ("Default apps") that Outlook, Teams, Microsoft Graph, Dataverse and
SharePoint credentials will name instead of holding their own client ID and secret. Shared
migration `0255_microsoft_oauth_apps.sql` creates `elitea_identity.microsoft_oauth_apps` and
`microsoft_oauth_app_projects`, and grants the new administration-mode permission
`configuration.microsoft_apps` to `super_admin` and `admin`. A new route set serves the console:
`/api/v2/admin/microsoft_oauth_apps` (list, create, update, disable, enable, test, settings) and a
read for credential forms, `GET /api/v2/microsoft_oauth_apps/{projectID}?credential_type=`.

**Nothing changes until an administrator registers an app.** An empty registry (the default)
changes no credential form and no sign-in. The registry itself is inert: the credential types and
the sign-in that use it ship in the following releases of the same programme.

**What you must do.**

- Run the migration with the release (the chart's migrate job does). Nothing else is required.
- The registry seals each client secret with the vault master key, so it answers 503 on a
  deployment without `SECRETS_MASTER_KEY` (Main already refuses to start without it outside a
  throwaway stack).
- To use Default apps, do the Entra side first and then register an app. **Configuration ›
  Microsoft apps** prints the redirect URI (`https://<host>/api/v2/microsoft_login:callback`, the
  same for web, desktop and mobile), the delegated permissions per credential type and the admin
  consent URL. See `/admin/microsoft-apps` in the docs.

**Helm (optional).** `microsoftOAuthApps` in the chart values seeds apps at Main's start. Each entry
names a Kubernetes Secret for its client secret (`secret: {existingSecret, key}`); the secret is
bound to the env var `ELITEA_MICROSOFT_OAUTH_APP_SECRET_<KEY>` of Main only and never appears in
the values, a ConfigMap, a log or an API response. The rendered `ELITEA_MICROSOFT_OAUTH_APPS` JSON
carries no secret. The default is an empty list, which seeds nothing. A malformed entry stops Main
from starting and names the field. A row an administrator disabled stays disabled across restarts.

**Rolling back.** The two tables are additive and nothing reads them yet; rolling Main back leaves
them in place. Registered apps and their sealed secrets are kept.
