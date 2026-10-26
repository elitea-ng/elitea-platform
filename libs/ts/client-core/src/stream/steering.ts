/**
 * Steering messages (client contract 1.26): pure helpers that the web and the
 * mobile clients share.
 *
 * A message that the user sends while a cloud turn runs goes to the server as
 * a steering message. The worker injects it at the next safe point. The
 * client keeps one list per running answer and folds three sources into it:
 * the POST/PATCH answers, the `GET .../steering` list and the `steer.*` SSE
 * events. These helpers do the folding, so that both clients apply the same
 * rules.
 */
import { STEERING_STATE, type SteeringState } from './frameTypes';

/** One steering message, as the clients keep it. */
export interface SteeringItem {
  readonly id: string;
  readonly responseMessageId: string;
  readonly authorId: number | null;
  readonly content: string;
  readonly state: SteeringState;
  readonly revision: number;
  readonly createdAt: string;
  /** The conversation message that holds the text, after injection. */
  readonly messageId: string | null;
}

/** The `error` codes that the steering routes can send. */
export const STEERING_ERROR = {
  invalid: 'steering_invalid',
  idempotencyKeyRequired: 'idempotency_key_required',
  turnNotRunning: 'turn_not_running',
  notSupported: 'steering_not_supported',
  queueFull: 'steering_queue_full',
  idempotencyConflict: 'idempotency_conflict',
  notFound: 'steering_not_found',
  notPending: 'steering_not_pending',
  rateLimited: 'rate_limited',
} as const;

/** The largest steering text, in UTF-8 bytes (the contract's 16 KiB). */
export const MAX_STEERING_BYTES = 16 * 1024;

/** The text that joins the leftover messages into one next message. */
export const STEERING_JOIN = '\n\n';

const KNOWN_STATES: ReadonlySet<string> = new Set<string>(Object.values(STEERING_STATE));

/** The state of a wire value. The contract says: an unknown value is `pending`. */
export function steeringStateOf(raw: unknown): SteeringState {
  return typeof raw === 'string' && KNOWN_STATES.has(raw) ? (raw as SteeringState) : STEERING_STATE.Pending;
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * Reads one `SteeringMessage` from the wire (a REST body or the `data` of a
 * `steer.*` event). Returns `undefined` when the value has no `id`, no
 * `response_message_id` or no `content`. Other fields are optional here, so
 * that a partial frame does not stop the queue view.
 */
export function parseSteeringMessage(raw: unknown): SteeringItem | undefined {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return undefined;
  const row = raw as Record<string, unknown>;
  const id = text(row['id']);
  const responseMessageId = text(row['response_message_id']);
  const content = row['content'];
  if (id === undefined || responseMessageId === undefined || typeof content !== 'string') return undefined;
  const revision = row['revision'];
  const author = row['author_id'];
  return {
    id,
    responseMessageId,
    authorId: typeof author === 'number' && Number.isFinite(author) ? author : null,
    content,
    state: steeringStateOf(row['state']),
    revision: typeof revision === 'number' && Number.isFinite(revision) ? revision : 1,
    createdAt: typeof row['created_at'] === 'string' ? row['created_at'] : '',
    messageId: text(row['message_id']) ?? null,
  };
}

/** Reads the `items` of a `SteeringMessageList`. Bad rows are skipped. */
export function parseSteeringList(raw: unknown): SteeringItem[] {
  if (typeof raw !== 'object' || raw === null) return [];
  const items = (raw as Record<string, unknown>)['items'];
  if (!Array.isArray(items)) return [];
  return items.map(parseSteeringMessage).filter((item): item is SteeringItem => item !== undefined);
}

/** The `error` code of a refused steering call body, if it has one. */
export function steeringErrorCode(body: unknown): string | undefined {
  if (typeof body !== 'object' || body === null) return undefined;
  return text((body as Record<string, unknown>)['error']);
}

/** A state that cannot change again. `pending` is the only open state. */
function isFinal(state: SteeringState): boolean {
  return state !== STEERING_STATE.Pending;
}

/**
 * Puts `incoming` into `items` (by `id`) and returns a new list in queue
 * order. An older copy never replaces a newer one: a higher `revision` wins,
 * and for the same revision a final state wins over `pending`. This keeps the
 * view correct when a replayed SSE event comes after a GET.
 */
export function mergeSteering(items: readonly SteeringItem[], incoming: SteeringItem): SteeringItem[] {
  const at = items.findIndex((item) => item.id === incoming.id);
  if (at === -1) return [...items, incoming];
  const current = items[at] as SteeringItem;
  if (incoming.revision < current.revision) return [...items];
  if (incoming.revision === current.revision && isFinal(current.state) && !isFinal(incoming.state)) return [...items];
  const next = [...items];
  next[at] = { ...incoming, messageId: incoming.messageId ?? current.messageId };
  return next;
}

/** Merges each of `incoming` into `items`, in order. */
export function mergeSteeringAll(items: readonly SteeringItem[], incoming: readonly SteeringItem[]): SteeringItem[] {
  return incoming.reduce<SteeringItem[]>((list, item) => mergeSteering(list, item), [...items]);
}

/**
 * The items to show in the transcript. A removed item is not shown. An
 * injected item whose `messageId` is already a row of the transcript is not
 * shown either: after a reload the server sends it as an ordinary user
 * message, and the client must not show it two times.
 */
export function visibleSteering(items: readonly SteeringItem[], transcriptIds: ReadonlySet<string>): SteeringItem[] {
  return items.filter((item) => {
    if (item.state === STEERING_STATE.Removed) return false;
    return item.messageId === null || !transcriptIds.has(item.messageId);
  });
}

/**
 * The items of this client that the turn did not read: they must go as the
 * next message. Only `own` ids count, because only the client that queued a
 * message sends it again. With `turnEnded` false, a `pending` item does not
 * count (the turn can still inject it); only `turn_ended` does.
 */
export function leftoverSteering(items: readonly SteeringItem[], own: ReadonlySet<string>, turnEnded: boolean): SteeringItem[] {
  return items.filter((item) => {
    if (!own.has(item.id)) return false;
    if (item.state === STEERING_STATE.TurnEnded) return true;
    return turnEnded && item.state === STEERING_STATE.Pending;
  });
}

/** The one next message that the leftover items become. Blank texts are skipped. */
export function joinSteeringTexts(items: readonly Pick<SteeringItem, 'content'>[]): string {
  return items
    .map((item) => item.content)
    .filter((content) => content.trim() !== '')
    .join(STEERING_JOIN);
}

/** True when the text is not blank and fits the contract's size limit. */
export function isSteeringTextValid(value: string): boolean {
  return value.trim() !== '' && new TextEncoder().encode(value).length <= MAX_STEERING_BYTES;
}
