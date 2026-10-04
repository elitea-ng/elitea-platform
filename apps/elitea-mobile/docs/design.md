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
