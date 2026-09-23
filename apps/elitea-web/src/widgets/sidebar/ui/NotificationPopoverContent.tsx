import type { ReactNode, UIEvent as ReactUIEvent } from 'react';
import { useCallback } from 'react';

import { useNavigate } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import CloseIcon from '@mui/icons-material/Close';

import { NotificationListItem, useBulkMarkSeenNotifications, useNotificationsInfiniteList } from '@/features/notifications';
import { t } from '@/shared/i18n';
import { BUTTON_VARIANTS, BaseBtn } from '@/shared/ui/BaseBtn';

/**
 * Issue 940/A4 — the popover now pages through the notifications API on
 * scroll (TanStack infinite query) instead of showing a fixed top-5
 * (old app: `NotificationList.jsx`'s `POPOVER_PAGE_SIZE = 5`, not carried
 * over). 20 matches `features/notifications/api/notifications.ts`'s own
 * `NOTIFICATION_PAGE_SIZE` default — the same group size the settings
 * page's page-based `NotificationsTablePagination` already uses, and the
 * size ELITEA-0747/0748's own test data assumes ("more than 20 notifications
 * ... at least 2 groups").
 */
const POPOVER_GROUP_SIZE = 20;

/** Fraction of the scrollable list's remaining height at which the next group loads — early enough that the user never sees the bottom edge before it appends. */
const SCROLL_LOAD_MORE_THRESHOLD_PX = 48;

export interface NotificationPopoverContentProps {
  readonly projectId: string;
  readonly onClose: () => void;
}

/**
 * The popover body: header + every loaded group of unread rows (paged in on
 * scroll, issue 940/A4) + mark-all/view-all. Split out of
 * `NotificationButton.tsx` purely to keep that file under the §3.5 400-line
 * budget — no behavioural reason, this is the exact same component that
 * file's own module doc already described splitting out "to keep
 * `NotificationButton`'s own prop/effect counts minimal".
 */
export function NotificationPopoverContent({ projectId, onClose }: NotificationPopoverContentProps): ReactNode {
  const navigate = useNavigate();
  const { data, isFetching, isFetchingNextPage, hasNextPage, fetchNextPage } = useNotificationsInfiniteList({
    projectId,
    pageSize: POPOVER_GROUP_SIZE,
    params: { only_new: true },
  });
  const bulkMarkSeen = useBulkMarkSeenNotifications();
  // ELITEA-0748 — every loaded group stays visible, appended below the ones
  // before it: `data.pages` is exactly that ordered list of groups, so
  // flattening it (rather than reading only the LAST page, `useQuery`'s own
  // shape) is what keeps group 1's rows on screen once group 2 has loaded.
  const rows = data?.pages.flatMap((page) => page.rows) ?? [];
  const hasUnreadRow = rows.some((row) => !row.isSeen);

  const handleMarkAllAsRead = useCallback(() => {
    bulkMarkSeen.mutate({ projectId, ids: 'all', isSeen: true });
  }, [projectId, bulkMarkSeen]);

  const handleViewAll = useCallback(() => {
    void navigate({ to: '/settings/notifications' });
    onClose();
  }, [navigate, onClose]);

  /**
   * ELITEA-0748 — infinite scroll: load the next group once the user nears
   * the bottom of the ALREADY-loaded rows. `hasNextPage` is what goes `false`
   * once the server's own `total` is fully accounted for (see
   * `useNotificationsInfiniteList`'s own doc comment) — the guard that makes
   * a last, partial (<20-item) group stop rather than firing one more
   * request that would just come back empty.
   */
  const handleScroll = useCallback(
    (event: ReactUIEvent<HTMLDivElement>) => {
      if (!hasNextPage || isFetchingNextPage) return;
      const target = event.currentTarget;
      const remaining = target.scrollHeight - target.scrollTop - target.clientHeight;
      if (remaining <= SCROLL_LOAD_MORE_THRESHOLD_PX) void fetchNextPage();
    },
    [hasNextPage, isFetchingNextPage, fetchNextPage],
  );

  return (
    <Box
      sx={(theme: Theme) => ({
        background: theme.vars.palette.background.notificationList,
        width: '20rem',
        display: 'flex',
        flexDirection: 'column',
        overflow: 'hidden',
      })}
    >
      <Box
        sx={(theme: Theme) => ({
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          padding: '0.75rem 1.25rem',
          borderBottom: `0.0625rem solid ${theme.vars.palette.border.notificationItem}`,
        })}
      >
        <Typography
          variant="labelMedium"
          color="text.secondary"
        >
          {t('widgets.sidebar.notification.title', 'Notifications')}
        </Typography>
        <IconButton
          size="small"
          aria-label={t('widgets.sidebar.notification.close', 'Close notifications')}
          onClick={onClose}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      </Box>

      <Box
        data-testid="sidebar-notification-scroll-area"
        onScroll={handleScroll}
        sx={{ maxHeight: '24rem', overflowY: 'auto' }}
      >
        {rows.map((notification) => (
          <NotificationListItem
            key={notification.id}
            notification={notification}
            projectId={projectId}
            context="list"
            onCloseNotificationList={onClose}
          />
        ))}
        {rows.length === 0 && (
          <Box sx={{ padding: '0.75rem 1.25rem' }}>
            <Typography
              variant="bodySmall"
              color="text.secondary"
            >
              {isFetching
                ? t('widgets.sidebar.notification.loading', 'Loading…')
                : t('widgets.sidebar.notification.empty', 'No new notifications right now')}
            </Typography>
          </Box>
        )}
        {rows.length > 0 && isFetchingNextPage && (
          <Box sx={{ padding: '0.5rem 1.25rem', textAlign: 'center' }}>
            <Typography
              variant="bodySmall"
              color="text.secondary"
            >
              {t('widgets.sidebar.notification.loading', 'Loading…')}
            </Typography>
          </Box>
        )}
      </Box>

      {rows.length > 0 && (
        <BaseBtn
          variant={BUTTON_VARIANTS.auxiliary}
          onClick={handleMarkAllAsRead}
          disabled={!hasUnreadRow}
          // `borderRadius: 0` (old app's own override here) dropped —
          // R-T10 bans ad-hoc radius even for the zero value; same
          // documented drop as `RunStateNodeGroup.tsx`.
          sx={(theme: Theme) => ({ borderTop: `0.0625rem solid ${theme.vars.palette.border.notificationItem}` })}
        >
          {t('widgets.sidebar.notification.markAllRead', 'Mark all as read')}
        </BaseBtn>
      )}
      <BaseBtn
        variant={BUTTON_VARIANTS.auxiliary}
        onClick={handleViewAll}
        sx={(theme: Theme) => ({ borderTop: `0.0625rem solid ${theme.vars.palette.border.notificationItem}` })}
      >
        {t('widgets.sidebar.notification.viewAll', 'View all')}
      </BaseBtn>
    </Box>
  );
}
