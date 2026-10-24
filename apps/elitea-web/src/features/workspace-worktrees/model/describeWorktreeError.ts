/**
 * What the parallel-task screens say for a rejected `worktree_*` command: by
 * `code` where the person can act on it, otherwise the host's own message
 * (already written for a person).
 */
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** Built when asked: the catalogue may not be loaded at import. */
const MESSAGES: Readonly<Record<string, () => string>> = {
  worktree_unknown: () => t('workspace.worktree.error.unknown', 'This parallel task is gone. Its list is refreshed.'),
  worktree_exists: () => t('workspace.worktree.error.exists', 'A parallel task for this thread already exists.'),
  not_a_git_repository: () => t('workspace.worktree.error.notGit', 'Parallel tasks need a folder that is a git repository.'),
  unsafe_repository: () => t('workspace.worktree.error.unsafe', 'Git does not trust this repository (it belongs to another user), so no parallel task can be made in it.'),
  workspace_busy: () => t('workspace.error.busy', 'An agent is still working in this folder. Wait for it to finish, or stop it, and try again.'),
  workspace_unknown: () => t('workspace.worktree.error.workspaceUnknown', 'This folder is no longer in your workspaces.'),
  preflight_expired: () => t('workspace.worktree.error.expired', 'The preview is out of date. Check again before merging back.'),
  folder_changed_since_preflight: () => t('workspace.worktree.error.changed', 'The folder or the parallel task changed after the preview. Check again before merging back.'),
  merge_conflicts: () => t('workspace.worktree.error.conflicts', 'Both sides changed the same files, so nothing was applied. The work stays on its branch.'),
};

export function describeWorktreeError(error: unknown): string {
  const { code, message } = toWorkspaceIpcError(error);
  const known = MESSAGES[code];
  if (known !== undefined) return known();
  return message === '' ? t('workspace.failed', 'That did not work. Try again.') : message;
}

/** The folder changed after the merge: undoing it reverts that too, so it is asked for again. */
export function undoNeedsConfirm(error: unknown): boolean {
  return toWorkspaceIpcError(error).code === 'undo_not_latest';
}

/** The preview must be taken again before an apply can succeed. */
export function needsNewPreview(error: unknown): boolean {
  const { code } = toWorkspaceIpcError(error);
  return code === 'preflight_expired' || code === 'folder_changed_since_preflight';
}
