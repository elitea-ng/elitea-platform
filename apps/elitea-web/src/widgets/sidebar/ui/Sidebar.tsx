import type { ReactNode } from 'react';
import { useCallback, useEffect } from 'react';

import Box from '@mui/material/Box';
import Drawer from '@mui/material/Drawer';
import type { Theme } from '@mui/material/styles';

import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';

import { readPersistedCollapsed, writePersistedCollapsed } from '../lib/collapsedPersistence';
import { useSidebarCollapsedStore } from '../model/sidebarCollapsed.store';
import { SidebarBody } from './SidebarBody';
import { COLLAPSED_SIDE_BAR_WIDTH_PX, SIDE_BAR_WIDTH_PX } from '@/shared/ui/layout/sidebarWidth';

// Re-exported, not redefined: these used to be literals here (208/72) that
// disagreed with the admin rail's own (220/60), and with the baseline's
// 216/64. See `shared/ui/layout/sidebarWidth.ts`. The re-export keeps this
// module's existing consumers importing from where they already do.
export { SIDE_BAR_WIDTH_PX, COLLAPSED_SIDE_BAR_WIDTH_PX };

export interface SidebarProps {
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
  /** Threaded straight through to `SidebarBody`'s footer bar — see `SidebarFooter.tsx`. */
  onToggleAssistant?: (() => void) | undefined;
}

/**
 * SHELL-001..012 — the sidebar's outer chrome: the permanent `Drawer` +
 * collapse toggle. Ported from `[fsd]/widgets/sidebar-root/ui/Sidebar.jsx` /
 * `[fsd]/app/layout/MainSidebar.jsx`. Consumed by `widgets/app-shell`, which
 * supplies the four data props (project list, selected project, and
 * permission set — none of which this widget fetches itself beyond its own
 * `usePermissionSet`/`useProjectOptions` used by callers that pass a
 * `projectId`; see `../index.ts`).
 */
export function Sidebar({ permissions, projects, selectedProjectId, onSelectProject, onToggleAssistant }: SidebarProps): ReactNode {
  const collapsed = useSidebarCollapsedStore((state) => state.collapsed);
  const setCollapsed = useSidebarCollapsedStore((state) => state.setCollapsed);

  // One-time hydration from storage on mount — see `collapsedPersistence.ts`'s
  // header for why this cannot happen at the store's own module scope.
  // Empty deps deliberately: storage is read exactly once per mount,
  // matching the old app's own one-shot Redux hydration.
  useEffect(() => {
    setCollapsed(readPersistedCollapsed());
  }, [setCollapsed]);

  const toggle = useCallback(() => {
    const next = !useSidebarCollapsedStore.getState().collapsed;
    writePersistedCollapsed(next);
    setCollapsed(next);
  }, [setCollapsed]);

  const width = collapsed ? COLLAPSED_SIDE_BAR_WIDTH_PX : SIDE_BAR_WIDTH_PX;

  return (
    <Box
      component="nav"
      aria-label={t('widgets.sidebar.nav.ariaLabel', 'side-bar')}
      sx={{ width: `${width}px`, flexShrink: 0, overflowX: 'hidden' }}
    >
      <Drawer
        variant="permanent"
        open
        sx={{
          display: { xs: 'none', sm: 'block' },
          background: 'transparent',
          position: 'relative',
          overflow: 'visible',
        }}
        slotProps={{
          paper: { sx: { boxSizing: 'border-box', width: `${width}px`, background: 'transparent' } },
        }}
      >
        <SidebarBody
          collapsed={collapsed}
          onToggleCollapsed={toggle}
          permissions={permissions}
          projects={projects}
          selectedProjectId={selectedProjectId}
          onSelectProject={onSelectProject}
          onToggleAssistant={onToggleAssistant}
        />
        <Box
          component="button"
          type="button"
          data-testid="sidebar-collapse-toggle"
          aria-label={
            collapsed
              ? t('widgets.sidebar.expand', 'Expand sidebar')
              : t('widgets.sidebar.collapse', 'Collapse sidebar')
          }
          onClick={toggle}
          sx={(theme: Theme) => ({
            position: 'fixed',
            top: '3rem',
            // `Sidebar.jsx`'s `collapseButton.left` is `12.75rem` = 204px
            // against a 216px rail, i.e. the 24px circle STRADDLES the rail's
            // right edge (204 + 24/2 === 216). The port's `- 24` tucked the
            // whole button inside the rail, 12px adrift.
            left: collapsed ? '3.25rem' : `${SIDE_BAR_WIDTH_PX - 12}px`,
            width: '1.5rem',
            height: '1.5rem',
            borderRadius: theme.vars.shape.radiusPill,
            border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
            display: 'flex',
            justifyContent: 'center',
            alignItems: 'center',
            cursor: 'pointer',
            background: theme.vars.palette.background.secondary,
            zIndex: 2101,
            padding: 0,
            appearance: 'none',
          })}
        >
          {collapsed ? (
            <ChevronRightIcon sx={{ width: '1rem', height: '1rem' }} />
          ) : (
            <ChevronLeftIcon sx={{ width: '1rem', height: '1rem' }} />
          )}
        </Box>
      </Drawer>
    </Box>
  );
}
