/**
 * The desktop app's frame (ADR-0029, workspace-first): the folders sidebar on
 * the left, the page in the middle, the command palette over both. It
 * replaces the web shell's sidebar in the `desktop` build only — `AppShell`
 * renders it from a lazy import the web build drops.
 *
 * Workspace pages draw their own header (folder, project, agent) and the
 * changes panel; every other page (the Elitea features, Settings) gets a slim
 * title row: the window's drag region, the name of what is open
 * (`PageTitle.tsx`: `Chats ▸ folder ▸ conversation`, or the page's name) and
 * the sidebar and inspector toggles. Settings opens as a dialog over the
 * page it was opened from (`SettingsDialog.tsx`). The sidebar is the way to everything, so
 * the row has no "back" button. A page with an inspector (the chat's
 * participants, Local work's changes, approvals and index) contributes tabs
 * to the frame's ONE full-height right panel through
 * `shared/desktop/inspectorSlot` (`InspectorPane.tsx`).
 *
 * The chat page shows no conversation list of its own in the frame: the
 * sidebar's Chats rows are the one list (a hidden sidebar shows none, as in
 * Mail). Both the sidebar and the title read ONE chat rail
 * (`model/chatRail.tsx`), while the Chats row is there.
 */
import { useEffect, useMemo, useRef, type ReactNode } from 'react';

import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { eliteaItems } from '../lib/eliteaSections';
import { ChatRailProvider } from '../model/chatRail';
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
import { InspectorPane, InspectorSlotScope, InspectorToggle, InspectorWindows, useInspectorHost, useInspectorView } from './InspectorPane';
import { NativeLook } from './NativeLook';
import { BannerStack, NoticeBanner } from './NoticeBanner';
import { PageTitle } from './PageTitle';
import { ReclaimRequestToast } from './ReclaimRequestToast';
import { PageSnapshot, SettingsDialog, usePreloadSettings, useSnapshotOnOpen, useSettingsOnScreen, useReturnAddress, type Snapshot } from './SettingsDialog';
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
  const run = useShellActions(selectedProjectId);
  const ipc = useAppIpc();
  useHostIntegration(run, ipc);
  useNativeWindowTheme(ipc);
  useRouteLogging();
  useActivityFeedSources();
  const location = useShellLocation();
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  const items = useMemo(() => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)), [permissions, selectedProjectId]);
  const inspector = useInspectorHost(ipc);
  const inspectorOpen = useDesktopLayout((state) => state.changesOpen);
  const maximized = useDesktopLayout((state) => state.inspectorMaximized);
  // A maximized inspector takes the main column's place while it is on screen; the page stays mounted under it.
  const inspectorGroups = useInspectorView(inspector);
  const inspectorShown = inspectorOpen && inspectorGroups.length > 0;
  const mainHidden = maximized && inspectorShown;
  const inSettings = useSettingsOnScreen();
  const returnTo = useReturnAddress();
  usePreloadSettings();
  const pageSnapshot = useRef<Snapshot>(null);
  const mainRef = useRef<HTMLElement>(null);
  useSnapshotOnOpen(mainRef, pageSnapshot);
  // The frame's notices show over whatever is in front: the page, or the Settings dialog.
  const banners = (
    <BannerStack>
      <ShellNotice />
      <ReclaimRequestToast run={run} />
    </BannerStack>
  );

  return (
    <NativeLook>
      <ChatRailProvider projectId={selectedProjectId} enabled={items.some((item) => item.value === 'chat')}>
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
            {inSettings ? (
              <PageSnapshot from={pageSnapshot} />
            ) : (
              <Box
                ref={mainRef}
                component="main"
                sx={(theme: Theme) => ({
                  flex: 1,
                  minWidth: 0,
                  height: '100vh',
                  display: mainHidden ? 'none' : 'flex',
                  flexDirection: 'column',
                  background: theme.vars.palette.background.default,
                  position: 'relative',
                })}
              >
                {!location.inFolders && (
                  <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
                    <TitleBarSpacer leading={!sidebarOpen} logo={false} trailing={<InspectorToggle host={inspector} />}>
                      <ShowSidebarButton />
                      <PageTitle pathname={location.pathname} items={items} projectId={selectedProjectId} />
                    </TitleBarSpacer>
                  </Box>
                )}
                <Box sx={{ flex: 1, minHeight: 0, overflow: 'auto', position: 'relative' }}>{children}</Box>
                {banners}
              </Box>
            )}
            {inSettings && <SettingsDialog returnTo={returnTo} banners={banners}>
                {children}
              </SettingsDialog>}
            <InspectorPane host={inspector} />
            <InspectorWindows host={inspector} />
            <CommandPalette items={items} run={run} />
          </Box>
        </InspectorSlotScope>
      </ChatRailProvider>
    </NativeLook>
  );
}
