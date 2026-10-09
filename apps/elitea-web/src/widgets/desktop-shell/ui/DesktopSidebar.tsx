/**
 * The desktop's left sidebar, workspace-first: search (the command palette),
 * FOLDERS — each opened workspace, expandable to the threads run in it —
 * "Open folder", then a compact "Elitea" list that opens the web features in
 * the main area, and Settings at the bottom.
 *
 * Each folder carries its project binding (its menu re-binds it), so there is
 * no global project switcher over the folders; the project the Elitea pages
 * work in is a quiet caption on that section.
 */
import { useMemo, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import AppsOutlinedIcon from '@mui/icons-material/AppsOutlined';
import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ChatBubbleOutlineOutlinedIcon from '@mui/icons-material/ChatBubbleOutlineOutlined';
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import Inventory2OutlinedIcon from '@mui/icons-material/Inventory2Outlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import SearchIcon from '@mui/icons-material/Search';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import StorefrontOutlinedIcon from '@mui/icons-material/StorefrontOutlined';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { readThreads, threadsQueryKey, useWorkspaceIpc, type WorkspaceThread } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { eliteaItems, selectedEliteaItem, type EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { WORKSPACE_LIST_KEY } from '../model/useShellActions';
import type { ShellLocation } from '../model/useShellLocation';
import { ShellRow, SectionCaption } from './rows';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

const ITEM_ICONS: Record<EliteaItemValue, React.JSX.Element> = {
  chat: <ChatBubbleOutlineOutlinedIcon />,
  agents: <SmartToyOutlinedIcon />,
  pipelines: <AccountTreeOutlinedIcon />,
  skills: <AutoAwesomeOutlinedIcon />,
  toolkits: <BuildOutlinedIcon />,
  mcps: <HubOutlinedIcon />,
  credentials: <KeyOutlinedIcon />,
  applications: <AppsOutlinedIcon />,
  artifacts: <Inventory2OutlinedIcon />,
  catalog: <StorefrontOutlinedIcon />,
  help: <HelpOutlineOutlinedIcon />,
};

export interface DesktopSidebarProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
}

const THREADS_SHOWN = 8;

function FolderThreads({
  workspace,
  location,
  run,
}: {
  workspace: Workspace;
  location: ShellLocation;
  run: (action: ShellAction) => void;
}): React.JSX.Element {
  const threads = useQuery({ queryKey: threadsQueryKey(workspace.id), queryFn: () => readThreads(workspace.id) });
  const [all, setAll] = useState(false);
  const list: WorkspaceThread[] = threads.data ?? [];
  const shown = all ? list : list.slice(0, THREADS_SHOWN);
  const here = location.workspaceId === workspace.id;
  return (
    <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }} aria-label={t('desktop.shell.threadsOf', 'Threads in {{name}}', { name: workspace.name })}>
      {shown.map((thread) => (
        <li key={thread.id}>
          <ShellRow
            testId="shell-thread"
            indent={2.5}
            label={thread.title === '' ? t('desktop.shell.untitledThread', 'Untitled thread') : thread.title}
            selected={here && location.conversationId === thread.id}
            onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id, conversationId: thread.id })}
          />
        </li>
      ))}
      {list.length > THREADS_SHOWN && (
        <li>
          <ShellRow
            indent={2.5}
            label={all ? t('desktop.shell.showFewer', 'Show fewer') : t('desktop.shell.showAll', 'Show all ({{n}})', { n: list.length })}
            onClick={() => setAll((value) => !value)}
          />
        </li>
      )}
      {list.length === 0 && (
        <li>
          <ShellRow
            indent={2.5}
            label={t('desktop.shell.newThread', 'New thread')}
            selected={here && location.conversationId === ''}
            onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id })}
          />
        </li>
      )}
    </Box>
  );
}

function FolderMenu({
  workspace,
  projects,
  run,
}: {
  workspace: Workspace;
  projects: readonly Project[];
  run: (action: ShellAction) => void;
}): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const ipc = useWorkspaceIpc();
  const queryClient = useQueryClient();
  const refresh = (): Promise<void> => queryClient.invalidateQueries({ queryKey: WORKSPACE_LIST_KEY });
  const bind = useMutation({ mutationFn: (projectId: number) => ipc?.bindProject(workspace.id, projectId) ?? Promise.resolve(), onSettled: refresh });
  const remove = useMutation({ mutationFn: () => ipc?.remove(workspace.id) ?? Promise.resolve(), onSettled: refresh });
  const close = (): void => setAnchor(null);
  return (
    <>
      <Tooltip title={t('desktop.shell.newThreadIn', 'New thread in {{name}}', { name: workspace.name })}>
        <IconButton
          size="small"
          aria-label={t('desktop.shell.newThreadIn', 'New thread in {{name}}', { name: workspace.name })}
          onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id })}
        >
          <AddOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <IconButton
        size="small"
        aria-label={t('desktop.shell.folderMenu', 'More for {{name}}', { name: workspace.name })}
        aria-haspopup="menu"
        onClick={(event) => setAnchor(event.currentTarget)}
      >
        <MoreHorizIcon fontSize="inherit" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true } }}>
        <ListSubheader>{t('desktop.shell.bindProject', 'Project for this folder')}</ListSubheader>
        {projects
          .filter((project) => !project.suspended)
          .map((project) => (
            <MenuItem
              key={project.id}
              selected={workspace.project_id === project.id}
              onClick={() => {
                close();
                if (workspace.project_id !== project.id) bind.mutate(project.id);
              }}
            >
              <Box component="span" sx={{ width: '1.25rem', display: 'inline-flex' }}>
                {workspace.project_id === project.id && <CheckOutlinedIcon fontSize="inherit" />}
              </Box>
              {project.name}
            </MenuItem>
          ))}
        <Divider />
        <MenuItem
          onClick={() => {
            close();
            remove.mutate();
          }}
        >
          {t('desktop.shell.removeFolder', 'Remove from sidebar')}
        </MenuItem>
      </Menu>
    </>
  );
}

function Folders({ location, run, projects }: Pick<DesktopSidebarProps, 'location' | 'run' | 'projects'>): React.JSX.Element {
  const ipc = useWorkspaceIpc();
  const list = useQuery({ queryKey: WORKSPACE_LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const workspaces = list.data ?? [];
  const projectName = (id: number | null): string | undefined =>
    id === null ? t('desktop.shell.noProject', 'No project') : projects.find((p) => p.id === id)?.name;

  return (
    <Box component="nav" aria-label={t('desktop.shell.folders', 'Folders')}>
      <SectionCaption
        action={
          <Tooltip title={`${t('desktop.shell.openFolder', 'Open folder')} (${modKey()}O)`}>
            <IconButton size="small" aria-label={t('desktop.shell.openFolder', 'Open folder')} onClick={() => run({ type: 'open_folder' })}>
              <CreateNewFolderOutlinedIcon fontSize="inherit" />
            </IconButton>
          </Tooltip>
        }
      >
        {t('desktop.shell.folders', 'Folders')}
      </SectionCaption>
      <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
        {workspaces.map((workspace) => {
          const open = !collapsed.has(workspace.id);
          const toggle = (): void =>
            setCollapsed((current) => {
              const next = new Set(current);
              if (next.has(workspace.id)) next.delete(workspace.id);
              else next.add(workspace.id);
              return next;
            });
          return (
            <li key={workspace.id} data-testid="shell-folder">
              <ShellRow
                label={workspace.name}
                caption={projectName(workspace.project_id)}
                icon={open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
                ariaExpanded={open}
                selected={location.workspaceId === workspace.id && !open}
                onClick={toggle}
                trailingOnHover
                trailing={<FolderMenu workspace={workspace} projects={projects} run={run} />}
              />
              {open && <FolderThreads workspace={workspace} location={location} run={run} />}
            </li>
          );
        })}
      </Box>
      <ShellRow icon={<AddOutlinedIcon />} label={t('desktop.shell.openFolder', 'Open folder')} onClick={() => run({ type: 'open_folder' })} />
    </Box>
  );
}

function ProjectCaption({
  projects,
  selectedProjectId,
  onSelectProject,
}: Pick<DesktopSidebarProps, 'projects' | 'selectedProjectId' | 'onSelectProject'>): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const current = projects.find((p) => String(p.id) === selectedProjectId);
  return (
    <>
      <ButtonBase
        aria-haspopup="menu"
        aria-label={t('desktop.shell.eliteaProject', 'Project: {{name}}', { name: current?.name ?? '' })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={(theme: Theme) => ({ borderRadius: theme.vars.shape.radiusSm, paddingX: 0.5, color: theme.vars.palette.text.metrics })}
      >
        <Typography variant="bodySmall" component="span" sx={{ maxWidth: '8rem', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current?.name ?? ''}
        </Typography>
        <ExpandMoreIcon sx={{ width: '0.875rem', height: '0.875rem' }} />
      </ButtonBase>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} slotProps={{ list: { dense: true } }}>
        {projects
          .filter((project) => !project.suspended)
          .map((project) => (
            <MenuItem
              key={project.id}
              selected={String(project.id) === selectedProjectId}
              onClick={() => {
                setAnchor(null);
                onSelectProject(String(project.id), project.name);
              }}
            >
              {project.name}
            </MenuItem>
          ))}
      </Menu>
    </>
  );
}

export function DesktopSidebar(props: DesktopSidebarProps): React.JSX.Element {
  const { location, run, permissions, selectedProjectId } = props;
  const items = useMemo(
    () => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)),
    [permissions, selectedProjectId],
  );
  const selected = selectedEliteaItem(location.pathname, items);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);

  return (
    <Box
      component="aside"
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
      <Box sx={{ paddingX: 1, paddingBottom: 0.5 }}>
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
      <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', paddingX: 1 }}>
        <Folders location={location} run={run} projects={props.projects} />
        <Box component="nav" aria-label={t('desktop.shell.elitea', 'Elitea')}>
          <SectionCaption
            action={<ProjectCaption projects={props.projects} selectedProjectId={selectedProjectId} onSelectProject={props.onSelectProject} />}
          >
            {t('desktop.shell.elitea', 'Elitea')}
          </SectionCaption>
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            {items.map((item) => (
              <li key={item.value}>
                <ShellRow
                  testId={`shell-elitea-${item.value}`}
                  icon={ITEM_ICONS[item.value]}
                  label={item.label}
                  selected={selected === item.value}
                  onClick={() => run({ type: 'go', to: item.url })}
                />
              </li>
            ))}
          </Box>
        </Box>
      </Box>
      <Box sx={(theme: Theme) => ({ paddingX: 1, paddingY: 0.5, borderTop: `1px solid ${theme.vars.palette.divider}` })}>
        <ShellRow
          icon={<SettingsOutlinedIcon />}
          label={t('desktop.shell.settings', 'Settings')}
          selected={location.pathname.startsWith('/settings')}
          onClick={() => run({ type: 'settings' })}
        />
      </Box>
    </Box>
  );
}
