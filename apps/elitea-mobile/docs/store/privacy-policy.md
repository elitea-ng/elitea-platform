# Privacy policy (draft for https://agent.zefir.ca/privacy)

> Draft for the owner. Replace every `[bracketed]` value, have it reviewed
> (Canadian PIPEDA, Quebec Law 25, GDPR and the US state laws apply depending on
> where users are), and publish it at `https://agent.zefir.ca/privacy` before
> the first external TestFlight round. It must match the App Store and Play
> answers in privacy.md; it follows answer A ("Who collects").

---

**Agent Zefir Privacy Policy**

Last updated: [date]

Agent Zefir ("the app") is published by [legal entity name], [address]
("we", "us"). This policy explains what the app does with your information.

### The short version

- Agent Zefir is a client for AI-agent workspaces run by your organization or
  by a hosting provider your organization chose. Your conversations go to that
  workspace, not to us.
- We have no advertising, no analytics, no tracking and no crash reporting in
  the app. We do not sell or share your information.
- What the app keeps on your phone is encrypted and is deleted when you log
  out.

### 1. Who is responsible for what

When you connect the app to a workspace, you sign in with your organization's
account, and your organization (or the provider it uses) runs that workspace
server. Your organization decides what the workspace stores, for how long, and
who can see it; its own privacy notice covers that. For a workspace that we
operate ourselves (for example a demo workspace), we are responsible for it,
and this policy covers it too.

### 2. Information the app sends to your workspace

Only to the workspace address you entered, scanned or opened, and only over
encrypted connections (HTTPS):

- **Sign-in and device information**: a device label such as "Agent Zefir on
  iPhone", the platform (iOS or Android) and the app version, so you and your
  administrator can see and sign out your devices. No hardware identifiers.
- **What you send**: your messages, the photos, pictures and files you choose
  to attach, your likes, dislikes and feedback comments, your searches in
  Discover, and actions such as dismissing a notification or deleting a memory.
- **Sign-in tokens** that let the app act for you. They are stored in your
  device's secure storage (Keychain or Keystore).

The workspace sends back your profile (name, email, picture), your
conversations, agents and notifications from your organization's systems.

### 3. Information that stays on your device

- **Dictation** is turned into text on your device. Audio is never sent to
  Apple, Google, your workspace or us.
- **Offline copy**: if your organization allows it, recent conversations are
  kept in an encrypted database on the device so you can read them without a
  connection. The key never leaves the device's secure storage.
- **Face ID, Touch ID and fingerprint** are handled by your phone. The app
  only learns whether unlocking succeeded.
- **Widgets and Live Activities** show counts (and titles only if your
  organization allows it). They hold no sign-in information.
- Device backups do not include the app's data (Android backup is turned off;
  iOS keeps the keys on this device only).

### 4. App updates (Expo)

The app can download bug fixes from our update service, provided by Expo
(650 Industries, Inc.). Each check sends the platform, the app's runtime
version and update ids, and a random identifier for this installation (it is
not linked to you and changes when you reinstall). Your IP address is visible
to Expo as with any internet request. Expo processes it for us to deliver
updates and count active installations; see https://expo.dev/privacy.

### 5. What we do not do

No advertising, no advertising identifiers, no tracking across apps or
websites, no analytics or crash-reporting services, no sale or sharing of
personal information, no push notifications (the app never asks for a push
token).

### 6. Deleting your information

- **On your device**: Settings, **Log out & wipe** deletes the offline copy,
  tokens, caches and temporary files. Removing a workspace, being signed out by
  your administrator, or uninstalling the app deletes them too.
- **On the workspace**: conversations and files you sent are kept by your
  organization's workspace. Ask your workspace administrator to delete them,
  or use the workspace's own deletion features.
- The app creates no account with us, so there is no account of ours to
  delete.

### 7. Children

The app is meant for people using their organization's workspace and is not
directed at children under [13 / 16, matching the age rating].

### 8. Security

Connections use HTTPS. Data at rest on the device is encrypted (SQLCipher,
with its key in the device's secure storage); the app can lock behind your
device passcode or biometrics, hides its content in the app switcher, and
stores no plaintext web cache.

### 9. Changes and contact

We will post changes here and update the date above. Questions or requests:
[privacy contact email], [postal address]. If you are in Canada you may also
contact the Office of the Privacy Commissioner of Canada; in the EU or UK, your
data protection authority.

Agent Zefir is an independent app. Elitea is a trademark of its respective
owner; Agent Zefir is not affiliated with or endorsed by it.
