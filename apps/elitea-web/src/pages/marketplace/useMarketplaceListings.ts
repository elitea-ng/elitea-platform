/**
 * The Marketplace's reads: the facet search API (ADR-0051 decision 2), one
 * route for both kinds.
 *
 * Search, every facet and the sort go to the SERVER; the page keeps no
 * client-side copy of the filter. The grid pages with `limit`/`offset` ("Show
 * more"), so a catalogue of any size costs one page of rows, not the whole
 * list. The Featured and Popular shelves are two small reads of their own.
 */
import { useMemo } from 'react';

import { useInfiniteQuery, useQuery } from '@tanstack/react-query';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { searchMarketplaceListings, useGetMarketplaceListing } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceFacets, MarketplaceListingDetail, MarketplaceListingPage } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

import { FEATURED_COUNT, LISTING_PAGE_SIZE, POPULAR_COUNT } from './constants';
import { apiParamsFromFilters, type ApiParams, type MarketplaceFilters } from './filters';
import { apiKind, listingCard } from './listing';
import type { ListingCardData, MarketplaceKind } from './types';

async function fetchPage(projectId: string, params: ApiParams): Promise<MarketplaceListingPage> {
  const response = await searchMarketplaceListings(projectId, params);
  if (response.status !== 200) throw new Error(`The Marketplace search answered ${response.status}`);
  return unwrapBody(response) as MarketplaceListingPage;
}

function isNotFoundError(error: unknown): boolean {
  return error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 404;
}

export interface MarketplaceListings {
  readonly cards: readonly ListingCardData[];
  readonly total: number;
  readonly facets: MarketplaceFacets | undefined;
  readonly isPending: boolean;
  readonly isError: boolean;
  readonly hasNextPage: boolean;
  readonly isFetchingNextPage: boolean;
  readonly fetchNextPage: () => void;
}

/** The grid: one page per request, and the facet counts of the first. */
export function useMarketplaceSearch(projectId: string | undefined, filters: MarketplaceFilters): MarketplaceListings {
  const params = apiParamsFromFilters(filters);
  const query = useInfiniteQuery({
    queryKey: ['marketplace', 'search', projectId, params],
    enabled: projectId !== undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => fetchPage(projectId ?? '', { ...params, limit: LISTING_PAGE_SIZE, offset: pageParam }),
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((sum, page) => sum + page.rows.length, 0);
      return last.rows.length > 0 && loaded < last.total ? loaded : undefined;
    },
  });
  const first = query.data?.pages[0];
  return {
    cards: (query.data?.pages ?? []).flatMap((page) => page.rows.map(listingCard)),
    total: first?.total ?? 0,
    facets: first?.facets,
    isPending: projectId === undefined || query.isPending,
    isError: query.isError,
    hasNextPage: query.hasNextPage,
    isFetchingNextPage: query.isFetchingNextPage,
    fetchNextPage: () => void query.fetchNextPage(),
  };
}

type Shelf = 'featured' | 'popular';

/** A shelf: the editorial Featured row (`featured=true`) or the most hired listings. Read only while no search or facet is on. */
export function useMarketplaceShelf(
  projectId: string | undefined,
  kind: MarketplaceKind,
  shelf: Shelf,
  enabled: boolean,
): readonly ListingCardData[] {
  const params: ApiParams =
    shelf === 'featured'
      ? { kind: apiKind(kind), featured: true, sort: 'newest', limit: FEATURED_COUNT }
      : { kind: apiKind(kind), sort: 'hires', limit: POPULAR_COUNT };
  const query = useQuery({
    queryKey: ['marketplace', 'shelf', projectId, shelf, kind],
    enabled: enabled && projectId !== undefined,
    queryFn: () => fetchPage(projectId ?? '', params),
  });
  return useMemo(() => (query.data?.rows ?? []).map(listingCard), [query.data]);
}

/** One listing by id: the card, its versions with release notes and requirements, and the hires of this project. */
export function useMarketplaceListing(
  projectId: string | undefined,
  listingId: string,
): {
  readonly detail: MarketplaceListingDetail | undefined;
  readonly isPending: boolean;
  /** The server answered 404: the listing is gone. */
  readonly isNotFound: boolean;
  /** Any other failure: a network error or a 5xx. */
  readonly isError: boolean;
  readonly refetch: () => void;
} {
  const query = useGetMarketplaceListing(projectId ?? '', listingId, {
    query: { enabled: projectId !== undefined && listingId !== '' },
  });
  const detail = query.data?.status === 200 ? (unwrapBody(query.data) as MarketplaceListingDetail) : undefined;
  return {
    detail,
    isPending: query.isPending && query.fetchStatus !== 'idle',
    isNotFound: isNotFoundError(query.error),
    isError: query.isError && !isNotFoundError(query.error),
    refetch: () => void query.refetch(),
  };
}
