/**
 * Archive and Delete for a Local work thread (the sidebar's thread rows and
 * task worktree rows). Each thread is a server conversation, so both use the
 * chats' own APIs:
 *
 * - **Archive** archives the conversation (`useArchiveConversation`, the
 *   chats' hook): it shows in the project's Archived chats, for a thread that
 *   runs here and one handed off to the cloud alike. The local record stays
 *   and is hidden (`archiveLocalThread`); unarchiving it on the Archived page
 *   shows it under its folder again.
 * - **Delete** asks first, then deletes the conversation
 *   (`conversationApi.remove`, as the chats do), the host's stored history
 *   (`thread_history_delete`) and the local record.
 *
 * A thread that works in a task worktree also removes the worktree, by
 * default: the dialog has "Also remove its worktree" checked. Archive asks
 * only then (otherwise it is one click). When the host refuses the removal
 * for uncommitted work (`worktree_dirty`), the person chooses "Remove anyway"
 * (force) or "Keep the worktree". The open thread (or the removed worktree)
 * leaves the screen for the folder's new thread.
 */
import { useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { useArchiveConversation } from '@/features/chat-conversation-list';
import { threadNames, useWorkspaceIpc } from '@/features/workspace';
import { RemoveWorktreeError, useRemoveWorktree, type WorktreeRef } from '@/features/workspace-worktrees';
import { getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import { archiveLocalThread, LOCAL_THREADS_KEY } from '@/shared/desktop/archivedThreads';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import { useDesktopLayout } from './desktopLayout.store';
import type { ContextAction } from './useContextMenu';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;

export interface LocalThreadTarget {
  /** The workspace the thread runs in: its folder, or its task worktree. */
  workspaceId: string;
  /** The folder whose new thread opens when this one leaves the screen. */
  folderId: string;
  projectId: number | null;
  conversationId: string;
  title: string;
  /** Its task worktree, when it works in one. */
  worktree?: WorktreeRef;
}

export type LifecycleKind = 'archive' | 'delete';

/** What the dialog asks: archive or delete (with the worktree checkbox), or what to do with a worktree that has uncommitted work. */
type LifecycleAsk = { kind: LifecycleKind } | { kind: 'dirty' } | null;

interface Where {
  /** The thread is the one on screen. */
  open: boolean;
  /** The screen is in the thread's workspace (a thread of its worktree). */
  inWorkspace: boolean;
  run: (action: ShellAction) => void;
}

const notify = (message: string): void => useDesktopLayout.getState().setNotice(message);

export interface ThreadLifecycle {
  /** The thread has a project (its conversation's): both actions need it. */
  readonly enabled: boolean;
  readonly worktree: WorktreeRef | undefined;
  readonly ask: LifecycleAsk;
  readonly busy: boolean;
  /** A forced removal's failure, shown in the dialog. */
  readonly error: string | null;
  readonly archive: () => void;
  readonly remove: () => void;
  readonly confirm: (withWorktree: boolean) => void;
  readonly forceRemove: () => void;
  readonly close: () => void;
}

export function useThreadLifecycle(target: LocalThreadTarget, where: Where): ThreadLifecycle {
  const queryClient = useQueryClient();
  const workspaceIpc = useWorkspaceIpc();
  const removeWorktree = useRemoveWorktree();
  const projectId = target.projectId === null ? undefined : String(target.projectId);
  const { archive: archiveConversation } = useArchiveConversation({ projectId, removeRow: () => undefined, toastError: notify });
  const [ask, setAsk] = useState<LifecycleAsk>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const worktree = removeWorktree === undefined ? undefined : target.worktree;

  const close = (): void => {
    setAsk(null);
    setBusy(false);
    setError(null);
  };

  const leave = (worktreeGoes: boolean): void => {
    if (where.open || (worktreeGoes && where.inWorkspace)) where.run({ type: 'open_workspace', workspaceId: target.folderId });
  };

  const removeTheWorktree = async (force: boolean): Promise<void> => {
    if (worktree === undefined || removeWorktree === undefined) return close();
    setBusy(true);
    try {
      if ((await removeWorktree(worktree, force)) === 'dirty') {
        setAsk({ kind: 'dirty' });
        setBusy(false);
        return;
      }
      close();
    } catch (failure) {
      const message = failure instanceof RemoveWorktreeError ? failure.message : t('workspace.failed', 'That did not work. Try again.');
      if (force) {
        setError(message);
        setBusy(false);
      } else {
        close();
        notify(message);
      }
    }
  };

  const finish = async (withWorktree: boolean): Promise<void> => {
    const goes = withWorktree && worktree !== undefined;
    void queryClient.invalidateQueries({ queryKey: LOCAL_THREADS_KEY });
    leave(goes);
    if (goes) await removeTheWorktree(false);
    else close();
  };

  const archiveNow = async (withWorktree: boolean): Promise<void> => {
    setBusy(true);
    if (!(await archiveConversation({ id: target.conversationId }))) return close();
    archiveLocalThread(target.conversationId);
    await finish(withWorktree);
  };

  const deleteNow = async (withWorktree: boolean): Promise<void> => {
    if (projectId === undefined) return;
    setBusy(true);
    try {
      await conversationApi.remove({ projectId, id: target.conversationId });
    } catch {
      close();
      notify(t('desktop.shell.threadDeleteFailed', 'The thread could not be deleted. Try again.'));
      return;
    }
    // The host's stored history goes with it; a host that has none (or is gone) leaves nothing behind.
    await workspaceIpc?.deleteThreadHistory(target.workspaceId, target.conversationId).catch(() => 0);
    threadNames.forget(target.workspaceId, target.conversationId);
    void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
    void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(projectId).slice(0, 1) });
    await finish(withWorktree);
  };

  return {
    enabled: projectId !== undefined,
    worktree,
    ask,
    busy,
    error,
    archive: () => {
      if (worktree !== undefined) setAsk({ kind: 'archive' });
      else void archiveNow(false);
    },
    remove: () => setAsk({ kind: 'delete' }),
    confirm: (withWorktree) => {
      if (ask?.kind === 'archive') void archiveNow(withWorktree);
      else if (ask?.kind === 'delete') void deleteNow(withWorktree);
    },
    forceRemove: () => void removeTheWorktree(true),
    close,
  };
}

/** The rows' Archive and Delete… entries, for the native context menu and the hover "⋯". */
export function lifecycleActions(lifecycle: ThreadLifecycle): ContextAction[] {
  return [
    { id: 'archive', label: t('desktop.chats.archive', 'Archive'), enabled: lifecycle.enabled, run: lifecycle.archive },
    { separator: true },
    { id: 'delete', label: t('desktop.chats.delete', 'Delete…'), enabled: lifecycle.enabled, run: lifecycle.remove },
  ];
}
