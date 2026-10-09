/**
 * Which folder on THIS computer a Local work conversation belongs to.
 *
 * The server cannot say (a conversation does not know its folder), so the
 * answer comes from what this machine remembers: the folder's thread list
 * (`readThreads`, browser storage) first, then the host's turn history store
 * (`thread_history`, which outlives a cleared thread list). `null` when no
 * folder here ran the thread — it was started on another computer, or the
 * folder was removed — and the caller then shows the read-only conversation.
 *
 * Desktop-only: reached through a dynamic `import()` behind
 * `import.meta.env.MODE === 'desktop'`, so the web build drops it.
 */
import { readThreads } from '@/features/workspace';
import { tauriWorkspaceIpc, type WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

export async function findLocalWorkThread(conversationId: string, ipc: WorkspaceIpc | undefined = tauriWorkspaceIpc()): Promise<string | null> {
  if (ipc === undefined || conversationId === '') return null;
  const workspaces = await ipc.list().catch(() => []);
  const remembered = workspaces.find((w) => readThreads(w.id).some((thread) => thread.id === conversationId));
  if (remembered !== undefined) return remembered.id;
  for (const workspace of workspaces) {
    const turns = await ipc.threadHistory(workspace.id, conversationId).catch(() => []);
    if (turns.length > 0) return workspace.id;
  }
  return null;
}
