import { ActivityIndicator, FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import {
  markAllSeen,
  notificationTitle,
  useInvalidateNotifications,
  useNotifications,
} from '@/api/queries/notifications';
import { OfflineNotice } from '@/components/OfflineNotice';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { formatAge } from '@/lib/time';
import { isOfflineCopy } from '@/sync/offlineReads';
import { useSession } from '@/state/session';

/** Recent notifications for the selected project. */
export default function NotificationsScreen() {
  const palette = usePalette();
  const projectId = useSession((s) => s.projectId);
  const notifications = useNotifications(projectId);
  const invalidate = useInvalidateNotifications(projectId);
  const rows = notifications.data ?? [];
  const anyUnseen = rows.some((n) => !n.is_seen);

  return (
    <View style={[styles.flex, { backgroundColor: palette.background }]}>
      <OfflineNotice visible={isOfflineCopy(notifications.data)} />
      {anyUnseen && projectId !== null && !isOfflineCopy(notifications.data) ? (
        <Pressable
          accessibilityRole="button"
          onPress={() => {
            void markAllSeen(projectId)
              .catch(() => 0)
              .then(invalidate);
          }}
          style={styles.markAll}>
          <Text style={{ color: palette.accent, fontWeight: '600' }}>Mark all as read</Text>
        </Pressable>
      ) : null}
      {notifications.isLoading ? (
        <ActivityIndicator style={styles.loading} color={palette.accent} />
      ) : (
        <FlatList
          data={rows}
          keyExtractor={(n) => String(n.id)}
          onRefresh={() => void notifications.refetch()}
          refreshing={notifications.isRefetching}
          ListEmptyComponent={
            <Text style={[styles.empty, { color: palette.textSecondary }]}>
              {notifications.error ? "Couldn't load notifications." : 'No notifications.'}
            </Text>
          }
          ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
          renderItem={({ item }) => (
            <View style={styles.row}>
              {!item.is_seen ? <View style={[styles.dot, { backgroundColor: palette.accent }]} /> : null}
              <Text style={[styles.title, { color: palette.text, fontWeight: item.is_seen ? '400' : '600' }]}>
                {notificationTitle(item.event_type)}
              </Text>
              <Text style={[styles.age, { color: palette.textSecondary }]}>{formatAge(item.created_at)}</Text>
            </View>
          )}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  markAll: { padding: Spacing.three, alignItems: 'flex-end' },
  loading: { padding: Spacing.four },
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  row: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two, padding: Spacing.three },
  dot: { width: 8, height: 8, borderRadius: 4 },
  title: { flex: 1, fontSize: 15 },
  age: { fontSize: 13 },
});
