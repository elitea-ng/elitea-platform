# The macOS download is a styled, notarised disk image; the zipped app is gone (#1699, ADR-0052)

- **What changed.**
  - **`Elitea-<version>-macos-arm64.dmg` replaces `Elitea-<version>-macos-arm64.app.zip`** on every stable release
    and nightly pre-release (owner decision 2026-11-22). Opening it shows one window: the brand's background, the app
    on the left, an Applications alias on the right and an arrow between them. The volume is named
    `<Brand> <version>` and carries the brand's icon. The disk image is codesigned with the Developer ID Application
    identity, notarised and stapled; the release checks it with `spctl -a -t open --context context:primary-signature`
    and `stapler validate`. `SHA256SUMS` and the release notes' **Desktop downloads** section list the `.dmg`.
  - **The `.pkg` stays**, unchanged: it is the device-management install (ADR-0052 decision 8).
  - **`desktop-macos-package` builds the `.dmg`** next to the `.pkg`, in the same `package-dir` output, and has new
    `dmg` and `dmg-settings` outputs. A pull request's dry run builds the unsigned styled image of the sample brand
    and checks its layout.
  - **A brand can ship its own background**: `assets/macos/dmg-background.png` (660x400, opaque) and
    `assets/macos/dmg-background@2x.png` (1320x800), drawn around the fixed layout (`scripts/brand/dmg.mjs`).
    Both are optional, but come as a pair: brand-check checks their size and alpha, and fails a brand that ships only
    one. Without them the build draws one from the brand's hue.
  - **`shortName` may no longer hold `/`, `:`, `"` or `\`**: it names the app bundle and the disk image volume, which
    Finder's AppleScript addresses by name. brand-check fails a manifest that does.
  - The signed macOS job's timeout is 150 minutes (it was 90): it now waits for three notarisations.
  - The keychain code `macos-pkg.sh` used is shared with the new `macos-dmg.sh` (`macos-keychain.sh`); the `.pkg` is
    signed as before.
- **Who is affected.**
  - People who download the desktop app for a Mac: they open the `.dmg` and drag the app to Applications, instead of
    unzipping it.
  - Anything that fetched the `.app.zip` from a release by name (a script, an MDM's URL): there is no such file from
    this release on.
  - Brand repositories: from a release with this change, their macOS job builds the `.dmg` too, and the template's
    keep step keeps it. A brand whose `shortName` holds one of the refused characters fails brand-check.
- **What to do.**
  - Nothing for a running deployment.
  - Point a script or an MDM that downloaded the `.app.zip` at the `.pkg` (for silent installs) or the `.dmg`.
  - A brand repository: copy the template's updated **Keep the packages** step (`docs/brand-template/.github/workflows/build.yml`),
    and, if you want your own artwork, add the two background files to your brand folder and run
    `node elitea/scripts/brand-check.mjs brand/<id>`.
