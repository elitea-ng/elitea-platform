/**
 * The Feedback section's reads and writes (ADR-0051 decision 5): a page of
 * items with the rating distribution and the signals, a verified hire's review,
 * and the publisher's and administrator's steps on one item.
 *
 * The items page by offset ("Show more"); the summary, the signals and what the
 * caller may do come with the first page.
 */
import { keepPreviousData, useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';

import {
  actOnMarketplaceFeedback,
  getMarketplaceFeedback,
  saveMarketplaceReview,
} from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceFeedback, MarketplaceFeedbackItem } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

const FEEDBACK_PAGE_SIZE = 20;

export type FeedbackKind = 'all' | 'review' | 'workforce_report';

export interface FeedbackFilters {
  /** A version id, or '' for every version. */
  readonly versionId: string;
  readonly kind: FeedbackKind;
}

async function fetchPage(
  projectId: string,
  listingId: string,
  filters: FeedbackFilters,
  offset: number,
): Promise<MarketplaceFeedback> {
  const response = await getMarketplaceFeedback(projectId, listingId, {
    limit: FEEDBACK_PAGE_SIZE,
    offset,
    ...(filters.versionId === '' ? {} : { version_id: filters.versionId }),
    ...(filters.kind === 'all' ? {} : { kind: filters.kind }),
  });
  if (response.status !== 200) throw new Error(`The feedback answered ${response.status}`);
  return unwrapBody(response) as MarketplaceFeedback;
}

interface ListingFeedbackData {
  /** The first page: the summary, the signals and the caller's rights. */
  readonly first: MarketplaceFeedback | undefined;
  readonly items: readonly MarketplaceFeedbackItem[];
  readonly isPending: boolean;
  readonly isError: boolean;
  readonly hasNextPage: boolean;
  readonly isFetchingNextPage: boolean;
  readonly fetchNextPage: () => void;
  readonly refetch: () => void;
}

export function useListingFeedback(
  projectId: string | undefined,
  listingId: string,
  filters: FeedbackFilters,
): ListingFeedbackData {
  const query = useInfiniteQuery({
    queryKey: ['marketplace', 'feedback', projectId, listingId, filters],
    enabled: projectId !== undefined && listingId !== '',
    initialPageParam: 0,
    queryFn: ({ pageParam }) => fetchPage(projectId ?? '', listingId, filters, pageParam),
    // A filter change keeps the page on screen until the next one arrives.
    placeholderData: keepPreviousData,
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((sum, page) => sum + page.items.length, 0);
      return last.items.length > 0 && loaded < last.total ? loaded : undefined;
    },
  });
  return {
    first: query.data?.pages[0],
    items: (query.data?.pages ?? []).flatMap((page) => page.items),
    isPending: projectId === undefined || query.isPending,
    isError: query.isError,
    hasNextPage: query.hasNextPage,
    isFetchingNextPage: query.isFetchingNextPage,
    fetchNextPage: () => void query.fetchNextPage(),
    refetch: () => void query.refetch(),
  };
}

export interface ReviewDraft {
  readonly versionId: string;
  readonly rating: number;
  readonly body: string;
}

/** Saves the caller's review: the first call creates it and a later one edits it. */
export function useSaveReview(projectId: string | undefined, listingId: string, onSaved: () => void) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: ReviewDraft) => {
      const response = await saveMarketplaceReview(projectId ?? '', listingId, {
        rating: draft.rating,
        body: draft.body.trim(),
        ...(draft.versionId === '' ? {} : { version_id: draft.versionId }),
      });
      if (response.status !== 200 && response.status !== 201) {
        throw new Error(`The review answered ${response.status}`);
      }
    },
    onSuccess: () => {
      // The listing card carries the average, the page the distribution.
      void queryClient.invalidateQueries({ queryKey: ['marketplace'] });
      onSaved();
    },
  });
}

type FeedbackAction = 'reply' | 'flag' | 'unflag' | 'hide' | 'unhide';

export interface ActionRequest {
  readonly feedbackId: string;
  readonly action: FeedbackAction;
  readonly text?: string;
  readonly reason?: string;
}

/** A reply, a flag or a hide on one item. A 409 on a reply means the item already has one. */
export function useFeedbackAction(projectId: string | undefined, listingId: string, onDone: () => void) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (request: ActionRequest) => {
      const response = await actOnMarketplaceFeedback(projectId ?? '', listingId, request.feedbackId, {
        action: request.action,
        ...(request.text === undefined ? {} : { text: request.text }),
        ...(request.reason === undefined ? {} : { reason: request.reason }),
      });
      if (response.status !== 200) throw new Error(`The step answered ${response.status}`);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['marketplace'] });
      onDone();
    },
  });
}
