/**
 * Docs navigation tree: tabs → groups → pages (PREAMBLE decision 2).
 *
 * This is the SINGLE SOURCE of the docs site's structure. `docs-content.test.ts`
 * enforces a bijection between the `slug`s below and the `.mdx` files under
 * `content/`: every slug here must resolve to a file, and every file must
 * appear somewhere in this tree. `App.tsx` renders the left nav straight from
 * this tree, and `router.ts` builds its slug → page lookup from the same
 * flattened list, so the three views (nav, router, tests) can never disagree
 * about what pages exist.
 *
 * Tab/group naming is carried over from the legacy Mintlify `docs.json`
 * (`Home` → `Welcome to Elitea`, `Getting Started` → `Quick Start`, `How-To
 * Guides` → `Chat & Conversations`, `Integrations` → `Toolkits`) but trimmed
 * to only the groups that have a real page under them today — the content
 * rewrite adds groups back as it ports pages, it does not pre-declare empty
 * ones.
 */

/** A single documentation page. `slug` is root-relative, no extension, no
 * leading slash (`''` is the home page) — matches the content rule that
 * internal links are written as root-relative slugs. */
export interface NavPage {
  readonly kind: 'page';
  readonly slug: string;
  readonly title: string;
}

/** A named group of pages and/or nested groups (the legacy site nests groups
 * up to a few levels for long tabs like "How-To Guides"; this tree supports
 * the same shape, we just have not populated deep nesting yet). */
export interface NavGroup {
  readonly kind: 'group';
  readonly title: string;
  readonly pages: readonly NavPage[];
  readonly groups?: readonly NavGroup[];
}

/** A top-level tab, the widest unit of the nav. */
export interface NavTab {
  readonly kind: 'tab';
  readonly title: string;
  readonly groups: readonly NavGroup[];
}

export const nav: readonly NavTab[] = [
  {
    kind: 'tab',
    title: 'Home',
    groups: [
      {
        kind: 'group',
        title: 'Welcome to Elitea',
        pages: [{ kind: 'page', slug: '', title: 'Welcome to Elitea' }],
      },
    ],
  },
  {
    kind: 'tab',
    title: 'Getting Started',
    groups: [
      {
        kind: 'group',
        title: 'Quick Start',
        pages: [{ kind: 'page', slug: 'quick-start', title: 'Quick start' }],
      },
    ],
  },
  {
    kind: 'tab',
    title: 'How-To Guides',
    groups: [
      {
        kind: 'group',
        title: 'Chat & Conversations',
        pages: [{ kind: 'page', slug: 'chat-basics', title: 'Chat basics' }],
      },
    ],
  },
  {
    kind: 'tab',
    title: 'Integrations',
    groups: [
      {
        kind: 'group',
        title: 'Toolkits',
        pages: [{ kind: 'page', slug: 'toolkits-overview', title: 'Toolkits overview' }],
      },
    ],
  },
];

/** Every page in the tree, in nav (document) order — the order `router.ts`
 * uses for prev/next and `docs-content.test.ts` uses for the nav↔files
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
