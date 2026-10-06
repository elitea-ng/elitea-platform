import { isApiError } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { offlineStore } from '@/db/offlineStore';
import {
  conversationsScope,
  countUnseenNotifications,
  hasSynced,
  notificationsScope,
  readConversation,
  readConversationRows,
  readMessageRows,
  readNotificationRows,
  type ConversationDetail,
  type ConversationMessage,
  type ConversationSummary,
  type Notification,
} from '@/db/repo';
import type { Db } from '@/db/sql';

/**
 * Offline reads: what the list, thread and notification queries show when the
 * workspace cannot be reached and the encrypted store is open. The network
 * stays the first source whenever it answers; these are the fallback.
 *
 * Every value returned here is marked (`isOfflineCopy`) so a screen can say
 * it is showing the copy saved on this device.
 */

/**
 * The thread's message-group shape. Upstream types `message_groups` as loose
 * records (the legacy projection); this is the subset the thread reads.
 */
export interface MessageGroup {
  id: number;
  uuid: string;
  author_participant_id: number;
  is_streaming: boolean;
  created_at: string;
  content: string;
  message_items: { id: number; item_type: string; order_index: number; item_details: Record<string, unknown> }[];
}
type Participant = components['schemas']['ConversationParticipant'];

const offlineCopies = new WeakSet<object>();

function mark<T extends object>(value: T): T {
  offlineCopies.add(value);
  return value;
}

/** Tests: make `value` look like a device copy served in place of the workspace's answer. */
export function __markOfflineCopyForTests<T extends object>(value: T): T {
  return mark(value);
}

export function isOfflineCopy(value: unknown): boolean {
  return typeof value === 'object' && value !== null && offlineCopies.has(value);
}

/** The workspace did not answer at all (as opposed to answering with an error). */
export function isNoAnswer(error: unknown): boolean {
  return isApiError(error) && (error.kind === 'network' || error.kind === 'timeout');
}

const storeDb = () => offlineStore.get();

/**
 * How long a read waits for the workspace before answering from the saved
 * copy (DEF-R5). The REST deadline is 20 s; with a copy on the device, a
 * workspace that has not answered in this time is treated as unreachable, so
 * an offline cold start shows the saved list within seconds. Without a copy
 * the read keeps the full deadline.
 */
export const OFFLINE_READ_DEADLINE_MS = 6_000;

export interface SavedCopySource {
  /** Is the store open (so a saved copy may exist)? */
  isOpen(): boolean;
  /** Told when the store opens or closes. */
  subscribe(listener: () => void): () => void;
}

const appStore: SavedCopySource = {
  isOpen: () => storeDb() !== null,
  subscribe: (listener) => offlineStore.subscribe(listener),
};

export interface FallbackReadOptions {
  signal?: AbortSignal;
  deadlineMs?: number;
  /** Where a saved copy would come from; defaults to the app's offline store. */
  store?: SavedCopySource;
}

/**
 * Reads from the workspace, falling back to the saved copy when the workspace
 * does not answer (network error, deadline), or has not answered within
 * `OFFLINE_READ_DEADLINE_MS` of the store being open AND a saved copy of this
 * read exists: at cold start the first reads are sent before the device lock
 * releases the store, so the short deadline starts when the store opens.
 * Without a saved copy (the reader returns null) the request is left running
 * to its full deadline. Any answered error (401, 404, 5xx) is thrown as
 * before. A caller abort is never a fallback.
 */
export async function readWithFallback<T>(
  online: (signal: AbortSignal | undefined) => Promise<T>,
  offline: () => Promise<T | null>,
  options: FallbackReadOptions = {},
): Promise<T> {
  const store = options.store ?? appStore;
  const deadlineMs = options.deadlineMs ?? OFFLINE_READ_DEADLINE_MS;
  const controller = new AbortController();
  const caller = options.signal;
  let done = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  // Settles with the saved copy once the short deadline has passed AND a copy
  // exists. Without one the read keeps waiting for the workspace (full deadline).
  let answerFromCopy: (value: T) => void = () => {};
  const fromCopy = new Promise<T>((resolve) => {
    answerFromCopy = resolve;
  });
  const arm = () => {
    if (timer !== null || !store.isOpen()) return;
    timer = setTimeout(() => {
      offline()
        .then((stored) => {
          if (stored === null || done || caller?.aborted) return;
          done = true;
          answerFromCopy(stored);
          controller.abort(new Error('offline_read_deadline'));
        })
        .catch(() => {});
    }, deadlineMs);
  };
  arm();
  const unsubscribe = timer === null ? store.subscribe(arm) : () => {};
  const onCallerAbort = () => controller.abort(caller?.reason);
  if (caller?.aborted) controller.abort(caller.reason);
  else caller?.addEventListener?.('abort', onCallerAbort);
  const answered = (async () => {
    try {
      return await online(controller.signal);
    } catch (error) {
      if (!done && isNoAnswer(error) && !caller?.aborted) {
        const stored = await offline();
        if (stored !== null) return stored;
      }
      throw error;
    }
  })();
  try {
    return await Promise.race([answered, fromCopy]);
  } finally {
    done = true;
    if (timer !== null) clearTimeout(timer);
    unsubscribe();
    caller?.removeEventListener?.('abort', onCallerAbort);
  }
}

export async function offlineConversationRows(
  projectId: number,
  offset: number,
  limit: number,
  getDb: () => Db | null = storeDb,
): Promise<{ rows: ConversationSummary[]; total: number } | null> {
  const db = getDb();
  if (db === null) return null;
  const stored = await readConversationRows(db, projectId, limit, offset);
  // A list never synced on this device is no copy (not an empty one).
  if (stored.total === 0 && !(await hasSynced(db, conversationsScope(projectId)))) return null;
  return mark(stored);
}

/**
 * A transcript row → the message-group shape the thread renders. The text
 * becomes one text item, followed by the row's own (attachment) items.
 */
export function toMessageGroup(row: ConversationMessage): MessageGroup | null {
  const id = Number(row.id);
  if (!Number.isSafeInteger(id) || id <= 0) return null;
  const items = Array.isArray(row.message_items) ? row.message_items : [];
  const text = typeof row.content === 'string' && row.content !== '' ? row.content : null;
  return {
    id,
    uuid: typeof row.uid === 'string' ? row.uid : '',
    author_participant_id: typeof row.author_participant_id === 'number' ? row.author_participant_id : 0,
    is_streaming: row.is_streaming === true,
    created_at: typeof row.created_at === 'string' ? row.created_at : '',
    content: text ?? '',
    message_items: [
      ...(text === null ? [] : [{ id: 0, item_type: 'text_message', order_index: -1, item_details: { content: text } }]),
      ...(items as MessageGroup['message_items']),
    ],
  };
}

/**
 * Participants for a transcript read without a saved conversation read: the
 * authors, as `user` or a non-addressable assistant (nothing can be sent
 * without the saved read's agent participant and uuid anyway).
 */
function participantsFromRows(rows: readonly ConversationMessage[]): Participant[] {
  const seen = new Map<number, Participant>();
  for (const row of rows) {
    const id = row.author_participant_id;
    if (typeof id !== 'number' || seen.has(id)) continue;
    seen.set(id, { id, entity_name: row.role === 'user' ? 'user' : 'assistant', meta: null });
  }
  return [...seen.values()];
}

export async function offlineConversation(
  projectId: number,
  ref: string,
  getDb: () => Db | null = storeDb,
): Promise<ConversationDetail | null> {
  const db = getDb();
  if (db === null) return null;
  const record = await readConversation(db, projectId, ref);
  if (record === null) return null;
  const rows = await readMessageRows(db, projectId, record.id);
  const groups = rows.map(toMessageGroup).filter((g): g is MessageGroup => g !== null);
  const base: ConversationDetail = record.detail
    ? { ...record.detail }
    : {
        id: String(record.id),
        project_id: String(projectId),
        name: record.row?.name ?? '',
        created_at: record.row?.created_at ?? '',
        updated_at: record.row?.updated_at ?? '',
        created_by: '',
        // Unknown on this device: read as private, as the server does.
        is_private: true,
        message_count: rows.length,
        meta: (record.row?.meta as Record<string, unknown> | null | undefined) ?? {},
        participants: participantsFromRows(rows),
      };
  // Newest first, as the network read answers with sort_order=desc.
  return mark({ ...base, message_groups: groups.reverse() as unknown as Record<string, unknown>[] });
}

export async function offlineNotifications(
  projectId: number,
  limit: number,
  getDb: () => Db | null = storeDb,
): Promise<Notification[] | null> {
  const db = getDb();
  if (db === null) return null;
  const rows = await readNotificationRows(db, projectId, limit);
  if (rows.length === 0 && !(await hasSynced(db, notificationsScope(projectId)))) return null;
  return mark(rows);
}

export async function offlineUnseenCount(projectId: number, getDb: () => Db | null = storeDb): Promise<number | null> {
  const db = getDb();
  if (db === null) return null;
  const count = await countUnseenNotifications(db, projectId);
  if (count === 0 && !(await hasSynced(db, notificationsScope(projectId)))) return null;
  return count;
}
