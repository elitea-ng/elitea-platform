# The desktop host and the local tools build and test on Windows (#1828, ADR-0052)

- **What changed.**
  - **`elitea-local-tools`, `elitea-local-index` and the desktop host compile on Windows.** The crates no longer stop
    the build with `compile_error!` on Windows. A new OS layer, `elitea_local_tools::sys`, gives the tools the same
    names on every OS:
    - macOS and Linux: the layer re-exports `rustix` and `std::os::unix`. Their behaviour does not change.
    - Windows: a safe shim over `std`. A command's process tree is a Job Object with kill-on-close: stopping the
      command closes the job, which ends every process in it. Terminal sessions use ConPTY. Private files have no
      owner or mode-bit checks, because the person's profile folder has an owner-only access control list. The file
      tools refuse a symlink or junction where macOS and Linux refuse a symlink. No crate adds `unsafe` code.
  - **Windows has no OS sandbox yet.** A command that a sandbox must confine (`read-only`, `workspace-write`, or
    `full-access` without the network) is refused with `sandbox_unavailable`. A command that needs no confinement
    (`full-access` with the network on) runs. The desktop app never runs a confined command unconfined.
  - **Windows notifications use the app's AUMID.** The MSI's Start Menu shortcut sets `System.AppUserModel.ID` to the
    brand's desktop identifier, and the notification plugin uses the same identifier. When the app runs from the
    MSIX, the host reads its package identity and shows notifications under `<PackageFamilyName>!App`.
  - **CI:** a new `windows-host` job in `.github/workflows/ci-desktop.yml` runs clippy (`-D warnings`) and the tests
    of the host, `elitea-local-tools`, `elitea-local-index` and `elitea-workspace-store` on `windows-latest`. Tests
    that need Unix files (mode bits, symlinks, `/bin/sh`) run on macOS and Linux only.
  - The Windows packaging job in `.github/workflows/desktop-package.yml` stays off. #1699 (W2b.2) turns it on.
- **Who is affected.**
  - Developers who build the desktop app or the local tools on Windows.
  - Administrators who plan to deploy the desktop app to Windows PCs. No Windows package is published yet.
  - Nobody on macOS or Linux: nothing changes there.
- **What to do.**
  - Nothing for a running deployment.
  - On Windows, to run local commands before a Windows sandbox exists, set `local_work_max_sandbox_mode` to
    `full-access` and turn on `local_work_network` (Admin › Configuration › Native client policy). Then choose
    **Full access** for the workspace in the desktop app. The commands then run unconfined, with the person's own
    rights.
