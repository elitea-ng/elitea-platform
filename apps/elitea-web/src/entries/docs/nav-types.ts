/**
 * Shared types for the docs navigation tree (PREAMBLE decision 2). Split out
 * of `nav.ts` so the per-tab data files under `nav/` (one per tab, kept under
 * the §3.5 file-length budget) and `nav.ts` itself (which assembles them)
 * can both import the shapes without a circular import between them.
 */

/** A single documentation page. `slug` is root-relative, no extension, no
 * leading slash (`''` is the home page) — matches the content rule that
 * internal links are written as root-relative slugs. */
export interface NavPage {
  readonly kind: 'page';
  readonly slug: string;
  readonly title: string;
}

/** A named group of pages and/or nested groups (e.g. Pipelines' "Nodes"
 * subgroup under the Guides tab). */
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
