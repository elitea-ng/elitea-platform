# Elitea AI

Elitea AI is the native mobile app (iOS and Android) of the Elitea platform, for
SaaS and self-hosted workspaces alike. Connect to your organization's workspace, sign in through your
organization's usual SSO, and chat with your workspace's agents on the go.

Built with Expo (React Native) and TypeScript.

It lives in the Elitea platform monorepo at `apps/elitea-mobile` and generates its typed API
client from the server's OpenAPI document in the same repository
(`services/elitea-main/api/openapi/v2.yaml`, see [contract/README.md](contract/README.md)).
Until 2026-10 it was developed in a separate repository; its history was imported with
it. References of the form `mobile-repo#N` in comments and docs are issues and pull
requests of that former repository.

**Status:** MVP foundation (B1–B3). The app generates a typed client from the server spec, wraps it with an origin-guarded bearer fetch, and can sign in with a
developer personal access token in builds that enable it. It lists conversations,
reads threads, sends messages with live SSE streaming and safe Markdown, switches
projects, starts new chats and shows in-app notifications. The security baseline is
in place: one logout-and-wipe path with crash resume, fresh-install purge,
persisted workspace restored behind the splash, device lock on launch and after
idle, and app-switcher/screenshot protection. B4 adds native sign-in, server
policy and brand packs, and the encrypted offline store: SQLCipher, opened only
after unlock and only when the workspace policy allows offline data, kept current
with `changes_since` sync, read when the workspace cannot be reached, with an
outbox for messages written offline. Attachments are not stored offline yet (see
[docs/open-questions.md](docs/open-questions.md)).
Chat quick wins (client contract 1.3): a message actions sheet (copy, copy code,
share, read aloud, like/dislike with a comment, regenerate), Stop on the server,
on-device dictation, transcript export to the share sheet, suggested agents, and
VoiceOver announcements, all behind the workspace's data-control policy.
Settings and home quick wins (1.3): swipe-to-dismiss notifications with a type
filter, a memory viewer, a usage card with a readable budget refusal in chat,
home-screen quick actions (titles only when the policy allows them), and MDM
AppConfig pre-fill and lock floor ([docs/mdm-appconfig.md](docs/mdm-appconfig.md)).
Native surfaces (iOS): home-screen and lock-screen widgets with the active
workspace's counts (pending approvals, unread, running), a local Live Activity
and Dynamic Island for a running answer, and open-app Siri / Shortcuts intents
(New chat with an agent, Ask an agent, Open notifications); counts and generic
text unless the workspace policy allows titles
([docs/native-surfaces.md](docs/native-surfaces.md)).
Discover (1.3): the workspace's public agents with search, category, tags and icon,
an agent page with its welcome message and conversation starters, and a chat started
in the active project.
Native behaviour is verified by hand with
[docs/device-verification.md](docs/device-verification.md).
See [docs/design.md](docs/design.md) for the planned product and
[docs/open-questions.md](docs/open-questions.md) for decisions still open.

**License:** Apache-2.0, as the rest of this repository (see the root `LICENSE` and `NOTICE`).

## Identifiers

All identifiers are defined once in [`app.config.ts`](app.config.ts). Runtime code
reads them through [`src/config/identity.ts`](src/config/identity.ts); do not
hardcode them anywhere else.

| What | Value |
|------|-------|
| Display name | `Elitea AI` (the same for every customer; there are no per-customer builds) |
| Expo slug | `elitea-mobile` |
| iOS bundle identifier | `ca.mlops.elitea` (permanent) |
| Android application id | `ca.mlops.elitea` (permanent) |
| Connect-link scheme | `elitea`, as in `elitea://connect?server=https://ai.example.com` |
| OAuth redirect (RFC 8252) | `ca.mlops.elitea:/oauth/callback` |
| Website | https://elitea.ai (privacy: `/privacy`, support: `/support`) |
| App Group (widget extension) | `group.ca.mlops.elitea` |
| Widget extension bundle id | `ca.mlops.elitea.widgets` |

## Running it

You need Node.js and npm. This is a standalone npm project with its own
`package-lock.json` (the monorepo has no npm workspace); run every command from
`apps/elitea-mobile`. CI: `.github/workflows/ci-mobile.yml` (typecheck, lint, tests and the
generated-client check).

```bash
npm install
npm start            # Expo dev server (press i / a for a simulator, or scan with a dev build)
npm run typecheck    # tsc --noEmit
npm run lint         # expo lint
npm test             # jest (jest-expo preset)
npx expo-doctor      # dependency and config health
```

The app is on Expo SDK 58 (React Native 0.88 release candidate). `.npmrc` sets
`legacy-peer-deps=true` because the SDK's own peer ranges disagree with its
prerelease React Native and worklets versions; the file explains when to drop it.
`jest` and `@types/jest` are excluded from `expo install --check` and expo-doctor
because jest-expo 58 requires jest 30 while the SDK's version table still lists 29.

### Developer sign-in

Users sign in with the workspace's native sign-in (ADR-0025: authorization code +
PKCE S256 in the system browser, `src/auth/nativeAuth.ts`). Development builds
(`__DEV__`) can also sign in by pasting a personal access token, for workspaces
that have not registered the app; no build flag or runtime setting enables that in
a release build, and CI asserts the public config carries no such switch.

Native `ios/` and `android/` directories are generated by `npx expo prebuild`
(Continuous Native Generation). They are git-ignored, and you configure native
behaviour in `app.config.ts` and config plugins, not by editing them. Planned
native modules such as SQLCipher, secure storage and biometrics require a
development build (`npx expo run:ios|android` or EAS Build), not Expo Go.

### Releases

Cloud builds, TestFlight, Play testing and over-the-air updates go through EAS
(`eas.json`, profiles `development`, `development-simulator`, `preview`,
`production`). The runbook, including the one-time account setup the owner
still has to do, is [docs/release.md](docs/release.md); store listing, privacy
and export-compliance drafts are in [docs/store/](docs/store/README.md). The
`Mobile EAS build` and `Mobile EAS submit` workflows (`.github/workflows/mobile-eas-*.yml`)
run only when dispatched by hand.

## Repository layout

```
app.config.ts              Expo config, the single source of the identifiers above (+ config plugins)
app-intents/               Siri / Shortcuts App Intents (Swift, compiled into the app target; open-app only)
src/
  app/                     expo-router routes
    _layout.tsx            root: splash gate, guards (connect → sign-in → app), lock gate,
                           privacy cover, "Signing out…", update-required, connect-link switch prompt
    connect.tsx            first run: "Connect to your workspace", discovery check and host confirmation
    sign-in.tsx            native sign-in (system browser); developer PAT form in __DEV__ builds
    +native-intent.tsx     keeps the OAuth redirect URL away from the router
    (app)/                 signed-in area
      index.tsx            conversations list
      c/[id].tsx           conversation thread, composer, live streaming
      new.tsx              new conversation (agent picker)
      projects.tsx         project switcher (personal + team, per workspace)
      notifications.tsx    in-app notifications
      settings.tsx         workspace, device-lock policy, switch workspace, "Log out & wipe"
      workspaces.tsx       workspace switcher (several connected workspaces, no wipe on switch)
  api/                     generated client (generated/), guarded fetch (token refresh + one replay),
                           errors, path guards, discovery document, anonymous requests,
                           react-query client and queries, sse/ (parser, transport, resumable stream)
  auth/
    nativeAuth.ts          native sign-in: authorization code + PKCE S256, state and iss checks
    nativeSession.ts       native tokens: storage, single-flight rotation, re-delivery grace, revoke
    tokenEndpoint.ts       token / revoke endpoints (form posts, outcome mapping)
    pkce.ts                PKCE verifier/challenge and state
    devSignIn.ts           developer PAT sign-in (__DEV__ only)
  config/identity.ts       runtime access to the app identifiers
  constants/               fallback theme (brand tokens from the workspace override it)
  theme/                   workspace brand pack → palette, logo, same-origin font face
  features/                chat (frames, turn reducer/controller, message model), markdown,
                           notifications, projects, connect (discovery check + confirmation),
                           surfaces/ (widgets, Live Activity), intents/ (Siri / Shortcuts)
  db/                      encrypted offline store: SQLCipher open/key, migrations, retention,
                           lifecycle (open after unlock, close on lock, wipe with the session)
  sync/                    changes_since sync engine, offline reads, outbox (idempotent replay)
  linking/                 connect-link parser, incoming connect-link handler
  security/
    secureStorage.ts       secure-storage facade + key registry (the only expo-secure-store importer)
    endSession.ts          THE logout-and-wipe orchestrator (crash-resume marker, capped revoke)
    freshInstall.ts        iOS Keychain-survives-uninstall purge
    policy.ts              client policy + strict defaults (server-fed in B4)
    lock/                  device lock: controller, LockGate, expo-local-authentication wrapper
    privacy/               app-switcher blur, screenshot policy, privacy cover
  state/                   session store, launch hydration, workspace adapter, project selection, splash gate
  lib/                     redacting logger, abort registry, app activity (AppState + lock), network state
assets/                    app icons and splash (the Elitea logo mark, docs/assets.md)
docs/
  design.md                product and security design
  device-verification.md   manual checklist for native behaviour (first EAS dev build)
  open-questions.md        unresolved decisions
contract/                  client-operations.json: the operations kept from the server spec (contract/README.md)
scripts/contract.mjs       client generation (`npm run contract:*`); scripts/check-generated-client.mjs is the CI drift gate
```
