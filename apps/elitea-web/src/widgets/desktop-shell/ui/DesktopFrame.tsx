/**
 * The desktop app's frame (ADR-0029, workspace-first): the folders sidebar on
 * the left, the page in the middle, the command palette over both. It
 * replaces the web shell's sidebar in the `desktop` build only — `AppShell`
 * renders it from a lazy import the web build drops.
 *
 * Workspace pages draw their own header (folder, project, agent) and the
 * changes panel; every other page (the Elitea features, Settings) gets a slim
 * title row with the way back to the folders.
 */
import { useMemo, type ReactNode } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Snackbar from '@mui/material/Snackbar';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { eliteaItems } from '../lib/eliteaSections';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useAppIpc } from '../model/appIpcContext';
import { useHostIntegration } from '../model/useHostIntegration';
import { useNativeWindowTheme } from '../model/useNativeWindowTheme';
import { useRouteLogging } from '../model/useRouteLogging';
import { useShellActions } from '../model/useShellActions';
import { useShellLocation } from '../model/useShellLocation';
import { CommandPalette } from './CommandPalette';
import { DesktopSidebar } from './DesktopSidebar';
import { NativeLook } from './NativeLook';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

export interface DesktopFrameProps {
  children: ReactNode;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
}

/** The "show sidebar" button a pane header shows while the sidebar is hidden. */
export function ShowSidebarButton(): React.JSX.Element | null {
  const open = useDesktopLayout((state) => state.sidebarOpen);
  if (open) return null;
  return (
    <Tooltip title={`${t('desktop.shell.showSidebar', 'Show sidebar')} (${modKey()}\\)`}>
      <IconButton size="small" aria-label={t('desktop.shell.showSidebar', 'Show sidebar')} onClick={() => useDesktopLayout.getState().setSidebarOpen(true)}>
        <ViewSidebarOutlinedIcon fontSize="inherit" />
      </IconButton>
    </Tooltip>
  );
}

function ShellNotice(): React.JSX.Element {
  const notice = useDesktopLayout((state) => state.notice);
  const close = (): void => useDesktopLayout.getState().setNotice(null);
  return (
    <Snackbar open={notice !== null} onClose={close} autoHideDuration={8000} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
      <Alert severity="warning" onClose={close} variant="filled">
        {notice ?? ''}
      </Alert>
    </Snackbar>
  );
}

export function DesktopFrame({ children, permissions, projects, selectedProjectId, onSelectProject }: DesktopFrameProps): React.JSX.Element {
  const run = useShellActions();
  const ipc = useAppIpc();
  useHostIntegration(run, ipc);
  useNativeWindowTheme(ipc);
  useRouteLogging();
  const location = useShellLocation();
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  const items = useMemo(() => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)), [permissions, selectedProjectId]);

  return (
    <NativeLook>
      <Box data-testid="desktop-frame" sx={{ display: 'flex', height: '100vh', overflow: 'hidden' }}>
        {sidebarOpen && (
          <DesktopSidebar
            location={location}
            run={run}
            permissions={permissions}
            projects={projects}
            selectedProjectId={selectedProjectId}
            onSelectProject={onSelectProject}
          />
        )}
        <Box
          component="main"
          sx={(theme: Theme) => ({
            flex: 1,
            minWidth: 0,
            height: '100vh',
            display: 'flex',
            flexDirection: 'column',
            background: theme.vars.palette.background.default,
          })}
        >
          {!location.inFolders && (
            <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
              <TitleBarSpacer leading={!sidebarOpen} logo={false}>
                <ShowSidebarButton />
                <Button size="small" startIcon={<ArrowBackIcon />} onClick={() => run({ type: 'folders' })}>
                  {t('desktop.shell.backToLocalWork', 'Local work')}
                </Button>
              </TitleBarSpacer>
            </Box>
          )}
          <Box sx={{ flex: 1, minHeight: 0, overflow: 'auto', position: 'relative' }}>{children}</Box>
        </Box>
        <CommandPalette items={items} run={run} />
        <ShellNotice />
      </Box>
    </NativeLook>
  );
}
