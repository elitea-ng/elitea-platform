import { isConversationRef } from '@/api/queries/conversation';
import { notificationTitle, type Notification } from '@/api/queries/notifications';

/**
 * What a notification row shows and where a tap takes it.
 *
 * `chat_user_mentioned` (client contract 1.1) carries `meta.conversation_id`
 * (the conversation UUID), `meta.message_id` (the mentioning question_id)
 * and `meta.sender {id, name}`. A row a pre-Go deployment wrote has a
 * NUMERIC conversation id and `initiator_name` instead of `sender`;
 * getConversation (the thread route) resolves either id form.
 */

export const MENTION_EVENT = 'chat_user_mentioned';

export interface NotificationView {
  title: string;
  /** The thread to open on tap (`/c/<id>`), when the row names one. */
  conversationRef: string | null;
}

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;

function cleanName(value: unknown): string | null {
  if (typeof value !== 'string') return null;
  // One line, bounded: a display name, not a paragraph.
  const name = value.replace(/\s+/g, ' ').trim();
  return name === '' ? null : name.slice(0, 80);
}

/** The conversation a mention row points at, as a route reference (uuid or numeric id). */
export function mentionConversationRef(meta: unknown): string | null {
  const raw = asRecord(meta)?.conversation_id;
  const ref = typeof raw === 'number' && Number.isSafeInteger(raw) && raw > 0 ? String(raw) : raw;
  return isConversationRef(ref) ? ref.toLowerCase() : null;
}

export function describeNotification(notification: Pick<Notification, 'event_type' | 'meta'>): NotificationView {
  if (notification.event_type !== MENTION_EVENT) {
    return { title: notificationTitle(notification.event_type), conversationRef: null };
  }
  const meta = asRecord(notification.meta);
  const sender = cleanName(asRecord(meta?.sender)?.name) ?? cleanName(meta?.initiator_name);
  return {
    title: sender ? `${sender} mentioned you` : 'You were mentioned',
    conversationRef: mentionConversationRef(meta),
  };
}

/** The inbox's type filter: everything, or one `event_type`. */
export type NotificationFilter = 'all' | string;

export interface FilterOption {
  value: NotificationFilter;
  label: string;
  count: number;
}

/** At most this many type chips; rarer types are still shown under "All". */
export const MAX_FILTER_TYPES = 6;

/** A chip label for an `event_type` (plural for mentions: "Mentions"). */
export function filterLabel(eventType: string): string {
  return eventType === MENTION_EVENT ? 'Mentions' : notificationTitle(eventType);
}

/**
 * The chips for the rows on screen: "All" first, then each event type that
 * occurs, most frequent first (ties by first appearance). No chips when
 * there is only one type (a filter would change nothing).
 */
export function filterOptions(rows: readonly Pick<Notification, 'event_type'>[]): FilterOption[] {
  const counts = new Map<string, number>();
  for (const row of rows) {
    if (typeof row.event_type === 'string' && row.event_type !== '') counts.set(row.event_type, (counts.get(row.event_type) ?? 0) + 1);
  }
  if (counts.size < 2) return [];
  const types = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, MAX_FILTER_TYPES);
  return [
    { value: 'all', label: 'All', count: rows.length },
    ...types.map(([type, count]) => ({ value: type, label: filterLabel(type), count })),
  ];
}

export function applyFilter<T extends Pick<Notification, 'event_type'>>(rows: readonly T[], filter: NotificationFilter): T[] {
  return filter === 'all' ? [...rows] : rows.filter((row) => row.event_type === filter);
}
