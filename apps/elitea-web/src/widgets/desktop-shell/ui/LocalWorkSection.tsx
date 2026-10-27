/**
 * The sidebar's "Local work" section: the opened folders (workspaces) of the
 * project the sidebar is on, each on one line with a folder glyph (open or
 * closed), expandable to the threads run in it, which line up under the
 * folder's name. A folder with no project yet shows in every project, marked
 * "No project", until it joins one; moving a folder that has a project to
 * another one asks first (`MoveFolderDialog.tsx`). The section header carries "Open folder";
 * with no folder yet, an "Open a folder" row stands in as the empty state.
 *
 * The cloud badges of the visible thread rows (every open folder's shown
 * threads that have a known UUID) come from ONE batched
 * `handoff_status_many` read for the whole section.
 *
 * An open git folder also lists its parallel tasks (task worktrees) under its
 * threads, each drawn as a thread (`WorktreeTaskRow`); a task worktree is never
 * a folder of its own here. "New thread in a worktree" (the folder's menus)
 * opens a new thread with "Work in › New worktree" chosen.
 */
import { Suspense, useState } from 'react';

import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { readThreadUuids, useHandoffIpc, useHandoffLiveEvents, useHandoffStatusMany } from '@/features/workspace-handoff';
import { readThreads, threadsQueryKey, useWorkspaceIpc } from '@/features/workspace';
import { preferWorktree, WorktreesPanel } from '@/features/workspace-worktrees';
import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { openedFolders, type Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { ActivityGroupSummary } from '@/shared/ui/ConversationActivity';

import type { ShellAction } from '../lib/shellActions';
import { folderContextActions } from '../lib/folderMenu';
import { useAppIpc } from '../model/appIpcContext';
import { useLocalNeedsYouCount, useLocalThreadUnreadTracking } from '../model/localThreadActivity';
import { useContextMenu, type ContextAction } from '../model/useContextMenu';
import { WORKSPACE_LIST_KEY } from '../model/useShellActions';
import type { ShellLocation } from '../model/useShellLocation';
import { FolderClosedIcon, FolderOpenIcon } from './folderIcons';
import { MoveFolderDialog, type PendingMove } from './MoveFolderDialog';
import { ProjectPickerSheet } from './ProjectPickerSheet';
import { CHILD_INDENT, ShellRow, SectionCaption } from './rows';
import { LocalThreadRow } from './LocalThreadRow';
import { WorktreeTaskRow } from './WorktreeTaskRow';
import { modKey } from './shortcutLabel';

const THREADS_SHOWN = 8;

interface SectionProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  projects: readonly Project[];
  /** The project the sidebar is on: its folders, and those with no project yet, are listed. */
  projectId: string | undefined;
}

const threadsQuery = (workspaceId: string) => ({ queryKey: threadsQueryKey(workspaceId), queryFn: () => readThreads(workspaceId) });

/** The threads a folder shows: all of them, or the first few. */
/** One remembered thread of a folder. */
type WorkspaceThread = ReturnType<typeof readThreads>[number];

const shownThreads = (list: readonly WorkspaceThread[], all: boolean): readonly WorkspaceThread[] => (all ? list : list.slice(0, THREADS_SHOWN));

/** Each thread, then the earlier attempts a rewind left under it; an attempt whose thread is gone stands alone. */
function nestedThreads(list: readonly WorkspaceThread[]): WorkspaceThread[] {
  const listed = new Set(list.map((thread) => thread.id));
  const under = (id: string) => list.filter((thread) => thread.earlierAttemptOf === id);
  return list.filter((thread) => thread.earlierAttemptOf === undefined || !listed.has(thread.earlierAttemptOf)).flatMap((thread) => [thread, ...under(thread.id)]);
}

interface FolderThreadsProps extends Omit<SectionProps, 'projects' | 'projectId'> {
  workspace: Workspace;
  all: boolean;
  onToggleAll: () => void;
  /** The batch's statuses, by UUID. */
  statuses: Readonly<Record<string, HandoffStatus>>;
}

function FolderThreads({ workspace, location, run, all, onToggleAll, statuses }: FolderThreadsProps): React.JSX.Element {
  const threads = useQuery(threadsQuery(workspace.id));
  // The UUIDs the thread pages read (handoff is keyed by them), to badge a row without opening it.
  const uuids = readThreadUuids(workspace.id);
  const list: WorkspaceThread[] = nestedThreads(threads.data ?? []);
  const shown = shownThreads(list, all);
  const here = location.workspaceId === workspace.id;
  return (
    <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }} aria-label={t('desktop.shell.threadsOf', 'Threads in {{name}}', { name: workspace.name })}>
      {shown.map((thread) => (
        <li key={thread.id}>
          <LocalThreadRow
            workspace={workspace}
            thread={thread}
            uuid={uuids[thread.id]}
            selected={here && location.conversationId === thread.id}
            run={run}
            status={uuids[thread.id] === undefined ? undefined : statuses[uuids[thread.id] as string]}
          />
        </li>
      ))}
      {list.length > THREADS_SHOWN && (
        <li>
          <ShellRow
            indent={CHILD_INDENT}
            muted
            label={all ? t('desktop.shell.showFewer', 'Show fewer') : t('desktop.shell.showAll', 'Show all ({{n}})', { n: list.length })}
            onClick={onToggleAll}
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

/** A new thread of the folder that will work in a new task worktree. */
function newWorktreeThread(workspace: Workspace, run: SectionProps['run']): void {
  preferWorktree(workspace.id);
  run({ type: 'open_workspace', workspaceId: workspace.id });
}

/** Binding a folder to a project and removing it from the sidebar: the ⋯ menu's and the context menu's shared actions. */
function useFolderMutations(workspace: Workspace) {
  const ipc = useWorkspaceIpc();
  const queryClient = useQueryClient();
  const refresh = (): Promise<void> => queryClient.invalidateQueries({ queryKey: WORKSPACE_LIST_KEY });
  const bind = useMutation({ mutationFn: (projectId: number) => ipc?.bindProject(workspace.id, projectId) ?? Promise.resolve(), onSettled: refresh });
  const remove = useMutation({ mutationFn: () => ipc?.remove(workspace.id) ?? Promise.resolve(), onSettled: refresh });
  return { bind, remove };
}

/**
 * Joining a project: at once for a folder with none, after a confirmation for
 * one that has a project (the dialog this returns asks).
 */
function useProjectChange(workspace: Workspace, projects: readonly Project[]) {
  const { bind, remove } = useFolderMutations(workspace);
  const [pending, setPending] = useState<PendingMove | null>(null);
  const name = (id: number | null): string => projects.find((project) => project.id === id)?.name ?? '';
  const request = (projectId: number): void => {
    if (workspace.project_id === projectId) return;
    if (workspace.project_id === null) {
      bind.mutate(projectId);
      return;
    }
    setPending({ folder: workspace.name, from: name(workspace.project_id), to: name(projectId), confirm: () => bind.mutate(projectId) });
  };
  const dialog = <MoveFolderDialog move={pending} onClose={() => setPending(null)} />;
  return { request, remove, dialog };
}

interface FolderMenuProps extends Omit<SectionProps, 'location' | 'projectId'> {
  workspace: Workspace;
  onBind: (projectId: number) => void;
  onRemove: () => void;
}

function FolderMenu({ workspace, projects, run, onBind, onRemove }: FolderMenuProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
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
        {workspace.is_git && (
          <MenuItem
            onClick={() => {
              close();
              newWorktreeThread(workspace, run);
            }}
          >
            {t('desktop.shell.newWorktreeThread', 'New thread in a worktree')}
          </MenuItem>
        )}
        {workspace.is_git && <Divider />}
        <ListSubheader>{workspace.project_id === null ? t('desktop.shell.bindProject', 'Project for this folder') : t('desktop.shell.moveToProject', 'Move to project')}</ListSubheader>
        {projects
          .filter((project) => !project.suspended)
          .map((project) => (
            <MenuItem
              key={project.id}
              selected={workspace.project_id === project.id}
              onClick={() => {
                close();
                onBind(project.id);
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
            onRemove();
          }}
        >
          {t('desktop.shell.removeFolder', 'Remove from sidebar')}
        </MenuItem>
      </Menu>
    </>
  );
}

interface FolderRowProps extends Omit<SectionProps, 'location' | 'projectId'> {
  workspace: Workspace;
  open: boolean;
  selected: boolean;
  /** The folder's project's name, or "No project". */
  project: string;
  onToggle: () => void;
}

/** A folder's row, with its hover actions and its native context menu. */
function FolderRow({ workspace, projects, run, open, selected, project, onToggle }: FolderRowProps): React.JSX.Element {
  const showMenu = useContextMenu();
  // Client contract 1.12 (desktop state): how many of the folder's threads need the person, from the
  // activity feed, the frames and the marks. A closed folder reads no thread list; an open one shares
  // the list its thread rows read (the same query).
  const threads = useQuery({ ...threadsQuery(workspace.id), enabled: open }).data ?? [];
  const needsYou = useLocalNeedsYouCount(workspace.id, readThreadUuids(workspace.id), threads);
  const appIpc = useAppIpc();
  const { request, remove, dialog } = useProjectChange(workspace, projects);
  const [picking, setPicking] = useState(false);
  const actions = (): ContextAction[] =>
    folderContextActions({
      workspace,
      projects,
      appIpc,
      primary: { label: t('desktop.shell.newThread', 'New thread'), run: () => run({ type: 'open_workspace', workspaceId: workspace.id }) },
      ...(workspace.is_git ? { worktree: () => newWorktreeThread(workspace, run) } : {}),
      bind: request,
      remove: () => remove.mutate(),
      choosePicker: () => setPicking(true),
    });
  return (
    <>
    <ShellRow
      label={workspace.name}
      // Every folder listed is in the sidebar's project; only one with none says so.
      meta={workspace.project_id === null ? project : ''}
      title={`${workspace.path}\n${t('desktop.shell.folderProject', 'Project: {{name}}', { name: project })}`}
      icon={open ? <FolderOpenIcon /> : <FolderClosedIcon />}
      ariaExpanded={open}
      selected={selected}
      status={<ActivityGroupSummary needsYou={needsYou} count={threads.length > 0 ? threads.length : undefined} />}
      onClick={onToggle}
      onContextMenu={(event) => showMenu(event, actions())}
      trailingOnHover
      trailing={<FolderMenu workspace={workspace} projects={projects} run={run} onBind={request} onRemove={() => remove.mutate()} />}
    />
    <ProjectPickerSheet open={picking} projects={projects} currentId={workspace.project_id} onPick={request} onClose={() => setPicking(false)} />
    {dialog}
    </>
  );
}

/** One project's live handoff changes, for the sidebar's badges (renders nothing). */
function HandoffLiveEvents({ projectId }: { readonly projectId: number }): null {
  useHandoffLiveEvents(projectId);
  return null;
}

export function LocalWorkSection({ location, run, projects, projectId }: SectionProps): React.JSX.Element {
  const ipc = useWorkspaceIpc();
  const list = useQuery({ queryKey: WORKSPACE_LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [expandedAll, setExpandedAll] = useState<ReadonlySet<string>>(new Set());
  const workspaces = openedFolders(list.data ?? []).filter((workspace) => workspace.project_id === null || String(workspace.project_id) === projectId);
  const toggleIn = (set: (update: (current: ReadonlySet<string>) => ReadonlySet<string>) => void, id: string): void =>
    set((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  // One status read for every visible thread row with a known UUID (the same thread lists the folders read).
  const openFolders = workspaces.filter((workspace) => !collapsed.has(workspace.id));
  const threadLists = useQueries({ queries: openFolders.map((workspace) => threadsQuery(workspace.id)) });
  const visibleUuids = openFolders.flatMap((workspace, index) => {
    const uuids = readThreadUuids(workspace.id);
    return shownThreads(threadLists[index]?.data ?? [], expandedAll.has(workspace.id))
      .map((thread) => uuids[thread.id])
      .filter((uuid): uuid is string => uuid !== undefined);
  });
  const handoffIpc = useHandoffIpc();
  const statuses = useHandoffStatusMany(handoffIpc, visibleUuids);
  // The badges follow each bound project's live handoff changes; the batch poll is the fallback.
  const liveProjects = handoffIpc === undefined ? [] : [...new Set(workspaces.map((workspace) => workspace.project_id).filter((id): id is number => id !== null))];
  const projectName = (id: number | null): string =>
    id === null ? t('desktop.shell.noProject', 'No project') : (projects.find((p) => p.id === id)?.name ?? '');
  const openFolder = (): void => run({ type: 'open_folder' });
  // A thread whose turn finishes off screen is unread until it is opened.
  useLocalThreadUnreadTracking(location.workspaceId === undefined ? '' : location.conversationId);

  return (
    <Box component="nav" aria-label={t('desktop.shell.localWork', 'Local work')}>
      {liveProjects.map((projectId) => (
        <HandoffLiveEvents key={projectId} projectId={projectId} />
      ))}
      <SectionCaption
        onOpen={() => run({ type: 'go', to: '/workspaces' })}
        testId="shell-local-work"
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
          const toggle = (): void => toggleIn(setCollapsed, workspace.id);
          return (
            <li key={workspace.id} data-testid="shell-folder">
              <FolderRow
                workspace={workspace}
                projects={projects}
                run={run}
                open={open}
                selected={location.workspaceId === workspace.id && !open}
                project={project}
                onToggle={toggle}
              />
              {open && (
                <FolderThreads
                  workspace={workspace}
                  location={location}
                  run={run}
                  all={expandedAll.has(workspace.id)}
                  onToggleAll={() => toggleIn(setExpandedAll, workspace.id)}
                  statuses={statuses}
                />
              )}
              {open && workspace.is_git && (
                <Suspense fallback={null}>
                  <WorktreesPanel workspaceId={workspace.id} workspaceName={workspace.name} renderEntry={(parts) => <WorktreeTaskRow {...parts} location={location} run={run} />} />
                </Suspense>
              )}
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
