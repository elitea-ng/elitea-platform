# Open questions

## 1. Push notifications

Push is out of the MVP. Three options are on the table for later:

- **Relay:** an Agent Zefir-operated push relay that holds the APNs/FCM credentials.
  Each server sends minimal, content-free wake-ups to the relay. This option needs:
  - an operated service
  - privacy terms
  - per-server registration and abuse controls
- **Direct:** each server sends to APNs/FCM itself. This needs the app's push
  credentials, which cannot be handed to self-hosted operators, so it is effectively
  limited to the SaaS offering.
- **Off:** in-app notifications only, which is the MVP behaviour.

Decide whether the payload ever carries content, or is wake-up-only with the content
fetched over the authenticated API.

## 2. Trademark clearance for "Agent Zefir"

Run a trademark search in the target jurisdictions (CA, US, EU) and the app stores before
publishing. Also confirm that store listing copy describes compatibility with Elitea
nominatively, without implying affiliation.

## 3. Publisher developer accounts

Decide which legal entity publishes the app. Then:

- Enroll in the Apple Developer Program as an organization, which requires a D-U-N-S
  number.
- Register a Google Play developer account as an organization.
- Assign ownership of the `ca.zefir.agent` identifiers and the signing keys.
- Decide whether EAS manages signing credentials.

## 4. Encryption export compliance

The offline store uses SQLCipher (`expo-sqlite` with `useSQLCipher`, AES-256), and
AES-GCM for cached attachments is planned. Both go beyond exempt-only encryption such
as HTTPS.

- Determine whether the exemptions apply, or whether a classification (for example a US
  ECCN 5D992 self-classification or an annual report) and the matching
  `ITSAppUsesNonExemptEncryption` / App Store Connect answers are required.
- Check the French import declaration if the app is distributed in France.

`app.config.ts` currently holds a placeholder value, which must be resolved before
submission.

## 5. MDM managed configuration

A library or module is needed to read managed app configuration:

- iOS: `com.apple.configuration.managed` in `NSUserDefaults`
- Android: `RestrictionsManager` app restrictions

Options:

- an existing community module, such as a react-native managed-config package (check
  maintenance and New Architecture support)
- a small in-repo Expo module (Swift and Kotlin)

Also define the key schema, such as `serverUrl`, `lockServer`, and policy overrides.

## 6. Offline attachments (follow-up)

The offline store keeps conversations, transcripts, notifications and the outbox.
Attachments are not stored, whatever `offline_attachments` says. Online, a file of a
sent message is downloaded when tapped (client contract 1.1), written to a temporary
file for the viewer or the system preview, and deleted when the viewer closes, at the
next open, when the thread goes, at app start and with the session's caches; the
inline thumbnails the model was given live in the encrypted transcript rows. Offline,
the files cannot be opened. The design
(per-file AES-GCM through `expo-crypto`, each file key wrapped by the database key,
files deleted with the store and by the same retention and size cap) is in
plan-app B4.6; it needs a decision on download triggers and size limits before it
is built.

