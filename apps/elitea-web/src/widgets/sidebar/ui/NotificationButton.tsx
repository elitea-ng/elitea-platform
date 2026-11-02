import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { useCallback, useEffect, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { useNavigate, useRouteContext } from '@tanstack/react-router';

import type { PopoverOrigin } from '@mui/material/Popover';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Popover from '@mui/material/Popover';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import NotificationsNoneOutlined from '@mui/icons-material/NotificationsNoneOutlined';

import { NOTIFICATIONS_QUERY_ROOT, useNotificationsList, useNotificationsSSE } from '@/features/notifications';
import { t } from '@/shared/i18n';

import { NotificationPopoverContent } from './NotificationPopoverContent';

/**
 * SHELL-013 (disclosed composition gap) — the header-bell notification
 * popover. Old app: `[fsd]/widgets/sidebar-root/ui/button/
 * NotificationButton.jsx` (trigger + unread badge + live socket push) and
 * `[fsd]/widgets/Notifications/ui/NotificationList.jsx` (the popover
 * content — NOT ported directly; this widget reuses `features/
 * notifications`'s already-built `NotificationListItem` with
 * `context="list"` instead, per this unit's task brief).
 *
 * **Icon choice:** `@mui/icons-material`'s `NotificationsNoneOutlined`
 * substitutes for the old app's hand-rolled `components/Icons/BellIcon.jsx`
 * — the SAME documented-substitution precedent `SidebarBody.tsx` already
 * established in this exact file tree (`ChatBubbleOutlineIcon` standing in
 * for `ChatIcon.jsx`), chosen over porting a local inline SVG (the
 * alternative precedent, `features/notifications/ui/NotificationIcon.tsx`'s
 * `HeartIcon`/`CommentIcon`/`MedalIcon`) because this file already sits in a
 * widget that leans on `@mui/icons-material` for exactly this situation.
 * The unread indicator is a MANUALLY POSITIONED dot `Box` (matching
 * `SidebarConnectionDot.tsx`'s own absolute-positioned dot, not MUI's
 * `<Badge>`) because `<Badge variant="dot">`'s only styling hook is the
 * `.MuiBadge-dot` internal selector, which `elitea/no-mui-internal-selector`
 * (R-T6) bans outside `shared/brand/mui-overrides/` — a manual dot avoids
 * that fence entirely and reuses a real in-app precedent instead of a new
 * one. Colour is `theme.vars.palette.icon.fill.error` (a real brand token,
 * same token `SidebarConnectionDot`'s disconnected state already uses —
 * never the old app's raw `'#D71616'`).
 *
 * **Personal-project scoping:** notifications are scoped to
 * `personal_project_id`, matching the rest of this Wave-2 unit
 * (`routes/_shell/settings/notifications.tsx`, `routes/_shell/settings/
 * tokens.tsx`). Neither route may be imported from a widget (R-L1), so the
 * small `PersonalProjectIdContext`/`isPersonalProjectIdContext`/
 * `selectPersonalProjectId` trio below is a duplicate of the identical
 * shim those two files each already carry independently — this codebase's
 * established convention for a seam with no shared accessor and near-zero
 * reuse value (documented at each of those two call sites).
 *
 * **Live push is SSE, not socket.io (issue #92):** the old app's
 * `useSocket(sioEvents.notifications_notify, ...)` subscription is gone;
 * `features/notifications`'s `useNotificationsSSE` subscribes to the Go
 * route `GET {vite_server_url}/notifications/events/prompt_lib/
 * {projectId}` instead (`services/elitea-main/internal/api/v2/
 * notifications/events.go`). Socket.io is permanently dead in the
 * E2E compose stack (`VITE_SOCKET_SERVER=""`), so the socket path could
 * never fire there at all. One behavioural difference, deliberate: the
 * socket subscription was unconditional, while the SSE stream is scoped to
 * `personal_project_id` — the server route takes the project id in its
 * path and authorizes against it, so there is no unscoped stream to open.
 *
 * **Graceful degradation:** no personal project id, no runtime config, or
 * a runtime with no `EventSource` at all ⇒ the hook no-ops and the widget
 * still renders fully. Only the "flip on immediately from a live push" half
 * is unavailable — the same degradation shape the socket version had with no
 * provider mounted.
 *
 * With no live stream the badge query is NOT polled by default: the app sets
 * no `refetchInterval` anywhere, so it refreshes on refocus and on remount
 * only. That is why this widget turns the badge query into a slow poll once
 * `useNotificationsSSE` reports every reconnect attempt spent. The poll is
 * scoped to this one query, and it stops while the tab is in the background.
 *
 * **Graceful degradation, no personal project:** matches the old app's own
 * `onClickNotificationButton` exactly — `navigate(RouteDefinitions.Chat)`
 * instead of opening the popover when `personal_project_id` is unset. This
 * app's equivalent route is `/chat` (`widgets/sidebar/lib/navSections.ts`).
 *
 * **Mount point — a disclosed correction, not a silent one:** this unit's
 * task brief describes the old app's relative position as "just above the
 * Settings/Help-Center footer buttons." That is NOT what
 * `SidebarBody.jsx:233` actually shows: `{!sideBarCollapsed &&
 * <Buttons.NotificationButton />}` sits inside `styles.header`, the STICKY
 * TOP row, as the second child right after the logo/toggle `IconButton` —
 * nowhere near the footer. (Corroborating evidence in THIS app:
 * `SidebarHeader.tsx`'s own header `Box` is already laid out with
 * `justifyContent: collapsed ? 'center' : 'space-between'` around a SINGLE
 * child, which only makes layout sense if a second child was always meant
 * to sit there.) This unit's file scope is `NotificationButton.tsx` (+
 * test) and `SidebarBody.tsx` (+ its test) only — not `SidebarHeader.tsx` —
 * so per the task brief's own "use your own judgment... but stay within
 * scope" instruction, this widget is mounted in `SidebarBody.tsx` exactly
 * where the brief directs (directly before `<SidebarFooter>`, gated on
 * `!collapsed`), with this comment as the disclosed record of the more
 * accurate old-app position for whoever next revisits the sidebar header.
 * See `SidebarBody.tsx`'s own doc comment for the mount-point wiring.
 */
interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function isPersonalProjectIdContext(value: unknown): value is PersonalProjectIdContext {
  return typeof value === 'object' && value !== null;
}

function selectPersonalProjectId(context: unknown): string | undefined {
  if (!isPersonalProjectIdContext(context)) return undefined;
  return context.auth?.getUser?.()?.personal_project_id;
}

/**
 * How often the badge query polls once the live stream is gone for good. One
 * minute is slow enough to cost nothing and fast enough that the dot is not
 * stale for a whole session.
 */
const DEAD_STREAM_POLL_MS = 60_000;

export interface NotificationBellProps {
  /** The personal project the notifications belong to; without one the bell opens the chat. */
  readonly projectId: string | undefined;
  readonly hasUnread: boolean;
  /**
   * Where the tooltip and the popover open: `right` beside the web
   * sidebar's header row, `below` under the desktop sidebar's title bar.
   */
  readonly placement?: 'right' | 'below';
}

const POPOVER_ORIGIN: Readonly<Record<'right' | 'below', { readonly anchor: PopoverOrigin; readonly transform: PopoverOrigin }>> = {
  right: { anchor: { vertical: 'bottom', horizontal: 'left' }, transform: { vertical: 'top', horizontal: 'left' } },
  below: { anchor: { vertical: 'bottom', horizontal: 'right' }, transform: { vertical: 'top', horizontal: 'right' } },
};

/** The bell itself: the unread dot and the popover of the latest notifications. The web sidebar feeds it (`NotificationButton`); the desktop feeds it from its one notifications subscription. */
export function NotificationBell({ projectId, hasUnread, placement = 'right' }: NotificationBellProps): ReactNode {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);

  const handleClick = useCallback(
    (event: ReactMouseEvent<HTMLButtonElement>) => {
      if (!projectId) {
        void navigate({ to: '/chat' });
        return;
      }
      setAnchorEl(event.currentTarget);
    },
    [navigate, projectId],
  );

  /**
   * ELITEA-0747 — "scroll load state resets correctly on popup close and
   * reopen". `NotificationPopoverContent` unmounts on close (the `Popover`
   * below is gated on `anchorEl` truthiness, not MUI's own `open` prop
   * alone), but its infinite-query CACHE would otherwise survive under the
   * same query key and rehydrate every previously loaded group straight back
   * on reopen. Removing (not merely invalidating) that one query on close is
   * what makes a reopen start from group 1 again, every time — invalidate
   * would refetch every stale page instead of dropping them.
   */
  const handleClose = useCallback(() => {
    setAnchorEl(null);
    if (projectId) {
      queryClient.removeQueries({ queryKey: [...NOTIFICATIONS_QUERY_ROOT, 'infinite', projectId] });
    }
  }, [projectId, queryClient]);

  const origin = POPOVER_ORIGIN[placement];
  return (
    <>
      <Tooltip title={t('widgets.sidebar.notification.tooltip', 'Notifications')} placement={placement === 'right' ? 'right' : 'bottom'}>
        <IconButton
          data-testid="sidebar-notification-button"
          aria-label={t('widgets.sidebar.notification.tooltip', 'Notifications')}
          onClick={handleClick}
          size={placement === 'below' ? 'small' : 'medium'}
          sx={(theme: Theme) => ({
            position: 'relative',
            ...(placement === 'right' ? { width: '2rem', height: '2rem' } : {}),
            '&:hover': { backgroundColor: theme.vars.palette.background.button.tertiary.hover },
            '&:active': { backgroundColor: theme.vars.palette.background.button.tertiary.pressed },
          })}
        >
          <NotificationsNoneOutlined sx={{ width: '1rem', height: '1rem' }} />
          {hasUnread && (
            <Box
              data-testid="sidebar-notification-unread-dot"
              sx={(theme: Theme) => ({
                position: 'absolute',
                top: placement === 'right' ? '0.375rem' : '0.125rem',
                right: placement === 'right' ? '0.375rem' : '0.125rem',
                width: '0.5rem',
                height: '0.5rem',
                borderRadius: theme.vars.shape.radiusPill,
                backgroundColor: theme.vars.palette.icon.fill.error,
              })}
            />
          )}
        </IconButton>
      </Tooltip>
      {anchorEl && projectId && (
        <Popover
          open
          anchorEl={anchorEl}
          onClose={handleClose}
          anchorOrigin={origin.anchor}
          transformOrigin={origin.transform}
          slotProps={{
            paper: { sx: (theme: Theme) => ({ borderRadius: theme.vars.shape.radiusMd, overflow: 'hidden' }) },
          }}
        >
          <NotificationPopoverContent projectId={projectId} onClose={handleClose} />
        </Popover>
      )}
    </>
  );
}

/** The web sidebar's bell: its own notifications subscription (SSE, a slow poll once the stream is spent) feeding `NotificationBell`. */
export function NotificationButton(): ReactNode {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);
  const [hasUnread, setHasUnread] = useState(false);

  // Live push, now over SSE (issue #92 — see this file's header). No-ops
  // without a personal project id, without runtime config, or in a runtime
  // with no `EventSource`; `notifications_ready` inside the hook also
  // invalidates the list query, so the authoritative effect below re-runs.
  // Declared BEFORE the badge query because `streamDead` decides whether
  // that query polls.
  const handleNotify = useCallback(() => setHasUnread(true), []);
  const { streamDead } = useNotificationsSSE(personalProjectId, handleNotify);

  // Badge-only signal: `pageSize: 1` + `only_new: true` and the response's
  // own `total` (the API has no "count only" param).
  const { data, dataUpdatedAt } = useNotificationsList(
    { projectId: personalProjectId ?? '', pageSize: 1, params: { only_new: true } },
    {
      enabled: !!personalProjectId,
      // The fallback for a stream that will not come back. See this file's
      // header: without it the dot only changes on refocus or remount.
      refetchInterval: streamDead ? DEAD_STREAM_POLL_MS : false,
      refetchIntervalInBackground: false,
    },
  );

  // Every fresh response is AUTHORITATIVE and can flip the dot back off (a
  // live push only sets it optimistically between refetches). `dataUpdatedAt`
  // is a dependency because structural sharing keeps `data`'s reference
  // stable across a refetch that resolves to an unchanged value.
  useEffect(() => {
    if (data !== undefined) setHasUnread(!!data.total);
  }, [data, dataUpdatedAt]);

  return <NotificationBell projectId={personalProjectId} hasUnread={hasUnread} />;
}
