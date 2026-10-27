/**
 * Where a new thread of a git folder works: in the folder itself, or in a new
 * task worktree. Two hand-offs between screens, both one-shot and keyed by
 * workspace id:
 *
 * - `preferWorktree`: the sidebar's "New thread in a worktree" opens the
 *   folder's new thread with the composer already on "New worktree";
 * - `holdFirstMessage`: a message written in the folder's new thread with
 *   "New worktree" chosen waits for the worktree's own thread to open, which
 *   sends it (`takeFirstMessage`).
 *
 * Module state, not storage: a reload in between drops the hand-off, and the
 * message is still in the folder thread's draft.
 */
export type WorkIn = 'folder' | 'worktree';

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

/** The composer's first choice for a new thread of the folder (read once). */
export function initialWorkIn(workspaceId: string): WorkIn {
  return preferred.delete(workspaceId) ? 'worktree' : 'folder';
}

export function holdFirstMessage<Args extends readonly unknown[]>(workspaceId: string, args: Args): void {
  held.set(workspaceId, args);
}

export function takeFirstMessage<Args extends readonly unknown[]>(workspaceId: string): Args | undefined {
  const args = held.get(workspaceId) as Args | undefined;
  held.delete(workspaceId);
  return args;
}
