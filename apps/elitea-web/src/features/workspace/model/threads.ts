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
 * A thread archived from the sidebar keeps its record here and is hidden
 * while its id is in the archived set (`shared/desktop/archivedThreads`), so
 * unarchiving the conversation brings it back; a deleted thread is forgotten
 * (`forgetThread`).
 *
 * The same module remembers where the person last was (folder + thread), so
 * the desktop opens there after sign-in.
 */
import { archivedThreadIds, LOCAL_THREADS_KEY } from '@/shared/desktop/archivedThreads';
import { createStorage } from '@/shared/lib/storage';

export interface WorkspaceThread {
  /** The conversation id. */
  id: string;
  title: string;
  /** Epoch ms of the last turn started in it. */
  updatedAt: number;
  /** A rewound thread's source: the id of the thread it was rewound into, listed under it as the earlier attempt. */
  earlierAttemptOf?: string;
  /** The title is the conversation's own name (generated, renamed, or read back from the server): it is not generated again. */
  named?: boolean;
}

export interface LastLocation {
  workspaceId: string;
  /** '' when the person was on a new, not yet started thread. */
  conversationId: string;
}

const MAX_THREADS = 50;
const MAX_TITLE = 80;
/** The longest name a rename (and so a thread's title from the server) has. */
const MAX_NAME = 256;
const threadsKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.threads`;
const LAST_LOCATION_KEY = 'desktop.lastLocation';

function isThread(raw: unknown): raw is WorkspaceThread {
  if (typeof raw !== 'object' || raw === null) return false;
  const { id, title, updatedAt, earlierAttemptOf, named } = raw as Record<string, unknown>;
  return (
    typeof id === 'string' &&
    id !== '' &&
    typeof title === 'string' &&
    typeof updatedAt === 'number' &&
    (earlierAttemptOf === undefined || typeof earlierAttemptOf === 'string') &&
    (named === undefined || typeof named === 'boolean')
  );
}

function validThreads(raw: unknown): WorkspaceThread[] {
  if (!Array.isArray(raw)) throw new Error('not a list');
  return raw.filter(isThread);
}

/** Every remembered thread, archived ones included. Never throws: unavailable or corrupt storage reads as "no threads". */
function readAllThreads(workspaceId: string): WorkspaceThread[] {
  try {
    return createStorage('local').getJSON(threadsKey(workspaceId), validThreads) ?? [];
  } catch {
    return [];
  }
}

/** The folder's threads as listed: without the archived ones. Never throws. */
export function readThreads(workspaceId: string): WorkspaceThread[] {
  const archived = archivedThreadIds();
  return readAllThreads(workspaceId).filter((thread) => !archived.has(thread.id));
}

function writeThreads(workspaceId: string, next: readonly WorkspaceThread[]): void {
  try {
    createStorage('local').setJSON(threadsKey(workspaceId), next);
  } catch {
    // Storage unavailable: the change is not remembered.
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
  const next = placed(readAllThreads(workspaceId), { ...thread, title }, rewoundFrom).slice(0, MAX_THREADS);
  writeThreads(workspaceId, next);
  return next;
}

/** Gives thread `id` the conversation's name `title` in place (its place and time stay); marks it named. */
export function renameThread(workspaceId: string, id: string, title: string): WorkspaceThread[] {
  const next = readAllThreads(workspaceId).map((thread) => (thread.id === id ? { ...thread, title: title.trim().slice(0, MAX_NAME), named: true } : thread));
  writeThreads(workspaceId, next);
  return next;
}

/** Forgets thread `id` (deleted); its earlier attempts stay, each standing alone. */
export function forgetThread(workspaceId: string, id: string): void {
  const all = readAllThreads(workspaceId);
  if (!all.some((thread) => thread.id === id)) return;
  writeThreads(
    workspaceId,
    all.filter((thread) => thread.id !== id).map(({ earlierAttemptOf, ...thread }) => (earlierAttemptOf === id || earlierAttemptOf === undefined ? thread : { ...thread, earlierAttemptOf })),
  );
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
export const threadsQueryKey = (workspaceId: string) => [...LOCAL_THREADS_KEY, workspaceId] as const;
