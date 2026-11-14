/**
 * Marketplace constants.
 */
import type { MarketplaceSortBy, MarketplaceSortOrder } from './types';

export const OTHER_CATEGORY = 'Other';

/** How many agent rows one request asks for; the handler's own cap (`publicApplicationsMaxLimit`). */
export const AGENT_LISTING_LIMIT = 1000;
/** The public skills route caps a page at 100 (`skillpublish/public.go`). */
export const SKILL_LISTING_LIMIT = 100;

/** The Featured row's size, and the Popular row's. */
export const FEATURED_COUNT = 4;
export const POPULAR_COUNT = 8;

/**
 * How long the search box waits after the last keystroke before the query
 * reaches the server: a typed word is one request, not one per character.
 */
export const SEARCH_DEBOUNCE_MS = 300;

export interface MarketplaceSortOption {
  readonly key: 'newest' | 'name' | 'likes';
  readonly labelKey: string;
  readonly labelFallback: string;
  readonly sortBy: MarketplaceSortBy;
  readonly sortOrder: MarketplaceSortOrder;
  /** Skills carry no likes, so "Most liked" is an agents-only choice. */
  readonly agentsOnly: boolean;
}

/**
 * The sort choices. `sortBy`/`sortOrder` are the values the agent catalogue
 * handler accepts; the skills route is sorted by the client over the page in
 * hand.
 */
export const SORT_OPTIONS: readonly MarketplaceSortOption[] = [
  { key: 'newest', labelKey: 'marketplace.sort.newest', labelFallback: 'Newest first', sortBy: 'created_at', sortOrder: 'desc', agentsOnly: false },
  { key: 'name', labelKey: 'marketplace.sort.name', labelFallback: 'Name A-Z', sortBy: 'name', sortOrder: 'asc', agentsOnly: false },
  { key: 'likes', labelKey: 'marketplace.sort.likes', labelFallback: 'Most liked', sortBy: 'likes', sortOrder: 'desc', agentsOnly: true },
];
