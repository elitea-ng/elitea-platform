import { parseServerTime } from '@/lib/time';

export type ChatSectionKey = 'pinned' | 'today' | 'yesterday' | 'week' | 'older' | 'results';

export const CHAT_SECTION_TITLE: Record<ChatSectionKey, string> = {
  pinned: 'Pinned',
  today: 'Today',
  yesterday: 'Yesterday',
  week: 'Previous 7 days',
  older: 'Earlier',
  results: 'Results',
};

export interface ChatSection<T> {
  key: ChatSectionKey;
  title: string;
  data: T[];
}

/** Local midnight of the day `ms` falls in. */
function startOfDay(ms: number): number {
  const d = new Date(ms);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/** The section a conversation updated at `updatedAt` belongs to, by the device's local days. */
export function chatSectionOf(updatedAt: unknown, now: number = Date.now()): ChatSectionKey {
  const ms = parseServerTime(updatedAt);
  if (ms === null) return 'older';
  const today = startOfDay(now);
  if (ms >= today) return 'today';
  const yesterday = startOfDay(today - 1);
  if (ms >= yesterday) return 'yesterday';
  if (ms >= startOfDay(today - 6 * 86_400_000)) return 'week';
  return 'older';
}

/**
 * The Chats list's sections (docs/screens.md §3): Pinned (client contract 1.4 `is_pinned`), then
 * Today, Yesterday, Previous 7 days, Earlier, in that order, each keeping the server's order
 * (newest first). A pinned chat is only in Pinned. Empty sections are left out.
 */
export function groupChatsByDate<T extends { updated_at?: unknown; pinned?: boolean }>(rows: readonly T[], now: number = Date.now()): ChatSection<T>[] {
  const order: ChatSectionKey[] = ['pinned', 'today', 'yesterday', 'week', 'older'];
  const buckets = new Map<ChatSectionKey, T[]>(order.map((key) => [key, []]));
  for (const row of rows) buckets.get(row.pinned === true ? 'pinned' : chatSectionOf(row.updated_at, now))!.push(row);
  return order.filter((key) => buckets.get(key)!.length > 0).map((key) => ({ key, title: CHAT_SECTION_TITLE[key], data: buckets.get(key)! }));
}
