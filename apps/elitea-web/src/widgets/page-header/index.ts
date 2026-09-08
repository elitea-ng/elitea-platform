/**
 * Public API — spec §3.3 (named exports only).
 *
 * Owns the list pages' header row (issue 841). Every list page built its
 * own: `pages/agents/Applications.tsx`, `pages/pipelines/Pipelines.tsx`,
 * `pages/toolkits/Toolkits.tsx` and `pages/apps/Apps.tsx` each carried a
 * byte-identical `pageSx`/`tabBarSx`/`tabPanelSx` trio, and
 * `pages/skills/Skills.tsx` and `pages/credentials/Credentials.tsx` each
 * carried their own title row.
 *
 * See `ui/PageHeader.tsx` for what was measured in the reference.
 */
export { PageHeader } from './ui/PageHeader';
export type { PageHeaderProps, PageHeaderSlots, PageHeaderTab, PageHeaderTabsConfig } from './ui/PageHeader';

/** The header's search box and the `query` search param it owns (`ui/ListSearchField.tsx`). */
export { ListSearchField, useListSearchQuery } from './ui/ListSearchField';
export type { ListSearchFieldProps } from './ui/ListSearchField';

/** The header's table/card switch (`ui/ListViewToggle.tsx`). */
export { ListViewToggle } from './ui/ListViewToggle';
export type { ListViewToggleProps } from './ui/ListViewToggle';
export { readPersistedListView, writePersistedListView } from './lib/listViewPersistence';
