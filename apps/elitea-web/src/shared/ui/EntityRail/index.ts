/**
 * A list page's Filters — tags, then the author card or the trending authors —
 * shown in the shell's right panel (`EntityListFilters.tsx`'s module doc). It
 * lives in `shared/ui` because `features/toolkits` and `pages/**` both compose
 * its parts, and `shared/` is the only layer both may import under
 * `.dependency-cruiser.cjs`.
 */
export { EntityListFilters, filtersPanelSx } from './EntityListFilters';
/** @public */
export type { EntityListFiltersProps } from './EntityListFilters';

export { RailTagsPanel, RailTagsPanelView } from './RailTagsPanel';
/** @public */
export type { RailTagsPanelProps, RailTagsPanelViewProps } from './RailTagsPanel';

export { RailAuthorCard, RailAuthorCardView, shouldShowAuthorCard } from './RailAuthorCard';
/** @public */
export type { RailAuthorCardProps, RailAuthorCardViewProps } from './RailAuthorCard';

export { RailTrendingAuthors, RailTrendingAuthorsView } from './RailTrendingAuthors';
/** @public */
export type { RailTrendingAuthor, RailTrendingAuthorsProps, RailTrendingAuthorsViewProps } from './RailTrendingAuthors';

export { sortTagsSelectedFirst, toggleTagName } from './lib/railTags';
/** @public */
export type { RailTag } from './lib/railTags';

export { railStatForKind, railStatForPath, railStatRoutePrefixes, resolveRailStat } from './lib/railStatistics';
/** @public */
export type { RailAuthorCounts, RailStatDescriptor, RailStatKind, RailStatValues } from './lib/railStatistics';

export { selectRailViewer, useRailFolderSelection, useRailTagSelection, useRailViewer } from './useRailContext';
/** @public */
export type { RailFolderSelection, RailTagSelection, RailViewer } from './useRailContext';
