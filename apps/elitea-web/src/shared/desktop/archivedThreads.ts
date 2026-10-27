/**
 * The desktop's archived Local work threads: the conversation ids the person
 * archived from the sidebar's Local work list. An archived thread's record
 * stays in its folder's thread list (`features/workspace` threads) and is
 * hidden while its id is here; unarchiving the conversation from the
 * Archived chats page takes the id out, so the thread shows under its folder
 * again. Kept on this machine in the `el.` namespace (logout's sweep clears
 * it); the archive itself is the server's (the conversation's own archive).
 *
 * Here, not in the workspace feature, so the Archived chats page (every
 * build) can unarchive without loading the desktop's workspace screens.
 */
import { createStorage } from '@/shared/lib/storage';

const KEY = 'desktop.archivedThreads';
/** The ids kept: the newest archives win. */
const MAX_IDS = 500;

/** The react-query key prefix every folder's thread list is read under (`threadsQueryKey`): invalidate it after a change here. */
export const LOCAL_THREADS_KEY = ['workspace', 'threads'] as const;

function ids(raw: unknown): string[] {
  if (!Array.isArray(raw)) throw new Error('not a list');
  return raw.filter((id): id is string => typeof id === 'string' && id !== '');
}

/** Never throws: unavailable or corrupt storage reads as "none archived". */
export function archivedThreadIds(): ReadonlySet<string> {
  try {
    return new Set(createStorage('local').getJSON(KEY, ids) ?? []);
  } catch {
    return new Set();
  }
}

function write(next: readonly string[]): void {
  try {
    const storage = createStorage('local');
    if (next.length === 0) storage.remove(KEY);
    else storage.setJSON(KEY, next.slice(-MAX_IDS));
  } catch {
    // Storage unavailable: the thread shows again in Local work.
  }
}

/** Hides the thread (conversation `id`) from Local work. */
export function archiveLocalThread(id: string): void {
  write([...[...archivedThreadIds()].filter((known) => known !== id), id]);
}

/** Shows the thread again (its conversation was unarchived); nothing when it was never archived here. */
export function unarchiveLocalThread(id: string): boolean {
  const known = archivedThreadIds();
  if (!known.has(id)) return false;
  write([...known].filter((entry) => entry !== id));
  return true;
}
