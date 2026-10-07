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
- **Wipe** of one workspace's tokens, database, attachments and outbox in two cases
  (every other connected workspace is untouched):
  - on logout of that workspace ("Log out & wipe", or Remove in the switcher)
  - on remote revocation of that workspace (the server rejects the refresh token, or
    policy signals a wipe)
  Switching between workspaces never wipes. "Can't unlock → reset" wipes every
  workspace on the device.
- **Several workspaces** (see "Multi-workspace" below): up to 8 connected at once, one
  active, each fully isolated.
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

- **One exit path.** Every logout, revocation, rejected credential and "can't unlock"
  reset goes through `src/security/endSession.ts`, which ends the ACTIVE workspace only
  (removing a workspace that is not active uses `wipeStoredWorkspace`). It writes a
  crash-resume marker first and removes it last; launch hydration re-runs an
  interrupted wipe before showing anything. Each step is attempted even if an earlier
  one fails. The server revoke (B4) is best effort and capped at 3 s.
- **Key registry.** Every secure-storage key is declared in
  `src/security/secureStorage.ts` (lint forbids `expo-secure-store` elsewhere), so the
  wipe covers new keys automatically. Keys are stored per workspace
  (`az.ws.<id>.<name>`); the registry of workspaces is the one device item
  (`az.workspaces`).
- **Reinstall.** The iOS Keychain outlives an uninstall; a marker file in the
  documents directory detects a reinstall and purges the registry
  (`src/security/freshInstall.ts`).
- **Restore behind the splash.** The workspace origin, credential and selected project
  are restored at launch while the native splash is up. A storage read error
  (device locked) is retried when the app is active, never treated as signed out.
- **Connect links.** A connect link for another workspace asks first, then switches to
  it (already connected) or adds it next to the current one (new). Nothing is wiped.
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
  `user_ids` / `is_mentioning_everyone`. "Me" comes from the "who am I" read
  (contract 1.2, see below), so other people's messages are drawn as theirs. A
  `chat_user_mentioned` notification opens the conversation.
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

## Client contract 1.2

Additive; a 1.1 server keeps working.

- **Who am I.** `getCurrentAuthor` (`GET /api/v2/social/author`) is read once per
  signed-in session by the app shell (`src/state/selfIdentity.ts`). Only the user id
  is kept: in memory, and in secure storage (`az.user.self`, bound to the workspace
  origin) so a launch offline still knows it. Both go with the session (wipe hook
  and the secure-key registry), and nothing is stored while a wipe is running. The
  route is older than 1.2, so a 1.1 server answers it too. Until it answers, a
  shared conversation draws every user message on my side but named, so nobody
  else's words read as mine. Nothing is inferred from notifications or sent
  messages any more.
- **Paused tool calls.** `agent_tool_paused` (and a persisted trace step with
  `finish_reason` `awaiting_approval`, `awaiting_input` or `interrupted`) shows the
  call as waiting for approval or for an answer, never as failed. A persisted
  paused call waits only while its answer is still paused, and reads as done once
  the pause is answered. A server before 1.2 sends `agent_tool_error` with the
  interrupt's repr instead; that is still recognised and shown as waiting.

## Multi-workspace

Several workspaces can be connected at once (`src/security/workspaceRegistry.ts`,
`src/state/workspace.tsx`). One is active; the others are kept, signed in or not.

- **Isolation.** Each workspace has a slot with a random 12-hex id: its own secure-store
  namespace (`az.ws.<id>.*`: origin, credential and device session, selected project,
  policy, brand, "me", database key, wipe marker), its own SQLCipher file
  (`az-offline-<id>.db`) and therefore its own outbox and sync cursors. Unqualified
  secure-store calls act on the bound (active) workspace and refuse while none is bound;
  the physical key is resolved when a call starts, so a write begun for one workspace
  cannot land in another's.
- **Switching never wipes.** `suspendActiveWorkspace` aborts in-flight work and runs every
  module's in-memory reset (query cache, live turns, credential, policy, brand, open
  database), unbinds the slot and signs out in memory; the target is then restored from
  its slot exactly as at launch. Guards against cross-workspace leaks: the offline store
  opens only the bound workspace's file; an outbox pass stops when another workspace
  becomes active; sync runs are keyed per database; a renewed native token is refused if
  the active workspace changed meanwhile; requests must target the active origin.
- **Device lock across a switch.** Presence carries over only when the user unlocked under
  a lock-requiring policy; switching into a lock-requiring workspace otherwise locks and
  prompts.
- **The host is always visible.** Every signed-in header shows the active workspace's host
  (and brand mark); the switcher, the connect screen's list and Settings name each host.
  Brands are applied per active workspace.
- **Removing.** The active workspace goes through `endSession`; another one is revoked
  best effort from its stored refresh record and wiped from its slot. `device_revoked`
  and rejected credentials end only the workspace they came from.
- **Projects.** The project switcher lists the personal project (from "who am I",
  `personal_project_id`) first and the team projects after it; the choice is stored in
  the workspace's namespace, so each workspace keeps its own.
- **Upgrade.** The single-workspace layout of earlier builds moves into a fixed first slot
  on the first launch (copy, registry, delete the old items; idempotent across crashes):
  no new sign-in, same project, same database file and key.
- **Limits.** At most 8 workspaces. Only the active workspace syncs and drains its outbox
  (no background execution); the others catch up when opened.

## Chat quick wins (client contract 1.3)

The server's 1.3 work is not released yet (elitea-platform PR #1090, `507a13b0c`), so
the pin stays at 1.2 and the gap goes through the overlay (`contract/README.md`). Every
1.3 feature is shown only when discovery says `client_contract` >= 1.3; on an older
server it is hidden, not broken.

- **Data controls.** The policy carries `allow_share_out`, `allow_share_in`,
  `allow_cloud_stt`, `notification_preview` (`none`|`title`), `allow_notification_actions`
  and `allow_system_surfaces`. With none sent (a server before 1.3) they take the server's
  own defaults (owner decision, SaaS behaviour): share out/in and actions on, cloud speech
  and system-surface titles off, preview `none` (`src/security/policy.ts`
  `DATA_CONTROL_DEFAULTS`). An enterprise admin turns each off; a discovery document can
  only tighten them after sign-in, and the tightening is persisted with the server policy.
- **Message actions.** A long press on a message, or its "•••" button (VoiceOver's way in),
  opens a sheet: Copy, Copy code (each fenced block, by language), Share, Read aloud,
  Good / Bad (with an optional comment, at most 2000 characters; tapping your rating again
  retracts it) and Regenerate. Copy, code copy, share and export need `allow_share_out`;
  with it off the sheet says the workspace does not allow it. Feedback needs 1.3, a
  persisted answer and the network.
- **Stop.** While an answer streams, Send becomes Stop: `cancelChatExecution` (DELETE
  `/elitea_core/task/prompt_lib/{project}/{response_message_id}`), then the stream is
  closed and the transcript read again, so the partial text stays. 204 also for a second
  stop by the same user; 409 means the answer had already ended. A stream that ends with
  `execution.failed` `CANCELLED` (a stop from another device too) settles as stopped, not
  failed.
- **Regenerate.** On the newest answer: `regenerateChatMessage` with a client
  `regeneration_id` (byte-identical retries; `agent_regeneration_pending` 409 waits for its
  Retry-After). The new answer streams in place of the old one. Afterwards the app checks
  that the persisted answer's `meta.execution_generation` is the regeneration id (the first
  answer carries its question's id); if not, the thread says the answer was not replaced.
  Proven on the zefir-wave stack: same uuid, new task id and item, generation = the id.
- **Speech, on the device only.** Dictation uses expo-speech-recognition with
  `requiresOnDeviceRecognition`: no audio leaves the device, and a device or language
  without an on-device model has no dictation rather than a server fallback. Apple's
  server recogniser is never used; `allow_cloud_stt` is reserved for the workspace's own
  transcription model (later). Read aloud uses expo-speech (the device synthesiser):
  Markdown marks are dropped and code blocks announced, one reading at a time, stopped with
  the thread and the session. Each speech module has one facade (lint). The on-device
  guarantee is native: expo-speech-recognition is pinned to exactly 57.1.0 and patched
  (`patches/`, applied on install) so `start()` refuses a language whose recogniser has no
  on-device model and the request always carries `requiresOnDeviceRecognition`; unpatched,
  a language other than the system default fell back to Apple's servers silently. Android
  dictates only from API 33, where the library has a real on-device recogniser.
- **Export.** "Export" in the thread header (1.3, `allow_share_out`, online) reads the whole
  transcript as Markdown (`exportConversation`), re-checks share-out and the workspace once
  it has arrived, writes it to a random folder under the caches directory and shares the
  FILE through expo-sharing (react-native's Share drops `url` on Android). iOS deletes it
  when the sheet closes; on Android the target may read it after the chooser returns, so it
  stays until the next export's sweep or the cache wipe. A conversation too long for one
  document is refused by the server (400) and sent to the web app, never exported
  truncated.
- **Behind the device lock.** Every sheet is a `LockAwareModal` (a React Native Modal is its
  own native window, above LockGate's cover): hidden while the lock covers the app, back
  after the unlock. Read aloud stops and dictation aborts when the app locks.
- **Suggested agents.** The empty new-chat screen lists the project's recommended agents
  (`getRecommendations`, at most 10) until a search is typed.
- **Discover.** `/discover` lists the workspace's public agents (`listPublicApplications`,
  20 per page by offset until the filtered `total`, most liked first, server-side search
  after a 300 ms pause) with icon, description, category and tags. `/discover/[id]` adds
  the published version's welcome message and conversation starters
  (`getPublicApplication`, optional). Start chat creates the conversation in the ACTIVE
  project (`createConversation`), then adds the agent from the public project at its
  published version (`addConversationParticipants`); an agent that cannot be added
  deletes the empty conversation again. A starter opens the chat with its text in the
  composer (in-memory hand-over, never sent by itself). Icons load only from the
  workspace origin, without credentials; anything else shows the monogram. Hidden below
  client contract 1.3. The catalogue is keyed under the active project, so a project or
  workspace switch reads it afresh.
- **Accessibility.** VoiceOver hears phase changes of the live answer (done, stopped,
  failed, waiting for approval, a question or a sign-in), debounced and never per token.
  Text follows Dynamic Type up to caps where rows have a fixed size
  (`src/constants/typeScale.ts`). With Reduce Motion on, sheets fade instead of sliding.
