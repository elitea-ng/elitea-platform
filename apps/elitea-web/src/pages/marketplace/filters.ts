/**
 * The Marketplace's filters: what the URL carries, what the search route is
 * asked, and which of them count as "active" for the Featured shelves and the
 * summary bar. The Browse tab and the summary bar read and write this one
 * shape, so a filtered view is a link.
 */
import { SORT_OPTIONS, type MarketplaceSort } from './constants';
import { apiKind, kindFromSearch } from './listing';
import type { MarketplaceKind } from './types';

/** The URL's side of the filters: every value a string, as the router hands it. */
export interface MarketplaceSearch {
  /** An old `?agentId=` link the redirect could not resolve yet (`useLegacyAgentLink.ts`). */
  agentId?: string;
  kind?: string;
  query?: string;
  category?: string;
  tag?: string;
  publisher_id?: string;
  max_tier?: string;
  requires?: string;
  min_rating?: string;
  hired_here?: string;
  sort_by?: string;
}

export interface MarketplaceFilters {
  readonly kind: MarketplaceKind;
  readonly query: string;
  readonly categories: readonly string[];
  readonly tags: readonly string[];
  readonly publisherId: string;
  readonly maxTier: string;
  readonly requires: readonly string[];
  readonly minRating: number;
  readonly hiredHere: boolean;
  readonly sort: MarketplaceSort;
}

const split = (value: string | undefined): string[] => (value ?? '').split(',').filter((part) => part !== '');

export function sortFromSearch(value: string | undefined): MarketplaceSort {
  return SORT_OPTIONS.find((option) => option.key === value)?.key ?? 'relevance';
}

export function filtersFromSearch(search: MarketplaceSearch): MarketplaceFilters {
  const rating = Number(search.min_rating);
  return {
    kind: kindFromSearch(search.kind),
    query: search.query ?? '',
    categories: split(search.category),
    tags: split(search.tag),
    publisherId: search.publisher_id ?? '',
    maxTier: search.max_tier ?? '',
    requires: split(search.requires),
    minRating: Number.isFinite(rating) && rating > 0 ? rating : 0,
    hiredHere: search.hired_here === '1',
    sort: sortFromSearch(search.sort_by),
  };
}

/** The URL for a set of filters; a default value is left out so the address stays short. */
export function searchFromFilters(filters: MarketplaceFilters): MarketplaceSearch {
  const out: MarketplaceSearch = {};
  if (filters.kind !== 'agents') out.kind = filters.kind;
  if (filters.query.trim() !== '') out.query = filters.query;
  if (filters.categories.length > 0) out.category = filters.categories.join(',');
  if (filters.tags.length > 0) out.tag = filters.tags.join(',');
  if (filters.publisherId !== '') out.publisher_id = filters.publisherId;
  if (filters.maxTier !== '') out.max_tier = filters.maxTier;
  if (filters.requires.length > 0) out.requires = filters.requires.join(',');
  if (filters.minRating > 0) out.min_rating = String(filters.minRating);
  if (filters.hiredHere) out.hired_here = '1';
  if (filters.sort !== 'relevance') out.sort_by = filters.sort;
  return out;
}

export type ApiParams = Record<string, string | number | boolean>;

/** The search route's parameters for a filter set (limit and offset are the pager's). */
export function apiParamsFromFilters(filters: MarketplaceFilters): ApiParams {
  const params: ApiParams = { kind: apiKind(filters.kind), sort: filters.sort };
  if (filters.query.trim() !== '') params['query'] = filters.query.trim();
  if (filters.categories.length > 0) params['category'] = filters.categories.join(',');
  if (filters.tags.length > 0) params['tag'] = filters.tags.join(',');
  if (filters.publisherId !== '') params['publisher_id'] = filters.publisherId;
  if (filters.maxTier !== '') params['max_tier'] = filters.maxTier;
  if (filters.requires.length > 0) params['requires'] = filters.requires.join(',');
  if (filters.minRating > 0) params['min_rating'] = filters.minRating;
  if (filters.hiredHere) params['hired_here'] = true;
  return params;
}

/** A search or a facet is on: the shelves give way to the answer. Kind and sort are views, not filters. */
export function hasActiveFilter(filters: MarketplaceFilters): boolean {
  return (
    filters.query.trim() !== '' ||
    filters.categories.length > 0 ||
    filters.tags.length > 0 ||
    filters.publisherId !== '' ||
    filters.maxTier !== '' ||
    filters.requires.length > 0 ||
    filters.minRating > 0 ||
    filters.hiredHere
  );
}
