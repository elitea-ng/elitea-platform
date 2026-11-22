---
date: 2026-11-22
---
## Download the desktop app from the release

Every release, and every nightly pre-release, now carries the desktop app. Open **Assets** on the release:

- **macOS, Apple silicon (M1 or later):** `Elitea-<version>-macos-arm64.pkg`, the installer package, or `Elitea-<version>-macos-arm64.app.zip`, the app to unzip into Applications. Both are signed and notarised.
- **Windows 10 2004 or later, x64:** `Elitea-<version>-windows-x64-unsigned.msi` and `.msix`. They are **unsigned** for now, so Windows SmartScreen warns before the MSI runs: choose **More info**, then **Run anyway**. The unsigned MSIX installs only with Developer Mode on. Use the MSI.
- `SHA256SUMS` holds the checksum of each file.

See [Managed devices](/admin/managed-devices#download-from-the-release).
