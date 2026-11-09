---
title: "Desktop app no longer attached to GitHub releases (ADR-0052 decision 8)"
issue: 1667
date: 2026-11-07
adr: ADR-0052
order: 92
---

Affects anyone who downloaded the macOS desktop app from a GitHub release.

- Releases no longer carry `Elitea_<version>_aarch64.dmg`,
  `Elitea_<version>_aarch64.app.zip` or `sbom-desktop.cdx.json`. The
  release job still builds, signs and notarises the app as a check, and
  publishes nothing for it.
- The desktop files were also removed from the existing releases
  (v7.1.0 to v7.5.0 and the nightly pre-releases).
- ELITEA's desktop and mobile apps reach devices through the
  organization's device management, signed by the brand owner. Ask your
  ELITEA contact for the managed installer. An app already installed keeps
  working.
