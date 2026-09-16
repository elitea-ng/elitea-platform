/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 * Unit A11 (`ROUTE-062` + `notifications_notify` domain).
 */

/* ── api ───────────────────────────────────────────────────────────────── */
export type { ListNotificationsParams } from './api/notifications';
export type { FullNotificationMeta, NormalizedNotification } from './api/normalize';
export type { NormalizedNotificationPage } from './api/useNotifications';
export {
  NOTIFICATIONS_QUERY_ROOT,
  useBulkDeleteNotifications,
  useBulkMarkSeenNotifications,
  useDeleteNotification,
  useNotificationsInfiniteList,
  useNotificationsList,
  useReadNotification,
} from './api/useNotifications';

/* ── lib ───────────────────────────────────────────────────────────────── */
export type { NotificationMessageSegment } from './lib/routes';
export { parseNotificationMessage, resolveNotificationHref } from './lib/routes';
export { useNotificationsSSE } from './lib/useNotificationsSSE';
export type { SoundNotificationConfig } from './lib/soundNotification';
export type { UseSoundNotificationResult } from './lib/useSoundNotification';
export { useSoundNotification } from './lib/useSoundNotification';

/* ── ui ────────────────────────────────────────────────────────────────── */
export type { NotificationListItemContext, NotificationListItemProps } from './ui/NotificationListItem';
export { NotificationListItem } from './ui/NotificationListItem';
