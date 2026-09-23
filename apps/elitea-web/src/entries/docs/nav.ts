/**
 * Docs navigation tree: tabs -> groups -> pages (PREAMBLE decision 2).
 *
 * This is the SINGLE SOURCE of the docs site's structure. `docs-content.test.ts`
 * enforces a bijection between the `slug`s below and the `.mdx` files under
 * `content/`: every slug here must resolve to a file, and every file must
 * appear somewhere in this tree. `App.tsx` renders the left nav straight from
 * this tree, and `router.ts` builds its slug -> page lookup from the same
 * flattened list, so the three views (nav, router, tests) can never disagree
 * about what pages exist.
 *
 * Generated from the writer batches' `nav.<batch>.json` files per product-map.md
 * section 6 (proposed navigation) — tab order Home, Getting started,
 * Development, Guides, Integrations, Deployment, Admin; group/subgroup order
 * as listed there. Four legacy fragment pages (buckets-and-files, evaluation,
 * share-a-conversation, voice) were folded into their menu pages during
 * review and do not get their own nav entries or files — see the
 * embedded-docs W5 merge report. Development and Deployment were added in a
 * later docs wave (local development environment, production Kubernetes
 * deployment) — see nav.L.json / nav.P.json in that wave's staging batch.
 *
 * The tree itself lives one file per tab under `nav/` — each tab's own data
 * kept growing (most recently Development and Deployment) until the whole
 * thing crossed the §3.5 400-line file-length budget (`check-budgets.mjs`).
 * Splitting it here, rather than waiving the budget, keeps every future
 * wave's addition scoped to one small file instead of growing this one
 * without bound — see `nav/*.ts` (data) and `nav-types.ts` (the shared
 * `NavPage`/`NavGroup`/`NavTab` shapes both this file and `nav/*.ts` import).
 */
import { adminTab } from './nav/admin';
import { deploymentTab } from './nav/deployment';
import { developmentTab } from './nav/development';
import { gettingStartedTab } from './nav/getting-started';
import { guidesTab } from './nav/guides';
import { homeTab } from './nav/home';
import { integrationsTab } from './nav/integrations';
import type { NavGroup, NavPage, NavTab } from './nav-types';

export type { NavGroup, NavPage, NavTab } from './nav-types';

export const nav: readonly NavTab[] = [
  homeTab,
  gettingStartedTab,
  developmentTab,
  guidesTab,
  integrationsTab,
  deploymentTab,
  adminTab,
];

/** Every page in the tree, in nav (document) order — the order `router.ts`
 * uses for prev/next and `docs-content.test.ts` uses for the nav<->files
 * bijection. Depth-first: a group's own pages before its nested groups'. */
export function flattenNav(tabs: readonly NavTab[] = nav): NavPage[] {
  const pages: NavPage[] = [];
  const walkGroup = (group: NavGroup): void => {
    pages.push(...group.pages);
    for (const child of group.groups ?? []) walkGroup(child);
  };
  for (const tab of tabs) {
    for (const group of tab.groups) walkGroup(group);
  }
  return pages;
}

/** `{ prev, next }` neighbours of `slug` in nav order, either possibly absent
 * at the first/last page. */
export function neighbours(
  slug: string,
  tabs: readonly NavTab[] = nav,
): { prev: NavPage | undefined; next: NavPage | undefined } {
  const pages = flattenNav(tabs);
  const index = pages.findIndex((page) => page.slug === slug);
  if (index === -1) return { prev: undefined, next: undefined };
  return { prev: pages[index - 1], next: pages[index + 1] };
}
