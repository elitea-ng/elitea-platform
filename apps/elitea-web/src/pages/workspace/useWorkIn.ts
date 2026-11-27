/**
 * "Work in" on a new thread of a git folder (`WorkInChip`): the folder itself,
 * or a new task worktree, which is proposed by default; the person's own
 * choice is remembered for the folder (`rememberWorkIn`) and proposed next
 * time, while the folder menu's "New thread in a worktree" forces it. With "New worktree", Send makes the worktree, hands
 * the message, the agents (`handOver`, #3128), the modes (`handOverChoices`, #3144) and "Respect AGENTS.md" chosen for
 * the thread to the worktree's own new thread
 * (`holdFirstMessage`) and opens it; that thread shows the message at once and sends it as soon as it can
 * (`useFirstSend`), so the person writes once. A worktree the host cannot make (not the repository's
 * top, a branch in the way) is said above the composer and nothing is sent.
 */
import { useEffect, useState } from 'react';

import { createSubmitIds } from '@elitea/client-core/conversation';

import { holdFirstMessage, initialWorkIn, listenForWorktree, rememberWorkIn, StartInWorktreeError, useStartInWorktree, useWorktreeIpc, type WorkIn } from '@/features/workspace-worktrees';
import type { WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

import { movePendingRespect } from './pendingRespect';
import type { PreThreadChoices } from './preThreadChoices';
import type { SelectionSeed } from './useAgentSelection';
import type { HandedFirstMessage } from './useFirstSend';
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
  send: Send;
  /** The agents chosen for the new thread: the worktree's thread starts with them (#3128). */
  handOver?: (() => SelectionSeed | undefined) | undefined;
  /** The composer's modes chosen for the new thread: the worktree's thread starts in them (#3144). */
  handOverChoices?: (() => PreThreadChoices | undefined) | undefined;
  onError: (message: string) => void;
  /** Open a workspace's new thread (the worktree's). */
  open: (workspaceId: string) => void;
  /** The host client; the app's (`WorktreeIpcProvider`, else the Tauri one) when not given. */
  ipc?: WorktreeIpc;
}

export function useWorkIn({ workspaceId, offered: asked, send, handOver, handOverChoices, onError, open, ipc }: WorkInInput): WorkInControl {
  // The provided host (tests), else the app's (its context, else the Tauri one).
  const host = useWorktreeIpc();
  const start = useStartInWorktree(ipc ?? host);
  // Without a host that can make worktrees, nothing is offered and the thread works in the folder.
  const offered = asked && start !== undefined;
  // Read once, at mount: the folder may become "offered" a moment later (its git state is read), and keeps the proposal.
  const [initial] = useState<WorkIn>(() => initialWorkIn(workspaceId));
  const [chosen, setChosen] = useState<WorkIn | undefined>(undefined);
  const value: WorkIn = offered ? (chosen ?? initial) : 'folder';

  useEffect(() => (offered ? listenForWorktree(workspaceId, () => setChosen('worktree')) : undefined), [offered, workspaceId]);

  // The new thread's id, kept until its message is handed over (#3077): a retry, or a second call that got past the
  // composer's gate, makes no second worktree (the host answers the one this id made).
  const [threadIds] = useState(createSubmitIds);
  const toWorktree: Send = async (...args) => {
    if (start === undefined) return false;
    // The agents as they were when the message was sent: a change made while the worktree is made is not carried.
    const seed = handOver?.();
    const choices = handOverChoices?.();
    try {
      const entry = await start(workspaceId, threadIds.current());
      threadIds.sent();
      const handed: HandedFirstMessage = { args, seed, choices };
      holdFirstMessage(entry.workspace_id, handed);
      // "Respect AGENTS.md" turned off for this new thread goes with it, and no longer applies to the folder's next one.
      movePendingRespect(workspaceId, entry.workspace_id);
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
