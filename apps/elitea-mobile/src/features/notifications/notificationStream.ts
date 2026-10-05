import { originOf } from '@/api/paths';
import { NotificationFrameSchema } from '@/api/queries/notifications';
import type { SseEvent } from '@/api/sse/parser';
import { ResumableStream, type ResumableStreamOptions } from '@/api/sse/stream';
import { projectScope } from '@/lib/abortRegistry';

export const NOTIFICATION_EVENTS = {
  ready: 'notifications_ready',
  notify: 'notifications_notify',
} as const;

export function notificationsStreamUrl(origin: string, projectId: number): string | null {
  const base = originOf(origin);
  if (base === null || !Number.isSafeInteger(projectId) || projectId <= 0) return null;
  return `${base}/api/v2/notifications/events/prompt_lib/${projectId}`;
}

/**
 * Handles one notifications frame. Returns true when it announces a
 * notification for `projectId` (the caller refreshes the bell).
 */
export function isNotificationForProject(event: SseEvent, projectId: number): boolean {
  if (event.event !== NOTIFICATION_EVENTS.notify) return false;
  let data: unknown;
  try {
    data = JSON.parse(event.data);
  } catch {
    return false;
  }
  const parsed = NotificationFrameSchema.safeParse(data);
  return parsed.success && parsed.data.project_id === projectId;
}

/**
 * The project's notifications stream. Without a cursor the server starts at
 * its high-water mark (`notifications_ready`), so only new notifications
 * arrive; the cursor then follows every id like any other stream.
 */
export function openNotificationsStream(
  origin: string,
  projectId: number,
  onNotification: () => void,
  overrides: Partial<ResumableStreamOptions> = {},
): ResumableStream | null {
  const url = notificationsStreamUrl(origin, projectId);
  if (url === null) return null;
  const stream = new ResumableStream({
    url,
    scope: projectScope(projectId),
    onEvent: (event) => {
      if (isNotificationForProject(event, projectId)) onNotification();
    },
    ...overrides,
  });
  void stream.start();
  return stream;
}
