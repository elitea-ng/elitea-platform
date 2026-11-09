---
title: "Desktop app brand from the brand manifest (#1692, ADR-0052 W2a)"
issue: 1692
date: 2026-11-08
adr: ADR-0052
order: 106
---

The desktop app's product name, identifier, icons, menu-bar image, publisher and
copyright now come from the brand manifest. ELITEA's build is unchanged:
`Elitea.app`, identifier `ai.elitea.desktop`, so every user's config, data,
logs and credentials stay where they are.

- **Building the app:** use the npm scripts (`npm run build`, `npm run dev`,
  `npm run build:debug`): they write `src-tauri/tauri.brand.json` and pass it
  with `--config`. A bare `tauri build` builds the neutral sample brand
  (`com.example.sample.desktop`). For `cargo` alone, export the overlay as
  `TAURI_CONFIG`.
- **Another brand:** set `ELITEA_BRAND` to its folder before the npm script.
  `desktop.identifier` must not change once a brand has shipped.
- **Icons** moved from `src-tauri/icons/` to `brand/<id>/assets/`
  (`desktop/`, `macos/`, `windows/`); the old folder is gone.
- **Translators:** the desktop menu labels name the product as `{product}`;
  the host puts in the build's product name. Keep the placeholder.
