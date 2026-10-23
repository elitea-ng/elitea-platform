import type { ConversationSummary } from '@/api/queries/conversations';
import type { SearchHit } from '@/db/search';
import type { ChatListItem } from '@/features/chats/ConversationList';
import { activityGlyphOf, activityStateOf, activityTitleBold, type ActivityRowFields } from '@/features/chats/activity';
import { filterConversations } from '@/features/chats/filterConversations';
import { executorOf } from '@/features/localWork/model';
import { formatAge, parseServerTime } from '@/lib/time';
import { applyOverlay, type ConversationOverlay } from '@/sync/conversationOps';

/**
 * What the Chats list shows, from its three sources:
 * - the loaded pages (the workspace's answer, or the saved copy offline);
 * - the saved copy's pinned rows (the synced list is complete, the loaded pages are not:
 *   a chat pinned long ago may sit on page five, and Pinned is always on top);
 * - the queued actions (rename, pin, archive, delete) not sent yet, applied over both.
 * Pinned rows come first, newest first; the rest keep the workspace's order. Archived rows
 * (client contract 1.10) are not on this list, also while their archive is still queued.
 */
export function buildChatRows({
  loaded,
  savedPinned,
  overlay,
  hidden,
  now,
  localWork = false,
}: {
  loaded: readonly ConversationSummary[];
  savedPinned: readonly ConversationSummary[];
  overlay: ReadonlyMap<number, ConversationOverlay>;
  /** Rows on their way out (a delete waiting out its undo). */
  hidden?: ReadonlySet<number>;
  now?: number;
  /** The Local work list: every row carries where its thread runs (the badge). */
  localWork?: boolean;
}): ChatListItem[] {
  const byId = new Set(loaded.map((row) => row.id));
  const merged = [...loaded, ...savedPinned.filter((row) => !byId.has(row.id))];
  const visible = applyOverlay(merged, overlay).filter((row) => !hidden?.has(row.id) && row.is_archived !== true);
  const pinned = visible.filter((row) => row.is_pinned === true);
  pinned.sort((a, b) => (parseServerTime(b.updated_at) ?? 0) - (parseServerTime(a.updated_at) ?? 0) || b.id - a.id);
  const rest = visible.filter((row) => row.is_pinned !== true);
  return [...pinned, ...rest].map((row) => toItem(row, now, null, localWork));
}

function toItem(
  row: Pick<ConversationSummary, 'id' | 'name' | 'updated_at' | 'is_pinned'> & { workspace_executor?: string } & ActivityRowFields,
  now?: number,
  snippet: ChatListItem['snippet'] = null,
  localWork = false,
): ChatListItem {
  return {
    id: row.id,
    name: row.name,
    updated_at: row.updated_at,
    age: formatAge(row.updated_at, now),
    pinned: row.is_pinned === true,
    snippet,
    // Client contract 1.12: the activity glyph, the bold unread title, the "needs you" count.
    activity: activityGlyphOf(row, now),
    unread: activityTitleBold(row),
    needsYou: activityStateOf(row) === 'needs_you',
    ...(localWork ? { executor: executorOf(row) } : {}),
  };
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
  localWork = false,
}: {
  hits: readonly SearchHit[] | null;
  loaded: readonly ConversationSummary[];
  query: string;
  overlay: ReadonlyMap<number, ConversationOverlay>;
  hidden?: ReadonlySet<number>;
  now?: number;
  localWork?: boolean;
}): ChatListItem[] {
  const fromIndex = (hits ?? []).map((hit) => ({
    id: hit.id,
    name: hit.name,
    updated_at: hit.updatedAt,
    is_pinned: hit.row?.is_pinned === true,
    is_archived: hit.row?.is_archived === true,
    snippet: hit.snippet,
    workspace_executor: hit.row?.workspace_executor,
    activity: hit.row?.activity,
    last_activity_at: hit.row?.last_activity_at,
    unread: hit.row?.unread,
  }));
  const seen = new Set(fromIndex.map((hit) => hit.id));
  const byTitle = filterConversations(applyOverlay(loaded, overlay).filter((row) => row.is_archived !== true), query)
    .filter((row) => !seen.has(row.id))
    .map((row) => ({
      id: row.id,
      name: row.name,
      updated_at: row.updated_at,
      is_pinned: row.is_pinned === true,
      is_archived: false,
      snippet: null,
      workspace_executor: row.workspace_executor,
      activity: row.activity,
      last_activity_at: row.last_activity_at,
      unread: row.unread,
    }));
  return applyOverlay([...fromIndex, ...byTitle], overlay)
    .filter((row) => !hidden?.has(row.id) && row.is_archived !== true)
    .map((row) => toItem(row, now, row.snippet, localWork));
}

/**
 * The Archived chats list (client contract 1.10): the workspace's `archived=only` rows with the
 * queued actions applied; a chat unarchived (or deleted) on this device leaves it at once. No
 * Pinned section: an archived chat keeps its pin, but it is shown with the other archived ones.
 * Each row names who archived it, when the row carries it (`archived_by`).
 */
export function buildArchivedRows({
  loaded,
  overlay,
}: {
  loaded: readonly ConversationSummary[];
  overlay: ReadonlyMap<number, ConversationOverlay>;
}): ChatListItem[] {
  return applyOverlay(loaded, overlay)
    .filter((row) => row.is_archived !== false)
    .map((row) => {
      const by = typeof row.archived_by?.name === 'string' ? row.archived_by.name.trim() : '';
      return { ...toItem({ ...row, is_pinned: false }), archived: true, ...(by !== '' ? { archivedBy: by } : {}) };
    });
}
