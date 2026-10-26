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
 * reload (`sessionStorage`), with the Idempotency-Key of an open hand-off
 * (so a retry after a reload gets the same answer). The lists themselves do not: after a reload the
 * GET of the list and the SSE replay fill them again.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { EXECUTION_EVENT, STEERING_DROP_REASON, STEERING_STATE, mergeSteeringAll, parseSteeringMessage, steeringDropReason, type SteeringItem } from '@elitea/client-core/stream';

import type { ExecutionEventData } from '@/shared/api/sse';
import { createStorage } from '@/shared/lib/storage';

/** The messages that this tab queued against one answer. */
export interface OwnSteeringRecord {
  readonly projectId: string;
  /** The A17 queue key of the conversation (`chatBoxQueueKey`). */
  readonly conversationKey: string;
  readonly ids: readonly string[];
  /** The Idempotency-Key of the hand-off that is not done yet, if one started. */
  readonly handoffKey?: string;
}

/** A steering message that the server did not send: the author sent a newer message first. */
export interface SupersededSteering {
  readonly id: string;
  readonly content: string;
  /** The A17 queue key of the conversation that shows the notice. */
  readonly conversationKey: string;
}

interface ChatSteeringState {
  readonly items: Readonly<Record<string, readonly SteeringItem[]>>;
  readonly own: Readonly<Record<string, OwnSteeringRecord>>;
  /** Answers whose turn refused steering (`steering_not_supported`, a pipeline). */
  readonly unsupported: Readonly<Record<string, true>>;
  readonly apply: (responseMessageId: string, incoming: readonly SteeringItem[]) => void;
  readonly addOwn: (responseMessageId: string, record: Omit<OwnSteeringRecord, 'ids' | 'handoffKey'>, id: string) => void;
  /** Forgets own ids (sent again, or final). The record goes when it has no ids. */
  readonly dropOwn: (responseMessageId: string, ids: readonly string[]) => void;
  /** Removes items from the view (they became the next message). */
  readonly forget: (responseMessageId: string, ids: readonly string[]) => void;
  readonly markUnsupported: (responseMessageId: string) => void;
  /** Superseded messages (`steer.dropped`, `reason: superseded`) that the notice shows. */
  readonly superseded: readonly SupersededSteering[];
  /** Ids that were superseded before, so a replayed event does not show a closed notice again. */
  readonly supersededSeen: readonly string[];
  readonly addSuperseded: (entry: SupersededSteering) => void;
  /** Closes the notice of these ids. */
  readonly dismissSuperseded: (ids: readonly string[]) => void;
  /** Keeps (a string) or forgets (`undefined`) the key of the open hand-off of the answer. */
  readonly setHandoffKey: (responseMessageId: string, key: string | undefined) => void;
}

const STORAGE_KEY = 'chat.steeringOwn';
const MAX_RECORDS = 50;
const MAX_IDS = 30;
const MAX_SEEN = 200;

function session(): ReturnType<typeof createStorage> | undefined {
  try {
    return createStorage('session');
  } catch {
    // Handled: without Web Storage the record lives in memory for this page life.
    return undefined;
  }
}

/** Reads the persisted `own` map. Bad rows are skipped. */
export function parseOwnRecords(raw: unknown): Record<string, OwnSteeringRecord> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return {};
  const out: Record<string, OwnSteeringRecord> = {};
  for (const [responseId, value] of Object.entries(raw as Record<string, unknown>).slice(-MAX_RECORDS)) {
    if (typeof value !== 'object' || value === null) continue;
    const row = value as Record<string, unknown>;
    const ids = Array.isArray(row['ids']) ? row['ids'].filter((id): id is string => typeof id === 'string').slice(-MAX_IDS) : [];
    if (typeof row['projectId'] !== 'string' || typeof row['conversationKey'] !== 'string' || ids.length === 0) continue;
    const key = row['handoffKey'];
    out[responseId] = { projectId: row['projectId'], conversationKey: row['conversationKey'], ids, ...(typeof key === 'string' && key !== '' ? { handoffKey: key } : {}) };
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

export function createChatSteeringStore(): ChatSteeringStore {
  return create<ChatSteeringState>((set, get) => {
    const writeOwn = (own: Record<string, OwnSteeringRecord>): void => {
      saveOwn(own);
      set({ own });
    };
    return {
      items: {},
      own: parseOwnRecords(session()?.getJSON<unknown>(STORAGE_KEY)),
      unsupported: {},
      apply: (responseMessageId, incoming) => {
        if (incoming.length === 0) return;
        const items = get().items;
        set({ items: { ...items, [responseMessageId]: mergeSteeringAll(items[responseMessageId] ?? [], incoming) } });
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
        writeOwn(own);
      },
      forget: (responseMessageId, ids) => {
        const items = get().items;
        const current = items[responseMessageId];
        if (current === undefined) return;
        set({ items: { ...items, [responseMessageId]: current.filter((item) => !ids.includes(item.id)) } });
      },
      setHandoffKey: (responseMessageId, key) => {
        const own = get().own;
        const current = own[responseMessageId];
        if (current === undefined || current.handoffKey === key) return;
        const { handoffKey: _old, ...rest } = current;
        writeOwn({ ...own, [responseMessageId]: key === undefined ? rest : { ...rest, handoffKey: key } });
      },
      markUnsupported: (responseMessageId) => {
        set({ unsupported: { ...get().unsupported, [responseMessageId]: true } });
      },
      superseded: [],
      supersededSeen: [],
      addSuperseded: (entry) => {
        const { superseded, supersededSeen } = get();
        if (supersededSeen.includes(entry.id)) return;
        set({ superseded: [...superseded, entry].slice(-MAX_IDS), supersededSeen: [...supersededSeen, entry.id].slice(-MAX_SEEN) });
      },
      dismissSuperseded: (ids) => {
        set({ superseded: get().superseded.filter((entry) => !ids.includes(entry.id)) });
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
 * message plus `reason` (`removed` or `superseded`); it is shown as removed
 * whatever state the frame names. A frame that is not a `SteeringMessage` is skipped.
 *
 * `superseded`: the turn ended first, and the author sent a newer message
 * before the hand-off, so the server did not send this one. The frame comes
 * on the stream of the NEWER turn, but its `response_message_id` names the
 * OLD answer. The item goes to the list of that old answer (hidden), and the
 * text goes to the notice of `conversationKey` (the conversation of the
 * stream), so the user can send it again. The message is final, so this tab
 * no longer owns it.
 */
export function applySteeringEvent(event: string, frame: ExecutionEventData, conversationKey?: string): void {
  const parsed = parseSteeringMessage(frame);
  if (parsed === undefined) return;
  const dropped = event === EXECUTION_EVENT.steerDropped;
  const item = dropped ? { ...parsed, state: STEERING_STATE.Removed } : parsed;
  const store = useChatSteeringStore.getState();
  store.apply(item.responseMessageId, [item]);
  if (!dropped || steeringDropReason(frame) !== STEERING_DROP_REASON.Superseded) return;
  const key = store.own[item.responseMessageId]?.conversationKey ?? conversationKey;
  store.dropOwn(item.responseMessageId, [item.id]);
  if (key !== undefined && item.content.trim() !== '') store.addSuperseded({ id: item.id, content: item.content, conversationKey: key });
}
