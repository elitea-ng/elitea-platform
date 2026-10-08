# Privacy answers: App Store, Google Play, privacy manifest

Drafted from the code on 2026-10-07 (not from intentions). Re-check this page
whenever a native module, a network destination or a stored field is added.

## The data flows

Agent Zefir has **no backend of its own**. It talks to exactly two kinds of
host:

1. **The workspace server the user connects to** (an Elitea-compatible server
   run by the user's organization, or a hosted workspace). The user types,
   scans or taps its address, confirms it, and signs in with the
   organization's own SSO in the system browser sheet.
2. **Expo's update service** (`u.expo.dev`), once the app is linked to EAS
   (docs/release.md §10): update checks only.

There is no analytics SDK, no crash reporter, no advertising SDK, no tracking,
no push service (no push token is ever requested) and no other third-party
host in the code.

### Sent to the workspace server

| Data | When | Code |
|---|---|---|
| Device label (`Agent Zefir on iPhone` / `on iPad` / `on <Android model>`), platform, app version | at sign-in, shown on the consent page and in the user's device list | src/auth/deviceInfo.ts, src/auth/nativeAuth.ts |
| Access and refresh tokens (bearer) | every request | src/auth/, src/api/ |
| Messages the user writes, regenerate and stop requests | when sending | src/api/queries/sendMessage.ts |
| Photos, camera pictures and files the user attaches | only when picked and sent | src/features/chat/attachments/ |
| Like / dislike and the optional comment | when given | src/api/queries/feedback.ts |
| Discover search text | when searching agents | src/api/queries/discover.ts |
| Notification read / dismiss, memory edits, device revoke | when done | src/api/queries/ |

What the server returns (the user's name, email and avatar from its directory,
conversations, agents, notifications, usage) comes from the organization's own
systems; the app does not create or send it.

### Kept on the device only

- Dictation: on-device speech recognition only
  (`requiresOnDeviceRecognition`); audio never leaves the device.
- The offline store (SQLCipher, key in the Keychain / Keystore), only when the
  workspace policy allows it; wiped on log out, revoke and workspace removal.
- Widgets and the Live Activity: counts (and titles only if the workspace
  policy allows), in the App Group; no credentials.
- Face ID / fingerprint: handled by the OS; the app gets only "unlocked".
- Android backup is off (`allowBackup: false`); no plaintext HTTP cache
  (plugins/withNoHttpCache.js).

### Sent to Expo (EAS Update)

Each update check sends the platform, runtime version, channel, the ids of the
updates already received, and a random per-installation id (expo-eas-client,
"EAS client ID", stored in native preferences, reset on reinstall). It
identifies an installation, not a person. Expo uses it to serve updates and to
count monthly active installations for billing. IP addresses reach Expo as
with any HTTPS request.

## Who collects (owner decision)

Apple: "collect" means transmitting data off the device so that **you and/or
your third-party partners** can access it for longer than needed to serve the
request in real time. Google's Data safety asks about data that leaves the
device, with exceptions for user-initiated transfers the user expects.

The workspace server is usually run by the user's employer, not by the
publisher. Two honest readings follow:

- **A. Conservative (the default in the repository).** Declare the data the
  app sends to workspaces as collected, linked to the user, for App
  Functionality. Right whenever the publisher (or a company it contracts)
  operates any workspace, for example a hosted SaaS workspace or a demo
  workspace, and safe otherwise. The iOS privacy manifest
  (`NSPrivacyCollectedDataTypes` in app.config.ts) carries this answer.
- **B. Client-only.** If the publisher operates no workspace at all and never
  receives workspace data, the workspace data is the organization's, not
  collected by the developer; only Expo's installation id remains (Device ID,
  not linked). Then trim the manifest's collected types to Device ID and
  answer the forms with B.

Pick one and use it in both stores and in the privacy policy. The demo
workspace the reviewer signs in to (listing.md) is operated by the publisher,
so A is the expected answer at launch.

## App Store Connect: App Privacy (answer A)

"Do you or your third-party partners collect data from this app?" **Yes.**

| Data type (Apple) | Collected | Linked to the user | Tracking | Purposes |
|---|---|---|---|---|
| Identifiers, User ID | yes (account and device ids in the workspace) | yes | no | App Functionality |
| Identifiers, Device ID | yes (Expo update installation id) | no | no | App Functionality |
| User Content, Photos or Videos | yes (attachments the user sends) | yes | no | App Functionality |
| User Content, Other User Content | yes (messages, attached files, feedback comments) | yes | no | App Functionality |
| Search History | yes (Discover search) | yes | no | App Functionality |
| Contact Info (name, email) | no: received from the organization's directory, not sent by the app | | | |
| Audio Data | no: dictation is on the device | | | |
| Location, Contacts, Health, Financial, Browsing History, Purchases, Diagnostics, Usage Data, Sensitive Info | no | | | |

Tracking: **no** (no data is linked with third-party data for advertising or
shared with data brokers; no App Tracking Transparency prompt).

If OTA updates are never enabled (no EAS project linked), drop the Device ID
row; the privacy manifest already leaves it out of unlinked builds.

## Google Play: Data safety (answer A)

- Does your app collect or share any of the required user data types? **Yes.**
- Is all of the user data collected by your app encrypted in transit? **Yes**
  (HTTPS only: release builds refuse plain-http workspace addresses; only
  development builds accept http, and only for a private host).
- Do you provide a way for users to request that their data is deleted?
  **Yes**: in the app, Settings, "Log out & wipe" deletes everything on the
  device; data on the workspace server is deleted through the organization's
  workspace administrator; the privacy policy explains both. The app creates no
  accounts, so Play's in-app account-deletion requirement does not apply.

| Category, type (Play) | Collected | Shared | Optional? | Purpose |
|---|---|---|---|---|
| Personal info, User IDs | yes | no | required | App functionality, Account management |
| Photos and videos, Photos | yes | no | optional (only when attached) | App functionality |
| Files and docs | yes | no | optional (only when attached) | App functionality |
| App activity, Other user-generated content (messages, feedback) | yes | no | required | App functionality |
| App activity, In-app search history | yes | no | optional | App functionality |
| Device or other IDs (Expo update installation id) | yes | no (Expo is a service provider) | required while OTA is on | App functionality |
| Audio, Location, Contacts, Financial, Health, Messages (SMS/email), Calendar, Web browsing, App info and performance | no | | | |

"Shared" stays **no**: sending data to the server the user chose to sign in to
is a user-initiated transfer, and Expo processes on the publisher's behalf.

Android permissions the listing will show (from the generated manifest):
`INTERNET`, `RECORD_AUDIO` (dictation, on-device), `USE_BIOMETRIC` /
`USE_FINGERPRINT` (unlock), `VIBRATE` (haptics), `READ_EXTERNAL_STORAGE` and
`WRITE_EXTERNAL_STORAGE` up to Android 12 (picking files on old versions).
`SYSTEM_ALERT_WINDOW` from the prebuild template is removed
(`android.blockedPermissions`). The camera is used through the system camera
app, so there is no `CAMERA` permission; photos come from the system photo
picker, so there is no `READ_MEDIA_IMAGES` and no Photo and Video
Permissions declaration.

## iOS privacy manifest (PrivacyInfo.xcprivacy)

Apple rejects uploads whose binaries call "required reason" APIs without a
declared reason (ITMS-91053), and it does not read the manifests inside static
CocoaPods, which is how every Expo module is linked
([Expo: Privacy manifests](https://docs.expo.dev/guides/apple-privacy/)). So
the app declares the union itself, in `ios.privacyManifests`
(app.config.ts), and the widget extension gets its own
(plugins/withExtensionPrivacyManifest.js), because Apple scans the `.appex`
too.

| API category | Reasons | Who calls it |
|---|---|---|
| UserDefaults | `CA92.1` (app's own defaults), `1C8F.1` (App Group shared with the widget extension) | React Native, expo-constants, expo-system-ui, expo-updates, expo-eas-client; expo-widgets, expo-app-intents, expo-sharing (App Group suite); managed app configuration |
| File timestamp | `C617.1` (files in the app container), `0A2A.1` (SDK wrapper), `3B52.1` (files the user granted) | React Native, Folly, glog, boost, expo-application, SQLite; expo-file-system; expo-document-picker |
| System boot time | `35F9.1` (elapsed time between in-app events) | React Native timing, boost |
| Disk space | `E174.1` (check space before writing), `85F4.1` (show space to the user) | expo-file-system; SQLite `statfs` |

Widget extension (`ExpoWidgetsTarget`, links React Native, expo-modules-core,
expo-widgets and @expo/ui): UserDefaults `CA92.1`, `1C8F.1`; file timestamp
`C617.1`; system boot time `35F9.1`. It collects nothing.

`NSPrivacyTracking` is false with no tracking domains, in both.

**Drift guard:** `scripts/eas-config.test.mjs` fails CI when an installed
module's own PrivacyInfo.xcprivacy declares a reason the app manifest lacks.
Modules that ship no manifest (expo-widgets, expo-app-intents, expo-sharing,
expo-document-picker, expo-sqlite's SQLite) were audited by reading their
native sources; re-audit when one is added or upgraded:

```bash
grep -rhoE "UserDefaults|systemUptime|mach_absolute_time|statfs|volumeAvailableCapacity|creationDate|modificationDate|activeInputModes" node_modules/<module>/ios
```

The first TestFlight upload is the real check: Apple emails ITMS-91053 naming
the binary and API if something is still missing.
