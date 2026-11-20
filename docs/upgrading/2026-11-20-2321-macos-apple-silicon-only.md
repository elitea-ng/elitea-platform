# Desktop: the macOS app is Apple silicon only; Intel Macs are no longer supported (#2321)

The macOS desktop app is now built for Apple silicon (arm64) only. The universal
(Apple silicon + Intel) build is gone, by owner decision on 2026-11-20.

- **The package name changed** from `<Name>-<version>-universal.pkg` to
  `<Name>-<version>-arm64.pkg`. Update any MDM policy, script or upload step
  that names the file.
- **Intel Macs are no longer supported.** The package's distribution names
  `arm64` as its only host architecture, so `installer` and MDM installs refuse
  it on an Intel Mac. An Intel Mac that already has an older universal build
  installed keeps that build, but it gets no newer versions. Scope the MDM
  assignment to Apple silicon computers: in Jamf, a smart group on
  Architecture Type `arm64`; in Intune, a device filter on the processor
  architecture.
- **Brand repositories** (`docs/brand-template/`) need no change. The packaging
  action (`.github/actions/desktop-macos-package`) builds for the runner's own
  architecture and fails unless that is Apple silicon, so keep the job on
  `macos-latest`.
