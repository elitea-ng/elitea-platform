/**
 * A17 — the "Waiting messages" queue (ELITEA-2864…2874).
 *
 * WHAT IT IS. While a turn is in flight the composer stays live; anything the
 * reader sends lands here instead of on the wire, and is delivered FIFO once
 * the current turn SETTLES. There is no server-side interjection channel on
 * this platform — `agent_start.go`'s `resolveAfterCurrentResponseSettles`
 * refuses (and then waits out) a second start while a response row is still
 * being written — so the queue is the client's half of that contract: it holds
 * the message until the run it would have collided with is over, and then
 * sends it down the ORDINARY send path, so every server-side rule (admission,
 * budget, guardrails, participant resolution) still applies to it unchanged.
 *
 * WHY IT IS A STORE AND NOT COMPONENT STATE. `ChatBox` is not remounted on the
 * `/chat` -> `/chat/{id}` navigation the first send performs (the route renders
 * its own content plus an `<Outlet/>`), but it IS remounted by "+ Create ->
 * Chat" (`useCreateChatReset` keys the subtree) and by a reload. A queue held
 * in component state would be silently destroyed by a reload mid-run, which is
 * the one moment it is guaranteed to be non-empty.
 *
 * PERSISTENCE IS SESSION-SCOPED, ON PURPOSE. `sessionStorage` through
 * `shared/lib/storage`'s namespaced wrapper — so the logout sweep
 * (`clearNamespace()`) takes it, and so a queue cannot outlive the tab that
 * typed it. A queued message is not durable intent; it is "what I was about to
 * say a moment ago", and resurrecting it in a new session days later would put
 * words into a conversation the reader has forgotten about.
 *
 * `interjected` is the second half of the same record: the ids of the questions
 * that WERE delivered from the queue, so the transcript can keep saying "Sent
 * while running" against them after a reload (ELITEA-2870). The id is the
 * client-generated `question_id` the start route persists as the question row's
 * own uuid, so the marker still matches the row the server sends back.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { createStorage } from '@/shared/lib/storage';

/** One message waiting for the current turn to settle. */
export interface QueuedChatMessage {
  /** Client-side identity — the list key, and what `remove` addresses. */
  readonly id: string;
  /** Exactly what the reader typed. Never trimmed: ELITEA-2868 requires the text to survive a Stop verbatim. */
  readonly text: string;
  readonly queuedAt: number;
}

/**
 * The conversation a queue belongs to, before the conversation exists.
 *
 * The first send of a brand-new chat COMMITS the conversation and then routes
 * to it, so anything queued between "send" and "the uuid arrives" is keyed by
 * this sentinel and adopted by the real key a moment later (`adopt`). Without
 * that hand-off the very first interjection of a conversation — the most
 * common one there is — would be stranded under a key nothing reads again.
 */
export const DRAFT_CONVERSATION_KEY = 'draft';

/**
 * Caps. A queue is a few sentences a person typed while waiting, not a buffer:
 * these exist so a runaway caller cannot grow `sessionStorage` without bound
 * (the quota is per-origin and shared with everything else this app persists).
 */
const MAX_QUEUED_PER_CONVERSATION = 50;
const MAX_INTERJECTED_PER_CONVERSATION = 200;

const STORAGE_KEY = 'chat.queuedMessages';

type QueueMap = Readonly<Record<string, readonly QueuedChatMessage[]>>;
type InterjectedMap = Readonly<Record<string, readonly string[]>>;

interface PersistedShape {
  readonly queues: QueueMap;
  readonly interjected: InterjectedMap;
}

interface QueuedMessagesState extends PersistedShape {
  /** Appends `text`; answers the stored row, or `undefined` when the text is blank (ELITEA-2873) or the cap is reached. */
  readonly enqueue: (conversationKey: string, text: string) => QueuedChatMessage | undefined;
  /** Drops one waiting message by id — the list's own remove control. */
  readonly remove: (conversationKey: string, id: string) => void;
  /** Takes the HEAD of the queue (FIFO, ELITEA-2865) and removes it. */
  readonly dequeue: (conversationKey: string) => QueuedChatMessage | undefined;
  readonly clear: (conversationKey: string) => void;
  /** Moves a draft chat's queue onto the conversation the first send created. */
  readonly adopt: (fromKey: string, toKey: string) => void;
  /** Records that `questionId` was delivered from the queue, so the transcript can label it. */
  readonly markInterjected: (conversationKey: string, questionId: string) => void;
}

/* ── persistence ─────────────────────────────────────────────────────────── */

/**
 * Built lazily, never at module scope: `createStorage` reads
 * `window.sessionStorage` when it is CALLED, and this module is imported by
 * files a node-environment test loads without a DOM.
 */
function session(): ReturnType<typeof createStorage> | undefined {
  try {
    return createStorage('session');
  } catch {
    // Handled (§3.6): a context with no Web Storage (a locked-down browser,
    // a node test) keeps the queue in memory rather than failing the render.
    return undefined;
  }
}

/** A queued row, or `undefined` for anything the persisted blob got wrong. */
function readQueuedRow(raw: unknown): QueuedChatMessage | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const row = raw as Record<string, unknown>;
  if (typeof row['id'] !== 'string' || typeof row['text'] !== 'string') return undefined;
  const queuedAt = typeof row['queuedAt'] === 'number' ? row['queuedAt'] : 0;
  return { id: row['id'], text: row['text'], queuedAt };
}

function readStringList(raw: unknown, cap: number): readonly string[] {
  if (!Array.isArray(raw)) return [];
  return raw.filter((entry): entry is string => typeof entry === 'string').slice(-cap);
}

/** Validates the persisted blob field by field — a corrupt value is treated as absent, never thrown (§3.6). */
export function parsePersistedQueues(raw: unknown): PersistedShape {
  if (typeof raw !== 'object' || raw === null) return { queues: {}, interjected: {} };
  const blob = raw as Record<string, unknown>;
  const queues: Record<string, readonly QueuedChatMessage[]> = {};
  const rawQueues = blob['queues'];
  if (typeof rawQueues === 'object' && rawQueues !== null) {
    for (const [key, value] of Object.entries(rawQueues as Record<string, unknown>)) {
      if (!Array.isArray(value)) continue;
      const rows = value.map(readQueuedRow).filter((row): row is QueuedChatMessage => row !== undefined);
      if (rows.length > 0) queues[key] = rows.slice(0, MAX_QUEUED_PER_CONVERSATION);
    }
  }
  const interjected: Record<string, readonly string[]> = {};
  const rawInterjected = blob['interjected'];
  if (typeof rawInterjected === 'object' && rawInterjected !== null) {
    for (const [key, value] of Object.entries(rawInterjected as Record<string, unknown>)) {
      const ids = readStringList(value, MAX_INTERJECTED_PER_CONVERSATION);
      if (ids.length > 0) interjected[key] = ids;
    }
  }
  return { queues, interjected };
}

function loadPersisted(): PersistedShape {
  const store = session();
  if (!store) return { queues: {}, interjected: {} };
  return parsePersistedQueues(store.getJSON<unknown>(STORAGE_KEY));
}

function savePersisted(next: PersistedShape): void {
  const store = session();
  if (!store) return;
  try {
    store.setJSON(STORAGE_KEY, next);
  } catch {
    // Handled (§3.6): a full or disabled quota must not take the composer
    // down with it — the in-memory queue still works for this page life.
  }
}

/* ── store ───────────────────────────────────────────────────────────────── */

type QueuedMessagesStore = UseBoundStore<StoreApi<QueuedMessagesState>>;

/** Drops a key entirely when its list is empty, so the persisted blob does not grow one entry per conversation ever opened. */
function withList<T>(map: Readonly<Record<string, readonly T[]>>, key: string, next: readonly T[]): Readonly<Record<string, readonly T[]>> {
  const copy = { ...map };
  if (next.length === 0) delete copy[key];
  else copy[key] = next;
  return copy;
}

export function createQueuedMessagesStore(): QueuedMessagesStore {
  return create<QueuedMessagesState>((set, get) => {
    const commit = (next: PersistedShape): void => {
      savePersisted(next);
      set(next);
    };
    return {
      ...loadPersisted(),
      enqueue: (conversationKey, text) => {
        // ELITEA-2873: a blank or whitespace-only message is not a message.
        // The composer already refuses to send one; this is the second half of
        // the same rule, for any caller that is not the composer.
        if (text.trim() === '') return undefined;
        const state = get();
        const current = state.queues[conversationKey] ?? [];
        if (current.length >= MAX_QUEUED_PER_CONVERSATION) return undefined;
        const row: QueuedChatMessage = {
          id: crypto.randomUUID(),
          text,
          queuedAt: Date.now(),
        };
        commit({
          queues: withList(state.queues, conversationKey, [...current, row]),
          interjected: state.interjected,
        });
        return row;
      },
      remove: (conversationKey, id) => {
        const state = get();
        const current = state.queues[conversationKey] ?? [];
        if (!current.some((row) => row.id === id)) return;
        commit({
          queues: withList(state.queues, conversationKey, current.filter((row) => row.id !== id)),
          interjected: state.interjected,
        });
      },
      dequeue: (conversationKey) => {
        const state = get();
        const current = state.queues[conversationKey] ?? [];
        const head = current[0];
        if (head === undefined) return undefined;
        commit({
          queues: withList(state.queues, conversationKey, current.slice(1)),
          interjected: state.interjected,
        });
        return head;
      },
      clear: (conversationKey) => {
        const state = get();
        if (state.queues[conversationKey] === undefined) return;
        commit({ queues: withList(state.queues, conversationKey, []), interjected: state.interjected });
      },
      adopt: (fromKey, toKey) => {
        const state = get();
        const moving = state.queues[fromKey] ?? [];
        const movingIds = state.interjected[fromKey] ?? [];
        if (moving.length === 0 && movingIds.length === 0) return;
        let queues = withList(state.queues, fromKey, []);
        queues = withList(queues, toKey, [...(state.queues[toKey] ?? []), ...moving].slice(0, MAX_QUEUED_PER_CONVERSATION));
        let interjected = withList(state.interjected, fromKey, []);
        interjected = withList(interjected, toKey, [...(state.interjected[toKey] ?? []), ...movingIds].slice(-MAX_INTERJECTED_PER_CONVERSATION));
        commit({ queues, interjected });
      },
      markInterjected: (conversationKey, questionId) => {
        if (questionId === '') return;
        const state = get();
        const current = state.interjected[conversationKey] ?? [];
        if (current.includes(questionId)) return;
        commit({
          queues: state.queues,
          interjected: withList(state.interjected, conversationKey, [...current, questionId].slice(-MAX_INTERJECTED_PER_CONVERSATION)),
        });
      },
    };
  });
}

let instance: QueuedMessagesStore | undefined;

function resolveStore(): QueuedMessagesStore {
  instance ??= createQueuedMessagesStore();
  return instance;
}

function useQueuedMessagesStoreHook<T>(selector: (state: QueuedMessagesState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily-constructed singleton, with the same hook + getState/setState surface this codebase's other stores expose. */
export const useQueuedMessagesStore = Object.assign(useQueuedMessagesStoreHook, {
  getState: (): QueuedMessagesState => resolveStore().getState(),
  setState: (partial: Partial<QueuedMessagesState>): void => resolveStore().setState(partial),
  /** Tests only: drop the singleton so the next read re-seeds from (a fresh) `sessionStorage`. */
  resetForTests: (): void => {
    instance = undefined;
  },
});
