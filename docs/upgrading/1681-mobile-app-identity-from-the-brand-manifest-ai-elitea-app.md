---
title: "Mobile app identity from the brand manifest: `ai.elitea.app` (#1681, ADR-0052 W1a)"
issue: 1681
date: 2026-11-08
adr: ADR-0052
order: 103
---

The mobile app's identifiers now come from `brand/elitea/brand.json`, and its
default identity is `ai.elitea.app` (owner decision 9). No mobile build had
been uploaded under the earlier placeholder identity, so no installed app moves.

- **Operators who registered a native client for the mobile app** (admin →
  native clients, or `NATIVE_CLIENTS_PATH`): register `ai.elitea.app` with
  redirect URI `ai.elitea.app:/oauth/callback`. A client registered under the
  earlier placeholder id no longer matches any build; remove it. The Helm
  values comment shows the entry.
- **Building another brand:** set `ELITEA_BRAND` to a brand folder for every
  `expo` and EAS command (docs: Development → Brands). `eas.json` names no
  app identifier; EAS reads them from the brand's Expo config.
- **EAS link and updates:** the EAS project id and owner move into the brand
  manifest's `eas` (environment variables still override). EAS Update is now
  off unless the brand sets `updates.mode: "on"` with its own project and a
  code signing certificate; a project id from the environment alone no longer
  turns it on. brand/elitea ships with updates off. The App Store Connect app id
  moves from `eas.json` to the manifest's `mobile.ascAppId`.
- **Other brands on EAS:** set `ELITEA_BRAND` as an EAS environment variable of
  the brand's project; the build server does not see your shell's.
- **Widget kind** is now `Counts` (was a product-named kind): a widget already on
  a home screen from an earlier test build must be added again.
- **Universal links and App Links:** builds declare `applinks:elitea.ai` and a
  verified App Link for exactly `https://elitea.ai/connect`. Until the domain serves
  matching well-known files (`npm run check:link-files`), connect links on it
  open in the browser; the `elitea://` scheme is unchanged.
