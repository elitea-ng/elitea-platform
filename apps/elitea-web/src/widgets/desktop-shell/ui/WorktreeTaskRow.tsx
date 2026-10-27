/**
 * A parallel task (task worktree) under its folder, drawn as one of the
 * folder's threads: a branch glyph, the title of the thread it runs (or "New
 * task" before its first message), and its quiet status (commits ahead,
 * uncommitted paths). The branch and the worktree's path are in the tooltip;
 * Diff, Merge back and Remove show on hover. A click opens the task's thread
 * in the worktree's own workspace.
 */
import { useQuery } from '@tanstack/react-query';

import AltRouteOutlinedIcon from '@mui/icons-material/AltRouteOutlined';

import { readThreads, threadsQueryKey } from '@/features/workspace';
import type { WorktreeRowParts } from '@/features/workspace-worktrees';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import type { ShellLocation } from '../model/useShellLocation';
import { CHILD_INDENT, ShellRow } from './rows';

interface WorktreeTaskRowProps extends WorktreeRowParts {
  location: ShellLocation;
  run: (action: ShellAction) => void;
}

export function WorktreeTaskRow({ entry, counts, actions, location, run }: WorktreeTaskRowProps): React.JSX.Element {
  const threads = useQuery({ queryKey: threadsQueryKey(entry.workspace_id), queryFn: () => readThreads(entry.workspace_id) }).data ?? [];
  const latest = threads[0];
  const title = latest === undefined || latest.title === '' ? t('workspace.worktree.untitled', 'New task') : latest.title;
  return (
    <ShellRow
      indent={CHILD_INDENT}
      icon={<AltRouteOutlinedIcon />}
      label={title}
      meta={counts}
      title={`${title}\n${t('workspace.worktree.branch', 'Branch: {{branch}}', { branch: entry.branch })}\n${entry.path}`}
      selected={location.workspaceId === entry.workspace_id}
      testId="worktree-task"
      onClick={() => run({ type: 'open_workspace', workspaceId: entry.workspace_id, ...(latest === undefined ? {} : { conversationId: latest.id }) })}
      trailingOnHover
      trailingRoom="5.5rem"
      trailing={actions}
    />
  );
}
