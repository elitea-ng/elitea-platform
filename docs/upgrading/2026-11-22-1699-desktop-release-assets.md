# The desktop packages are attached to every release; Windows ships unsigned (#1699, ADR-0052 decision 10 amended)

- **What changed.**
  - **The GitHub release carries the desktop app** (owner decision 2026-11-22, amending ADR-0052 decision 10, which
    kept the packages off the release). Each stable release and nightly pre-release gets, named
    `<Brand>-<version>-<os>-<arch>[-unsigned].<ext>`:
    - `Elitea-<version>-macos-arm64.pkg` and `Elitea-<version>-macos-arm64.app.zip`: Apple silicon, signed with the
      Developer ID, notarised and stapled;
    - `Elitea-<version>-windows-x64-unsigned.msi` and `Elitea-<version>-windows-x64-unsigned.msix`: x64, unsigned;
    - `SHA256SUMS`: the SHA-256 of each file.
    The release notes get a **Desktop downloads** section.
  - **The Windows packages are built in every release.** The repository variable `DESKTOP_WINDOWS_PACKAGE` no longer
    gates the Windows job (it was never set, so the job never ran). The job builds unsigned while no Windows signer's
    secrets exist, and signs as soon as one signer's full set is set. A partial set still fails the job.
  - **`desktop-package.yml` inputs.** `require_signed` is replaced by `require_signed_macos` (default `true`) and
    `require_signed_windows` (default `false`), and `upload_release_assets` (default `false`) is new. `publish.yml`
    passes `true`, `false` and `true`.
  - **An unsigned MSIX's Publisher carries the unsigned-publisher OID**
    (`OID.2.25.311729368913984317654407730594956997722=1`), so Windows can install it with Developer Mode on through
    `Add-AppxPackage -AllowUnsigned`. A signed MSIX's Publisher is the certificate subject (`WINDOWS_PUBLISHER`), as
    before.
  - A failed desktop build never deletes a release: rollback does not wait on it. The new `desktop-assets` job attaches
    whatever was built, so a broken Windows build still leaves the signed macOS packages on the release.
- **Who is affected.**
  - People who install the desktop app: they can download it from the release.
  - Administrators who deploy it to Windows PCs: the MSI is unsigned. Windows SmartScreen warns before it runs
    (**More info** › **Run anyway**). Intune and SCCM install it silently. A policy that refuses unsigned installers
    (AppLocker, WDAC, Smart App Control) refuses it.
  - Anyone calling `desktop-package.yml` with `require_signed`: the input is gone.
- **What to do.**
  - Nothing for a running deployment.
  - To deploy on Windows today, use the MSI and check it against `SHA256SUMS`. Do not use the MSIX outside testing.
  - To sign the Windows packages, set one signer's full set of repository secrets (Azure Artifact Signing or a PFX; see
    `apps/elitea-desktop/README.md`). Then set `require_signed_windows: true` in `publish.yml` so an unsigned build can
    no longer reach a release.
  - A workflow that calls `desktop-package.yml` with `require_signed` must pass `require_signed_macos` (and, if it
    wants Windows signed, `require_signed_windows`) instead.
