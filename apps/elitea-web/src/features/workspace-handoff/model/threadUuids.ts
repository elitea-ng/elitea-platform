/**
 * Which server conversation UUID a folder's thread has. Handoff is keyed by
 * the UUID, but a folder's thread list holds only the conversation id, and
 * the sidebar must badge a row without opening the thread. A thread page
 * remembers the UUID it read; the sidebar and the take back toast look it up.
 * Kept per folder on this machine in the `el.` namespace, like the thread
 * list itself: logout's `clearNamespace()` sweep wipes it.
 */
import { createStorage } from '@/shared/lib/storage';

const key = (workspaceId: string): string => `desktop.workspace.${workspaceId}.threadUuids`;

function isMap(raw: unknown): Record<string, string> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new Error('not a map');
  return Object.fromEntries(Object.entries(raw).filter((entry): entry is [string, string] => typeof entry[1] === 'string'));
}

/** Conversation id → UUID. Never throws: unavailable or corrupt storage reads as none. */
export function readThreadUuids(workspaceId: string): Record<string, string> {
  try {
    return createStorage('local').getJSON(key(workspaceId), isMap) ?? {};
  } catch {
    return {};
  }
}

export function rememberThreadUuid(workspaceId: string, conversationId: string, uuid: string): void {
  const known = readThreadUuids(workspaceId);
  if (known[conversationId] === uuid) return;
  try {
    createStorage('local').setJSON(key(workspaceId), { ...known, [conversationId]: uuid });
  } catch {
    // Not remembered: the thread's row simply has no cloud badge until it is opened again.
  }
}
