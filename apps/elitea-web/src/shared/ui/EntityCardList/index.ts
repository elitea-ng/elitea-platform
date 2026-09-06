/**
 * The shared list surface for every entity page (agents, pipelines, skills,
 * toolkits, MCPs, credentials, applications): the card grid, the
 * `?view=table` table, the empty state and the pagination footer.
 *
 * Ported from `apps/elitea-ui/src/components/{CardList,DataCards,Card}.jsx`,
 * `[fsd]/widgets/data-table` and `[fsd]/entities/empty-state-page`. See each
 * module's own doc comment for the geometry it reproduces and the disclosed
 * narrowings.
 */
export { EntityCard } from './EntityCard';
export type { EntityCardProps } from './EntityCard';

export { EntityCardList } from './EntityCardList';
export type { EntityCardListProps } from './EntityCardList';

export { EntityEmptyState } from './EntityEmptyState';
export type { EntityEmptyStateProps, EmptyStateArt } from './EntityEmptyState';

export { EntityListPagination, PAGE_SIZE_OPTIONS } from './EntityListPagination';
export type { EntityListPaginationProps } from './EntityListPagination';

export { EntityListTable } from './EntityListTable';
export type { EntityListTableProps } from './EntityListTable';

export { entityTypeIcon } from './EntityTypeIcon';
export type { EntityKind } from './EntityTypeIcon';

export { ENTITY_LIST_VIEW } from './model';
export type { EntityListAuthor, EntityListItem, EntityListTag, EntityListView } from './model';
