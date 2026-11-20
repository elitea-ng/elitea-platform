---
date: 2026-11-19
order: 5
---
## Microsoft sign-in: one redirect URI for the web, desktop and mobile apps

Signing in to a Microsoft credential (Outlook, Teams, Microsoft Graph, Dataverse, delegated SharePoint) now runs through Main. One redirect URI, `https://<host>/api/v2/microsoft_login:callback`, serves the web popup, the desktop app and the mobile app, for **Custom app** and **Default app**. The code exchange happens in Main with PKCE, so no client secret and no refresh token reaches a browser or an app, and the redirect page attaches nothing to anyone: only the person who started the sign-in can collect its result, within 60 seconds, so a sign-in link someone else started cannot link your Microsoft account to their credential. The **Allow Custom app** switch is now enforced at sign-in as well as in the form (`custom_apps_disabled`). The screens that use this arrive with the shared sign-in button and the desktop and mobile sign-in in the next releases. See [Microsoft apps](/admin/microsoft-apps). Run the new migration (shared 0257) with the release.
