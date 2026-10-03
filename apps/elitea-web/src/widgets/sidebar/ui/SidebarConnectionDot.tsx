import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { useRealtimeStatus, type RealtimeStatus } from '@/shared/api/sse';
import { t } from '@/shared/i18n';

/**
 * SHELL-012 — the sidebar's live-connection indicator. Old app:
 * `SidebarBody.jsx`'s `socketIconContainer` dot + `useSocketIcon.hooks.jsx`,
 * driven by socket.io lifecycle listeners — at the time, socket.io WAS the
 * app's live push channel, so the dot meant "live updates are flowing".
 *
 * It used to read the socket.io client here too. No deployment has a
 * socket.io server any more (elitea-main deleted its prototype in #126, and
 * every chart and compose file ships `vite_socket_server: ""`), so the noop
 * client's constant `'disconnected'` painted the dot red with
 * "Disconnected" on a working app. The live channels are SSE now (#92); the
 * dot reads their aggregated health from `shared/api/sse`'s
 * `useRealtimeStatus` (see `realtimeStatus.ts` for the decision table).
 *
 * `idle` — no live channel subscribed yet, e.g. before the personal project
 * resolves — renders nothing: there is nothing to claim either way.
 */
export function SidebarConnectionDot(): ReactNode {
  const status = useRealtimeStatus();
  if (status === 'idle') return null;

  return (
    <Tooltip title={connectionLabel(status)} placement="right">
      <Box
        data-testid="sidebar-connection-dot"
        data-status={status}
        sx={(theme: Theme) => ({
          width: '0.5rem',
          height: '0.5rem',
          borderRadius: theme.vars.shape.radiusPill,
          backgroundColor: dotColor(theme, status),
          position: 'absolute',
          top: 0,
          right: 0,
        })}
      />
    </Tooltip>
  );
}

function connectionLabel(status: Exclude<RealtimeStatus, 'idle'>): string {
  switch (status) {
    case 'connected':
      return t('widgets.sidebar.connection.connected', 'Connected');
    case 'connecting':
      return t('widgets.sidebar.connection.connecting', 'Connecting…');
    case 'reconnecting':
      return t('widgets.sidebar.connection.reconnecting', 'Connection lost — reconnecting…');
    case 'offline':
      return t('widgets.sidebar.connection.offline', 'Offline — live updates unavailable');
  }
}

function dotColor(theme: Theme, status: Exclude<RealtimeStatus, 'idle'>): string {
  switch (status) {
    case 'connected':
      return theme.vars.palette.icon.fill.success;
    case 'connecting':
    case 'reconnecting':
      return theme.vars.palette.icon.fill.warning;
    case 'offline':
      return theme.vars.palette.icon.fill.error;
  }
}
