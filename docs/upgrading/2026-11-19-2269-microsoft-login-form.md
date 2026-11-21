# Microsoft sign-in in the web credential form (issue 2269, ADR-0057, epic 2254 story S1d)

**What changed.** The Outlook, Teams, Microsoft Graph and Dataverse credential types are no longer
hidden: they appear in the **New credential** list of the web app (and the desktop app, which shows
the same screen). The credential form gains the sign-in:

- **Save and sign in with Microsoft** saves the credential, opens a Microsoft window and finishes the
  sign-in through `POST /api/v2/microsoft_login:start`, the callback page and
  `POST /api/v2/microsoft_login:redeem` (story S1c). The result is matched on the sign-in's `state`,
  received by `postMessage` and, when an identity page detached the opener, by the
  `elitea-microsoft-login` BroadcastChannel.
- The form shows the account, the tenant and when the access token ends, runs the connection check
  with the token, and offers **Sign in again** and **Sign out**. A check that answers 401
  `requires_authorization` while a token is held drops the token and asks for a new sign-in.
- Outlook and Teams scopes are checkboxes with the consent note. **Default app** and **Custom app** are
  offered side by side once an app is registered for the type and project; with an empty registry the
  form shows only Custom app, and with `microsoft.custom_apps` off only the Default apps.
- A person who cannot update credentials in the project sees **Login managed by a project admin**.

The access token is kept in the browser's session storage under `{credential uuid}:{authority}`, where
runs read it. No refresh token and no client secret is returned to the browser.

**Catalogue.** The four types lose `metadata.hidden`. The Default app auth subsection now also holds
the scopes (and, for Outlook, the shared mailbox), because the person's scope choice applies in both
modes; the app's ceiling still cuts it. Stored credentials are unchanged.

**What you must do.** Nothing. There is no migration and no setting. For a Default app, register the
Entra app first (Configuration > Microsoft apps); the Entra redirect URI is the one printed there,
`https://<host>/api/v2/microsoft_login:callback`.

**Not in this release.** The desktop app's system-browser sign-in (loopback) and the mobile app's
sign-in follow in story S1e (issue 2271); until then sign in on the web. Scheduled and unattended runs
cannot use these credentials until the stored login (S1h) ships.

**Rolling back.** Nothing to undo: credentials created by this release are ordinary rows, and rolling
back re-hides the types.
