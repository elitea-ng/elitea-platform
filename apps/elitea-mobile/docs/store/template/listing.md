# Store listing: {productName}

Placeholders: a name in braces comes from brand.json, `[bracketed]` is yours. Wording rules:

- The app is yours: `{productName}` is the name and your brand's icon is the icon.
- No claims the app cannot back: no "end-to-end encrypted", no "HIPAA", no
  specific model names (the workspace decides the models).
- Name no other company's product or deployment.

## Shared

| Field | Value |
|---|---|
| App name (App Store 30, Play 30) | `{productName}` |
| Bundle ID / package | `{mobile.bundleId}` |
| Privacy policy URL | `{privacyUrl}` |
| Support URL | `{supportUrl}` |
| Marketing URL | `{webUrl}` |
| Price | Free, no in-app purchases, no ads |
| Copyright (App Store) | `[year] [legal entity name]` |

## App Store

**Subtitle** (30): `[subtitle]`, for example `AI agents for your workspace` (28).

**Promotional text** (170): `[promotional text]`.

**Description** (4000): adapt ELITEA's (../listing.md, "Description") with
`{productName}` for the name and `[your workspace product name]` for
"Elitea workspaces". Keep the feature list to what the release you build
does, and end with:

> {productName} works with [your workspace product name] workspaces. You need a
> workspace account from your organization to use it.

**Keywords** (100): `[keywords]`. No trademark of others, no words already in
the name.

**Categories**: Primary **Productivity**, secondary **Business** (or yours).

**Age rating**: answer the questionnaire as in ../listing.md, "Age rating"; the
app's content is the same.

## Google Play

**Title** (30): `{productName}`.

**Short description** (80): `[short description]`.

**Full description**: the App Store description, adapted for Android as in
../listing.md.

**Contact details**: `{supportEmail}`, website `{webUrl}`.

**Content rating, target audience, app access, ads**: as in ../listing.md.

## Review notes (both stores)

Before the first external TestFlight round and before each review, create:

1. **A demo workspace** reachable from the internet, operated by
   [legal entity name], with native clients enabled for `{mobile.bundleId}`
   (redirect `{mobile.oauthRedirectScheme}:/oauth/callback`), a public agent or
   two in Discover, a project with sample conversations, and models that answer.
2. **A reviewer account** on that workspace: a username and password sign-in
   with **no** MFA app, SMS, hardware key or VPN, valid for the review period.
3. Keep both alive: a review that cannot sign in is rejected under guideline 2.1.

App Review Information (and Play's App access, TestFlight's Test Information):

> **Sign-in required.** {productName} is a client for an organization's AI-agent
> workspace; accounts are issued by the organization, so the app has no sign-up.
>
> 1. Open the app and tap "Enter workspace address".
> 2. Enter `[demo workspace address]` and tap Continue, then check the
>    workspace name "[workspace name]" and tap Connect.
> 3. Tap "Continue with SSO". In the sign-in sheet, use:
>    username `[reviewer username]`, password `[reviewer password]`.
> 4. Allow Face ID when asked (or skip).
>
> To try it: open the Chats tab and start a chat with "[sample agent]"; open
> Discover; tap the bell on Home for notifications. Settings, "Log out & wipe"
> removes everything from the device.
>
> Notes: dictation uses on-device speech recognition only. The app collects no
> analytics and has no ads. The workspace server is operated by
> [legal entity name] for review purposes.
>
> Device management and notifications: the app reads managed app
> configuration (server address, device lock, screenshots, an IT support
> contact) only on a device
> enrolled in an organization's MDM, and reports its connection and sign-in
> state back to that MDM (managed app feedback); without one these settings
> are absent, nothing is reported, and they change nothing. It never asks for a push token, and has no Local Push
> connectivity extension: notifications are shown in the app, fetched from the
> workspace. None of this needs testing for review.

Check this paragraph against the release you build: a release that adds push
or Local Push says so in its upgrade notes, and the paragraph changes with it.

Contact: [name, phone, email].
