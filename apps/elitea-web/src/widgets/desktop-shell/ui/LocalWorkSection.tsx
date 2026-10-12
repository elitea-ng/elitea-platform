/**
 * The sidebar's "Local work" section: each opened folder (workspace) on one
 * line — its name, its bound project as quiet trailing text that gives way to
 * the row's actions on hover — expandable to the threads run in it, which
 * line up under the folder's name. The section header carries "Open folder";
 * with no folder yet, an "Open a folder" row stands in as the empty state.
 */
import { useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { readThreads, threadsQueryKey, useWorkspaceIpc, type WorkspaceThread } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import { WORKSPACE_LIST_KEY } from '../model/useShellActions';
import type { ShellLocation } from '../model/useShellLocation';
import { CHILD_INDENT, ShellRow, SectionCaption } from './rows';
import { modKey } from './shortcutLabel';

const THREADS_SHOWN = 8;

interface SectionProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  projects: readonly Project[];
}

function FolderThreads({ workspace, location, run }: { workspace: Workspace } & Omit<SectionProps, 'projects'>): React.JSX.Element {
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
            indent={CHILD_INDENT}
            label={thread.title === '' ? t('desktop.shell.untitledThread', 'Untitled thread') : thread.title}
            selected={here && location.conversationId === thread.id}
            onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id, conversationId: thread.id })}
          />
        </li>
      ))}
      {list.length > THREADS_SHOWN && (
        <li>
          <ShellRow
            indent={CHILD_INDENT}
            muted
            label={all ? t('desktop.shell.showFewer', 'Show fewer') : t('desktop.shell.showAll', 'Show all ({{n}})', { n: list.length })}
            onClick={() => setAll((value) => !value)}
          />
        </li>
      )}
      {list.length === 0 && (
        <li>
          <ShellRow
            indent={CHILD_INDENT}
            muted={!(here && location.conversationId === '')}
            label={t('desktop.shell.newThread', 'New thread')}
            selected={here && location.conversationId === ''}
            onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id })}
          />
        </li>
      )}
    </Box>
  );
}

function FolderMenu({ workspace, projects, run }: { workspace: Workspace } & Omit<SectionProps, 'location'>): React.JSX.Element {
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

export function LocalWorkSection({ location, run, projects }: SectionProps): React.JSX.Element {
  const ipc = useWorkspaceIpc();
  const list = useQuery({ queryKey: WORKSPACE_LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const workspaces = list.data ?? [];
  const projectName = (id: number | null): string =>
    id === null ? t('desktop.shell.noProject', 'No project') : (projects.find((p) => p.id === id)?.name ?? '');
  const openFolder = (): void => run({ type: 'open_folder' });

  return (
    <Box component="nav" aria-label={t('desktop.shell.localWork', 'Local work')}>
      <SectionCaption
        action={
          <Tooltip title={`${t('desktop.shell.openFolder', 'Open folder')} (${modKey()}O)`}>
            <IconButton size="small" aria-label={t('desktop.shell.openFolder', 'Open folder')} onClick={openFolder}>
              <CreateNewFolderOutlinedIcon fontSize="inherit" />
            </IconButton>
          </Tooltip>
        }
      >
        {t('desktop.shell.localWork', 'Local work')}
      </SectionCaption>
      <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
        {workspaces.map((workspace) => {
          const open = !collapsed.has(workspace.id);
          const project = projectName(workspace.project_id);
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
                meta={project}
                title={`${workspace.path}\n${t('desktop.shell.folderProject', 'Project: {{name}}', { name: project })}`}
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
      {(list.isSuccess || ipc === undefined) && workspaces.length === 0 && (
        <ShellRow icon={<AddOutlinedIcon />} muted label={t('desktop.shell.openFirstFolder', 'Open a folder to work in')} onClick={openFolder} />
      )}
    </Box>
  );
}
