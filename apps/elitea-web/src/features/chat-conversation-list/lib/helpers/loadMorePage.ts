import type { Conversation } from '@/entities/conversation';
import type { FolderConversationRef } from '@/entities/folder';

function toConversation(ref: FolderConversationRef): Conversation {
  return {
    id: ref.id,
    name: ref.name ?? '',
    isPrivate: ref.isPrivate ?? true,
    ...(ref.authorId !== undefined ? { authorId: ref.authorId } : {}),
    ...(ref.updatedAt !== undefined ? { updatedAt: ref.updatedAt } : {}),
    ...(ref.createdAt !== undefined ? { createdAt: ref.createdAt } : {}),
    ...(ref.isPlayback !== undefined ? { isPlayback: ref.isPlayback } : {}),
    ...(ref.source !== undefined ? { source: ref.source } : {}),
  };
}

export interface LoadMorePage {
  readonly conversations: readonly FolderConversationRef[];
  readonly total?: number;
}

export interface LoadMoreBucket {
  readonly conversations: readonly Conversation[];
  readonly offset?: number;
  readonly total?: number;
}

export interface MergedBucket {
  readonly conversations: readonly Conversation[];
  readonly offset: number;
  readonly total?: number;
  readonly exhausted: boolean;
}

/** Where the bucket's next page starts. */
export function nextPageOffset(bucket: LoadMoreBucket): number {
  return bucket.offset ?? bucket.conversations.length;
}

/**
 * Whether the bucket's offset no longer points where the server's rows are
 * (#3030). Rows the bucket already holds were deleted, archived, pinned or
 * moved (in another tab, on another device): the bucket's total fell, and
 * the rows behind them moved up past the offset, so the next page would skip
 * them. An empty page whose total still lies above the offset says the same.
 * A page with no total says nothing.
 */
function isStalePage(bucket: LoadMoreBucket, page: LoadMorePage): boolean {
  if (page.total === undefined) return false;
  if (bucket.total !== undefined && page.total < bucket.total) return true;
  return page.conversations.length === 0 && page.total > nextPageOffset(bucket);
}

/**
 * Appends a "Show more" page to a date group or folder: rows already shown
 * (or pinned) are skipped, and the page is kept in server order. The bucket
 * is exhausted once the offset reaches the total, or a page with no total
 * comes back empty. A stale page is the caller's to rule out first
 * (`isStalePage`); `mergeLoadMorePage` does both.
 */
export function appendLoadMorePage(bucket: LoadMoreBucket, page: LoadMorePage, pinnedIds: ReadonlySet<string>): MergedBucket {
  const existingIds = new Set(bucket.conversations.map((c) => c.id));
  const newConversations = page.conversations.filter((c) => !existingIds.has(c.id) && !pinnedIds.has(c.id)).map(toConversation);
  const newOffset = nextPageOffset(bucket) + page.conversations.length;
  const total = page.total ?? bucket.total;
  return {
    conversations: [...bucket.conversations, ...newConversations],
    offset: newOffset,
    ...(total !== undefined ? { total } : {}),
    exhausted: total === undefined ? page.conversations.length === 0 : newOffset >= total,
  };
}

/**
 * A "Show more" page merged into its bucket (`appendLoadMorePage`), unless
 * the page is stale (`isStalePage`): then the bucket comes back as it was,
 * not exhausted, with `refetch` set, and the caller re-reads it from the top
 * (`resyncBucket`).
 */
export function mergeLoadMorePage(bucket: LoadMoreBucket, page: LoadMorePage, pinnedIds: ReadonlySet<string>): MergedBucket & { readonly refetch: boolean } {
  if (!isStalePage(bucket, page)) return { ...appendLoadMorePage(bucket, page, pinnedIds), refetch: false };
  return {
    conversations: bucket.conversations,
    offset: nextPageOffset(bucket),
    ...(bucket.total !== undefined ? { total: bucket.total } : {}),
    exhausted: false,
    refetch: true,
  };
}

/** How many rows the re-read of a stale bucket asks for: what it holds, and one more page. */
export function resyncLimit(bucket: LoadMoreBucket, pageSize: number): number {
  return bucket.conversations.length + pageSize;
}

/**
 * The bucket re-read from offset 0 (`resyncLimit` rows) after a stale page:
 * the server's rows, in its order, replace what the bucket held. A row the
 * bucket held keeps its fields, with the server's on top; a held row the
 * window no longer carries has left the bucket and is dropped.
 */
export function resyncBucket(bucket: LoadMoreBucket, page: LoadMorePage, pinnedIds: ReadonlySet<string>): MergedBucket {
  const held = new Map(bucket.conversations.map((c) => [c.id, c]));
  const conversations = page.conversations.filter((ref) => !pinnedIds.has(ref.id)).map((ref) => ({ ...held.get(ref.id), ...toConversation(ref) }));
  const offset = page.conversations.length;
  const total = page.total ?? bucket.total;
  return {
    conversations,
    offset,
    ...(total !== undefined ? { total } : {}),
    exhausted: total === undefined ? page.conversations.length === 0 : offset >= total,
  };
}
