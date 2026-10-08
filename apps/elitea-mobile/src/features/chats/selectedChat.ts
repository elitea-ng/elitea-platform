import { useCallback, useSyncExternalStore } from 'react';

import { registerStoredWipeHook, registerWipeHook } from '@/security/endSession';

/**
 * The Chats tab's selected conversation (issue #15): the chat open in the detail column at
 * regular width, and the last chat opened on its own screen. It outlives a change of width
 * (rotation, Split View, Stage Manager), so the same chat stays open when the two panes
 * collapse into the phone stack and back.
 *
 * Scoped: a selection belongs to one workspace and one project (conversation ids are per
 * project), and reads as nothing anywhere else. Memory only, never persisted, and wiped with
 * the session, so after a sign-out or a wipe the next account never opens it.
 */
export interface ChatScope {
  workspaceId: string | null;
  projectId: number | null;
}

interface Selection extends ChatScope {
  id: number;
}

let selection: Selection | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** The selected conversation id in `scope`, or null (none, or another workspace's or project's). */
export function getSelectedChat(scope: ChatScope): number | null {
  if (selection === null || scope.projectId === null) return null;
  return selection.workspaceId === scope.workspaceId && selection.projectId === scope.projectId ? selection.id : null;
}

/** Selects conversation `id` in `scope` (a project must be known). */
export function selectChat(scope: ChatScope, id: number): void {
  if (scope.projectId === null || !Number.isSafeInteger(id) || id <= 0) return;
  if (selection?.id === id && selection.workspaceId === scope.workspaceId && selection.projectId === scope.projectId) return;
  selection = { workspaceId: scope.workspaceId, projectId: scope.projectId, id };
  emit();
}

/**
 * A conversation opened on its own screen becomes the selection. Its route takes an id or a
 * uuid; only an id is comparable with the list's rows, so a uuid leaves the selection alone.
 */
export function rememberOpenChat(scope: ChatScope, ref: string): void {
  if (/^[1-9]\d{0,15}$/.test(ref)) selectChat(scope, Number(ref));
}

/** Clears the selection if it is `id` in `scope` (that chat was deleted), or entirely with no arguments. */
export function clearSelectedChat(scope?: ChatScope, id?: number): void {
  if (selection === null) return;
  if (scope !== undefined && getSelectedChat(scope) !== id) return;
  selection = null;
  emit();
}

export function useSelectedChat(scope: ChatScope): number | null {
  const { workspaceId, projectId } = scope;
  const read = useCallback(() => getSelectedChat({ workspaceId, projectId }), [workspaceId, projectId]);
  return useSyncExternalStore(subscribe, read, read);
}

registerWipeHook('chat_selection', () => clearSelectedChat());
// A workspace wiped while another one is active (removed, revoked at check-in).
registerStoredWipeHook('chat_selection', (workspaceId) => {
  if (selection?.workspaceId === workspaceId) clearSelectedChat();
});
