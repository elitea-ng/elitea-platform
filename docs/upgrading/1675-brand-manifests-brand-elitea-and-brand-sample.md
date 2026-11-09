---
title: "Brand manifests: brand/elitea and brand/sample (#1675, ADR-0052 W0)"
issue: 1675
date: 2026-11-08
adr: ADR-0052
order: 102
---

No server, migration, configuration or client change. The repository gains
the brand manifest format of ADR-0052: `brand/elitea/` (EPAM's ELITEA, the
default build) and `brand/sample/` (a neutral test brand, never published),
each a `brand.json` and an `assets/` folder, checked by
`node scripts/brand-check.mjs`. Builds select a brand with `ELITEA_BRAND`
(unset means `brand/elitea`).

- Nothing reads the manifests yet except `brand-check`; the mobile, desktop
  and web builds move to them in the next ADR-0052 steps, and their output
  for the ELITEA brand stays the same.
- Contributors changing ELITEA's icons or names: change `brand/elitea/` too;
  `scripts/brand/elitea-sync.test.mjs` fails until both agree.
- Docs: Development → Brands.
