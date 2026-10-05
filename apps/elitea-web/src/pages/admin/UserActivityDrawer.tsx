/**
 * The per-user activity drawer (Admin › Users → the row's activity control).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/UsersPage/UserActivityDrawer.jsx`
 * (read-only).
 *
 * ## Why this exists now and did not before
 *
 * The Users port shipped this control DISABLED, because at the time elitea-main
 * served no audit endpoints at all. A14's Audit Trail port then gave the
 * service the real ones (`services/elitea-main/internal/api/v2/eliteacore/audit.go`),
 * and all four accept `user_id` as a bound filter — see `conditions()` in
 * `audit_query.go`. So the data was there and only the VIEW was missing. This
 * is that view; the control is live and the tooltip that explained its absence
 * is gone.
 *
 * ## It is the project drawer with a different pin
 *
 * Filter bar and results block come from `./AuditDrawerParts`, the heatmap from
 * `./AuditHeatmap`, the state from `./useUserActivityDrawer` — all shared with
 * `./ProjectActivityDrawer`. What is local is the heading and the absence of a
 * per-member strip, which has no meaning for a single person.
 *
 * ## One thing the traces view means here, and does not on the page
 *
 * With `user_id` pinned, the TRACES listing returns every trace containing at
 * least one span by this user — the server filters spans and then groups by
 * `trace_id`. A trace can therefore carry spans by somebody else (a shared
 * agent run). That is the reference's behaviour too, and the Spans tab is the
 * strictly per-user view for anyone who needs it.
 */
import CloseIcon from '@mui/icons-material/Close';
import RefreshOutlinedIcon from '@mui/icons-material/RefreshOutlined';
import Box from '@mui/material/Box';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { AuditDrawerFilters, AuditDrawerResults } from './AuditDrawerParts';
import { AuditHeatmap } from './AuditHeatmap';
import type { AdminUserRow } from './api/adminUsersApi';
import { useUserActivityDrawer } from './useUserActivityDrawer';

export interface UserActivityDrawerProps {
  /** The user to describe, or `null` when the drawer is closed. */
  readonly user: AdminUserRow | null;
  readonly onClose: () => void;
}

export function UserActivityDrawer({ user, onClose }: UserActivityDrawerProps) {
  return (
    <Drawer
      anchor="right"
      open={user !== null}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: { xs: '100%', md: '75vw' } } } }}
    >
      {/*
        Keyed on the user id, for the same reason the project drawer is keyed on
        the project: every piece of drawer state is rebuilt per subject, so
        opening user B never shows them through user A's range, sort or
        drill-down. The reference reset some of those on close and leaked the
        rest.
      */}
      {user !== null ? <UserActivityContent key={user.id} user={user} onClose={onClose} /> : null}
    </Drawer>
  );
}

function UserActivityContent({
  user,
  onClose,
}: {
  readonly user: AdminUserRow;
  readonly onClose: () => void;
}) {
  const state = useUserActivityDrawer(user.id);

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        height: '100%',
        padding: '1rem 1.25rem',
        gap: '0.75rem',
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '1rem' }}>
        <Box>
          <Typography variant="headingSmall" component="h2">
            {t('pages.admin.users.activity.heading', 'User activity')}
          </Typography>
          {/*
            Name OR email, never both padded together: system users have an
            email and an empty name, and `"  (ID: 7)"` is what the reference
            rendered for them.
          */}
          <Typography variant="bodyMedium" color="text.secondary">
            {`${user.name || user.email} (ID: ${user.id})`}
          </Typography>
        </Box>
        <Box sx={{ display: 'flex', gap: '0.25rem' }}>
          <Tooltip title={t('pages.admin.users.activity.refresh', 'Refresh')}>
            <IconButton
              size="small"
              onClick={state.onRefresh}
              aria-label={t('pages.admin.users.activity.refresh', 'Refresh')}
            >
              <RefreshOutlinedIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          <IconButton
            size="small"
            onClick={onClose}
            aria-label={t('pages.admin.users.activity.close', 'Close')}
          >
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
      </Box>

      <AuditDrawerFilters
        state={state}
        searchPlaceholder={t('pages.admin.users.activity.search', 'Search actions, tools, projects')}
        searchTestId="user-activity-search"
      />

      <AuditHeatmap
        heatmap={state.heatmap}
        isFetching={state.isHeatmapFetching}
        viewMode={state.viewMode}
        onCellSelect={state.onCellSelect}
      />

      <AuditDrawerResults
        state={state}
        errorMessage={t('pages.admin.users.activity.loadError', 'Failed to load this user’s activity.')}
      />
    </Box>
  );
}
