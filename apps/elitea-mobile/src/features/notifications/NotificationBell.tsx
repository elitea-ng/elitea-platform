import { Link } from 'expo-router';
import { useEffect } from 'react';
import { StyleSheet, Text, View } from 'react-native';

import { useInvalidateNotifications, useUnseenCount } from '@/api/queries/notifications';
import { usePalette } from '@/constants/use-palette';
import { openNotificationsStream } from '@/features/notifications/notificationStream';
import { useSession } from '@/state/session';

/**
 * Header bell with the unseen count. While mounted it holds the project's
 * notifications stream (paused in the background by the stream itself) and
 * refreshes the count when a notification arrives.
 */
export function NotificationBell() {
  const palette = usePalette();
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

  const count = unseen.data ?? 0;
  const label = count > 99 ? '99+' : String(count);
  return (
    <Link
      href="/notifications"
      accessibilityLabel={count > 0 ? `Notifications, ${count} unread` : 'Notifications'}>
      <View style={styles.wrap}>
        <Text style={[styles.bell, { color: palette.accent }]}>🔔</Text>
        {count > 0 ? (
          <View style={[styles.badge, { backgroundColor: palette.danger }]}>
            <Text style={styles.badgeText}>{label}</Text>
          </View>
        ) : null}
      </View>
    </Link>
  );
}

const styles = StyleSheet.create({
  wrap: { paddingHorizontal: 2 },
  bell: { fontSize: 18 },
  badge: {
    position: 'absolute',
    top: -6,
    right: -10,
    minWidth: 18,
    height: 18,
    borderRadius: 9,
    paddingHorizontal: 4,
    alignItems: 'center',
    justifyContent: 'center',
  },
  badgeText: { color: '#ffffff', fontSize: 11, fontWeight: '700' },
});
