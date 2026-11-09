---
title: "Upgrade notes are one file per change (#1833)"
issue: 1833
date: 2026-11-09
---

Nothing to do for a deployment. The upgrade notes moved from one file to one
file per change, and the docs site shows them on
[Upgrading](../../apps/elitea-docs/src/content/docs/deployment/upgrading.mdx),
the newest first.

- **Readers:** read the entries on the docs page, not in `docs/UPGRADING.md`.
  That file is now a pointer to the page. Each entry has an anchor, its file
  name without `.md`.
- **Contributors:** add `docs/upgrading/<issue>-<slug>.md` with front matter
  (`title`, `issue`, `date`, optional `adr`). Do not edit `docs/UPGRADING.md`
  or an entry that another pull request added.
  `docs/upgrading/README.md` has the template.
  `scripts/ci/check-upgrading-fragments.mjs` enforces the rules.
- The 110 entries that were in `docs/UPGRADING.md` are now files in
  `docs/upgrading/`, with the same text and in the same order.
