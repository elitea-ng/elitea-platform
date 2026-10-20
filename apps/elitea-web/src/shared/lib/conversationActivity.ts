/**
 * The activity status of a conversation list row (client contract 1.12):
 * whether a turn runs, whether the conversation waits for the person, and
 * whether it has an answer the person did not open yet. One source of truth
 * for the web chat list, the desktop sidebar's Chats and its Local work
 * threads: the rows read `resolveRowActivity`, the glyph and the title weight
 * follow from it.
 *
 * The rows come from the listing (`activity`, `last_activity_at`, `unread`).
 * Between two reads, two things change them without a refetch:
 *
 * - a `conversation_activity` frame of the project event stream (a turn
 *   started, paused or stopped, or the person read the conversation on
 *   another device), applied with `applyActivityFrame`;
 * - this client's own read (`markReadLocally`, the optimistic half of
 *   markConversationRead).
 *
 * Both go into one overlay store, keyed by the conversation id (and its
 * UUID, which the Local work threads know). `resolveRowActivity` merges a
 * row with its overlay entry by time, so an old frame never hides a newer
 * listing and a duplicate frame changes nothing.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

/** `activity.state` on the wire. A string, not an enum: an unknown value reads as `idle`. */
export type ConversationActivityState = 'running' | 'needs_you' | 'idle';

export interface RowActivity {
  readonly state: ConversationActivityState;
  /** When the conversation entered `state` (RFC 3339), when known. */
  readonly since?: string | undefined;
  /** The last write of any message (RFC 3339), when known. */
  readonly lastActivityAt?: string | undefined;
  readonly unread: boolean;
}

/** The list row fields this module reads (a domain row, camelCase). */
export interface ActivityRowFields {
  readonly id: string;
  readonly uuid?: string | undefined;
  readonly activity?: { readonly state: ConversationActivityState; readonly since?: string | undefined } | undefined;
  readonly lastActivityAt?: string | undefined;
  readonly unread?: boolean | undefined;
}

function parseActivityState(value: unknown): ConversationActivityState {
  return value === 'running' || value === 'needs_you' ? value : 'idle';
}

/** The wire's `activity` object, or `undefined` for a server before 1.12. */
export function parseActivityWire(value: unknown): { readonly state: ConversationActivityState; readonly since?: string } | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const { state, since } = value as Record<string, unknown>;
  return { state: parseActivityState(state), ...(typeof since === 'string' && since !== '' ? { since } : {}) };
}

/** Epoch ms of an RFC 3339 time, or `null`. */
function activityTime(value: string | undefined): number | null {
  if (value === undefined || value === '') return null;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? null : ms;
}

/* ── the overlay ── */

interface OverlayEntry {
  readonly state?: ConversationActivityState;
  readonly since?: string;
  /** The frame's `last_activity_at`: the overlay's state is newer than a row older than this. */
  readonly lastActivityAt?: string;
  /** A frame said a new answer arrived (after `lastActivityAt`'s row). */
  readonly unreadHint?: boolean;
  /** The person read up to here (epoch ms): an answer at or before it is read. */
  readonly readAt?: number;
}

/** One `conversation_activity` frame, parsed. */
export interface ActivityFrame {
  readonly conversationId: string;
  readonly conversationUuid?: string;
  readonly state?: ConversationActivityState;
  readonly since?: string;
  readonly lastActivityAt?: string;
  readonly unreadHint?: boolean;
  readonly readAt?: string;
}

/** The SSE event name of an activity frame on the project stream. */
export const CONVERSATION_ACTIVITY_EVENT = 'conversation_activity';

/** Parse one frame strictly; `null` for anything else. */
export function parseActivityFrame(data: string): ActivityFrame | null {
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof value !== 'object' || value === null) return null;
  const frame = value as Record<string, unknown>;
  const id = frame['conversation_id'];
  if (typeof id !== 'number' || !Number.isInteger(id) || id <= 0) return null;
  const uuid = textField(frame, 'conversation_uuid');
  const hint = frame['unread_hint'];
  return {
    conversationId: String(id),
    ...(uuid !== undefined ? { conversationUuid: uuid.toLowerCase() } : {}),
    ...(frame['state'] !== undefined ? { state: parseActivityState(frame['state']) } : {}),
    ...optionalText('since', textField(frame, 'since')),
    ...optionalText('lastActivityAt', textField(frame, 'last_activity_at')),
    ...(typeof hint === 'boolean' ? { unreadHint: hint } : {}),
    ...optionalText('readAt', textField(frame, 'read_at')),
  };
}

function textField(frame: Readonly<Record<string, unknown>>, key: string): string | undefined {
  const field = frame[key];
  return typeof field === 'string' && field !== '' ? field : undefined;
}

function optionalText<K extends string>(key: K, value: string | undefined): Partial<Record<K, string>> {
  return value === undefined ? {} : ({ [key]: value } as Record<K, string>);
}

interface OverlayState {
  readonly entries: Readonly<Record<string, OverlayEntry>>;
  /** UUID (lower case) → conversation id, from the frames. */
  readonly uuids: Readonly<Record<string, string>>;
}

type OverlayStore = UseBoundStore<StoreApi<OverlayState>>;

/** A fresh overlay store (tests); the app uses the lazily-built one below. */
function createConversationActivityOverlayStore(): OverlayStore {
  return create<OverlayState>(() => ({ entries: {}, uuids: {} }));
}

let overlayInstance: OverlayStore | undefined;

function overlayStore(): OverlayStore {
  overlayInstance ??= createConversationActivityOverlayStore();
  return overlayInstance;
}

function useOverlayHook<T>(selector: (state: OverlayState) => T): T {
  return overlayStore()(selector);
}

/** The lazily-constructed overlay, with the zustand `getState`/`setState`/`subscribe` surface. */
export const useConversationActivityOverlay = Object.assign(useOverlayHook, {
  getState: (): OverlayState => overlayStore().getState(),
  setState: (update: (state: OverlayState) => Partial<OverlayState>): void => overlayStore().setState(update),
  subscribe: (listener: (state: OverlayState) => void): (() => void) => overlayStore().subscribe(listener),
});

function laterTime(a: string | undefined, b: string | undefined): boolean {
  const left = activityTime(a);
  const right = activityTime(b);
  return left !== null && (right === null || left >= right);
}

function foldFrame(previous: OverlayEntry | undefined, frame: ActivityFrame): OverlayEntry {
  let next: OverlayEntry = previous ?? {};
  // A state frame applies only when it is not older than the overlay's own.
  if (frame.state !== undefined && laterTime(frame.lastActivityAt, next.lastActivityAt)) {
    next = {
      ...next,
      state: frame.state,
      ...(frame.since !== undefined ? { since: frame.since } : {}),
      ...(frame.lastActivityAt !== undefined ? { lastActivityAt: frame.lastActivityAt } : {}),
      ...(frame.unreadHint === true ? { unreadHint: true } : {}),
    };
  }
  if (frame.unreadHint === false) {
    const readAt = activityTime(frame.readAt) ?? Date.now();
    next = { ...next, readAt: Math.max(readAt, next.readAt ?? 0) };
  }
  return next;
}

/** Apply one live frame. Idempotent; an out-of-order frame never moves a row back. */
export function applyActivityFrame(frame: ActivityFrame): void {
  useConversationActivityOverlay.setState((state) => ({
    entries: { ...state.entries, [frame.conversationId]: foldFrame(state.entries[frame.conversationId], frame) },
    uuids: frame.conversationUuid === undefined ? state.uuids : { ...state.uuids, [frame.conversationUuid]: frame.conversationId },
  }));
}

/**
 * The optimistic read allows for this much clock difference between the
 * client and the server; the server's answer (`confirmReadLocally`) then
 * puts the exact marker in its place.
 */
const READ_SKEW_MS = 60_000;

function setReadAt(id: string, readAt: (previous: OverlayEntry) => number): void {
  useConversationActivityOverlay.setState((state) => {
    const key = state.uuids[id.toLowerCase()] ?? id;
    const previous = state.entries[key] ?? {};
    return { entries: { ...state.entries, [key]: { ...previous, readAt: readAt(previous) } } };
  });
}

/**
 * This client read the conversation (the optimistic half of
 * markConversationRead): every answer it knows of is read. `id` is the
 * conversation's numeric id or its UUID.
 */
export function markReadLocally(id: string, now: number = Date.now()): void {
  setReadAt(id, (previous) => Math.max(now + READ_SKEW_MS, activityTime(previous.lastActivityAt) ?? 0, previous.readAt ?? 0));
}

/** The server's read marker for the conversation (markConversationRead's `read_at`). */
export function confirmReadLocally(conversationId: string, readAt: string): void {
  const at = activityTime(readAt);
  if (at !== null) setReadAt(conversationId, () => at);
}

/** Test and sign-out reset. */
export function resetConversationActivityOverlay(): void {
  useConversationActivityOverlay.setState(() => ({ entries: {}, uuids: {} }));
}

function overlayFor(state: OverlayState, row: Pick<ActivityRowFields, 'id' | 'uuid'>): OverlayEntry | undefined {
  const byId = state.entries[row.id];
  if (byId !== undefined || row.uuid === undefined) return byId;
  const uuid = row.uuid.toLowerCase();
  return state.entries[state.uuids[uuid] ?? ''] ?? state.entries[uuid];
}

function rowBase(row: ActivityRowFields): RowActivity {
  return { state: row.activity?.state ?? 'idle', since: row.activity?.since, lastActivityAt: row.lastActivityAt, unread: row.unread === true };
}

/** The overlay's state, when it is not older than the row's. */
function withOverlayState(base: RowActivity, overlay: OverlayEntry): RowActivity {
  if (overlay.state === undefined || !laterTime(overlay.lastActivityAt, base.lastActivityAt)) return base;
  const merged: RowActivity = { ...base, state: overlay.state, since: overlay.since ?? base.since, lastActivityAt: overlay.lastActivityAt };
  // A new answer after the row the listing gave.
  const strictlyNewer = activityTime(overlay.lastActivityAt) !== activityTime(base.lastActivityAt);
  return overlay.unreadHint === true && strictlyNewer ? { ...merged, unread: true } : merged;
}

/** A read at or after the newest activity clears `unread`. */
function withOverlayRead(merged: RowActivity, overlay: OverlayEntry): RowActivity {
  if (!merged.unread || overlay.readAt === undefined) return merged;
  const last = activityTime(merged.lastActivityAt);
  return last === null || last <= overlay.readAt ? { ...merged, unread: false } : merged;
}

/** A row merged with its overlay entry. Pure. */
export function mergeRowActivity(row: ActivityRowFields, overlay: OverlayEntry | undefined): RowActivity {
  const base = rowBase(row);
  return overlay === undefined ? base : withOverlayRead(withOverlayState(base, overlay), overlay);
}

/** The effective activity of one row: the listing's, with what changed since. */
export function useRowActivity(row: ActivityRowFields): RowActivity {
  const overlay = useConversationActivityOverlay((state) => overlayFor(state, row));
  return mergeRowActivity(row, overlay);
}

/** The effective activity of a row, outside React (counts, tests). */
export function resolveRowActivity(row: ActivityRowFields): RowActivity {
  return mergeRowActivity(row, overlayFor(useConversationActivityOverlay.getState(), row));
}

/* ── what the rows show ── */

/** How long a finished, read conversation keeps its grey check. */
const RECENT_FINISH_MS = 24 * 60 * 60 * 1000;

/** The trailing glyph: `null` shows none. */
export type ActivityGlyphKind = 'running' | 'needs_you' | 'unread' | 'done';

/**
 * The glyph of a row. A running turn and a pause win; an unread answer shows
 * the filled check while it is recent, else the bold title alone; a read,
 * recent finish shows the grey check; an old, read conversation shows none.
 */
export function activityGlyph(activity: RowActivity, now: number = Date.now()): ActivityGlyphKind | null {
  if (activity.state === 'running') return 'running';
  if (activity.state === 'needs_you') return 'needs_you';
  const last = activityTime(activity.lastActivityAt);
  const recent = last !== null && now - last < RECENT_FINISH_MS;
  if (!recent) return null;
  return activity.unread ? 'unread' : 'done';
}

/** The title is bold while the row has an answer the person did not open. */
export function activityTitleBold(activity: RowActivity): boolean {
  return activity.unread && activity.state !== 'running';
}

/**
 * A group's "needs you" count. The listing counts the WHOLE group
 * (`needs_you`), including rows it did not page in; the rows it did deliver
 * are recounted from their effective state, so a live change shows at once.
 */
export function countNeedsYou(rows: readonly ActivityRowFields[], serverCount?: number): number {
  const loadedServer = rows.filter((row) => row.activity?.state === 'needs_you').length;
  const loadedLive = rows.filter((row) => resolveRowActivity(row).state === 'needs_you').length;
  return Math.max(0, (serverCount ?? loadedServer) - loadedServer + loadedLive);
}

/** `countNeedsYou`, re-rendered on every overlay change. */
export function useNeedsYouCount(rows: readonly ActivityRowFields[], serverCount?: number): number {
  useConversationActivityOverlay((state) => state.entries);
  return countNeedsYou(rows, serverCount);
}
