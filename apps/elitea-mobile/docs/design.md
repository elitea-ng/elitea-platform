# Agent Zefir: design summary

Agent Zefir is a native iOS and Android client for Elitea AI-agent platform servers,
covering both the SaaS offering and self-hosted installations. One store build serves
every customer. The app is always called "Agent Zefir", and the workspace's identity
comes from the server at runtime.

## MVP scope

### In scope

- **Connect** to a workspace in any of four ways:
  - scanning a QR code shown by the workspace (the payload is a bare `https://host` URL or a connect link)
  - opening a connect link, `agentzefir://connect?server=https://...`
  - typing the server address
  - MDM managed app configuration, which pre-sets the server for enterprise-managed devices
    (the module is still to be chosen; see open questions)

  All four go through `src/linking/parseConnectLink.ts`. It accepts only an https
  origin, strips any path, query or fragment, and rejects URLs that carry credentials.
  Plain http is allowed only behind a dev flag, and only for localhost, 10.x and
  192.168.x.
- **SSO** in the system browser (ASWebAuthenticationSession / Custom Tabs). The flow is
  OAuth 2.0 authorization code with PKCE, as a public client with no client secret. The
  redirect URI is `ca.zefir.agent:/oauth/callback`, following RFC 8252. The app relies
  on the server's native-client support. That server-side work is tracked upstream as a
  generic "native client support" ADR in the Elitea docs, not in this repository.
- **Biometric lock** with `expo-local-authentication`, required on launch and on resume
  after a timeout set by server policy.
- **Token storage** in `expo-secure-store` with device-only accessibility
  (`WHEN_UNLOCKED_THIS_DEVICE_ONLY`). Tokens are never included in backups and never
  migrate to another device. Android `allowBackup` is `false`.
- **Offline support:**
  - The local store is encrypted SQLite, using `expo-sqlite` with SQLCipher.
  - The database key lives in the Keychain or Keystore and is released only after a
    successful biometric check.
  - Messages composed offline go into an encrypted outbox.
  - Sync is incremental, using cursors or `updated_since`.
  - Attachments are encrypted at rest with AES-GCM, using per-file keys wrapped by the
    database key.
- **Wipe** of every token, the database, attachments and the outbox in three cases:
  - on logout
  - on a server switch, after explicit confirmation
  - on remote revocation (the server rejects the refresh token, or policy signals a wipe)
- **Screen privacy:** a cover view in the app switcher, with screenshot blocking where
  policy requires it.
- **Server-driven policy:** the server can set the lock timeout, offline allowed or not,
  attachment caching, screenshot blocking and maximum offline age. The app falls back to
  strict defaults when no policy is available.
  - Implementation (`src/security/policyStore.ts`): the full policy of the latest token
    response wins, else the discovery document's public subset (which may tighten, never
    loosen, a server policy), else strict defaults. It is persisted per workspace so the
    launch lock uses it. `idle_lock_seconds: 0` means "never relock on resume" (the
    server's definition); the launch lock still applies. A tightened policy applies at
    once. `min_client_version` (token response or discovery) shows the update screen
    before the server has to answer `426`.
- **Streaming chat** over Server-Sent Events, read via `fetch` streaming with
  `Authorization: Bearer`. A dropped stream resumes with `Last-Event-ID`.
- **In-app notifications only:**
  - When the app is open, it shows notifications in-app (a bell or toasts), fed by the
    same authenticated channel.
  - **Push notifications are explicitly out of the MVP.** They need a separate decision
    (see open questions).
- **Runtime branding:**
  - After connecting, the app fetches the server's brand pack JSON: colours, logo and
    fonts. These apply inside the app only, with a fallback palette in
    `src/constants/theme.ts`.
  - Implementation (`src/theme/`): only an allowlist of tokens is read and only plain
    colours (`#rgb`, `#rrggbb`, `#rrggbbaa`, `rgb()`, `rgba()`) are accepted; a scheme
    whose text misses WCAG 4.5:1 on its background is dropped. Logos and font faces load
    only from the workspace origin (raster logos may also be inline `data:` URIs). An
    SVG logo is fetched through the origin guard (no cross-origin redirect) and rendered
    from its markup only when every reference in it is local (`#id`): no `<image>`,
    `<foreignObject>`, external `href` or `url()`; web
    font stacks are ignored unless the pack ships a same-origin `.ttf`/`.otf` face. The
    workspace host on the connect confirmation is always in the system font.
  - The app name is always "Agent Zefir".
  - There are no per-customer builds.
- **Icons:** the app bundles exactly two icon sets.
  1. The SaaS default.
  2. A workspace or self-hosted variant.

  The app switches between them with alternate app icons, and only after the user
  confirms (the OS shows its own prompt as well). The brand pack can suggest a variant,
  but it never switches silently and never downloads icons.

### Out of scope (MVP)

Administration, pipelines, toolkits, MCP, wiki, agent authoring and push notifications.

## API contract

The app talks to the server only through its public HTTP API.

- A pinned copy of the server's OpenAPI document (`v2.yaml`) will live under `contract/`
  (to be added). It is updated deliberately by copying a released version, never fetched
  at build time.
- The app generates its own typed client from that file. The generator choice is still
  to be made, and the generated code is committed or rebuilt reproducibly.
- **Nothing is imported from Elitea source code**, including no shared packages and no
  copied UI or logic. The contract file is the only coupling.
- Compatibility is checked against the server's advertised version or capabilities at
  connect time. If a server lacks native-client support, the connect flow reports it
  clearly instead of failing at login.

## Security notes

- The only accepted transport is https. Self-hosted servers with private CAs depend on
  device trust settings, and certificate pinning is not planned for the MVP.
- No secrets are bundled. The OAuth client is public, and PKCE protects the code exchange.
- Logs never contain tokens, message content or attachment names.
- Encryption export compliance needs review before the first store submission. For now
  `ITSAppUsesNonExemptEncryption` is a placeholder (see `app.config.ts` and the open
  questions).

## Security baseline as implemented (B3)

The decisions above are implemented as follows. Native behaviour is checked by hand
with [device-verification.md](device-verification.md).

- **One exit path.** Every logout, revocation, rejected credential, server switch and
  "can't unlock" reset goes through `src/security/endSession.ts`. It writes a
  crash-resume marker first and removes it last; launch hydration re-runs an
  interrupted wipe before showing anything. Each step is attempted even if an earlier
  one fails. The server revoke (B4) is best effort and capped at 3 s.
- **Key registry.** Every secure-storage key is declared in
  `src/security/secureStorage.ts` (lint forbids `expo-secure-store` elsewhere), so the
  wipe covers new keys automatically.
- **Reinstall.** The iOS Keychain outlives an uninstall; a marker file in the
  documents directory detects a reinstall and purges the registry
  (`src/security/freshInstall.ts`).
- **Restore behind the splash.** The workspace origin, credential and selected project
  are restored at launch while the native splash is up. A storage read error
  (device locked) is retried when the app is active, never treated as signed out.
- **Server switch.** A connect link for another workspace asks first, then wipes.
- **Device lock.** `src/security/lock/`: a session restored at launch is locked, the
  app relocks after `idle_lock_seconds` in the background (strict default 300 s),
  'inactive' alone never locks, biometrics fall back to the passcode, and a device
  without a passcode is blocked when the policy requires a lock. A locked app counts
  as backgrounded for streams and refetch (`src/lib/appActivity.ts`).
- **Screen privacy.** `src/security/privacy/`: iOS app-switcher blur, screenshot and
  recording blocking while the policy disallows screenshots (Android FLAG_SECURE also
  blanks Recents), and an opaque in-app cover whenever the app is not active.
- **Policy.** `src/security/policy.ts` holds the strict defaults until the server
  supplies a policy (B4).

## Chat enrichment (client contract 1.1)

What the chat does beyond plain text, against servers that speak client contract 1.1
(discovery `client_contract`; every addition is hidden for a 1.0 server or a 404/501):

- **Mentions.** "@" over the conversation's other people (and "Everyone"); the ids are
  derived from the final text on send (`src/features/chat/mentions/`) and sent as
  `user_ids` / `is_mentioning_everyone`. "Me" is learned from notification rows and
  from messages this device sent (`src/state/selfIdentity.ts`), so other people's
  messages are drawn as theirs. A `chat_user_mentioned` notification opens the
  conversation.
- **Agents, skills, tools.** Several agents per conversation ("To:" chip, "#" to add
  one pinned to its version), a participants sheet (people and agents; toolkits and
  models stay web-only), "~" skills and "/" toolkit tools (text only).
- **Tool activity.** Live tool calls from the progress frames, and from the message
  traces after a reload; inputs and outputs are capped at 8 KB in memory and never
  stored.
- **Pauses.** Approve, reject, edit, block with comment and ask_user answers; toolkit
  sign-in can be skipped (no OAuth token ever lives on the device; "Open in web" for
  sign-in); the output-limit Continue. Online only, never queued.
- **Attachments.** Photos, camera and files, checked against the discovery limits,
  HEIC converted and JPEG re-encoded (no EXIF/GPS leaves the phone), uploaded in chunks
  through the guarded fetch, and only then sent. Downloads go through the guarded fetch
  to a temporary file that is never kept (see open question 6); HTML and SVG are never
  rendered by the app, only handed to the system preview.

