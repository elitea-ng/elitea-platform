import { router } from 'expo-router';
import { useEffect } from 'react';

import { useInvalidateNotifications, useUnseenCount } from '@/api/queries/notifications';
import { openNotificationsStream } from '@/features/notifications/notificationStream';
import { useSession } from '@/state/session';
import type { HeaderAction } from '@/ui';

/**
 * The unseen-notification count of the active project. While mounted it holds the project's
 * notifications stream (paused in the background by the stream itself) and refreshes the
 * count when a notification arrives. Mount it once, where the bell is shown.
 */
export function useNotificationCount(): number {
  const projectId = useSession((s) => s.projectId);
  const origin = useSession((s) => s.serverOrigin);
  const unseen = useUnseenCount(projectId);
  const invalidate = useInvalidateNotifications(projectId);

  useEffect(() => {
    if (projectId === null || origin === null) return;
    const stream = openNotificationsStream(origin, projectId, invalidate);
    return () => stream?.close();
    // `invalidate` is rebuilt per render; the stream depends only on the project.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId, origin]);

  return unseen.data ?? 0;
}

export function notificationsLabel(count: number): string {
  return count > 0 ? `Notifications, ${count} unread` : 'Notifications';
}

/**
 * The bell as a navigation-bar action (`headerActionOptions`): an SF Symbol bar button with the
 * system badge on iOS 26 (inside the Liquid Glass group, never clipped), a 48 dp top-app-bar
 * icon with an in-frame badge on Android.
 */
export function notificationBellAction(count: number): HeaderAction {
  return {
    id: 'notifications',
    icon: 'bell',
    label: 'Notifications',
    accessibilityLabel: notificationsLabel(count),
    badge: count,
    onPress: () => router.push('/notifications'),
  };
}
