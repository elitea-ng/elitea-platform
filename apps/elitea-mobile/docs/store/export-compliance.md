# Encryption export compliance

**Decision in the repository:** `ITSAppUsesNonExemptEncryption = NO` (`false`
in [app.config.ts](../../app.config.ts)), because on iOS every cryptographic
operation in the app runs inside Apple's operating system.

**Owner action:** confirm this before the first TestFlight upload (it is the
publisher's legal responsibility, not Apple's or Expo's), and re-check it
whenever a library with its own cryptography is added. This page is the
reasoning, not legal advice. If anything below stops being true, the answer
changes (see "When to revisit").

Checked against the code and the installed native modules on 2026-10-07.

## What Apple asks

When an app is uploaded to App Store Connect it is exported from the United
States, so US export rules (the EAR) apply wherever the publisher is based.
App Store Connect's documentation table
([Export compliance documentation for encryption](https://developer.apple.com/help/app-store-connect/reference/export-compliance-documentation-for-encryption)):

| Encryption in use | Documentation |
|---|---|
| Limited to that within the Apple operating system | none |
| An industry-standard algorithm not provided by the Apple operating system | the French encryption declaration (only if distributed in France) |
| Proprietary or non-standard algorithms | US CCATS and the French declaration |

The Info.plist key answers the question once for every build: set it to `NO`
"if your app, including any third-party libraries it links against, doesn't
use encryption, or if it only uses forms of encryption that are exempt from
export compliance documentation requirements"
([Complying with Encryption Export Regulations](https://developer.apple.com/documentation/security/complying-with-encryption-export-regulations)).
"Typically, the use of encryption that's built into the operating system, for
example HTTPS through URLSession, is exempt."

## What the iOS app actually does

| Use | Implementation | Inside Apple's OS? |
|---|---|---|
| HTTPS to the workspace server, Expo update checks | `URLSession` (React Native networking, `expo/fetch`) | yes |
| Offline store at rest (conversations, transcripts, notifications, outbox) | SQLCipher, AES-256 page encryption with HMAC and PBKDF2 key derivation. expo-sqlite compiles it with `-DSQLCIPHER_CRYPTO_CC` (node_modules/expo-sqlite/ios/ExpoSQLite.podspec), so every primitive is CommonCrypto: `CCCrypt`, `CCHmac`, `CCKeyDerivationPBKDF`, `SecRandomCopyBytes` | yes |
| Database key, tokens | Keychain (expo-secure-store), `ThisDeviceOnly` | yes |
| PKCE challenge, ids | expo-crypto: SHA-256 digest, random bytes, random UUIDs. No cipher is used | yes (CommonCrypto, SecRandom) |
| Sign-in | `ASWebAuthenticationSession` (expo-web-browser) | yes |
| Device unlock | LocalAuthentication | yes |

No iOS native module in `node_modules` bundles OpenSSL, BoringSSL, libsodium,
mbedTLS or similar (checked across every podspec), and no JavaScript cipher is
used. Planned per-file AES-GCM for offline attachments
(docs/open-questions.md §6) must also go through an Apple API (CryptoKit or
CommonCrypto) to keep this answer.

So the iOS build is "encryption limited to that within the Apple operating
system": no CCATS, no French declaration, key `NO`.

## The US annual self-classification report

Apple notes that exempt-but-encrypting apps "might alternatively be required to
submit a year-end self-classification report". Since BIS's rule of
29 March 2021, mass-market encryption items under License Exception ENC
§740.17(b)(1), mobile apps included, no longer need that report; it remains
only for some chips, chipsets and similar components
([BIS mass-market guidance](https://www.bis.gov/learn-support/encryption-controls/mass-market);
law-firm summaries: [Wiley](https://www.wiley.law/alert-Commerce-Eases-Encryption-Export-Controls-Reporting-Requirements),
[Goodwin](https://www.goodwinlaw.com/en/insights/publications/2021/03/03_31-spring-cleaning-us-department-of-commerce)).
An AI chat client that encrypts its own local data with standard algorithms is
a mass-market item. No report is expected; the owner confirms.

## Android

Google Play asks no export question, but the Android build differs: expo-sqlite
links SQLCipher against a bundled OpenSSL (`io.github.ronickg:openssl`,
`-DSQLCIPHER_CRYPTO_OPENSSL`), which is standard cryptography that is not part
of the operating system. Distribution happens from the publisher's country
(Canada: the Export Control List, Group 1, category 5 Part 2, follows the
Wassenaar mass-market note) and from Google's infrastructure. Standard
algorithms in a mass-market consumer app are generally covered by the
mass-market provisions on both sides; the owner confirms. Nothing has to be
filed with Google.

## France

Apple shows the French declaration only for apps with non-OS standard or
proprietary encryption. The iOS app has neither, so nothing is filed for the
App Store in France. For Google Play in France, the bundled OpenSSL makes the
Android app "secure storage" in ANSSI's terms; ANSSI's simplified regime
covers supply of mass-market products with standard cryptography, and the
owner should check whether a declaration is needed before enabling France on
Play.

## When to revisit

Change the key, and this page, if any of these happens:

- a library with its own cryptography lands in the iOS build (OpenSSL,
  libsodium, a pure-JS cipher, a VPN or messaging SDK with its own crypto);
- end-to-end encryption of messages or notification payloads (open-questions
  §1) is built with anything other than CryptoKit or CommonCrypto;
- any proprietary or non-standard algorithm appears;
- Apple changes its question set.

A new build with the key `YES` then needs the matching documentation uploaded
in App Store Connect (App Information, App Encryption Documentation) and the
code Apple returns as `ITSEncryptionExportComplianceCode`.
