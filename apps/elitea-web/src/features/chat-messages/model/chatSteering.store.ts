/**
 * Steering messages of running chat turns (client contract 1.27), by answer.
 *
 * The store keys each list by the answer's `response_message_id`, because
 * every `SteeringMessage` carries that id. So an SSE frame can go into the
 * store without a closure over the conversation on screen
 * (`applySteeringEvent` is a plain function), and the transcript reads the
 * list of each answer row by its own id.
 *
 * `own` records which messages THIS tab queued, with the project and the
 * conversation they belong to. Only the client that queued a message sends
 * it again when the turn ends before injection, so this part survives a
 * reload (`sessionStorage`), with the Idempotency-Key and the question id of
 * an open hand-off (so a retry after a reload gets the same answer and sends
 * the same question), and with `held` (the user stopped the turn: the client
 * offers the texts and does not send them by itself). The lists themselves
 * do not: after a reload the GET of the list and the SSE replay fill them
 * again.
 *
 * CARRY-OVER. When the author sent a newer message before the hand-off, the
 * server moves the leftover messages of the old answer to the newer answer
 * (`steer.queued`: the same id, the newer `response_message_id`, a higher
 * `revision`). `apply` then moves the item AND this tab's ownership of the id
 * to the newer answer, so the hand-off of the newer answer sends it, and the
 * hand-off of the old answer does not. The settle of the old answer can
 * forget an id before the frame comes, so `released` keeps the ids that this
 * tab owned last, for this page life.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { EXECUTION_EVENT, STEERING_STATE, foldSteeringByAnswer, isSteeringCarryOver, parseSteeringMessage, type SteeringItem } from '@elitea/client-core/stream';

import type { ExecutionEventData } from '@/shared/api/sse';
import { createStorage } from '@/shared/lib/storage';

/** The messages that this tab queued against one answer. */
interface OwnSteeringRecord {
  readonly projectId: string;
  /** The A17 queue key of the conversation (`chatBoxQueueKey`). */
  readonly conversationKey: string;
  readonly ids: readonly string[];
  /** The Idempotency-Key of the hand-off that is not done yet, if one started. */
  readonly handoffKey?: string;
  /** The question id of the send that follows that hand-off (the hand-off body names it). */
  readonly handoffQuestionId?: string;
  /** The user stopped the turn: the client offers the leftover texts, it does not send them by itself. */
  readonly held?: true;
}

/** The keys of one hand-off: they stay the same on every retry. */
export interface SteeringHandoff {
  readonly key: string;
  readonly questionId: string;
}

/** An id that this tab owned and forgot, with the answer and the record it had. */
interface ReleasedSteering {
  readonly from: string;
  /** The revision that the tab knew last, if it still had the item. */
  readonly revision?: number;
  readonly projectId: string;
  readonly conversationKey: string;
}

interface ChatSteeringState {
  readonly items: Readonly<Record<string, readonly SteeringItem[]>>;
  readonly own: Readonly<Record<string, OwnSteeringRecord>>;
  /** Answers whose turn refused steering (`steering_not_supported`, a pipeline). */
  readonly unsupported: Readonly<Record<string, true>>;
  /** Own ids that `dropOwn` forgot, by id (in memory only): a later carry-over of one is still this tab's. */
  readonly released: Readonly<Record<string, ReleasedSteering>>;
  /** Counts the own items that came to an answer by a carry-over; a change starts the end-of-turn rule again. */
  readonly carried: number;
  /** Folds items into the list of the answer. A carry-over moves the item and its ownership (see the file comment). */
  readonly apply: (responseMessageId: string, incoming: readonly SteeringItem[]) => void;
  readonly addOwn: (responseMessageId: string, record: Pick<OwnSteeringRecord, 'projectId' | 'conversationKey'>, id: string) => void;
  /** Forgets own ids (sent again, or final). The record goes when it has no ids. */
  readonly dropOwn: (responseMessageId: string, ids: readonly string[]) => void;
  /** Removes items from the view (they became the next message). */
  readonly forget: (responseMessageId: string, ids: readonly string[]) => void;
  readonly markUnsupported: (responseMessageId: string) => void;
  /** Keeps (a value) or forgets (`undefined`) the keys of the open hand-off of the answer. */
  readonly setHandoff: (responseMessageId: string, handoff: SteeringHandoff | undefined) => void;
  /** Marks the record of the answer `held` (the user stopped the turn). */
  readonly hold: (responseMessageId: string) => void;
}

const STORAGE_KEY = 'chat.steeringOwn';
const MAX_RECORDS = 50;
const MAX_IDS = 30;
const MAX_RELEASED = 200;

function session(): ReturnType<typeof createStorage> | undefined {
  try {
    return createStorage('session');
  } catch {
    // Handled: without Web Storage the record lives in memory for this page life.
    return undefined;
  }
}

/** Reads the persisted `own` map. Bad rows are skipped. */
function parseOwnRecords(raw: unknown): Record<string, OwnSteeringRecord> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return {};
  const out: Record<string, OwnSteeringRecord> = {};
  for (const [responseId, value] of Object.entries(raw as Record<string, unknown>).slice(-MAX_RECORDS)) {
    if (typeof value !== 'object' || value === null) continue;
    const row = value as Record<string, unknown>;
    const ids = Array.isArray(row['ids']) ? row['ids'].filter((id): id is string => typeof id === 'string').slice(-MAX_IDS) : [];
    if (typeof row['projectId'] !== 'string' || typeof row['conversationKey'] !== 'string' || ids.length === 0) continue;
    const key = row['handoffKey'];
    const question = row['handoffQuestionId'];
    // A hand-off needs both keys: a record with only one starts a new hand-off.
    const handoff = typeof key === 'string' && key !== '' && typeof question === 'string' && question !== '' ? { handoffKey: key, handoffQuestionId: question } : {};
    out[responseId] = { projectId: row['projectId'], conversationKey: row['conversationKey'], ids, ...handoff, ...(row['held'] === true ? { held: true as const } : {}) };
  }
  return out;
}

function saveOwn(own: Readonly<Record<string, OwnSteeringRecord>>): void {
  try {
    session()?.setJSON(STORAGE_KEY, own);
  } catch {
    // Handled: a full quota keeps the in-memory record for this page life.
  }
}

type ChatSteeringStore = UseBoundStore<StoreApi<ChatSteeringState>>;

function createChatSteeringStore(): ChatSteeringStore {
  return create<ChatSteeringState>((set, get) => {
    const writeOwn = (own: Record<string, OwnSteeringRecord>): void => {
      saveOwn(own);
      set({ own });
    };
    return {
      items: {},
      own: parseOwnRecords(session()?.getJSON<unknown>(STORAGE_KEY)),
      unsupported: {},
      released: {},
      carried: 0,
      apply: (responseMessageId, incoming) => {
        if (incoming.length === 0) return;
        const { items, own, released, carried } = get();
        const { lists, moved } = foldSteeringByAnswer(items, responseMessageId, incoming);
        const nextOwn = { ...own };
        const nextReleased = { ...released };
        let adopted = 0;
        // This tab owned the id under another answer: now it owns it under this answer.
        const take = (id: string, record: Pick<OwnSteeringRecord, 'projectId' | 'conversationKey'>): void => {
          const target = nextOwn[responseMessageId];
          const ids = target?.ids ?? [];
          if (!ids.includes(id)) nextOwn[responseMessageId] = { projectId: record.projectId, conversationKey: record.conversationKey, ...target, ids: [...ids, id].slice(-MAX_IDS) };
          delete nextReleased[id];
          adopted += 1;
        };
        for (const move of moved) {
          const from = nextOwn[move.from];
          if (from?.ids.includes(move.id)) {
            const left = from.ids.filter((id) => id !== move.id);
            if (left.length === 0) delete nextOwn[move.from];
            else nextOwn[move.from] = { ...from, ids: left };
            take(move.id, from);
          } else {
            const gone = nextReleased[move.id];
            if (gone !== undefined) take(move.id, gone);
          }
        }
        // The settle of the old answer forgot the item and the id first: the released record still knows the owner.
        for (const item of incoming) {
          const gone = nextReleased[item.id];
          const here = (lists[responseMessageId] ?? []).some((held) => held.id === item.id);
          if (gone !== undefined && here && isSteeringCarryOver(gone.from, item, gone.revision)) take(item.id, gone);
        }
        if (adopted === 0) {
          set({ items: lists });
          return;
        }
        saveOwn(nextOwn);
        set({ items: lists, own: nextOwn, released: nextReleased, carried: carried + adopted });
      },
      addOwn: (responseMessageId, record, id) => {
        const own = get().own;
        const current = own[responseMessageId];
        const ids = current?.ids ?? [];
        if (ids.includes(id)) return;
        writeOwn({ ...own, [responseMessageId]: { ...current, ...record, ids: [...ids, id].slice(-MAX_IDS) } });
      },
      dropOwn: (responseMessageId, ids) => {
        const own = { ...get().own };
        const current = own[responseMessageId];
        if (current === undefined) return;
        const left = current.ids.filter((id) => !ids.includes(id));
        if (left.length === 0) delete own[responseMessageId];
        else own[responseMessageId] = { ...current, ids: left };
        const released = { ...get().released };
        for (const id of current.ids.filter((owned) => ids.includes(owned))) {
          const revision = get().items[responseMessageId]?.find((item) => item.id === id)?.revision;
          delete released[id];
          released[id] = { from: responseMessageId, projectId: current.projectId, conversationKey: current.conversationKey, ...(revision === undefined ? {} : { revision }) };
        }
        set({ released: Object.fromEntries(Object.entries(released).slice(-MAX_RELEASED)) });
        writeOwn(own);
      },
      forget: (responseMessageId, ids) => {
        const items = get().items;
        const current = items[responseMessageId];
        if (current === undefined) return;
        set({ items: { ...items, [responseMessageId]: current.filter((item) => !ids.includes(item.id)) } });
      },
      setHandoff: (responseMessageId, handoff) => {
        const own = get().own;
        const current = own[responseMessageId];
        if (current === undefined || (current.handoffKey === handoff?.key && current.handoffQuestionId === handoff?.questionId)) return;
        const { handoffKey: _key, handoffQuestionId: _question, ...rest } = current;
        writeOwn({ ...own, [responseMessageId]: handoff === undefined ? rest : { ...rest, handoffKey: handoff.key, handoffQuestionId: handoff.questionId } });
      },
      hold: (responseMessageId) => {
        const own = get().own;
        const current = own[responseMessageId];
        if (current === undefined || current.held === true) return;
        writeOwn({ ...own, [responseMessageId]: { ...current, held: true } });
      },
      markUnsupported: (responseMessageId) => {
        set({ unsupported: { ...get().unsupported, [responseMessageId]: true } });
      },
    };
  });
}

let instance: ChatSteeringStore | undefined;

function resolveStore(): ChatSteeringStore {
  instance ??= createChatSteeringStore();
  return instance;
}

function useChatSteeringStoreHook<T>(selector: (state: ChatSteeringState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily built singleton, with the hook + `getState` surface of the other stores here. */
export const useChatSteeringStore = Object.assign(useChatSteeringStoreHook, {
  getState: (): ChatSteeringState => resolveStore().getState(),
  /** Tests only: drop the singleton so that the next read starts again from `sessionStorage`. */
  resetForTests: (): void => {
    instance = undefined;
  },
});

/**
 * Folds one steering SSE frame into the store. `steer.dropped` carries the
 * message plus `reason` (`removed`); it is shown as removed whatever state
 * the frame names. A `steer.queued` frame can name a newer answer than the
 * item had (a carry-over): `apply` moves it. A frame that is not a
 * `SteeringMessage` is skipped.
 */
export function applySteeringEvent(event: string, frame: ExecutionEventData): void {
  const parsed = parseSteeringMessage(frame);
  if (parsed === undefined) return;
  const item = event === EXECUTION_EVENT.steerDropped ? { ...parsed, state: STEERING_STATE.Removed } : parsed;
  useChatSteeringStore.getState().apply(item.responseMessageId, [item]);
}
