/**
 * One square per project member, shaded by how much they did in the selected
 * window (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/ProjectsPage/UserActivityHeatmap.jsx`
 * (read-only). Two corrections to it:
 *
 *  1. **The shading is a SCALE, not a boolean.** The reference paints every
 *     member with any events at all the same green, and puts the count in a
 *     tooltip — so a member with one event and a member with four hundred are
 *     indistinguishable, and the component is named "heatmap" while carrying no
 *     heat. Here the opacity is the count's share of the busiest member's.
 *  2. **A failed query says so.** The reference has no error branch at all, so
 *     a 404 — which is what this endpoint used to be, being a stub with no
 *     route — rendered as "No users found", i.e. as a fact about the project.
 *
 * The join it relies on is `activity.user_id === member.id`. Those really are
 * the same identity space: the member listing formats `auth_core__user.id` with
 * `%d`, and `centry.audit_events.user_id` references the same table — which is
 * why `id` is parsed here rather than compared across types.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { memo, useMemo } from 'react';

import { t } from '@/shared/i18n';

import type { ProjectMemberRow, ProjectUserActivityRow } from './api/adminProjectsApi';

export interface ProjectUserActivityProps {
  readonly members: readonly ProjectMemberRow[];
  readonly activity: readonly ProjectUserActivityRow[];
  readonly isFetching: boolean;
  readonly isError: boolean;
}

interface MemberSquare {
  readonly id: number;
  readonly label: string;
  readonly email: string;
  readonly eventCount: number;
  /** 0 for an inactive member, else the count's share of the busiest member's. */
  readonly intensity: number;
}

/** The floor keeps a member with a single event visible rather than invisible. */
const MINIMUM_ACTIVE_INTENSITY = 0.25;

function buildSquares(
  members: readonly ProjectMemberRow[],
  activity: readonly ProjectUserActivityRow[],
): MemberSquare[] {
  const counts = new Map<number, number>();
  for (const row of activity) counts.set(row.user_id, row.event_count);
  const busiest = Math.max(0, ...counts.values());

  return members.map((member) => {
    const eventCount = counts.get(Number(member.id)) ?? 0;
    const share = busiest > 0 ? eventCount / busiest : 0;
    return {
      id: Number(member.id),
      label: member.name || member.email,
      email: member.email,
      eventCount,
      intensity:
        eventCount > 0 ? MINIMUM_ACTIVE_INTENSITY + (1 - MINIMUM_ACTIVE_INTENSITY) * share : 0,
    };
  });
}

export const ProjectUserActivity = memo(function ProjectUserActivity({
  members,
  activity,
  isFetching,
  isError,
}: ProjectUserActivityProps) {
  const squares = useMemo(() => buildSquares(members, activity), [members, activity]);
  const activeCount = squares.filter((square) => square.eventCount > 0).length;

  if (isError) {
    return (
      <Alert severity="error" sx={{ marginBottom: '0.5rem' }}>
        {t('pages.admin.projects.activity.error', 'Failed to load per-user activity.')}
      </Alert>
    );
  }

  if (isFetching && squares.length === 0) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', padding: '0.75rem' }}>
        <CircularProgress size={20} />
      </Box>
    );
  }

  if (squares.length === 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.projects.activity.noMembers', 'This project has no members.')}
      </Typography>
    );
  }

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.375rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {`${t('pages.admin.projects.activity.title', 'User activity')} · ${activeCount} / ${squares.length} ${t('pages.admin.projects.activity.active', 'active')}`}
      </Typography>
      <Box
        data-testid="project-user-activity"
        sx={{
          display: 'flex',
          flexWrap: 'wrap',
          gap: '0.25rem',
          maxHeight: '10rem',
          overflowY: 'auto',
        }}
      >
        {squares.map((square) => (
          <Tooltip
            key={square.id}
            arrow
            title={
              <Box>
                <Typography variant="bodySmall" sx={{ fontWeight: 600, display: 'block' }}>
                  {square.label}
                </Typography>
                <Typography variant="bodySmall" sx={{ display: 'block' }}>
                  {square.email}
                </Typography>
                <Typography variant="bodySmall" sx={{ display: 'block' }}>
                  {square.eventCount > 0
                    ? `${square.eventCount} ${t('pages.admin.projects.activity.events', 'events')}`
                    : t('pages.admin.projects.activity.none', 'No activity')}
                </Typography>
              </Box>
            }
          >
            <Box
              aria-label={`${square.label}: ${square.eventCount}`}
              sx={(theme) => ({
                width: '1.25rem',
                height: '1.25rem',
                borderRadius: theme.vars.shape.radiusSm,
                border: '1px solid',
                borderColor: 'divider',
                // The colour carries the count; `success.main` at a computed
                // opacity so it reads in both themes without a hardcoded hex
                // (R-T1 forbids raw colour literals).
                backgroundColor:
                  square.eventCount > 0 ? 'success.main' : 'action.disabledBackground',
                opacity: square.eventCount > 0 ? square.intensity : 1,
                transition: 'transform 120ms',
                '&:hover': { transform: 'scale(1.2)' },
              })}
            />
          </Tooltip>
        ))}
      </Box>
    </Box>
  );
});
