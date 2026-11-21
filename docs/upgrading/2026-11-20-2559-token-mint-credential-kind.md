# Security: only a sign-in can create a personal access token (#2559)

`POST /api/v2/auth/token/` now checks which credential is calling. Before this
change, any credential that authenticated as a user could create a personal
access token, including another personal access token and a desktop or mobile
device session. The new token acted with the owner's full permission set,
administration included.

- **A browser session** (the web app) creates tokens as before.
- **A personal access token, a run token or any other bearer or API-key
  credential** can no longer create one. The route answers `403` with
  `{"error":{"type":"permission_error","code":"token_mint_browser_session_required",...}}`.
- **A native device session** (the desktop app; the mobile app has no token
  page) creates tokens only for an owner who holds no administration-mode
  permission. For an administrator the route answers `403` with `code`
  `token_mint_native_admin`; that person creates tokens in the web app.
- **A credential bound to a project** creates only a token bound to that same
  project (`token_mint_wider_than_caller`; `error.project_id` names it).
- **A caller the server cannot classify**, or a native caller whose
  administration permissions the store refuses to answer, gets `403`
  `token_mint_caller_unresolved`. An error-level log line names the reason.
  A store that does not answer gives `500` `token_mint_permissions_unavailable`.
- Every refusal writes an audit row (`event_type` `api`, action
  `token.mint_refused`, entity name the refusal code).

**What to check.** Search your automation for scripts that call
`POST /api/v2/auth/token/` with an `Authorization: Bearer` or `X-API-Key`
header. They now get `403`. Create the token once in the web app and store it
as the script's secret instead. Tokens that already exist keep working; nothing
is revoked.
