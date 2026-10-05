import { useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
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
