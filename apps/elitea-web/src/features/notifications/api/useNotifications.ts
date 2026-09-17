/**
 * features/notifications/api/useNotifications.ts — TanStack Query hooks
 * wrapping `./notifications.ts` (unit A11). Substitutes for the baseline's
 * RTK Query cache (`TAG_NOTIFICATIONS`): query-key based caching replaces
 * `providesTags`, explicit `invalidateQueries` replaces `invalidatesTags`
 * (spec §2.3 — no Redux/RTK Query anywhere in the new app). Every query
 * result is normalized (`./normalize.ts`) before it reaches a caller, so
 * `entities/notification`'s selectors work directly against hook output.
 *
 * Query-key convention: `['notifications', 'list', projectId, ...]` so
 * `invalidateQueries({ queryKey: ['notifications'] })` (no further keys)
 * invalidates every notifications query for every project at once — the
 * same "wipe the whole notifications surface" scope the baseline's single
 * shared `TAG_NOTIFICATIONS` tag had (`notifications.js:9,25,32,41,49,57`:
 * every mutation invalidates the one tag every list `providesTags`).
 */
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { InfiniteData, UseInfiniteQueryResult, UseMutationResult, UseQueryResult } from '@tanstack/react-query';

import type { NormalizedNotification } from './normalize';
import { normalizeNotificationList } from './normalize';
import {
  bulkDeleteNotifications,
  bulkMarkSeenNotifications,
  deleteNotification,
  listNotifications,
  readNotification,
} from './notifications';
import type { ListNotificationsParams } from './notifications';

/** Root key every notifications query/mutation shares — the invalidation scope. */
export const NOTIFICATIONS_QUERY_ROOT = ['notifications'] as const;

export interface NormalizedNotificationPage {
  readonly rows: readonly NormalizedNotification[];
  readonly total: number;
}

/* ── API-167 ───────────────────────────────────────────────────────────── */

/**
 * `refetchInterval` is opt-in per call site, and defaults to off.
 *
 * The live SSE stream is the normal source of freshness. A caller that has
 * lost that stream for good (`useNotificationsSSE`'s `streamDead`) can turn
 * its OWN query into a slow poll. Setting an interval inside this hook would
 * poll for every caller, including the popover list and every user whose
 * stream is healthy.
 */
export function useNotificationsList(
  params: ListNotificationsParams,
  options: {
    readonly enabled?: boolean;
    readonly refetchInterval?: number | false;
    readonly refetchIntervalInBackground?: boolean;
  } = {},
): UseQueryResult<NormalizedNotificationPage> {
  return useQuery({
    queryKey: [...NOTIFICATIONS_QUERY_ROOT, 'list', params],
    queryFn: async ({ signal }) => {
      const page = await listNotifications(params, signal);
      return { rows: normalizeNotificationList(page.rows), total: page.total };
    },
    enabled: options.enabled ?? true,
    refetchInterval: options.refetchInterval ?? false,
    refetchIntervalInBackground: options.refetchIntervalInBackground ?? false,
  });
}

/* ── Issue 940/A4 — bell popover infinite scroll ──────────────────────────
 *
 * The real popover (`widgets/sidebar/ui/NotificationButton.tsx`) used to
 * show a fixed top-`POPOVER_PAGE_SIZE` (5) slice via `useNotificationsList`
 * and never paged further (ELITEA-0747/0748's own grep evidence). This
 * builds a real page-through-on-scroll query on the SAME `listNotifications`
 * fetcher (`page`/`pageSize` → `offset`/`limit`, already wired and already
 * used by the settings page's own page-based `NotificationsTablePagination`
 * — untouched by this addition, which is a second, independent consumer of
 * the same fetcher, not a replacement for it).
 *
 * `getNextPageParam` compares rows loaded so far against the server's own
 * `total` (the same field the badge query already reads) — the last group
 * naturally reports `hasNextPage: false` once every row is accounted for,
 * without any group needing to know it is "the last" one up front. This is
 * what ELITEA-0748's "last partial group... does not auto-trigger another
 * load" comes from: a false `hasNextPage`, not a special partial-group case.
 */
export interface NotificationsInfiniteListParams {
  readonly projectId: string | number;
  readonly pageSize?: number;
  readonly params?: Readonly<Record<string, string | number | boolean>>;
}

export function useNotificationsInfiniteList(
  params: NotificationsInfiniteListParams,
  options: { readonly enabled?: boolean } = {},
): UseInfiniteQueryResult<InfiniteData<NormalizedNotificationPage>, Error> {
  const { projectId, pageSize = 20, params: extra } = params;
  return useInfiniteQuery({
    queryKey: [...NOTIFICATIONS_QUERY_ROOT, 'infinite', projectId, pageSize, extra],
    queryFn: async ({ pageParam, signal }) => {
      const page = await listNotifications({ projectId, page: pageParam, pageSize, ...(extra !== undefined ? { params: extra } : {}) }, signal);
      return { rows: normalizeNotificationList(page.rows), total: page.total };
    },
    initialPageParam: 0,
    getNextPageParam: (_lastPage, allPages) => {
      const loaded = allPages.reduce((sum, loadedPage) => sum + loadedPage.rows.length, 0);
      const total = allPages[allPages.length - 1]?.total ?? 0;
      return loaded < total ? allPages.length : undefined;
    },
    enabled: options.enabled ?? true,
  });
}

/* ── API-168 (no baseline UI call site — see notifications.ts header) ────── */

export function useReadNotification(): UseMutationResult<
  unknown,
  Error,
  { readonly projectId: string | number; readonly id: string | number }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, id }) => readNotification(projectId, id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
    },
  });
}

/* ── API-169 (no baseline UI call site — see notifications.ts header) ────── */

export function useDeleteNotification(): UseMutationResult<
  unknown,
  Error,
  { readonly projectId: string | number; readonly id: string | number }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, id }) => deleteNotification(projectId, id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
    },
  });
}

/* ── API-170 / ACT-053 ─────────────────────────────────────────────────── */

export function useBulkDeleteNotifications(): UseMutationResult<
  unknown,
  Error,
  { readonly projectId: string | number; readonly ids: readonly (string | number)[] }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, ids }) => bulkDeleteNotifications(projectId, ids),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
    },
  });
}

/* ── API-171 / ACT-054 ─────────────────────────────────────────────────── */

export function useBulkMarkSeenNotifications(): UseMutationResult<
  unknown,
  Error,
  { readonly projectId: string | number; readonly ids: readonly (string | number)[] | 'all'; readonly isSeen: boolean }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, ids, isSeen }) => bulkMarkSeenNotifications(projectId, ids, isSeen),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
    },
  });
}
