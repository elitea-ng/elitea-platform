import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { offlineStore } from '@/db/offlineStore';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';
import { offlineNotifications, offlineUnseenCount, readWithFallback } from '@/sync/offlineReads';

export type Notification = components['schemas']['Notification'];

export const NOTIFICATIONS_PAGE_SIZE = 30;

/** Falls back to the offline store when the workspace does not answer (soon, DEF-R5). */
function withOfflineFallback<T>(
  online: (signal: AbortSignal | undefined) => Promise<T>,
  offline: () => Promise<T | null>,
  signal: AbortSignal | undefined,
): Promise<T> {
  return readWithFallback(online, offline, { signal });
}

export async function fetchUnseenCount(projectId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal) {
  return withOfflineFallback(
    async (deadline) => {
      const page = await unwrap(
        client.GET('/notifications/notifications/prompt_lib/{project_id}', {
          params: { path: { project_id: String(projectId) }, query: { only_new: true, only_total: true } },
          signal: deadline,
        }),
      );
      return typeof page?.total === 'number' && page.total >= 0 ? page.total : 0;
    },
    () => offlineUnseenCount(projectId),
    signal,
  );
}

export async function fetchNotifications(projectId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal) {
  return withOfflineFallback(
    async (deadline) => {
      const page = await unwrap(
        client.GET('/notifications/notifications/prompt_lib/{project_id}', {
          params: {
            path: { project_id: String(projectId) },
            query: { limit: NOTIFICATIONS_PAGE_SIZE, offset: 0, sort_order: 'desc' },
          },
          signal: deadline,
        }),
      );
      return Array.isArray(page?.rows) ? page.rows : [];
    },
    () => offlineNotifications(projectId, NOTIFICATIONS_PAGE_SIZE),
    signal,
  );
}

export async function markAllSeen(projectId: number, client: ApiClient = sessionApiClient()): Promise<number> {
  const result = await unwrap(
    client.PUT('/notifications/notifications/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) } },
      body: { ids: 'all', is_seen: true },
    }),
  );
  return typeof result?.updated === 'number' ? result.updated : 0;
}

/** Marks one notification seen (`markNotificationSeen`); true when the server accepted it. */
export async function markSeen(projectId: number, notificationId: number, client: ApiClient = sessionApiClient()): Promise<boolean> {
  const { response } = await client.PUT('/notifications/notification/prompt_lib/{project_id}/{notification_id}', {
    params: { path: { project_id: String(projectId), notification_id: notificationId } },
  });
  return response.ok;
}

/** The server answers an id that is not (or no longer) the caller's with this 400 text, not a 404. */
const NOT_FOUND_TEXT = /not found/i;

/**
 * Deletes one of the caller's notifications (`deleteNotification`, client
 * contract 1.3). The server records a `deleted` tombstone, so the row also
 * leaves the caller's other devices with their next `changes_since` sync.
 * True when it is gone: deleted now (204), or already gone (400 "Notification
 * is not found", the answer for an id that is not, or no longer, the caller's).
 * Throws an ApiError otherwise (no answer, 403, 5xx): the row stays.
 */
export async function deleteNotification(projectId: number, notificationId: number, client: ApiClient = sessionApiClient()): Promise<true> {
  if (!Number.isSafeInteger(notificationId) || notificationId <= 0) throw new Error('invalid notification id');
  const { error, response } = await client.DELETE('/notifications/notification/prompt_lib/{project_id}/{notification_id}', {
    params: { path: { project_id: String(projectId), notification_id: notificationId } },
  });
  if (response.ok) return true;
  const text = (error as { error?: unknown } | undefined)?.error;
  if (response.status === 400 && typeof text === 'string' && NOT_FOUND_TEXT.test(text)) return true;
  // Rethrow as the usual ApiError (status, code, Retry-After).
  return unwrap(Promise.resolve({ error, response })) as Promise<never>;
}

/** Removes a dismissed row from the offline copy at once (its tombstone would do it on the next sync). */
export async function forgetNotification(db: Db | null, projectId: number, notificationId: number): Promise<void> {
  if (db === null) return;
  await db.run('DELETE FROM notifications WHERE project_id = ? AND id = ?', [projectId, notificationId]);
}

/** Drops a dismissed row from every cached notification list of the project. */
/**
 * Drops the row from every cached list of the project. A list fetch already
 * in flight is cancelled first: the server may have answered it before the
 * delete, and landing after this edit it would put the row back.
 */
export async function dropFromCache(queryClient: QueryClient, projectId: number, notificationId: number): Promise<void> {
  await queryClient.cancelQueries({ queryKey: [...queryKeys.notifications(projectId), 'list'] });
  queryClient.setQueriesData<Notification[]>({ queryKey: [...queryKeys.notifications(projectId), 'list'] }, (rows) =>
    Array.isArray(rows) ? rows.filter((row) => row.id !== notificationId) : rows,
  );
}

/**
 * Dismisses a notification: the server delete first (online only; a
 * failure leaves the row where it was), then the row leaves the cached list
 * and the offline copy, and the unseen count is read again.
 */
export function useDismissNotification(projectId: number | null) {
  const queryClient = useQueryClient();
  return async (notificationId: number): Promise<boolean> => {
    if (projectId === null) return false;
    try {
      await deleteNotification(projectId, notificationId);
    } catch (error) {
      log.info('notification not dismissed', { error });
      return false;
    }
    await dropFromCache(queryClient, projectId, notificationId);
    await forgetNotification(offlineStore.get(), projectId, notificationId).catch((error: unknown) =>
      log.warn('dismissed notification kept offline until the next sync', { error }),
    );
    void queryClient.invalidateQueries({ queryKey: [...queryKeys.notifications(projectId), 'unseen'] });
    return true;
  };
}

export function useUnseenCount(projectId: number | null) {
  return useQuery({
    queryKey: [...queryKeys.notifications(projectId ?? 0), 'unseen'],
    enabled: projectId !== null,
    queryFn: ({ signal }) => fetchUnseenCount(projectId as number, sessionApiClient(), signal),
  });
}

export function useNotifications(projectId: number | null) {
  return useQuery({
    queryKey: [...queryKeys.notifications(projectId ?? 0), 'list'],
    enabled: projectId !== null,
    queryFn: ({ signal }) => fetchNotifications(projectId as number, sessionApiClient(), signal),
  });
}

export function useInvalidateNotifications(projectId: number | null) {
  const queryClient = useQueryClient();
  return () => {
    if (projectId !== null) void queryClient.invalidateQueries({ queryKey: queryKeys.notifications(projectId) });
  };
}

/** A pushed notification frame (`notifications_notify`), validated at the boundary. */
export const NotificationFrameSchema = z.looseObject({
  id: z.number().int().positive(),
  uuid: z.string().min(1).max(64),
  is_seen: z.boolean(),
  project_id: z.number().int().positive(),
  event_type: z.string().min(1).max(200),
});

/** Human label for a notification's `event_type` (`agent_finished` → "Agent finished"). */
export function notificationTitle(eventType: string): string {
  const words = eventType.replace(/[_.-]+/g, ' ').trim();
  return words === '' ? 'Notification' : words.charAt(0).toUpperCase() + words.slice(1);
}
