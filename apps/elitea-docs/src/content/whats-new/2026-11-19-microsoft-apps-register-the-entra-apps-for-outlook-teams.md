---
date: 2026-11-19
order: 4
---
## Microsoft apps: register the Entra apps for Outlook, Teams, Microsoft Graph, Dataverse and SharePoint once

**Configuration › Microsoft apps** is the registry of **Default apps**: Microsoft Entra apps an administrator registers once, so a Microsoft credential can name an app instead of holding its own client ID and secret. The page prints everything an Entra administrator needs for this deployment (the one redirect URI, the delegated permissions per credential type, the admin consent URL) and warns 30 days before a client secret expires. The client secret is write-only and sealed: no screen, response, log or export ever shows it. Each app has a permission ceiling per type, a project scope and a state, and a **Test** that checks the client ID, the authority metadata, the redirect URI and the secret. **Allow Custom app** keeps or removes the option of entering your own client. Apps can also be seeded from Helm (`microsoftOAuthApps`), with the secret read from a Kubernetes Secret. Nothing is on by default: with no app registered, credential forms offer only Custom app. The Microsoft credential types and their sign-in follow in the next releases. See [Microsoft apps](/admin/microsoft-apps). Run the new migration (shared 0255) with the release.
