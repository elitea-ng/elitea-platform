/**
 * The desktop app's frame (ADR-0029, workspace-first): the folders sidebar on
 * the left, the page in the middle, the command palette over both. It
 * replaces the web shell's sidebar in the `desktop` build only — `AppShell`
 * renders it from a lazy import the web build drops.
 *
 * Workspace pages draw their own header (folder, project, agent) and the
 * changes panel; every other page (the Elitea features, Settings) gets a slim
 * title row with the way back to the folders. A page with an inspector (the
 * chat's participants) fills the frame's full-height right pane through
 * `shared/desktop/inspectorSlot` (`InspectorPane.tsx`). The sidebar's Chats
 * list tells the chat page that it is on screen (`shared/desktop/sidebarChatList`),
 * so the page shows its own list only while the sidebar's is gone.
 */
import { useEffect, useMemo, type ReactNode } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { SidebarChatListProvider } from '@/shared/desktop/sidebarChatList';
import { t } from '@/shared/i18n';
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { eliteaItems } from '../lib/eliteaSections';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useAppIpc } from '../model/appIpcContext';
import { useActivityFeedSources } from '../model/useActivityFeedSources';
import { useHostIntegration } from '../model/useHostIntegration';
import { useNativeWindowTheme } from '../model/useNativeWindowTheme';
import { useRouteLogging } from '../model/useRouteLogging';
import { useShellActions } from '../model/useShellActions';
import { useShellLocation } from '../model/useShellLocation';
import { CommandPalette } from './CommandPalette';
import { DesktopSidebar } from './DesktopSidebar';
import { InspectorPane, InspectorSlotScope, InspectorToggle, useInspectorHost } from './InspectorPane';
import { NativeLook } from './NativeLook';
import { BannerStack, NoticeBanner } from './NoticeBanner';
import { ReclaimRequestToast } from './ReclaimRequestToast';
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

function ShellNotice(): React.JSX.Element | null {
  const notice = useDesktopLayout((state) => state.notice);
  const close = (): void => useDesktopLayout.getState().setNotice(null);
  // Fades itself after a while, like a status message.
  useEffect(() => {
    if (notice === null) return undefined;
    const timer = window.setTimeout(close, NOTICE_MS);
    return () => window.clearTimeout(timer);
  }, [notice]);
  if (notice === null) return null;
  return (
    <NoticeBanner tone="warning" testId="shell-notice" onClose={close}>
      {notice}
    </NoticeBanner>
  );
}

const NOTICE_MS = 8000;

export function DesktopFrame({ children, permissions, projects, selectedProjectId, onSelectProject }: DesktopFrameProps): React.JSX.Element {
  const run = useShellActions();
  const ipc = useAppIpc();
  useHostIntegration(run, ipc);
  useNativeWindowTheme(ipc);
  useRouteLogging();
  useActivityFeedSources();
  const location = useShellLocation();
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  const items = useMemo(() => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)), [permissions, selectedProjectId]);
  const inspector = useInspectorHost();

  return (
    <NativeLook>
      <SidebarChatListProvider>
        <InspectorSlotScope host={inspector}>
          {/* `clip`, not `hidden`: a hidden box can still be scrolled by code (a pane's scrollIntoView), which shifted the whole window. */}
          <Box data-testid="desktop-frame" sx={{ display: 'flex', height: '100vh', overflow: 'clip' }}>
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
                position: 'relative',
              })}
            >
              {!location.inFolders && (
                <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
                  <TitleBarSpacer leading={!sidebarOpen} logo={false} trailing={<InspectorToggle host={inspector} />}>
                    <ShowSidebarButton />
                    <Button size="small" startIcon={<ArrowBackIcon />} onClick={() => run({ type: 'folders' })}>
                      {t('desktop.shell.backToLocalWork', 'Local work')}
                    </Button>
                  </TitleBarSpacer>
                </Box>
              )}
              <Box sx={{ flex: 1, minHeight: 0, overflow: 'auto', position: 'relative' }}>{children}</Box>
              <BannerStack>
                <ShellNotice />
                <ReclaimRequestToast run={run} />
              </BannerStack>
            </Box>
            <InspectorPane host={inspector} />
            <CommandPalette items={items} run={run} />
          </Box>
        </InspectorSlotScope>
      </SidebarChatListProvider>
    </NativeLook>
  );
}
