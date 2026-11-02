/**
 * The desktop's left sidebar: the notifications bell and the hide button in
 * the title bar, search (the command palette), the project switcher
 * (`ProjectSwitcher.tsx`) — everything under it belongs to that project —
 * then "Local work", the project's opened folders, expandable to the threads
 * run in them (`LocalWorkSection.tsx`), the Chats with their folders, then
 * Applications, Artifacts and the "Workshop" group (agents, pipelines,
 * skills, toolkits and MCPs) — the Chats caption, the Chats list and the
 * place rows are the web sidebar's own (`widgets/chat-rail`), each place
 * with its "+" — and the account menu at the bottom (`widgets/account-menu`),
 * which also carries Credentials, Catalog and help.
 *
 * A folder belongs to one project and shows under it; moving it to another
 * asks first. A folder with no project yet shows under every project.
 */
import { useMemo, useState } from 'react';

import SearchIcon from '@mui/icons-material/Search';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { ChatsCaption, ChatsSection, NavRows, SectionCaption, useChatsSectionOpen, useStartChat } from '@/widgets/chat-rail';
import { AccountMenu } from '@/widgets/account-menu';
import { computeIsSelectedProjectPublic, NotificationBell, useCreateGate } from '@/widgets/sidebar';

import { ACCOUNT_MENU_ITEMS, eliteaItems, selectedEliteaItem, type EliteaItem, type EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useContextMenu } from '../model/useContextMenu';
import { useDesktopBell } from '../model/useDesktopNotifications';
import type { ShellLocation } from '../model/useShellLocation';
import { useSwitchProject } from '../model/useSwitchProject';
import { LocalWorkSection } from './LocalWorkSection';
import { ProjectSwitcher } from './ProjectSwitcher';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

export interface DesktopSidebarProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
}

/** Which Elitea row is highlighted: on a conversation (or the archived page) its own row is, not Chats. */
function highlightedItem(pathname: string, selected: EliteaItemValue | undefined): EliteaItemValue | undefined {
  if (selected === 'chat' && pathname !== '/chat' && pathname !== '/chat/') return undefined;
  return selected;
}

function notice(message: string): void {
  useDesktopLayout.getState().setNotice(message);
}

/** The title bar's trailing controls: the notifications bell, then hide the sidebar. */
function TitleBarActions({ run }: { readonly run: (action: ShellAction) => void }): React.JSX.Element {
  const bell = useDesktopBell();
  const hide = t('desktop.shell.hideSidebar', 'Hide sidebar');
  return (
    <>
      <NotificationBell projectId={bell.projectId} hasUnread={bell.hasUnread} placement="below" />
      <Tooltip title={`${hide} (${modKey()}\\)`}>
        <IconButton size="small" aria-label={hide} onClick={() => run({ type: 'toggle_sidebar' })}>
          <ViewSidebarOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
    </>
  );
}

function SearchPill({ run }: { readonly run: (action: ShellAction) => void }): React.JSX.Element {
  return (
    <ButtonBase
      onClick={() => run({ type: 'command_palette' })}
      sx={(theme: Theme) => ({
        width: '100%',
        justifyContent: 'flex-start',
        gap: 1,
        paddingX: 1,
        paddingY: 0.5,
        borderRadius: theme.vars.shape.radiusSm,
        border: `1px solid ${theme.vars.palette.divider}`,
        color: theme.vars.palette.text.metrics,
        '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
      })}
    >
      <SearchIcon sx={{ width: '1rem', height: '1rem' }} />
      <Typography variant="labelSmall" component="span" sx={{ flex: 1, textAlign: 'left' }}>
        {t('desktop.shell.search', 'Search')}
      </Typography>
      <Typography variant="bodySmall" component="kbd" sx={{ fontFamily: 'inherit' }}>
        {`${modKey()}K`}
      </Typography>
    </ButtonBase>
  );
}

/** The place rows, each with its "+" where the person may create one in this project. */
function usePlaceRows(items: readonly EliteaItem[], permissions: ReadonlySet<string>, projectId: string | undefined, run: (action: ShellAction) => void) {
  const mayCreate = useCreateGate(permissions, projectId);
  return items.map((item) => ({
    value: item.value,
    label: item.label,
    url: item.url,
    create:
      item.create !== undefined && mayCreate(item.value)
        ? { label: item.create.label, testId: `shell-create-${item.value}`, onCreate: () => run({ type: 'go', to: item.create?.url ?? item.url }) }
        : undefined,
  }));
}

export function DesktopSidebar(props: DesktopSidebarProps): React.JSX.Element {
  const { location, run, permissions, selectedProjectId } = props;
  const switchProject = useSwitchProject(location.pathname, props.onSelectProject);
  const newChat = useStartChat();
  const showMenu = useContextMenu();
  const items = useMemo(
    () => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)),
    [permissions, selectedProjectId],
  );
  const sidebarItems = items.filter((item) => !ACCOUNT_MENU_ITEMS.has(item.value));
  const places = usePlaceRows(
    sidebarItems.filter((item) => item.value !== 'chat'),
    permissions,
    selectedProjectId,
    run,
  );
  const mayCreate = useCreateGate(permissions, selectedProjectId);
  const selected = selectedEliteaItem(location.pathname, sidebarItems);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  const [chatsOpen, toggleChats, expandChats] = useChatsSectionOpen(location.pathname);
  const hasChat = sidebarItems.some((item) => item.value === 'chat');
  const [creatingFolder, setCreatingFolder] = useState(false);
  const onNewFolder = permissions.has(PERMISSIONS.chat.folders.create)
    ? () => {
        expandChats();
        setCreatingFolder(true);
      }
    : undefined;

  return (
    <Box
      component="aside"
      data-chrome
      data-testid="desktop-sidebar"
      sx={(theme: Theme) => ({
        width: '16rem',
        flexShrink: 0,
        height: '100vh',
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
        borderRight: `1px solid ${theme.vars.palette.divider}`,
        position: 'relative',
        isolation: 'isolate',
        // Over the window's native material: a tint of the secondary surface strong
        // enough for AA text on any backdrop (`lib/sidebarTint.ts`); else that surface.
        '&::before': {
          content: '""',
          position: 'absolute',
          inset: 0,
          zIndex: -1,
          background: theme.vars.palette.background.secondary,
          opacity: vibrancy ? SIDEBAR_TINT_OPACITY : 1,
        },
      })}
    >
      <TitleBarSpacer leading trailing={<TitleBarActions run={run} />} />
      {/* One rhythm down the sidebar: each block is a caption (12px above, 2px below) and its rows; no block adds a gap of its own. */}
      <Box sx={{ paddingX: 1 }}>
        <SearchPill run={run} />
      </Box>
      <Box component="section" aria-label={t('desktop.shell.project', 'Project')} sx={{ paddingX: 1 }}>
        <SectionCaption>{t('desktop.shell.project', 'Project')}</SectionCaption>
        <ProjectSwitcher projects={props.projects} selectedProjectId={selectedProjectId} onSelectProject={switchProject} />
      </Box>
      <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', paddingX: 1 }}>
        <LocalWorkSection location={location} run={run} projects={props.projects} projectId={selectedProjectId} />
        <Box component="nav" aria-label={t('desktop.shell.elitea', 'Elitea')}>
          {hasChat && (
            <>
              <ChatsCaption open={chatsOpen} onToggle={toggleChats} onExpand={expandChats} onNewFolder={onNewFolder} onNewChat={mayCreate('chat') ? newChat : undefined} />
              {chatsOpen && (
                <ChatsSection
                  projectId={selectedProjectId}
                  pathname={location.pathname}
                  creatingFolder={creatingFolder}
                  onCreatingFolderDone={() => setCreatingFolder(false)}
                  canArchive={permissions.has(PERMISSIONS.chat.archive)}
                  canMoveToFolder={permissions.has(PERMISSIONS.chat.folders.update)}
                  canDeleteFolder={permissions.has(PERMISSIONS.chat.folders.delete)}
                  onError={notice}
                  onNativeContextMenu={showMenu}
                />
              )}
            </>
          )}
          <Box component="ul" sx={{ margin: 0, padding: 0, paddingTop: 1.5, listStyle: 'none' }}>
            <NavRows items={places} selected={highlightedItem(location.pathname, selected)} go={(to) => run({ type: 'go', to })} />
          </Box>
        </Box>
      </Box>
      <Box sx={(theme: Theme) => ({ paddingX: 1, paddingY: 0.5, borderTop: `1px solid ${theme.vars.palette.divider}` })}>
        <AccountMenu
          items={items.filter((item) => ACCOUNT_MENU_ITEMS.has(item.value))}
          go={(to) => run({ type: 'go', to })}
          platform="desktop"
          settingsShortcut={`${modKey()},`}
          selected={location.pathname.startsWith('/settings') || ACCOUNT_MENU_ITEMS.has(selectedEliteaItem(location.pathname, items) ?? 'chat')}
        />
      </Box>
    </Box>
  );
}
