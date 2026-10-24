/**
 * The threads of a folder: the conversations a workspace session ran turns
 * in, newest first, as the desktop sidebar lists them under the folder.
 *
 * The server cannot answer "which conversations ran in this folder" (a
 * conversation does not know the folder), so the list is remembered on this
 * machine, per workspace, in the `el.` namespace — logout's
 * `clearNamespace()` sweep wipes it with everything else. Losing it loses
 * nothing on the server: each thread is still an ordinary conversation.
 *
 * The same module remembers where the person last was (folder + thread), so
 * the desktop opens there after sign-in.
 */
import { createStorage } from '@/shared/lib/storage';

export interface WorkspaceThread {
  /** The conversation id. */
  id: string;
  title: string;
  /** Epoch ms of the last turn started in it. */
  updatedAt: number;
  /** A rewound thread's source: the id of the thread it was rewound into, listed under it as the earlier attempt. */
  earlierAttemptOf?: string;
}

export interface LastLocation {
  workspaceId: string;
  /** '' when the person was on a new, not yet started thread. */
  conversationId: string;
}

const MAX_THREADS = 50;
const MAX_TITLE = 80;
const threadsKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.threads`;
const LAST_LOCATION_KEY = 'desktop.lastLocation';

function isThread(raw: unknown): raw is WorkspaceThread {
  if (typeof raw !== 'object' || raw === null) return false;
  const { id, title, updatedAt, earlierAttemptOf } = raw as Record<string, unknown>;
  return typeof id === 'string' && id !== '' && typeof title === 'string' && typeof updatedAt === 'number' && (earlierAttemptOf === undefined || typeof earlierAttemptOf === 'string');
}

function validThreads(raw: unknown): WorkspaceThread[] {
  if (!Array.isArray(raw)) throw new Error('not a list');
  return raw.filter(isThread);
}

/** Never throws: unavailable or corrupt storage reads as "no threads". */
export function readThreads(workspaceId: string): WorkspaceThread[] {
  try {
    return createStorage('local').getJSON(threadsKey(workspaceId), validThreads) ?? [];
  } catch {
    return [];
  }
}

/** Where `thread` goes in `list`: first, or (a rewind) in its source's place with the source right under it. */
function placed(list: readonly WorkspaceThread[], thread: WorkspaceThread, rewoundFrom: string | undefined): WorkspaceThread[] {
  const others = list.filter((t) => t.id !== thread.id);
  const at = rewoundFrom === undefined ? -1 : others.findIndex((t) => t.id === rewoundFrom);
  if (at < 0) return [thread, ...others];
  // The source's own earlier attempts move with it, under the new thread.
  const source = others[at] as WorkspaceThread;
  const attempts = others.filter((t) => t.earlierAttemptOf === source.id);
  const rest = others.filter((t, index) => index !== at && t.earlierAttemptOf !== source.id);
  rest.splice(Math.min(at, rest.length), 0, thread, { ...source, earlierAttemptOf: thread.id }, ...attempts.map((t) => ({ ...t, earlierAttemptOf: thread.id })));
  return rest;
}

/**
 * Puts `thread` first (new, or moved up with its new title/time); keeps the newest `MAX_THREADS`. With
 * `rewoundFrom` (a rewind), the new thread takes its source's place and the source is listed under it as the
 * earlier attempt.
 */
export function recordThread(workspaceId: string, thread: WorkspaceThread, rewoundFrom?: string): WorkspaceThread[] {
  const title = thread.title.trim().slice(0, MAX_TITLE);
  const next = placed(readThreads(workspaceId), { ...thread, title }, rewoundFrom).slice(0, MAX_THREADS);
  try {
    createStorage('local').setJSON(threadsKey(workspaceId), next);
  } catch {
    // Storage unavailable: the thread is simply not remembered.
  }
  return next;
}

export function forgetThreads(workspaceId: string): void {
  try {
    createStorage('local').remove(threadsKey(workspaceId));
  } catch {
    // Nothing to forget.
  }
}

function isLastLocation(raw: unknown): LastLocation {
  const { workspaceId, conversationId } = (raw ?? {}) as Record<string, unknown>;
  if (typeof workspaceId !== 'string' || workspaceId === '' || typeof conversationId !== 'string') throw new Error('invalid');
  return { workspaceId, conversationId };
}

export function readLastLocation(): LastLocation | null {
  try {
    return createStorage('local').getJSON(LAST_LOCATION_KEY, isLastLocation);
  } catch {
    return null;
  }
}

export function writeLastLocation(location: LastLocation | null): void {
  try {
    const storage = createStorage('local');
    if (location === null) storage.remove(LAST_LOCATION_KEY);
    else storage.setJSON(LAST_LOCATION_KEY, location);
  } catch {
    // Not remembered.
  }
}

/** The react-query key the sidebar reads a folder's threads under; invalidate it after `recordThread`. */
export const threadsQueryKey = (workspaceId: string) => ['workspace', 'threads', workspaceId] as const;
