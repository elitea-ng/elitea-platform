/**
 * The desktop's left sidebar: search (the command palette), the project
 * switcher (`ProjectSwitcher.tsx`) — everything under it belongs to that
 * project — then "Local work", the project's opened folders, expandable to
 * the threads run in them (`LocalWorkSection.tsx`), the Chats with their
 * folders, then Applications, Artifacts and the "Workshop" group (agents,
 * pipelines, skills, toolkits and MCPs, `WorkshopGroup.tsx`), and the account menu at the
 * bottom (`AccountMenu.tsx`), which also carries Credentials, Catalog and help.
 *
 * A folder belongs to one project and shows under it; moving it to another
 * asks first. A folder with no project yet shows under every project.
 */
import { useEffect, useMemo, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';
import AppsOutlinedIcon from '@mui/icons-material/AppsOutlined';
import ChatBubbleOutlineOutlinedIcon from '@mui/icons-material/ChatBubbleOutlineOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Inventory2OutlinedIcon from '@mui/icons-material/Inventory2Outlined';
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
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { ACCOUNT_MENU_ITEMS, WORKSHOP_ITEMS, eliteaItems, selectedEliteaItem, type EliteaItem, type EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useChatsSectionOpen } from '../model/useEliteaSectionOpen';
import { useSwitchProject } from '../model/useSwitchProject';
import { AccountMenu } from './AccountMenu';
import { ChatsSection } from './ChatsSection';
import type { ShellLocation } from '../model/useShellLocation';
import { LocalWorkSection } from './LocalWorkSection';
import { ShellRow, SectionCaption } from './rows';
import { ProjectSwitcher } from './ProjectSwitcher';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';
import { WORKSHOP_ICONS, WorkshopGroup } from './WorkshopGroup';

const ITEM_ICONS: Partial<Record<EliteaItemValue, React.JSX.Element>> = {
  ...WORKSHOP_ICONS,
  chat: <ChatBubbleOutlineOutlinedIcon />,
  applications: <AppsOutlinedIcon />,
  artifacts: <Inventory2OutlinedIcon />,
};

export interface DesktopSidebarProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
}

interface ChatsRowActionsProps {
  readonly open: boolean;
  readonly onToggle: () => void;
  /** Absent without the folder create permission. */
  readonly onNewFolder?: (() => void) | undefined;
}

/** The Chats caption's actions: a new chat, a new folder, and the disclosure of the conversations under it. */
/** A new chat, even on `/chat` itself: `create=1` (`processes/chat/ui/useCreateChatReset.ts`). */
function useStartChat(): () => void {
  const navigate = useNavigate();
  return () => void navigate({ to: '/chat', search: { create: '1' } as never });
}

function ChatsRowActions({ open, onToggle, onNewFolder }: ChatsRowActionsProps): React.JSX.Element {
  const startChat = useStartChat();
  const newChat = t('desktop.chats.new', 'New chat');
  const newFolder = t('desktop.chats.newFolder', 'New folder');
  const disclosure = open ? t('desktop.chats.hide', 'Hide chats') : t('desktop.chats.show', 'Show chats');
  return (
    <>
      {onNewFolder !== undefined && (
        <Tooltip title={newFolder}>
          <IconButton size="small" aria-label={newFolder} data-testid="shell-chats-new-folder" onClick={onNewFolder}>
            <CreateNewFolderOutlinedIcon fontSize="inherit" />
          </IconButton>
        </Tooltip>
      )}
      <Tooltip title={newChat}>
        <IconButton size="small" aria-label={newChat} data-testid="shell-chats-new" onClick={startChat}>
          <AddOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <Tooltip title={disclosure}>
        <IconButton size="small" aria-label={disclosure} aria-expanded={open} data-testid="shell-chats-toggle" onClick={onToggle}>
          {open ? <ExpandMoreIcon fontSize="inherit" /> : <ChevronRightIcon fontSize="inherit" />}
        </IconButton>
      </Tooltip>
    </>
  );
}

/** The chat routes: `/chat`, a conversation, the archived chats page. */
function isChatPath(pathname: string): boolean {
  return pathname === '/chat' || pathname.startsWith('/chat/');
}

/**
 * Opening the chat expands the Chats disclosure, so its conversations are on screen. It runs on each navigation to a chat route, not
 * on every render: a person who collapses them on the chat page keeps them
 * collapsed until the next navigation.
 */
function useExpandOnChat(pathname: string, expandChats: () => void): void {
  const onChat = isChatPath(pathname);
  useEffect(() => {
    if (onChat) expandChats();
  }, [onChat, pathname, expandChats]);
}

function isWorkshopItem(item: EliteaItem): boolean {
  return WORKSHOP_ITEMS.has(item.value);
}

/** Which Elitea row is highlighted: on a conversation (or the archived page) its own row is, not Chats. */
function highlightedItem(pathname: string, selected: EliteaItemValue | undefined): EliteaItemValue | undefined {
  if (selected === 'chat' && pathname !== '/chat' && pathname !== '/chat/') return undefined;
  return selected;
}

export function DesktopSidebar(props: DesktopSidebarProps): React.JSX.Element {
  const { location, run, permissions, selectedProjectId } = props;
  const switchProject = useSwitchProject(location.pathname, props.onSelectProject);
  const newChat = useStartChat();
  const items = useMemo(
    () => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)),
    [permissions, selectedProjectId],
  );
  const sidebarItems = items.filter((item) => !ACCOUNT_MENU_ITEMS.has(item.value));
  const topItems = sidebarItems.filter((item) => !isWorkshopItem(item));
  const selected = selectedEliteaItem(location.pathname, sidebarItems);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  const [chatsOpen, toggleChats, expandChats] = useChatsSectionOpen();
  useExpandOnChat(location.pathname, expandChats);
  const hasChat = topItems.some((item) => item.value === 'chat');
  const [creatingFolder, setCreatingFolder] = useState(false);
  const onNewFolder = permissions.has(PERMISSIONS.chat.folders.create)
    ? () => {
        expandChats();
        setCreatingFolder(true);
      }
    : undefined;
  const highlighted = highlightedItem(location.pathname, selected);

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
      <TitleBarSpacer
        leading
        trailing={
          <Tooltip title={`${t('desktop.shell.hideSidebar', 'Hide sidebar')} (${modKey()}\\)`}>
            <IconButton size="small" aria-label={t('desktop.shell.hideSidebar', 'Hide sidebar')} onClick={() => run({ type: 'toggle_sidebar' })}>
              <ViewSidebarOutlinedIcon fontSize="inherit" />
            </IconButton>
          </Tooltip>
        }
      />
      {/* One rhythm down the sidebar: each block is a caption (12px above, 2px below) and its rows; no block adds a gap of its own. */}
      <Box sx={{ paddingX: 1 }}>
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
              <SectionCaption onOpen={newChat} testId="shell-chats-caption" action={<ChatsRowActions open={chatsOpen} onToggle={toggleChats} onNewFolder={onNewFolder} />}>
                {t('widgets.sidebar.nav.chat', 'Chats')}
              </SectionCaption>
              {chatsOpen && (
                <ChatsSection projectId={selectedProjectId} pathname={location.pathname} creatingFolder={creatingFolder} onCreatingFolderDone={() => setCreatingFolder(false)} canArchive={permissions.has(PERMISSIONS.chat.archive)} canMoveToFolder={permissions.has(PERMISSIONS.chat.folders.update)} />
              )}
            </>
          )}
          <Box component="ul" sx={{ margin: 0, padding: 0, paddingTop: 1.5, listStyle: 'none' }}>
            {topItems
              .filter((item) => item.value !== 'chat')
              .map((item) => (
                <li key={item.value}>
                  <ShellRow testId={`shell-elitea-${item.value}`} icon={ITEM_ICONS[item.value]} label={item.label} selected={highlighted === item.value} onClick={() => run({ type: 'go', to: item.url })} />
                </li>
              ))}
            <WorkshopGroup items={sidebarItems.filter(isWorkshopItem)} selected={highlighted} run={run} />
          </Box>
        </Box>
      </Box>
      <Box sx={(theme: Theme) => ({ paddingX: 1, paddingY: 0.5, borderTop: `1px solid ${theme.vars.palette.divider}` })}>
        <AccountMenu
          items={items.filter((item) => ACCOUNT_MENU_ITEMS.has(item.value))}
          run={run}
          selected={location.pathname.startsWith('/settings') || ACCOUNT_MENU_ITEMS.has(selectedEliteaItem(location.pathname, items) ?? 'chat')}
        />
      </Box>
    </Box>
  );
}
