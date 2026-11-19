# Microsoft sign-in: start, callback and handoff (issue 2268, ADR-0057, epic 2254 story S1c)

**What changed.** Main gains the server side of a Microsoft sign-in for the Outlook, Teams,
Microsoft Graph, Dataverse and delegated SharePoint credentials, for Custom app and Default app:

- `POST /api/v2/microsoft_login:start` builds the Microsoft Entra authorization URL for a saved
  credential (PKCE S256, a random state bound to the user, project, credential, app and client kind).
- `GET /api/v2/microsoft_login:callback` is the one https redirect URI an Entra app registers. It is
  public by design (no session), exchanges the code in Main with the client secret and the PKCE
  verifier, and parks the result as a pending handoff for 60 seconds. It attaches nothing to anyone.
- `POST /api/v2/microsoft_login:redeem` hands the access token to the person who started the
  sign-in (session on web, the verifier of the client challenge on desktop and mobile).

Shared migration `0256_microsoft_login.sql` adds `elitea_identity.microsoft_login_states` and
`microsoft_login_handoffs`. No token or secret is stored in plaintext (AES-256-GCM under a key
derived from the vault master key), and no refresh token is kept or returned.

**Behaviour change.** The platform setting `microsoft.custom_apps` (Allow Custom app) is now enforced
at sign-in as well as in the credential form: with it off, a credential that signs in with its own
client is refused with `custom_apps_disabled`. Default app credentials are not affected. Nothing is
turned off by this release; the setting stays on by default.

**What you must do.**

- Run the migration with the release (the chart's migrate job does).
- The sign-in needs the vault master key (`SECRETS_MASTER_KEY`) and a public origin Entra accepts as a
  redirect URI (https, or localhost for development). Without either, the three routes answer 503.
- Egress: Main reaches `login.microsoftonline.com` (token endpoint), `graph.microsoft.com` and a
  Dataverse environment's `*.dynamics.com` host through the same egress guard as the Microsoft
  connection check. If you restrict that guard with `ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST`, nothing new is
  needed for these public hosts.
- In Entra, an app registration needs the **Web** redirect URI
  `https://<host>/api/v2/microsoft_login:callback` and nothing else (no mobile or desktop platform).
  Configuration > Microsoft apps prints it for your host.

**SharePoint.** The existing SharePoint sign-in (the MCP OAuth proxy callback) keeps working
unchanged for one release so a popup opened before the upgrade completes. The shared web sign-in that
moves SharePoint onto the new callback ships in a following release of the same programme.

**Rolling back.** The two tables are additive and short lived (a state lasts 10 minutes, a handoff 60
seconds); rolling Main back leaves them in place and nothing else reads them.
