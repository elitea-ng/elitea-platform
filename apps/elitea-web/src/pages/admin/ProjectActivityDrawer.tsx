/**
 * The per-project activity drawer (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/ProjectsPage/ProjectActivityDrawer.jsx`
 * (read-only) — 601 lines, the largest single component of the reference page.
 *
 * ## Almost all of it is the Audit Trail port, reused
 *
 * The drawer asks the same four audit questions with `project_id` pinned, so it
 * renders `AuditHeatmap` unchanged, takes its filter bar and results block from
 * `./AuditDrawerParts` (shared with the per-user drawer) and runs on
 * `./useAuditDrawer` via `./useProjectActivityDrawer`. What is genuinely local
 * here is the heading and the per-member activity strip.
 *
 * ## Two reference behaviours deliberately NOT carried over
 *
 *  - Closing the reference drawer resets the search, the page and the range but
 *    NOT `pageSize`, the sort or the refresh token, so those leaked between
 *    projects: opening project B showed it sorted by whatever project A was
 *    left on. Here the whole hook is remounted per project (`key={project.id}`
 *    on the content), so every field resets together and none can leak.
 *  - The reference's own `project_id` filter box is gone. This drawer describes
 *    the project it was opened for; a box that could change that is the page's
 *    job, and `pages/admin/AuditTrail.tsx` has it.
 */
import CloseIcon from '@mui/icons-material/Close';
import RefreshOutlinedIcon from '@mui/icons-material/RefreshOutlined';
import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { AuditDrawerFilters, AuditDrawerResults } from './AuditDrawerParts';
import { AuditHeatmap } from './AuditHeatmap';
import { ProjectUserActivity } from './ProjectUserActivity';
import { useProjectMembers, type AdminProjectRow } from './api/adminProjectsApi';
import { useProjectActivityDrawer } from './useProjectActivityDrawer';

export interface ProjectActivityDrawerProps {
  /** The project to describe, or `null` when the drawer is closed. */
  readonly project: AdminProjectRow | null;
  readonly onClose: () => void;
}

export function ProjectActivityDrawer({ project, onClose }: ProjectActivityDrawerProps) {
  return (
    <Drawer
      anchor="right"
      open={project !== null}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: { xs: '100%', md: '75vw' } } } }}
    >
      {/*
        Keyed on the project id so every piece of drawer state — range, sort,
        page size, drill-down — is rebuilt per project. See this file's header:
        the reference reset some of them and leaked the rest.

        The `project !== null` check is a TYPE guard, not a behavioural one:
        MUI's Drawer unmounts its children while closed (no `keepMounted`), so
        removing this check changes nothing observable — confirmed by mutation,
        which is why no test can be written to defend it. It stays because
        `ProjectActivityContent` requires a non-null project.
      */}
      {project !== null ? (
        <ProjectActivityContent key={project.id} project={project} onClose={onClose} />
      ) : null}
    </Drawer>
  );
}

function ProjectActivityContent({
  project,
  onClose,
}: {
  readonly project: AdminProjectRow;
  readonly onClose: () => void;
}) {
  const state = useProjectActivityDrawer(project.id);
  const membersQuery = useProjectMembers(project.id);

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
            {t('pages.admin.projects.activity.heading', 'Project activity')}
          </Typography>
          <Typography variant="bodyMedium" color="text.secondary">
            {`${project.name} (ID: ${project.id})`}
          </Typography>
        </Box>
        <Box sx={{ display: 'flex', gap: '0.25rem' }}>
          <Tooltip title={t('pages.admin.projects.activity.refresh', 'Refresh')}>
            <IconButton
              size="small"
              onClick={state.onRefresh}
              aria-label={t('pages.admin.projects.activity.refresh', 'Refresh')}
            >
              <RefreshOutlinedIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          <IconButton
            size="small"
            onClick={onClose}
            aria-label={t('pages.admin.projects.activity.close', 'Close')}
          >
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
      </Box>

      <AuditDrawerFilters
        state={state}
        searchPlaceholder={t('pages.admin.projects.activity.search', 'Search actions, tools, users')}
        searchTestId="project-activity-search"
      />

      <Divider />

      <ProjectUserActivity
        members={membersQuery.data ?? []}
        activity={state.userActivity}
        isFetching={membersQuery.isFetching || state.isUserActivityFetching}
        isError={membersQuery.isError || state.isUserActivityError}
      />

      <AuditHeatmap
        heatmap={state.heatmap}
        isFetching={state.isHeatmapFetching}
        viewMode={state.viewMode}
        onCellSelect={state.onCellSelect}
      />

      <AuditDrawerResults
        state={state}
        errorMessage={t('pages.admin.projects.activity.loadError', 'Failed to load this project’s activity.')}
      />
    </Box>
  );
}
