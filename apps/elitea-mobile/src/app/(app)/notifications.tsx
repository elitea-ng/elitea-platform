import { router, Stack } from 'expo-router';
import { useState } from 'react';
import { RefreshControl } from 'react-native';

import { speaksContract } from '@/api/discovery';
import { markAllSeen, markSeen, useDismissNotification, useInvalidateNotifications, useNotifications } from '@/api/queries/notifications';
import { OfflineNotice } from '@/components/OfflineNotice';
import { useDesign } from '@/design/DesignProvider';
import { applyFilter, filterOptions, type NotificationFilter } from '@/features/notifications/notificationModel';
import { NotificationsView } from '@/features/notifications/NotificationsView';
import { formatAge } from '@/lib/time';
import { isOfflineCopy } from '@/sync/offlineReads';
import { useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { haptic, headerActionOptions } from '@/ui';

/**
 * Recent notifications for the selected project. Client contract 1.3 adds
 * swipe to dismiss (a server delete whose tombstone also removes the row on
 * the caller's other devices); a type filter narrows the list to one kind.
 * "Mark all as read" is the navigation bar's action while anything is unread.
 */
export default function NotificationsScreen() {
  const design = useDesign();
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
  const canMarkAll = all.some((n) => !n.is_seen) && projectId !== null && !offline;

  return (
    <>
      <Stack.Screen
        // Options merge into the previous ones: with nothing unread the action is cleared explicitly.
        options={{
          headerRight: undefined,
          unstable_headerRightItems: undefined,
          ...headerActionOptions(
            canMarkAll
              ? [
                  {
                    id: 'mark-all',
                    icon: 'markAllRead',
                    label: 'Mark all as read',
                    onPress: () => {
                      if (projectId === null) return;
                      haptic('tabOrChipChange');
                      void markAllSeen(projectId)
                        .catch(() => 0)
                        .then(invalidate);
                    },
                  },
                ]
              : [],
            design,
          ),
        }}
      />
      <NotificationsView
        rows={rows.map((n) => ({
          id: n.id,
          event_type: n.event_type,
          meta: n.meta,
          is_seen: n.is_seen,
          created_at: n.created_at,
          age: formatAge(n.created_at),
        }))}
        status={notifications.isLoading ? 'loading' : notifications.error && all.length === 0 ? 'error' : 'ready'}
        filters={options}
        filter={filter}
        onFilter={setPicked}
        onOpen={(row, ref) => {
          if (!row.is_seen && projectId !== null && !offline) {
            void markSeen(projectId, row.id)
              .catch(() => false)
              .then(invalidate);
          }
          router.push({ pathname: '/c/[id]', params: { id: ref } });
        }}
        onDismiss={canDismiss ? (row) => dismiss(row.id) : null}
        onRetry={() => void notifications.refetch()}
        header={<OfflineNotice visible={offline} />}
        refreshControl={
          <RefreshControl refreshing={notifications.isRefetching} onRefresh={() => void notifications.refetch()} tintColor={design.colors.accent} />
        }
      />
    </>
  );
}
