# Desktop: MDM packages, and a new signing secret for releases (#1697, ADR-0052 W2b)

The desktop app is now built for device management: a universal macOS app,
notarised, inside a signed, notarised `.pkg` that installs silently into
`/Applications` (`.github/workflows/desktop-package.yml`). Nothing is
attached to a release; the brand owner runs the workflow by hand with
`keep_artifacts` to get the package (see Admin › Managed devices in the docs).

- **Repository admins: add `APPLE_INSTALLER_SIGNING_IDENTITY`** — the full
  name of a **Developer ID Installer** identity (`Developer ID Installer:
  <Name> (<TEAMID>)`), exported into the same `.p12` as
  `APPLE_CERTIFICATE`. Until it is set, a release's `desktop` check fails
  ("macOS signing secrets not set"); the images and charts are still
  released, and the job can be re-run alone once the secret is there.
- **Windows (MSI, MSIX):** wired but off until the desktop host builds on
  Windows (#1828). Its secrets (Azure Artifact Signing or a PFX, plus
  `WINDOWS_PUBLISHER`) are listed in `apps/elitea-desktop/README.md`.
