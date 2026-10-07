# Device verification checklist

The CI machine has no Xcode or Android SDK. Everything that depends on native
behaviour is covered in jest by mocks only, so it must be checked by hand on
the first EAS **development build** (`eas build --profile development`) on a
real iPhone and a real Android phone, and again before each store submission.

How to use this file: copy the checklist into the PR or release ticket, fill
in the device, OS version and build, and tick each item. A failed item is a
bug against the module named in its row.

| Field | iOS | Android |
|---|---|---|
| Device / OS version | | |
| Build (version, EAS build id) | | |
| Workspace (origin, server version) | | |
| Date / tester | | |

Test setup: a workspace reachable from the phone with Agent Zefir registered as a
native client (redirect URI `ca.zefir.agent:/oauth/callback`), or a personal access
token for developer sign-in (development profile only, `__DEV__`),
a device passcode, and Face ID / Touch ID / fingerprint enrolled unless an item
says otherwise.

## 1. Secure storage and fresh install (`src/security/secureStorage.ts`, `freshInstall.ts`)

- [ ] **iOS reinstall purge.** Sign in, delete the app, reinstall it. The app opens on
      "Connect to your workspace", not signed in. (The Keychain survives an uninstall;
      the marker file in the documents directory does not.)
- [ ] **Android reinstall.** Same steps; the app opens on "Connect". (`allowBackup` is
      false and Keystore entries go with the app.)
- [ ] **No device migration.** iOS: restore an encrypted backup to a second device, or
      Android: run a device-to-device transfer. The token does not come across
      (`WHEN_UNLOCKED_THIS_DEVICE_ONLY`).
- [ ] **Locked-device launch.** Lock the phone and get the app launched in the
      background (for example by a debugger attach), or force-quit it and open it
      immediately while the device is locking. A secure-storage read error must NOT
      sign the user out: once unlocked and active, the session is restored.

## 2. Launch, hydration and splash (`src/state/hydrate.ts`, `src/app/_layout.tsx`)

- [ ] Cold start while signed in: the native splash stays up until the lock screen
      shows. There is no flash of the connect or sign-in screen.
- [ ] Cold start while connected but signed out: the splash goes straight to "Sign in".
- [ ] The selected project survives a restart (the header chip shows the same project).
- [ ] Kill the app during "Log out & wipe" (tap it, then force-quit within a second).
      On the next launch the wipe finishes before anything shows: the app opens on
      "Connect".

## 3. Device lock (`src/security/lock/*`)

- [ ] **Launch lock.** Cold start while signed in shows "Agent Zefir is locked" and the
      Face ID / fingerprint prompt opens by itself.
- [ ] **Face ID usage string.** The iOS prompt shows "Agent Zefir uses Face ID to unlock
      your workspace." (Missing string = crash on first use.)
- [ ] **Passcode fallback.** Fail biometrics until the passcode option appears; the
      passcode unlocks the app.
- [ ] **Cancel.** Cancelling the prompt leaves the lock screen with "Unlock was
      cancelled." and a working "Unlock" button.
- [ ] **No lock loop.** Unlocking does not immediately lock again (the Face ID sheet
      makes iOS go inactive → active).
- [ ] **Short background.** Background the app for under 5 minutes, return: no lock.
- [ ] **Idle relock.** Background it for 5 minutes or more (strict default 300 s),
      return: locked, and the screen you were on is still there after unlocking
      (navigation state preserved).
- [ ] **Inactive only.** Pull down Control Center / the notification shade for a long
      time without leaving the app: no lock.
- [ ] **Streams pause while locked.** Start a long answer, background the app past the
      idle time, return. Behind the lock nothing streams (server log or proxy shows the
      event stream closed); after unlocking the answer resumes from where it was
      (`Last-Event-ID`).
- [ ] **No device security.** Remove the device passcode (and so biometrics). Launch:
      "Set a device passcode" blocks the app; only "Log out & wipe" works. Set a
      passcode again in Settings, return to the app: it moves to the lock screen.
- [ ] **Can't unlock.** "Can't unlock? Log out & wipe" asks for confirmation and then
      lands on "Connect" with nothing stored.
- [ ] **Lockout.** Exceed the biometric attempts: the hint reads "Too many attempts…".
- [ ] Release build (production profile): the "Development build: continue without
      unlocking" link is absent.

## 4. Screen privacy (`src/security/privacy/*`)

- [ ] **iOS app switcher.** Open the app switcher on a conversation: the snapshot is
      blurred/covered, no message text is readable.
- [ ] **iOS screenshots (strict default).** Take a screenshot of a conversation: the
      captured image does not show the app's content. Screen recording likewise.
      Note any rendering glitch the capture block causes (black views, broken text
      input) as a bug.
- [ ] **Android Recents.** Open Recents: the app's card is blank (FLAG_SECURE).
- [ ] **Android screenshots.** A screenshot is refused ("Can't take screenshot due to
      security policy" or a black image).
- [ ] **Cover.** On both platforms, the branded cover appears whenever the app is not
      active, above the lock screen too, and disappears when it is active again.
- [ ] Known limitation, record the observed behaviour: with a policy that allows
      screenshots (B4), Android may show content in Recents because the snapshot can
      be taken before JS renders the cover.

## 5. Workspaces and connect links (`src/state/workspace.tsx`, `src/linking/*`)

- [ ] **Upgrade.** Install the previous build, sign in, open a few conversations and
      queue a message offline; install this build over it: the app opens signed in to
      the same workspace and project, the cached conversations and the queued message
      are still there (no new sign-in).
- [ ] Open `agentzefir://connect?server=https://<other host>` while signed in: "Add
      workspace?" names both hosts; Cancel changes nothing; "Add workspace" opens the
      confirmation for the new host, and "Back to <old host>" returns to the old one
      signed in. Confirm and sign in: both are listed in Workspaces.
- [ ] Switch between the two from the header (host under the title) and from Settings:
      each opens signed in with its own brand, project, conversations and policy; the
      header always shows the right host; nothing is re-downloaded from scratch.
- [ ] With one workspace requiring the device lock and the other not: switching into
      the locked one from the unlocked-without-lock one prompts; switching between two
      lock-requiring ones after unlocking does not prompt again.
- [ ] Remove the workspace that is NOT active: its database file and Keychain items are
      gone, its device appears revoked on the server, the active one is untouched.
- [ ] Revoke the active workspace's device from the server: only that workspace is
      wiped; the connect screen names it and lists the other one, which opens signed in.
- [ ] Open a connect link for a workspace already connected (not active): "Switch
      workspace?"; it opens without a wipe.
- [ ] Open a connect link for the workspace already in use: nothing happens.
- [ ] Open a connect link while the app is locked: no dialog until after unlocking.
- [ ] Cold start from a connect link while not connected: the address field is filled.

## 6. Networking and streaming (B1/B2 carry-over)

- [ ] **Incremental streaming.** A long answer appears token by token on iOS and on
      Android (OkHttp must not buffer the event stream).
- [ ] **Redirects.** The API does not forward `Authorization` across hosts: point a test
      route at a 30x to another host and confirm the request fails without the header
      reaching that host (REST: global fetch; SSE: `redirect: 'error'`).
- [ ] **Resume.** Toggle airplane mode for 10 s during a streamed answer: it resumes
      without duplicated or missing text.
- [ ] **Give-up.** Keep the network off for over 2 minutes: the thread shows a
      "Reconnect" action that works once the network is back.
- [ ] **Thread layout.** The inverted message list and the composer stay above the
      keyboard on both platforms; long Markdown answers scroll smoothly while
      streaming (note any jank with the 50 ms render throttle).
- [ ] **Markdown.** `react-native-svg` loads (no red screen); images render as a host
      chip and are never fetched (proxy shows no image request); an https link asks
      first and opens in the system browser.
- [ ] **Notifications bell.** A notification created on the server appears on the bell
      while the app is in the foreground; the stream closes in the background.

## 7. Logout and revocation

- [ ] Settings → "Log out & wipe": lands on "Connect"; a relaunch stays there.
- [ ] Revoke or delete the PAT on the server, then pull to refresh: the app wipes and
      lands on "Connect" with the previous address pre-filled (an in-memory hint
      only; after a relaunch the field is empty).
- [ ] Logs (Xcode console / `adb logcat`) during all of the above contain no token, no
      message text and no URL query strings.

## 8. Native sign-in (ADR-0025, `src/auth/*`, `src/app/+native-intent.tsx`)

Needs a workspace with Agent Zefir registered (`client_id` `ca.zefir.agent`,
redirect URI `ca.zefir.agent:/oauth/callback`) and `DEPLOYMENT_URL` set.

- [ ] **Connect confirmation.** Typing the workspace address shows the host in large
      text, the workspace name and logo, "Self-hosted"/"Hosted service" and the
      server version before anything is stored; "Use a different address" stores
      nothing (relaunch lands on "Connect").
- [ ] **System browser.** "Sign in with …" opens ASWebAuthenticationSession (iOS) /
      a Custom Tab (Android), never an in-app web view; the workspace's usual sign-in
      runs, then its one-line confirmation names Agent Zefir and this device.
- [ ] **Redirect delivery.** After confirming, the browser closes and the app signs in.
      On Android, confirm the single-slash `ca.zefir.agent:/oauth/callback?…` intent
      reaches the app and the router does not show an "unmatched route" screen.
- [ ] **Cancel.** Closing the browser sheet returns to "Sign in" without an error.
- [ ] **Killed during sign-in.** Kill the app while the browser is open, then finish
      the sign-in: the app opens on "Sign in" (the attempt is gone) and a new attempt
      works.
- [ ] **Refresh.** Leave the app signed in past the access-token lifetime (15 min by
      default); the next action works without a sign-in prompt.
- [ ] **Lost refresh answer.** With a proxy, drop the response of one refresh call:
      the app re-presents the same refresh token and continues (server grace window).
- [ ] **Revocation.** Revoke the device from the workspace's device list (or suspend
      the user): the next request wipes the app and lands on "Connect".
- [ ] **Sign out.** Settings → "Log out & wipe" revokes the device session (it shows as
      `signed_out` in the workspace's device list).
- [ ] **Minimum version.** Raise `min_client_version` above the app's version: the
      next request (and the connect check) shows "Update Agent Zefir"; signing out
      still works.
- [ ] **Policy.** Change `native_client_policy` on the workspace (lock required on/off,
      idle seconds, screenshots): within one access-token lifetime (or at once after
      "Log out & wipe" and sign-in) Settings shows the new values, the lock and the
      screenshot block follow them, and a relaunch keeps them before any network call.

## 9. Encrypted offline store (ADR-0025 WP6, `src/db/*`)

Set `native_client_policy.offline_retention_days` > 0 on the workspace first; with
0 (the default) the app must never create the file.

- [ ] **SQLCipher is linked.** On a dev build, open a conversation, then pull the
      app container (Xcode → Devices → Download Container; `adb exec-out run-as
      ca.zefir.agent` on Android) and run `sqlite3 az-offline.db .tables` on
      `SQLite/az-offline.db`: it must fail with "file is not a database" (the file is
      encrypted). `PRAGMA cipher_version` was non-empty (the app refuses to store
      anything otherwise; a warning "this build has no SQLCipher" in the log means the
      plugin option did not reach the native build).
- [ ] **No file without policy.** With `offline_retention_days = 0`, sign in and use
      the app: no `az-offline*.db` exists in the container for that workspace. Turning offline off while
      signed in deletes the file at the next token refresh (or sign-in).
- [ ] **Key behind the lock.** With `require_device_lock`, relaunch: the store opens
      only after Face ID / fingerprint / passcode (the key is read after unlock), and
      it is closed again when the app relocks after `idle_lock_seconds`.
- [ ] **Wipe.** "Log out & wipe" and a device revocation each delete that workspace's
      `az-offline-<id>.db` (or `az-offline.db` for the upgraded one) with its
      `-wal`/`-shm` files and its `az.ws.<id>.db.key` Keychain / Keystore item, and
      nothing of the other workspaces.
- [ ] **Offline read.** Open a few conversations online, then turn on airplane mode
      and relaunch: after unlock, the list, those threads (with their latest
      messages) and the notifications show with the "Offline" line; a conversation
      never opened shows read-only from the synced transcript.
- [ ] **Sync.** Back online, rename or delete a conversation in the web app: after the
      app returns to the foreground (or pull to refresh) the offline copy follows
      (renamed / gone). Remove yourself from the project: the next sync drops that
      project's offline data.
- [ ] **Outbox.** In airplane mode, send two messages in a thread: both show
      "Queued". Turn the network back on: they are admitted in order (each answer
      appears), with no duplicate turns in the web app. Kill the app while one is
      "Sending…" and relaunch online: it is sent once (the server replays the same
      `question_id`).

## 10. Chat quick wins (client contract 1.3: `src/features/chat/actions/*`, `src/features/speech/*`, `src/features/a11y/*`)

Needs a 1.3 workspace (discovery `client_contract` 1.3) for feedback, Stop, export and
suggestions; copy, share, read aloud, dictation and regenerate also work on 1.2.

- [ ] **Actions sheet.** Long-press an answer with a code block, and tap its "•••": Copy and
      "Copy python code" put the text on the clipboard (paste it in Notes); Share opens the
      system sheet with the text.
- [ ] **Share out off.** With `allow_share_out: false` in the admin policy (after the next
      token refresh, or at once via discovery): no Copy, Share or Export; the sheet says the
      workspace does not allow it.
- [ ] **Feedback.** Bad with a comment, then Good: the web app shows the like (and no
      dislike); tapping Good again removes it.
- [ ] **Stop.** Send `[[mock:slow]] tell me a story` (mock LLM) or a long prompt: Stop appears
      instead of Send; tap it: the stream stops at once, the partial answer stays, and the web
      app shows the run as stopped (no more tokens billed).
- [ ] **Regenerate.** On the newest answer: the old text is replaced by the new stream, and
      no "didn't replace this answer" note appears. The web app shows one answer, not two.
- [ ] **Dictation, on the device.** First tap on the microphone asks for the microphone and
      speech recognition (the texts say on-device). With airplane mode ON, dictation still
      works (proof that no server recogniser is used); on a device or language without an
      on-device model, the composer says dictation is not available and asks nothing.
      iOS with a region/language whose on-device model is missing (Settings → General →
      Language & Region, then a language not downloaded for dictation): tapping the microphone
      ends with "not available", never with words (proof the patched library refuses rather
      than using Apple's server recogniser).
- [ ] **Read aloud.** Reads the answer with "Code block." in place of code; Stop in the bar
      ends it; leaving the thread ends it; logout ends it.
- [ ] **Export (iOS).** Export → share sheet with `<conversation name>.md`; after the sheet closes,
      the app container's `Library/Caches/export-tmp` is empty.
- [ ] **Export (Android).** Export → chooser; send to Gmail or Drive: the target gets the `.md`
      FILE (not empty text). The file stays in `cache/export-tmp` until the next export.
- [ ] **Suggested agents.** New chat with no search shows "Suggested"; tapping one starts the
      chat with that agent.
- [ ] **Discover.** On a 1.3 workspace the conversation list starts with "Discover"; on a 1.2
      workspace it does not. Discover lists the public agents (icon from the workspace,
      category, tags); a search narrows them; an agent page shows its welcome message and
      starters. Start chat opens a new conversation in the project shown in the header chip,
      and the agent answers; a starter opens it with that text in the composer, unsent.
      Switch project: the next chat from Discover lands in the new project.
- [ ] **VoiceOver.** With VoiceOver on, send a message: "<agent> answered." is spoken once at
      the end (not while it streams); a sensitive tool pause says "Approval needed to run …".
- [ ] **Dynamic Type and Reduce Motion.** At the largest accessibility size the composer,
      chips and sheet rows stay usable; with Reduce Motion on, sheets fade in.

## Settings and home quick wins (client contract 1.3)

- [ ] **Dismiss a notification.** Swipe a row left: it slides out and stays gone after
      pull-to-refresh, after a relaunch offline (the offline copy), and on a second device
      signed in to the same workspace after its next sync (the server's tombstone). With
      VoiceOver, the row offers a "Dismiss" action. On a 1.2 workspace nothing swipes.
- [ ] **Type filter.** With several kinds of notifications, the chips filter the list;
      dismissing the last row of a kind returns to All.
- [ ] **Memory.** Settings → Memory lists the memories the web app shows for this project;
      swipe one away (the web app no longer has it); Clear all asks first. In airplane mode
      the screen says it needs a connection and shows nothing cached.
- [ ] **Usage.** Settings shows "Usage this month" for You and Project; as a project editor
      (not admin) only percentages, no amounts. With a member budget of $0 in the admin
      app, sending a message ends with "Your budget in this project is used up…", no Retry,
      and "View usage" opens Settings.
- [ ] **Quick actions.** Long-press the app icon: New chat and Notifications (with the unread
      count). No conversation names unless the admin turns on `allow_system_surfaces`; then
      the two latest chats appear and open their thread (behind the lock when it is due).
      iOS only: Android never lists chats by name, because a pinned shortcut would keep
      its title after a wipe (#35).
      Log out: the quick actions are gone; switch workspace: they follow the active one.
- [ ] **MDM AppConfig.** Push `server_url`, `require_device_lock: true`, `idle_lock_seconds: 60`
      and `allow_screenshots: false` (Apple Configurator or an MDM, see docs/mdm-appconfig.md):
      the connect screen starts with the address ("Suggested by your organization"), the app
      locks after a minute in the background even when the workspace does not require a lock,
      and Settings says the device management made the lock stricter.


## 11. Native surfaces (iOS: `src/features/surfaces/*`, `src/features/intents/*`, `app-intents/`)

See [native-surfaces.md](native-surfaces.md) for what each surface may show.

- [ ] **Widgets, counts only.** Add the Agent Zefir widget (small and medium) and the
      lock-screen widgets (inline, circular, rectangular). After a sync they show
      "waiting for you", "unread" and "running" counts for the active workspace, and no
      host or conversation name.
- [ ] **Widgets with titles.** Turn on `allow_system_surfaces` for the workspace: after
      the next foreground the medium widget shows the host and the waiting conversations'
      names. Turn it off: they are gone after the next foreground.
- [ ] **Widget tap.** With one conversation waiting, tapping opens it (behind the lock when
      it is due); with unread only, the inbox; otherwise the conversation list.
- [ ] **Widget wipe.** Log out & wipe: the widgets say "Open the app to sign in" at once.
      Switch workspace: they show the new workspace's counts after its first sync, never
      the old one's.
- [ ] **Running count goes stale.** Start a long answer, background the app: the widget's
      running count drops to 0 after ten minutes even though the app never ran again.
- [ ] **Live Activity.** Ask something that takes more than 5 s: an activity appears on the
      lock screen and in the Dynamic Island ("Your agent is answering", then "Using a
      tool…" / "Writing the answer…"), and ends with "Answer ready" (gone after ~2 min).
      A short answer shows none.
- [ ] **Live Activity on suspend.** Background or lock the app mid-answer: the activity
      says "Open the app for the latest" and is de-emphasised; return: it is live again.
- [ ] **Live Activity preview policy.** With `notification_preview: title` AND
      `allow_system_surfaces` the headline is the conversation's title; with either off
      (both are by default) it is generic. Never message text.
- [ ] **Live Activity cleanup.** Force-quit mid-answer and relaunch: the old activity is
      gone. Log out mid-answer, or with an
      answer waiting for approval: it is gone at once, while "Signing out..." still shows.
- [ ] **Siri: New chat.** "New chat in Agent Zefir" opens the agent picker. With
      `allow_system_surfaces` on, "New chat with <agent> in Agent Zefir" offers the
      selected project's agents and opens that agent's new conversation after unlock.
- [ ] **Siri: Ask.** "Ask <agent> in Agent Zefir": Siri does NOT ask "What do you want to
      ask?"; the app opens that agent's new chat with an empty composer. In Shortcuts, an
      Ask Agent action with a typed question opens with that question in the composer;
      nothing is sent until you tap Send. Siri reads nothing back.
- [ ] **Siri: Open notifications.** Opens the inbox, behind the app lock when it is due.
- [ ] **Siri while locked.** With the device locked, Siri asks to unlock the device first.
- [ ] **Siri after logout.** Log out: Shortcuts offers no agent names any more; a shortcut
      saved with an agent opens the picker of whichever workspace is active.
