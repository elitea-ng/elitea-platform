import { router } from 'expo-router';
import { useState } from 'react';
import { ActivityIndicator, FlatList, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { speaksContract } from '@/api/discovery';
import {
  markAllSeen,
  markSeen,
  useDismissNotification,
  useInvalidateNotifications,
  useNotifications,
} from '@/api/queries/notifications';
import {
  applyFilter,
  describeNotification,
  filterOptions,
  type NotificationFilter,
} from '@/features/notifications/notificationModel';
import { OfflineNotice } from '@/components/OfflineNotice';
import { SwipeToDelete } from '@/components/SwipeToDelete';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { formatAge } from '@/lib/time';
import { isOfflineCopy } from '@/sync/offlineReads';
import { useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Recent notifications for the selected project. Client contract 1.3 adds
 * swipe to dismiss (a server delete whose tombstone also removes the row on
 * the caller's other devices); a type filter narrows the list to one kind.
 */
export default function NotificationsScreen() {
  const palette = usePalette();
  const projectId = useSession((s) => s.projectId);
  const notifications = useNotifications(projectId);
  const invalidate = useInvalidateNotifications(projectId);
  const dismiss = useDismissNotification(projectId);
  const workspace = useWorkspaceInfo();
  const offline = isOfflineCopy(notifications.data);
  // Dismiss is a 1.3 operation, and only for the live list (never the offline copy).
  const canDismiss = speaksContract(workspace?.discovery, 1, 3) && !offline && projectId !== null;
  const all = notifications.data ?? [];
  const options = filterOptions(all);
  const [picked, setPicked] = useState<NotificationFilter>('all');
  // A filter whose last row went away falls back to everything.
  const filter = options.some((o) => o.value === picked) ? picked : 'all';
  const rows = applyFilter(all, filter);
  const anyUnseen = all.some((n) => !n.is_seen);

  return (
    <View style={[styles.flex, { backgroundColor: palette.background }]}>
      <OfflineNotice visible={offline} />
      {options.length > 0 ? (
        <ScrollView
          testID="notification-filters"
          horizontal
          showsHorizontalScrollIndicator={false}
          // A ScrollView grows by default: without this the chip bar takes half the screen.
          style={styles.chipBar}
          contentContainerStyle={styles.chips}
          accessibilityRole="tablist">
          {options.map((option) => {
            const selected = option.value === filter;
            return (
              <Pressable
                key={option.value}
                testID={`filter-${option.value}`}
                accessibilityRole="tab"
                accessibilityState={{ selected }}
                accessibilityLabel={`${option.label}, ${option.count}`}
                onPress={() => setPicked(option.value)}
                style={[
                  styles.chip,
                  { borderColor: selected ? palette.accent : palette.border, backgroundColor: selected ? palette.accent : 'transparent' },
                ]}>
                <Text style={{ color: selected ? palette.background : palette.text, fontWeight: selected ? '600' : '400' }}>
                  {option.label}
                </Text>
              </Pressable>
            );
          })}
        </ScrollView>
      ) : null}
      {anyUnseen && projectId !== null && !offline ? (
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
              {notifications.error ? "Couldn't load notifications." : filter !== 'all' ? 'Nothing of this kind.' : 'No notifications.'}
            </Text>
          }
          ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
          renderItem={({ item }) => {
            const view = describeNotification(item);
            const row = (
              <>
                {!item.is_seen ? <View style={[styles.dot, { backgroundColor: palette.accent }]} /> : null}
                <Text style={[styles.title, { color: palette.text, fontWeight: item.is_seen ? '400' : '600' }]}>
                  {view.title}
                </Text>
                <Text style={[styles.age, { color: palette.textSecondary }]}>{formatAge(item.created_at)}</Text>
              </>
            );
            const ref = view.conversationRef;
            const onDelete = canDismiss ? () => dismiss(item.id) : null;
            if (ref === null) {
              return (
                <SwipeToDelete actionLabel="Dismiss" onDelete={onDelete} testID={`notification-${item.id}`}>
                  {(a11y) => (
                    <View {...a11y} style={styles.row} accessible accessibilityLabel={view.title}>
                      {row}
                    </View>
                  )}
                </SwipeToDelete>
              );
            }
            return (
              <SwipeToDelete actionLabel="Dismiss" onDelete={onDelete} testID={`notification-${item.id}`}>
                {(a11y) => (
                  <Pressable
                    {...a11y}
                    accessibilityRole="link"
                    accessibilityHint="Opens the conversation"
                    style={styles.row}
                    onPress={() => {
                      if (!item.is_seen && projectId !== null && !offline) {
                        void markSeen(projectId, item.id)
                          .catch(() => false)
                          .then(invalidate);
                      }
                      router.push({ pathname: '/c/[id]', params: { id: ref } });
                    }}>
                    {row}
                  </Pressable>
                )}
              </SwipeToDelete>
            );
          }}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  chipBar: { flexGrow: 0, flexShrink: 0 },
  chips: { alignItems: 'center', paddingHorizontal: Spacing.three, paddingTop: Spacing.two, gap: Spacing.two },
  chip: { borderWidth: 1, borderRadius: 16, paddingHorizontal: Spacing.three, paddingVertical: 6 },
  markAll: { padding: Spacing.three, alignItems: 'flex-end' },
  loading: { padding: Spacing.four },
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  row: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two, padding: Spacing.three },
  dot: { width: 8, height: 8, borderRadius: 4 },
  title: { flex: 1, fontSize: 15 },
  age: { fontSize: 13 },
});
