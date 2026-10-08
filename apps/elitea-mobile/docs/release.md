# Release runbook: EAS Build, Submit, TestFlight and Play

How Agent Zefir goes from `main` to testers and the stores. The repository is
ready for this; the accounts are not. Sections 1 to 6 are one-time setup that
only the owner can do (they need a legal entity, payment and identity checks).
Sections 7 onwards are the repeatable release work.

Store copy, privacy answers and the export-compliance reasoning live in
[docs/store/](store/README.md).

| Piece | Where | State |
|-------|-------|-------|
| Build, submit and update profiles | [`eas.json`](../eas.json) | ready |
| EAS project link, OTA config, privacy manifest, export-compliance key | [`app.config.ts`](../app.config.ts) | ready; project id empty until `eas init` |
| Widget extension privacy manifest | [`plugins/withExtensionPrivacyManifest.js`](../plugins/withExtensionPrivacyManifest.js) | ready |
| Manual build and submit workflows | [`.github/workflows/eas-build.yml`](../.github/workflows/eas-build.yml), [`eas-submit.yml`](../.github/workflows/eas-submit.yml) | ready; need `EXPO_TOKEN` |
| Structural checks of all of the above | [`scripts/eas-config.test.mjs`](../scripts/eas-config.test.mjs) (runs in CI) | ready |
| Expo account, Apple organization, Play Console | owner | **to do** (sections 1 to 6) |

Normal CI (`ci.yml`) never builds natively and never talks to Expo. Builds
happen only when someone dispatches a workflow or runs `eas` by hand.

Run EAS CLI as `npx eas-cli@latest <command>` (written `eas` below). The
workflows pin the CLI version (`EAS_CLI_VERSION`); `eas.json` sets the floor.

---

## 1. Accounts to create (owner)

Do these in order: the Apple and Google organization accounts both need the
D-U-N-S number, and both take days to verify.

### 1.1 Legal entity and D-U-N-S number

1. Decide which legal entity publishes the app (docs/open-questions.md §3).
   The store listings show its legal name as the seller and developer.
2. Look the entity up, or request a number for free, through Apple's D-U-N-S
   lookup (developer.apple.com, enrolment, "Look up your D-U-N-S number").
   The name and address must match the entity's registration exactly.
   Allow one to two weeks for a new number, plus a few days before Apple sees it.
3. Have a website on the entity's domain (agent.zefir.ca or zefir.ca). Apple
   checks it; the privacy and support pages in section 8 go there.

### 1.2 Apple Developer Program (organization)

1. Create, or choose, an Apple Account with an address on the entity's domain,
   two-factor authentication on. It becomes the **Account Holder**: keep it as a
   shared, recoverable account, not one person's personal account.
2. Enrol as an **Organization** at developer.apple.com/programs/enroll with
   the D-U-N-S number. You need legal authority to bind the entity. The fee is
   USD 99 a year. Apple may phone to verify.
3. In App Store Connect, **Business** (Agreements, Tax and Banking): the
   Account Holder accepts the Free Apps agreement. The app is free; no paid
   agreement, tax or bank forms are needed unless that changes.
4. **Users and Access**: invite the people who will release (App Manager role)
   and testers (Developer or Marketing role; internal testers must be users).

### 1.3 Expo account and organization

1. Sign up at expo.dev (free plan is enough to start; see the note on build
   minutes below).
2. Create an **organization** (for example `zefir-ca`) so the project does not
   belong to one person, and invite the release team.
3. Under the organization, **Settings, Access tokens**: create a **robot user**
   (role: Developer) and an access token for it. That token is the GitHub
   `EXPO_TOKEN` secret (section 6). Do not use a personal token in CI.

The free plan has a limited number of cloud builds a month and lower priority.
Update usage is billed per monthly active user. Check expo.dev/pricing before
the first external TestFlight round.

### 1.4 Google Play Console (organization)

1. Register at play.google.com/console as an **Organization** with the same
   D-U-N-S number. One-time fee USD 25. Google verifies identity and the
   organization; allow several days.
2. Organization accounts are not subject to the 12-tester, 14-day closed-test
   rule that personal accounts created after November 2023 have; internal
   testing works as soon as the app exists.
3. Invite the release team under **Users and permissions**.

## 2. App Store Connect app record (owner)

1. **Certificates, Identifiers and Profiles** (developer.apple.com): EAS
   registers `ca.zefir.agent`, the widget extension `ca.zefir.agent.widgets`
   and the App Group `group.ca.zefir.agent` on the first build (section 4), so
   nothing is needed here first. If you prefer to create them by hand, enable
   the App Groups capability on both identifiers and attach the group.
2. App Store Connect, **Apps, +, New App**:
   - Platform: iOS
   - Name: `Agent Zefir` (docs/store/listing.md)
   - Primary language: English (U.S.) or English (Canada)
   - Bundle ID: `ca.zefir.agent` (permanent: it can never change)
   - SKU: `agent-zefir-ios`
   - User access: Full access
3. Open the new app, **App Information**: copy the numeric **Apple ID** (for
   example `6740000000`) into [`eas.json`](../eas.json) as
   `submit.production.ios.ascAppId` and merge that one-line change. The submit
   workflow refuses to run until it is there.
4. Fill in **App Privacy** from docs/store/privacy.md, **Age Rating** from
   docs/store/listing.md, the privacy policy URL `https://agent.zefir.ca/privacy`,
   and the category.

## 3. Google Play app (owner)

1. Play Console, **Create app**: name `Agent Zefir`, default language English,
   App, Free, accept the declarations.
2. **App content**: privacy policy URL, Data safety (docs/store/privacy.md),
   content rating questionnaire, target audience (18 and over, or 13 and over
   with a reason: docs/store/listing.md), ads: none, app access: the review
   sign-in from docs/store/listing.md.
3. **Play App Signing** stays on (the default): Google keeps the app signing
   key, EAS holds the upload key.
4. **Service account for submissions**: Google Cloud console, create a project
   (or use one), enable the Google Play Android Developer API, create a service
   account and a JSON key. In Play Console **Users and permissions**, invite the
   service account email with "Release apps to testing tracks" (and production
   later). Upload the JSON key to EAS, not to the repository:
   `eas credentials -p android`, Google Service Account, upload. Delete the
   local copy afterwards.
5. **The first upload must be manual.** Google accepts API uploads only after
   one bundle exists: download the first production `.aab` from its EAS build
   page and upload it in **Testing, Internal testing, Create new release**.
   While the app has never been reviewed, API releases can only be drafts, so
   `eas.json` submits with `releaseStatus: "draft"`; switch it to `"completed"`
   once the app has passed its first review.

## 4. Link the EAS project (owner, once)

```bash
eas login                  # the Expo account from 1.3 (a member of the organization)
eas init                   # creates the project under the organization
```

`app.config.ts` is dynamic, so `eas init` cannot write the link itself: it
prints the project id. Put the id and the organization name into `easLink` in
[`app.config.ts`](../app.config.ts) and merge the change:

```ts
const easLink = {
  projectId: '<uuid from eas init>',
  owner: 'zefir-ca',
};
```

Committing the link (it is not a secret) is the recommended way: every
machine, CI job and EAS build server then evaluates the same config, so the
runtime fingerprint of a build and of an update agree. `EAS_PROJECT_ID` and
`EXPO_OWNER` in the environment override it (handy for a fork or a dry run);
if you rely on them instead, set them as EAS environment variables in all
three environments too (`eas env:set`), or the build server sees an unlinked
app.

Once linked, the app has `updates.url` and EAS Update is on (section 10).
Unlinked builds (any local build today) have updates disabled.

Then protect the production update channel so only organization admins can
publish to it:

```bash
eas channel:create production   # the first build creates channels too
eas channel:protect production
```

## 5. Credentials (let EAS manage them)

EAS generates, stores and renews signing credentials. Nothing secret is ever
committed: `.gitignore` already refuses keystores, `.p8`, `.p12` and
provisioning profiles, and a test refuses key paths in `eas.json`.

**iOS**: the first production build runs on your machine, interactively,
because it needs the Apple Account login once:

```bash
eas build --platform ios --profile production
```

Answer yes to logging in to Apple and to letting EAS generate everything. It
creates the distribution certificate, the App Store provisioning profiles for
`ca.zefir.agent` and `ca.zefir.agent.widgets`, and the App Group. Then create
the App Store Connect API key EAS uses to submit:

```bash
eas credentials --platform ios   # production, App Store Connect: Manage your API Key, set up
```

After that, CI can build and submit iOS without a person.

**Android**: the first `eas build --platform android --profile production`
generates the upload keystore on EAS. Download a backup once
(`eas credentials -p android`, Keystore, download) and keep it in the
organization's password manager. With Play App Signing, a lost upload key can
be reset through Google, but it takes days.

**Internal iOS builds** (`development`, `preview`) are ad hoc: they install
only on registered devices. Register each tester's device with
`eas device:create` (it gives a link or QR to open on the device); the next
build includes it.

## 6. GitHub Actions secret

1. Create the robot token (1.3, step 3). Expo shows it once.
2. Add it to the repository: Settings, Secrets and variables, Actions,
   New repository secret, name `EXPO_TOKEN`; or
   `gh secret set EXPO_TOKEN --repo zefir-ca/agent-zefir` and paste it.
3. Rotate it by creating a new robot token, replacing the secret, and deleting
   the old token in Expo. Revoke it at once if it leaks: it can publish OTA
   updates to every installed app.

Nothing else goes into GitHub: Apple and Google credentials stay on EAS.

## 7. Builds

| Profile | For | iOS | Android | Channel |
|---------|-----|-----|---------|---------|
| `development` | Developers on devices: dev client, loads JS from Metro | ad hoc, registered devices | debug APK | `development` |
| `development-simulator` | Developers on the iOS Simulator | simulator `.app` | debug APK | `development` |
| `preview` | QA and stakeholders before TestFlight: release build, installed from the EAS link | ad hoc, registered devices | release APK | `preview` |
| `production` | TestFlight, Play internal testing, then the stores | App Store signed | app bundle (`.aab`) | `production` |

From GitHub: **Actions, EAS build, Run workflow**, pick the profile and
platform; tick "submit" with `production` to send the build to TestFlight and
Play internal testing when it finishes. The job queues the build and prints
its EAS page; the native build itself runs on EAS. From a terminal:

```bash
eas build --profile preview --platform all
eas build --profile production --platform ios --auto-submit
```

Every EAS environment (`development`, `preview`, `production`) can hold
environment variables (`eas env:set --environment production ...`). The app
needs none today: the workspace URL is chosen by the user, and nothing secret
belongs in a mobile binary.

### First internal build, step by step

1. Sections 1 to 6 done; `easLink` and `ascAppId` merged.
2. Local, interactive, once per platform (credentials, section 5):
   `eas build -p ios --profile production` and `eas build -p android --profile production`.
3. iOS: `eas submit -p ios --profile production --latest` (or the EAS submit
   workflow). Wait for "Ready to Test" in TestFlight (processing takes 10 to 30
   minutes; Apple emails if the upload is rejected, for example ITMS-91053 for
   a missing privacy-manifest reason).
4. Android: upload that first `.aab` by hand (section 3, step 5).
5. From then on: the EAS build workflow with `production` and "submit".

## 8. TestFlight and Play testing

**TestFlight internal testing** (up to 100 App Store Connect users, no review):
TestFlight, Internal Testing, create a group (for example "Team"), add users,
turn on automatic distribution. Every processed build reaches them.

**TestFlight external testing** (up to 10,000 people by email or public link):
create an external group. The first build of each version goes through Beta
App Review (usually about a day). Fill in **Test Information** first: beta
description, feedback email, privacy policy URL, and the sign-in details from
docs/store/listing.md ("Review notes"), because reviewers must be able to sign
in to a workspace.

**Play internal testing** (up to 100 testers): Testing, Internal testing,
Testers, an email list; share the opt-in link. Closed and open testing tracks
follow the same pattern with review.

Export compliance is answered by the Info.plist key, so TestFlight does not ask
per build (docs/store/export-compliance.md).

## 9. Versioning

- **App version** (`version` in app.config.ts, `CFBundleShortVersionString`,
  Android `versionName`): semantic, changed by hand in a pull request when a
  release is cut. App Store Connect needs a new version for each store release.
- **Build number** (`CFBundleVersion`, Android `versionCode`): kept on EAS
  (`cli.appVersionSource: "remote"`) and incremented by every `preview` and
  `production` build. Never edit it in the config. To align with a build
  uploaded some other way: `eas build:version:set`.
- **Runtime version**: the native fingerprint, computed by EAS (section 10).
  You never set it.
- The app sends its version as `client_version` at sign-in, and servers can
  refuse old clients (426). Raise the version before a release that needs a
  newer server contract.

## 10. OTA updates (EAS Update)

Each build belongs to a channel (`eas.json`) and has a **runtime version**
computed from its native layer (`runtimeVersion: { policy: "fingerprint" }`).
An update reaches a build only on the same channel and with the same
fingerprint, so JavaScript that needs a native change can never land on a
binary without it. Updates download in the background at launch and apply on
the next cold start (`fallbackToCacheTimeout: 0`); a launch never waits for the
network.

**What may ship over the air**: JavaScript and asset fixes (bugs, copy, layout,
logic) for a binary already in testers' or users' hands.

**What needs a store build**: anything native (a new module, a config plugin,
a permission, an Info.plist or manifest change, an SDK upgrade). The
fingerprint enforces this: such a change produces a new runtime version, and
an update published from it reaches no existing build.

**Policy**:

1. Every change merges through a pull request with green CI first. Updates are
   published from `main`, never from a branch or a dirty tree.
2. Preview first: `eas update --channel preview --environment preview -m "..."`,
   check it on a `preview` build.
3. Then production, rolled out gradually:
   `eas update --channel production --environment production -m "..." --rollout-percentage 10`,
   watch `eas update:insights` and `eas channel:insights`, then raise the
   percentage with `eas update:edit` until it reaches 100.
4. Changes to authentication, the device lock, the offline store, the wipe
   path or policy enforcement go out in a store build, not only over the air,
   so they are reviewed and land with a version number the server can see.
5. The production channel is protected (section 4): only organization admins
   publish to it. The CI robot token can still publish (keep it secret).

**Hardening to decide before the first public release** (owner):
end-to-end code signing of updates (`updates.codeSigningCertificate` in the
app config, a private key kept outside EAS, `eas update --private-key-path`),
so a leaked Expo token alone cannot push code to devices. Check that the Expo
plan includes it. Some enterprise customers also forbid OTA for managed apps;
a future workspace policy or MDM key could turn update checks off.

Every update check sends Expo the platform, the runtime version, the channel
and a random per-installation id (expo-eas-client). The privacy answers in
docs/store/privacy.md include it.

## 11. Rollback

**An OTA update is bad**:

- Roll back the latest update: `eas update:rollback` (republishes the previous
  update group, or the embedded bundle if there is none).
- Or go straight back to the JavaScript inside the binary:
  `eas update:roll-back-to-embedded --channel production`.
- Or stop the channel while you investigate: `eas channel:pause production`
  (`eas channel:resume` later).
- A rollout in progress: `eas update:revert-update-rollout`.

Devices pick the rollback up on their next launch, then apply it on the one
after.

**A store build is bad**:

- iOS: during a phased release, pause it in App Store Connect (up to 30 days).
  A released version cannot be withdrawn to the previous binary: fix it over
  the air if the problem is JavaScript, otherwise submit a new build with an
  expedited review request. "Remove from sale" is the last resort.
- Android: halt the staged rollout in Play Console (or submit with
  `releaseStatus: "halted"`). Play never accepts a lower `versionCode`: ship a
  new build (EAS increments it) built from the last good commit.
- TestFlight: expire the build in App Store Connect; testers keep the previous
  one.

## 12. Per-release checklist

- [ ] `version` bumped in app.config.ts; release notes written (docs/store/listing.md "What's new").
- [ ] CI green on the release commit; device checks in docs/device-verification.md done on a `preview` build.
- [ ] Production build for both platforms; TestFlight internal and Play internal testing pass.
- [ ] No new native module without its privacy-manifest reasons (`npm run test:scripts` fails otherwise) and its Data safety and App Privacy answers.
- [ ] No new cryptography outside the operating system on iOS (docs/store/export-compliance.md); otherwise the key must be revisited.
- [ ] Review sign-in still works (demo workspace and test account, docs/store/listing.md).
- [ ] Screenshots still match the app (docs/store/screenshots.md).
- [ ] Submit for review; phased release (iOS) and staged rollout (Android) on.

## 13. Owner decisions still open

| Decision | Where | Default in the repository |
|----------|-------|---------------------------|
| Publishing entity, D-U-N-S, account holders | 1.1, open-questions §3 | none |
| `ITSAppUsesNonExemptEncryption = NO` confirmed | docs/store/export-compliance.md | `false` (iOS uses only Apple OS crypto) |
| Who "collects" data for the privacy labels | docs/store/privacy.md, "Who collects" | the conservative answer (declared) |
| Age rating / target audience | docs/store/listing.md | 13+ (Apple) / Teen, 18+ audience (Play) |
| Demo workspace and reviewer test account | docs/store/listing.md, "Review notes" | none: must exist before the first external TestFlight round |
| OTA code signing; OTA for MDM-managed installs | section 10 | not enabled |
| Trademark clearance for the name | open-questions §2 | pending |
