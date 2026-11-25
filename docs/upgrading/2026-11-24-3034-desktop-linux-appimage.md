# Desktop: a Linux release, an x86_64 AppImage and a .deb (#3034, ADR-0052)

**What changed.** `desktop-package.yml` has a third job, `linux`, which runs on
the self-hosted pool. It builds the desktop app for Linux on x86_64 with
`tauri build --bundles appimage,deb` (the new composite action
`.github/actions/desktop-linux-package`). It then smoke-tests the AppImage
under `xvfb-run`: `--version` must print the stamped version, and the app must
still be running after 30 s. From the next nightly pre-release and the next
release on, `publish.yml` attaches two more files, listed in `SHA256SUMS` and
in the notes' **Desktop downloads** section:

- `<Brand>-<version>-linux-x86_64.AppImage`
- `<Brand>-<version>-linux-x86_64.deb`

The files carry no code signature, and their names carry no `-unsigned`,
because no Linux signer exists or is planned. The app gains a `--version` flag
on every system: it prints `elitea-desktop <version>` and exits.
`desktop-package.mjs release-assets --complete` now also requires the two
Linux files when the `desktop` job passed. The `.deb` recommends `bubblewrap`.

**Who is affected.** Release managers and anyone who downloads the desktop
app. Nothing changes for the server, its charts or its configuration. No
secret is added. A brand repository that builds from a release tag can call
the new action, as it calls the macOS and Windows ones.

**What to do.** Nothing for an upgrade. For Linux users, the install steps are
in the docs under Admin › Managed devices › Install on Linux: `chmod +x`, FUSE
or `--appimage-extract-and-run`, desktop integration, and the `.deb`. That page
also lists what the Linux app needs and what it does not do yet: bubblewrap
for the sandbox, no GitHub CLI login in approval rules (#3012), no managed
configuration, and x86_64 only.
