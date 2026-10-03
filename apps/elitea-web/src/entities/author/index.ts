/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 */
export type { Author, TrendingAuthor } from './model/types';
export type { AuthorLike } from './model/selectors';
export { authorDisplayName, isCurrentUserAuthor, isDeletedAuthor, isSameAuthor } from './model/selectors';
