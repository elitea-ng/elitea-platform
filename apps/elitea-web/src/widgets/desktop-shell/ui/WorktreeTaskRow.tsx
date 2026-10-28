/**
 * A parallel task (task worktree) under its folder, drawn as one of the
 * folder's threads: a branch glyph, the title of the thread it runs (or "New
 * task" before its first message), and its quiet status (commits ahead,
 * uncommitted paths). The branch and the worktree's path are in the tooltip;
 * Diff, Merge back, Remove and a "⋯" show on hover. A click opens the task's
 * thread in the worktree's own workspace.
 *
 * Its context menu and "⋯" archive or delete that thread
 * (`useThreadLifecycle`), and remove the worktree with it by default.
 */
import { useQuery } from '@tanstack/react-query';

import AltRouteOutlinedIcon from '@mui/icons-material/AltRouteOutlined';

import { readThreads, threadsQueryKey } from '@/features/workspace';
import type { WorktreeRowParts } from '@/features/workspace-worktrees';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import { useContextMenu, type ContextAction } from '../model/useContextMenu';
import { lifecycleActions, useThreadLifecycle } from '../model/useThreadLifecycle';
import type { ShellLocation } from '../model/useShellLocation';
import { RowMenu } from './ConversationRow';
import { CHILD_INDENT, ShellRow } from './rows';
import { ThreadLifecycleDialog } from './ThreadLifecycleDialog';

interface WorktreeTaskRowProps extends WorktreeRowParts {
  /** The folder the worktree belongs to (what `worktree_remove` takes). */
  folder: Workspace;
  location: ShellLocation;
  run: (action: ShellAction) => void;
}

export function WorktreeTaskRow({ entry, counts, actions, folder, location, run }: WorktreeTaskRowProps): React.JSX.Element {
  const showMenu = useContextMenu();
  const threads = useQuery({ queryKey: threadsQueryKey(entry.workspace_id), queryFn: () => readThreads(entry.workspace_id) }).data ?? [];
  const latest = threads[0];
  const title = latest === undefined || latest.title === '' ? t('workspace.worktree.untitled', 'New task') : latest.title;
  const here = location.workspaceId === entry.workspace_id;
  const lifecycle = useThreadLifecycle(
    {
      workspaceId: entry.workspace_id,
      folderId: folder.id,
      // No thread yet: nothing to archive or delete (Remove takes the worktree).
      projectId: latest === undefined ? null : folder.project_id,
      conversationId: latest?.id ?? '',
      title,
      worktree: { parentId: folder.id, threadUuid: entry.thread_uuid, branch: entry.branch },
    },
    { open: here && latest !== undefined && location.conversationId === latest.id, inWorkspace: here, run },
  );
  const open = (): void => run({ type: 'open_workspace', workspaceId: entry.workspace_id, ...(latest === undefined ? {} : { conversationId: latest.id }) });
  const items: ContextAction[] = [{ id: 'open', label: t('desktop.menu.open', 'Open'), run: open }, { separator: true }, ...lifecycleActions(lifecycle)];
  return (
    <>
      <ShellRow
        indent={CHILD_INDENT}
        icon={<AltRouteOutlinedIcon />}
        label={title}
        meta={counts}
        title={`${title}\n${t('workspace.worktree.branch', 'Branch: {{branch}}', { branch: entry.branch })}\n${entry.path}`}
        selected={here}
        testId="worktree-task"
        onClick={open}
        onContextMenu={(event) => showMenu(event, items)}
        trailingOnHover
        trailingRoom="7.25rem"
        trailing={
          <>
            {actions}
            <RowMenu items={items} label={t('desktop.shell.threadMore', 'More for {{name}}', { name: title })} testId="worktree-task-more" />
          </>
        }
      />
      <ThreadLifecycleDialog lifecycle={lifecycle} title={title} />
    </>
  );
}
