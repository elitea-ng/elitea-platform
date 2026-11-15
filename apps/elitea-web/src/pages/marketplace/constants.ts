/**
 * Marketplace constants.
 */

/** How many listings one page of the grid asks for. */
export const LISTING_PAGE_SIZE = 24;

/** The Featured row's size, and the Popular row's. */
export const FEATURED_COUNT = 4;
export const POPULAR_COUNT = 8;

/**
 * How long the search box waits after the last keystroke before the query
 * reaches the server: a typed word is one request, not one per character.
 */
export const SEARCH_DEBOUNCE_MS = 300;

/** The sort keys the search route accepts (internal/marketplace/search.go). */
export type MarketplaceSort = 'relevance' | 'hires' | 'rating' | 'newest' | 'name';

export interface MarketplaceSortOption {
  readonly key: MarketplaceSort;
  readonly labelKey: string;
  readonly labelFallback: string;
}

export const SORT_OPTIONS: readonly MarketplaceSortOption[] = [
  { key: 'relevance', labelKey: 'marketplace.sort.relevance', labelFallback: 'Relevance' },
  { key: 'hires', labelKey: 'marketplace.sort.hires', labelFallback: 'Most hired' },
  { key: 'rating', labelKey: 'marketplace.sort.rating', labelFallback: 'Top rated' },
  { key: 'newest', labelKey: 'marketplace.sort.newest', labelFallback: 'Newest first' },
  { key: 'name', labelKey: 'marketplace.sort.name', labelFallback: 'Name A-Z' },
];

/** The risk tiers, lowest first. */
export const TIERS: readonly string[] = ['low', 'medium', 'high', 'critical'];
