/**
 * Where a new thread of a git folder works: in the folder itself, or in a new
 * task worktree. A new worktree is proposed by default; the person's last
 * explicit choice for the folder is remembered on this machine
 * (`rememberWorkIn`, the `el.` namespace) and proposed next time. Two
 * hand-offs between screens, both one-shot and keyed by workspace id:
 *
 * - `preferWorktree`: the sidebar's "New thread in a worktree" opens the
 *   folder's new thread with the composer already on "New worktree";
 * - `holdFirstMessage`: a message written in the folder's new thread with
 *   "New worktree" chosen waits for the worktree's own thread to open, which
 *   sends it (`takeFirstMessage`).
 *
 * The hand-offs are module state, not storage: a reload in between drops the
 * hand-off, and the message is still in the folder thread's draft.
 */
import { createStorage } from '@/shared/lib/storage';

export type WorkIn = 'folder' | 'worktree';

const choiceKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.workIn`;

/** The folder's remembered choice; a new worktree when there is none (or storage is unavailable). */
function rememberedWorkIn(workspaceId: string): WorkIn {
  try {
    return createStorage('local').get(choiceKey(workspaceId)) === 'folder' ? 'folder' : 'worktree';
  } catch {
    return 'worktree';
  }
}

/** The person chose where the folder's new threads work: proposed again next time. */
export function rememberWorkIn(workspaceId: string, choice: WorkIn): void {
  try {
    createStorage('local').set(choiceKey(workspaceId), choice);
  } catch {
    // Not remembered: the next new thread proposes a worktree again.
  }
}

const preferred = new Set<string>();
const held = new Map<string, readonly unknown[]>();
/** The new thread of a folder that is open now: it takes the choice at once (no remount brings it there). */
const listening = new Map<string, () => void>();

export function preferWorktree(workspaceId: string): void {
  const open = listening.get(workspaceId);
  if (open === undefined) preferred.add(workspaceId);
  else open();
}

/** While a folder's new thread is shown, "New thread in a worktree" switches it in place. */
export function listenForWorktree(workspaceId: string, onPrefer: () => void): () => void {
  listening.set(workspaceId, onPrefer);
  return () => {
    if (listening.get(workspaceId) === onPrefer) listening.delete(workspaceId);
  };
}

/** The composer's first choice for a new thread of the folder (read once): a worktree when asked for, else the remembered choice. */
export function initialWorkIn(workspaceId: string): WorkIn {
  return preferred.delete(workspaceId) ? 'worktree' : rememberedWorkIn(workspaceId);
}

export function holdFirstMessage<Args extends readonly unknown[]>(workspaceId: string, args: Args): void {
  held.set(workspaceId, args);
}

export function takeFirstMessage<Args extends readonly unknown[]>(workspaceId: string): Args | undefined {
  const args = held.get(workspaceId) as Args | undefined;
  held.delete(workspaceId);
  return args;
}
