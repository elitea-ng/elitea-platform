/**
 * "Work in" on a new thread of a git folder (`WorkInChip`): the folder itself,
 * or a new task worktree, which is proposed by default; the person's own
 * choice is remembered for the folder (`rememberWorkIn`) and proposed next
 * time, while the folder menu's "New thread in a worktree" forces it. With "New worktree", Send makes the worktree, hands
 * the message to the worktree's own new thread (`holdFirstMessage`) and opens
 * it; that thread sends it as soon as it can (`takeFirstMessage`), so the
 * person writes once. A worktree the host cannot make (not the repository's
 * top, a branch in the way) is said above the composer and nothing is sent.
 */
import { useEffect, useState } from 'react';

import { holdFirstMessage, initialWorkIn, listenForWorktree, rememberWorkIn, StartInWorktreeError, takeFirstMessage, useStartInWorktree, type WorkIn } from '@/features/workspace-worktrees';
import type { WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

import type { WorkspaceComposerProps } from './WorkspaceComposer';

type Send = WorkspaceComposerProps['onSend'];

export interface WorkInControl {
  /** Offered: a new thread of a folder that is its repository's top. */
  offered: boolean;
  value: WorkIn;
  choose: (next: WorkIn) => void;
  /** The composer's send, going to a new worktree when that is the choice. */
  send: Send;
}

export interface WorkInInput {
  workspaceId: string;
  /** A new thread of a folder that is its repository's top. */
  offered: boolean;
  /** The thread can take a message now. */
  canSend: boolean;
  send: Send;
  onError: (message: string) => void;
  /** Open a workspace's new thread (the worktree's). */
  open: (workspaceId: string) => void;
  /** The host client; the Tauri one when not given (tests pass a fake). */
  ipc?: WorktreeIpc;
}

export function useWorkIn({ workspaceId, offered: asked, canSend, send, onError, open, ipc }: WorkInInput): WorkInControl {
  const start = useStartInWorktree(ipc);
  // Without a host that can make worktrees, nothing is offered and the thread works in the folder.
  const offered = asked && start !== undefined;
  // Read once, at mount: the folder may become "offered" a moment later (its git state is read), and keeps the proposal.
  const [initial] = useState<WorkIn>(() => initialWorkIn(workspaceId));
  const [chosen, setChosen] = useState<WorkIn | undefined>(undefined);
  const value: WorkIn = offered ? (chosen ?? initial) : 'folder';

  useEffect(() => (offered ? listenForWorktree(workspaceId, () => setChosen('worktree')) : undefined), [offered, workspaceId]);

  // The message written in the folder's new thread, sent here once this thread can take it.
  useEffect(() => {
    if (!canSend) return;
    const first = takeFirstMessage<Parameters<Send>>(workspaceId);
    if (first !== undefined) void send(...first);
  }, [canSend, workspaceId, send]);

  const toWorktree: Send = async (...args) => {
    if (start === undefined) return false;
    try {
      const entry = await start(workspaceId);
      holdFirstMessage(entry.workspace_id, args);
      open(entry.workspace_id);
      return true;
    } catch (failure) {
      onError(failure instanceof StartInWorktreeError ? failure.message : t('workspace.workIn.failed', 'The worktree could not be made.'));
      return false;
    }
  };

  const choose = (next: WorkIn): void => {
    setChosen(next);
    rememberWorkIn(workspaceId, next);
  };

  return { offered, value, choose, send: offered && value === 'worktree' ? toWorktree : send };
}
