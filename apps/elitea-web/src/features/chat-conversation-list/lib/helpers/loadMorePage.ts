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

export interface MergedBucket {
  readonly conversations: readonly Conversation[];
  readonly offset: number;
  readonly total?: number;
  readonly exhausted: boolean;
}

/**
 * Appends a "Show more" page to a date group or folder: rows already shown
 * (or pinned) are skipped, and the page is kept in server order. The bucket
 * is exhausted once a page comes back empty or the offset reaches the total.
 */
export function mergeLoadMorePage(bucket: { readonly conversations: readonly Conversation[]; readonly offset?: number; readonly total?: number }, page: LoadMorePage, pinnedIds: ReadonlySet<string>): MergedBucket {
  const existingIds = new Set(bucket.conversations.map((c) => c.id));
  const newConversations = page.conversations.filter((c) => !existingIds.has(c.id) && !pinnedIds.has(c.id)).map(toConversation);
  const newOffset = (bucket.offset ?? bucket.conversations.length) + page.conversations.length;
  const total = page.total ?? bucket.total;
  return {
    conversations: [...bucket.conversations, ...newConversations],
    offset: newOffset,
    ...(total !== undefined ? { total } : {}),
    exhausted: page.conversations.length === 0 || newOffset >= (total ?? 0),
  };
}
