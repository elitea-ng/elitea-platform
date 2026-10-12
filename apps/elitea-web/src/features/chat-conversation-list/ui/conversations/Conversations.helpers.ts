import { genConversationId } from '@/shared/lib/chat';
import type { Conversation } from '@/entities/conversation';
import type { FolderConversationRef } from '@/entities/folder';

import type { ConversationsFolder, ConversationsProps } from './Conversations.types';

export interface ResolvedConversationsDefaults {
  readonly collapsed: boolean;
  readonly isLoadConversations: boolean;
  readonly enableDragAndDrop: boolean;
  readonly isFolderOperationInProgress: boolean;
  readonly isEditingCanvas: boolean;
  readonly sortBy: string;
  readonly sortOrder: string;
}

/** Same complexity-budget technique `ConversationItem.tsx`'s own `resolveConversationItemDefaults` doc comment explains — each `??` resolved inline would be one more branch directly on `Conversations`'s own complexity count. */
export function resolveConversationsDefaults(props: ConversationsProps): ResolvedConversationsDefaults {
  return {
    collapsed: props.collapsed ?? false,
    isLoadConversations: props.isLoadConversations ?? false,
    enableDragAndDrop: props.enableDragAndDrop ?? true,
    isFolderOperationInProgress: props.isFolderOperationInProgress ?? false,
    isEditingCanvas: props.isEditingCanvas ?? false,
    sortBy: props.sortBy ?? 'updated_at',
    sortOrder: props.sortOrder ?? 'desc',
  };
}

/**
 * `FolderConversationRef` (the folder-domain list row) -> `Conversation`
 * (this feature's own state-tree element type).
 *
 * This function is a copy of the same-named private helper in
 * `useQueryFoldersList.hooks.ts`. That helper is not exported. Keep the two
 * bodies equal.
 *
 * The copy drifted once. It dropped `authorId` and `name`, so a row that
 * arrived through "Load more" showed an empty title. The row menu also
 * denied Delete and Edit on the user's own conversation.
 *
 * `name: ''` and `isPrivate: true` stay as the fallback values. They are the
 * synthesized placeholders that the sibling helper also uses.
 */
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
 * `onLoadMoreInGroup`/`onLoadMoreInFolder`'s near-identical merge step
 * (`Conversations.jsx:150-170,213-233`), extracted so both call sites in
 * `Conversations.tsx` share one implementation. No `sortConversations`
 * call, matching the baseline: a load-more page is appended as-is, trusting
 * server pagination order. `total` is spread in conditionally rather than
 * assigned `total: total` directly — `exactOptionalPropertyTypes` forbids
 * an optional property being present with an explicit `undefined` value,
 * the same pattern `usePinConversation.hooks.ts`'s own destructure-out fix
 * already established for this class of gotcha.
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

/** Baseline `conversation.isNew` (`Conversations.jsx:124`) — `entities/conversation`'s `Conversation` doesn't model it (only the differently-shaped `DraftConversation` sibling does); read via a permissive narrowing check rather than an unsafe cast, same "read a field the base type doesn't model" precedent `ui/folders/FolderItem.tsx`'s own `readTargetConversationId`/`readFolderOwnerId` helpers already established for this exact class of gap. */
export function isDraftConversation(conversation: Conversation): boolean {
  return (conversation as { readonly isNew?: boolean }).isNew === true;
}

export interface FolderActivity {
  readonly isActive: boolean;
  readonly shouldExpandByDefault: boolean;
}

/**
 * `containsActiveConversation`/`shouldExpandByDefault` (`Conversations.jsx`
 * née `Folders.jsx:61-72`). `folder.hasSearchMatches` (baseline's third
 * OR-branch) has no equivalent field on `FolderListItem` anywhere in this
 * codebase (grepped) — a real, disclosed gap, not reproduced. `isActive`
 * (new: `ui/folders/FolderItem.tsx`'s own prop, never actually wired by the
 * baseline's `Folders.jsx` at all — confirmed by reading it) gets the
 * precise "contains the active conversation" half of the baseline's
 * compound expression, the most literal reading of what `FolderAccordion`'s
 * `isActive`-driven "selected" styling is for.
 */
export function computeFolderActivity(folder: ConversationsFolder, selectedConversationId: string | undefined, ungroupedConversationsCount: number): FolderActivity {
  const isActive = selectedConversationId !== undefined && folder.conversations.some((conversation) => genConversationId(conversation) === selectedConversationId);
  const shouldExpandByDefault = isActive || (ungroupedConversationsCount === 0 && folder.conversations.length > 0);
  return { isActive, shouldExpandByDefault };
}
