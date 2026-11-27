/**
 * "Respect AGENTS.md" turned off before a thread's first message (issue 3130). The thread does not exist yet, so the
 * host cannot keep the choice; it is held here, per folder, for this page load, and the send that creates the thread
 * hands it to the host before its first turn starts (`applyPendingRespect`), so even that turn runs without the files.
 */
import { useSyncExternalStore } from 'react';

import type { ThreadInstructionsCommands } from '@/shared/desktop/threadInstructionsIpc';

/** The folders whose next new thread does not respect AGENTS.md. */
const optedOut = new Set<string>();
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function setPendingRespect(workspaceId: string, respect: boolean): void {
  if (respect) optedOut.delete(workspaceId);
  else optedOut.add(workspaceId);
  listeners.forEach((listener) => listener());
}

/**
 * The folder's new thread moved to a worktree before its first message (#3144): the choice is the worktree's new
 * thread's now, and the folder's next new thread starts on again.
 */
export function movePendingRespect(from: string, to: string): void {
  if (!optedOut.has(from)) return;
  optedOut.delete(from);
  setPendingRespect(to, false);
}

/** The choice for the folder's thread not created yet (on unless turned off), and the setter. */
export function usePendingRespect(workspaceId: string): [boolean, (respect: boolean) => void] {
  const respect = useSyncExternalStore(subscribe, () => !optedOut.has(workspaceId));
  return [respect, (next) => setPendingRespect(workspaceId, next)];
}

/**
 * A send that created `conversationId`: a choice made before it is kept by the host for the new thread before its
 * first turn starts. A failure throws (the send shows it and keeps the choice, so a retry hands it over again).
 */
export async function applyPendingRespect(ipc: ThreadInstructionsCommands | undefined, workspaceId: string, conversationId: string, startsNew: boolean): Promise<void> {
  if (!startsNew || ipc === undefined || !optedOut.has(workspaceId)) return;
  await ipc.setThreadRespectAgentsMd(workspaceId, conversationId, false);
  setPendingRespect(workspaceId, true);
}
