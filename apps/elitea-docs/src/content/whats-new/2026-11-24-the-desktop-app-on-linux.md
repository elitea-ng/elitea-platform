---
date: 2026-11-24
---
## The desktop app on Linux

Each release now carries the desktop app for Linux on x86_64 in two forms. The AppImage is one file that runs on most distributions with glibc 2.39 or later, such as Ubuntu 24.04, Debian 13 and Fedora 40. Make it executable with `chmod +x` and run it. Without FUSE, run it with `--appimage-extract-and-run`. The `.deb` installs the app with a menu entry on Debian and Ubuntu. Check either file against the release's `SHA256SUMS`, because Linux packages carry no code signature. For local work, install bubblewrap (`bwrap`), the sandbox the app confines commands with. Approval rules cannot yet give the GitHub CLI its login on Linux. See [Install on Linux](/admin/managed-devices/#install-on-linux).
