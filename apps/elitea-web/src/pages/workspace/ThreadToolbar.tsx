/**
 * The thread's toolbar, a native unified title bar: sidebar toggle, the title
 * (folder ▸ thread, a button that opens the folder's path card), an optional
 * centre slot (the activity island), and at the trailing edge the inspector
 * toggle and a ⋯ menu. Everything else that used to sit here lives where a
 * native app keeps it: the agent, mode and run location in the composer's
 * footer; the index in the inspector; the rest in the Thread and Folder menus.
 */
import { useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import FolderOpenOutlinedIcon from '@mui/icons-material/FolderOpenOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Popover from '@mui/material/Popover';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import { readThreads, type ProjectChoice } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { IndexIdleLine } from '@/features/workspace-index';
import { ActivityIsland, conversationLink, copyText as copy, ShowSidebarButton, TitleBarSpacer, useAppIpc, useDesktopLayout } from '@/widgets/desktop-shell';

import { PanelToggle } from './sessionParts';

export interface ThreadToolbarProps {
  workspace: Workspace;
  conversationId: string;
  projectId: number;
  projects: readonly ProjectChoice[];
  /** A turn runs: the folder cannot move to another project now. */
  busy: boolean;
  onChangeProject: (projectId: number) => void;
  /** The path card is opened from elsewhere too (`NoAgents`' "Use another project"). */
  pathOpen: boolean;
  onPathOpenChange: (open: boolean) => void;
  /** Items the inspector's badge counts. */
  attention: number;
  onIndexSettings?: (() => void) | undefined;
  /** ⋯ ▸ Fork Thread: the whole thread into a new one in this folder. Absent: not offered. */
  onFork?: (() => void) | undefined;
}

function PathCard({ workspace, projectId, projects, busy, onChangeProject, onClose }: Pick<ThreadToolbarProps, 'workspace' | 'projectId' | 'projects' | 'busy' | 'onChangeProject'> & { onClose: () => void }): React.JSX.Element {
  const appIpc = useAppIpc();
  return (
    <Box data-testid="path-card" sx={{ width: 320, padding: 1.5, display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Typography variant="headingSmall" component="p">
        {workspace.name}
      </Typography>
      <Typography
        variant="bodySmall"
        data-selectable
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, wordBreak: 'break-all' })}
      >
        {workspace.path}
      </Typography>
      <Box sx={{ display: 'flex', gap: 1 }}>
        {appIpc !== undefined && (
          <ButtonBase
            sx={(theme: Theme) => ({ ...rowButtonSx(theme), flex: 1 })}
            onClick={() => {
              void appIpc.revealPath(workspace.id, '').catch(() => undefined);
              onClose();
            }}
          >
            {t('workspace.path.reveal', 'Reveal in Finder')}
          </ButtonBase>
        )}
        <ButtonBase
          sx={(theme: Theme) => ({ ...rowButtonSx(theme), flex: 1 })}
          onClick={() => {
            copy(workspace.path);
            onClose();
          }}
        >
          {t('workspace.path.copy', 'Copy Path')}
        </ButtonBase>
      </Box>
      <Divider />
      <Typography variant="labelSmall" component="p" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.path.project', 'Project')}
      </Typography>
      <MenuList dense disablePadding aria-label={t('workspace.project', 'Project')}>
        {projects.map((project) => (
          <MenuItem
            key={project.id}
            role="menuitemradio"
            aria-checked={project.id === projectId}
            disabled={busy}
            onClick={() => {
              if (project.id !== projectId) onChangeProject(project.id);
              onClose();
            }}
          >
            <Box component="span" sx={{ width: 16, display: 'inline-flex' }}>
              {project.id === projectId && <CheckOutlinedIcon fontSize="inherit" />}
            </Box>
            {project.name}
          </MenuItem>
        ))}
      </MenuList>
    </Box>
  );
}

const rowButtonSx = (theme: Theme) => ({
  minHeight: 22,
  justifyContent: 'center',
  borderRadius: '6px',
  border: `1px solid ${theme.vars.palette.divider}`,
  padding: '0 10px',
  fontSize: '0.8125rem',
  fontWeight: 500,
  '&:hover': { background: theme.vars.palette.action.hover },
});

/** The title: folder ▸ thread. Clicking opens the path card. */
function ThreadTitle(props: ThreadToolbarProps): React.JSX.Element {
  const { workspace, conversationId, pathOpen, onPathOpenChange } = props;
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const thread = conversationId === '' ? undefined : readThreads(workspace.id).find((x) => x.id === conversationId);
  const title = thread?.title !== undefined && thread.title !== '' ? thread.title : t('workspace.thread.untitled', 'New thread');
  const open = anchor !== null && pathOpen;
  return (
    <>
      <ButtonBase
        data-testid="thread-title"
        aria-haspopup="dialog"
        aria-expanded={open}
        title={workspace.path}
        ref={setAnchor}
        onClick={() => onPathOpenChange(true)}
        sx={(theme: Theme) => ({
          minWidth: 0,
          maxWidth: '100%',
          gap: 0.75,
          padding: '2px 6px',
          borderRadius: '6px',
          justifyContent: 'flex-start',
          '&:hover': { background: theme.vars.palette.action.hover },
        })}
      >
        <FolderOpenOutlinedIcon sx={(theme: Theme) => ({ fontSize: 15, color: theme.vars.palette.text.metrics, flexShrink: 0 })} />
        <Typography component="h1" variant="labelMedium" sx={{ margin: 0, whiteSpace: 'nowrap', flexShrink: 0 }}>
          {workspace.name}
        </Typography>
        <ChevronRightIcon aria-hidden sx={(theme: Theme) => ({ fontSize: 14, color: theme.vars.palette.text.metrics, flexShrink: 0, marginX: -0.25 })} />
        <Typography variant="labelMedium" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}>
          {title}
        </Typography>
      </ButtonBase>
      <Popover
        open={open}
        anchorEl={anchor}
        onClose={() => onPathOpenChange(false)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        slotProps={{ paper: { sx: { borderRadius: '10px', marginTop: 0.5 } } }}
      >
        <PathCard {...props} onClose={() => onPathOpenChange(false)} />
      </Popover>
    </>
  );
}

/** Trailing ⋯: what the thread offers that has no toolbar button of its own. */
function Overflow({ conversationId, onIndexSettings, onFork, busy }: Pick<ThreadToolbarProps, 'conversationId' | 'onIndexSettings' | 'onFork' | 'busy'>): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const navigate = useNavigate();
  const link = conversationLink(conversationId);
  const close = (): void => setAnchor(null);
  return (
    <>
      <Tooltip title={t('workspace.more', 'More')}>
        <IconButton size="small" aria-label={t('workspace.more', 'More')} aria-haspopup="menu" onClick={(event) => setAnchor(event.currentTarget)}>
          <MoreHorizIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true } }}>
        <ListSubheader sx={{ lineHeight: '22px', background: 'transparent' }}>{t('workspace.thread.menu', 'Thread')}</ListSubheader>
        <MenuItem
          disabled={conversationId === ''}
          onClick={() => {
            close();
            void navigate({ to: '/chat/$conversationId', params: { conversationId } });
          }}
        >
          {t('workspace.openInChat', 'Open in chat')}
        </MenuItem>
        <MenuItem
          disabled={link === null}
          onClick={() => {
            close();
            if (link !== null) copy(link);
          }}
        >
          {t('workspace.copyLink', 'Copy Link')}
        </MenuItem>
        {onFork !== undefined && (
          <MenuItem
            disabled={conversationId === '' || busy}
            onClick={() => {
              close();
              onFork();
            }}
          >
            {t('workspace.fork.thread', 'Fork Thread')}
          </MenuItem>
        )}
        {onIndexSettings !== undefined && <Divider />}
        {onIndexSettings !== undefined && (
          <MenuItem
            onClick={() => {
              close();
              onIndexSettings();
            }}
          >
            {t('workspace.index.menu', 'Index settings…')}
          </MenuItem>
        )}
      </Menu>
    </>
  );
}

export function ThreadToolbar(props: ThreadToolbarProps): React.JSX.Element {
  const changesOpen = useDesktopLayout((state) => state.changesOpen);
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  return (
    <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
      <TitleBarSpacer
        leading={!sidebarOpen}
        logo={false}
        trailing={
          <>
            {!changesOpen && <PanelToggle count={props.attention} />}
            <Overflow conversationId={props.conversationId} onIndexSettings={props.onIndexSettings} onFork={props.onFork} busy={props.busy} />
          </>
        }
      >
        <ShowSidebarButton />
        <Box sx={{ flex: '0 1 auto', minWidth: 0 }}>
          <ThreadTitle {...props} />
        </Box>
        <Box sx={{ flex: 1, display: 'flex', justifyContent: 'center', minWidth: 0 }}>
          <IndexIdleLine workspaceId={props.workspace.id}>{(line) => <ActivityIsland idle={line} />}</IndexIdleLine>
        </Box>
      </TitleBarSpacer>
    </Box>
  );
}
