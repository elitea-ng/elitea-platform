/**
 * Public API — spec §3.3 (named exports only).
 *
 * The toolbar row every page draws under the shell's title row (tabs or a
 * summary on the left; search, Filters, view switch, secondary actions and
 * the primary "+ New X" on the right). The page's title is not here: the
 * shell's breadcrumb owns it (`shared/ui/page-chrome`).
 */
export { PageToolbar, pageBodySx, pageSx } from './ui/PageToolbar';
export type { PageToolbarPrimaryAction, PageToolbarProps, PageToolbarTab, PageToolbarTabsConfig } from './ui/PageToolbar';

/** The toolbar's search box and the `query` search param it owns (`ui/PageSearch.tsx`). */
export { PageSearch, useListSearchQuery } from './ui/PageSearch';
export type { PageSearchProps } from './ui/PageSearch';

/** The toolbar's table/card switch (`ui/ListViewToggle.tsx`). */
export { ListViewToggle } from './ui/ListViewToggle';
export type { ListViewToggleProps } from './ui/ListViewToggle';
export { readPersistedListView, writePersistedListView } from './lib/listViewPersistence';
