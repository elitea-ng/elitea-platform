import type { ConversationSummary } from '@/api/queries/conversations';
import type { SearchHit } from '@/db/search';
import type { ChatListItem } from '@/features/chats/ConversationList';
import { filterConversations } from '@/features/chats/filterConversations';
import { formatAge, parseServerTime } from '@/lib/time';
import { applyOverlay, type ConversationOverlay } from '@/sync/conversationOps';

/**
 * What the Chats list shows, from its three sources:
 * - the loaded pages (the workspace's answer, or the saved copy offline);
 * - the saved copy's pinned rows (the synced list is complete, the loaded pages are not:
 *   a chat pinned long ago may sit on page five, and Pinned is always on top);
 * - the queued actions (rename, pin, delete) not sent yet, applied over both.
 * Pinned rows come first, newest first; the rest keep the workspace's order.
 */
export function buildChatRows({
  loaded,
  savedPinned,
  overlay,
  hidden,
  now,
}: {
  loaded: readonly ConversationSummary[];
  savedPinned: readonly ConversationSummary[];
  overlay: ReadonlyMap<number, ConversationOverlay>;
  /** Rows on their way out (a delete waiting out its undo). */
  hidden?: ReadonlySet<number>;
  now?: number;
}): ChatListItem[] {
  const byId = new Set(loaded.map((row) => row.id));
  const merged = [...loaded, ...savedPinned.filter((row) => !byId.has(row.id))];
  const visible = applyOverlay(merged, overlay).filter((row) => !hidden?.has(row.id));
  const pinned = visible.filter((row) => row.is_pinned === true);
  pinned.sort((a, b) => (parseServerTime(b.updated_at) ?? 0) - (parseServerTime(a.updated_at) ?? 0) || b.id - a.id);
  const rest = visible.filter((row) => row.is_pinned !== true);
  return [...pinned, ...rest].map((row) => toItem(row, now));
}

function toItem(row: Pick<ConversationSummary, 'id' | 'name' | 'updated_at' | 'is_pinned'>, now?: number, snippet: ChatListItem['snippet'] = null): ChatListItem {
  return { id: row.id, name: row.name, updated_at: row.updated_at, age: formatAge(row.updated_at, now), pinned: row.is_pinned === true, snippet };
}

/**
 * Search results: the offline index's hits (titles, then messages, with the matched passage),
 * then any loaded row whose title matches that the index does not hold yet (the store is off or
 * not synced). Without an index (`hits` null) it is the title filter over the loaded rows.
 */
export function buildSearchRows({
  hits,
  loaded,
  query,
  overlay,
  hidden,
  now,
}: {
  hits: readonly SearchHit[] | null;
  loaded: readonly ConversationSummary[];
  query: string;
  overlay: ReadonlyMap<number, ConversationOverlay>;
  hidden?: ReadonlySet<number>;
  now?: number;
}): ChatListItem[] {
  const fromIndex = (hits ?? []).map((hit) => ({
    id: hit.id,
    name: hit.name,
    updated_at: hit.updatedAt,
    is_pinned: hit.row?.is_pinned === true,
    snippet: hit.snippet,
  }));
  const seen = new Set(fromIndex.map((hit) => hit.id));
  const byTitle = filterConversations(applyOverlay(loaded, overlay), query)
    .filter((row) => !seen.has(row.id))
    .map((row) => ({ id: row.id, name: row.name, updated_at: row.updated_at, is_pinned: row.is_pinned === true, snippet: null }));
  return applyOverlay([...fromIndex, ...byTitle], overlay)
    .filter((row) => !hidden?.has(row.id))
    .map((row) => toItem(row, now, row.snippet));
}
