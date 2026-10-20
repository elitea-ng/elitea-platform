/**
 * The conversation list as a model, for a list that is not `Conversations`:
 * the desktop sidebar's chats (`widgets/desktop-shell`). It owns the same
 * three state containers `processes/chat/model/useConversationSidebar.ts`
 * owns for the web rail and fills them through the SAME hooks — the grouped
 * listing (`useQueryFoldersList`, so one `['folder', 'list', …]` cache entry
 * serves both lists), the optimistic pin (`usePinConversation`), the per-bucket
 * pages (`mergeLoadMorePage`), the folders (`useCreateFolder`,
 * `useMoveToFolderConversation`, see `useConversationRailFolders`) — plus
 * rename and delete through `entities/conversation`'s API, the calls the web
 * rail makes.
 *
 * There is no second cache: the listing is the folders query, and every
 * change is applied to the containers the way the web rail applies it. Each
 * change also marks the cached listing stale (`invalidate`), so the other
 * list (the chat page's own, which the desktop shows while the sidebar's is
 * hidden) and a remount read the change instead of the old rows. The chat
 * page does the same for a conversation it creates, renames or deletes, so
 * this list learns of those through the same refetch.
 */
import { useCallback, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import type { Conversation } from '@/entities/conversation';
import { conversationApi } from '@/entities/conversation';
import { folderApi } from '@/entities/folder';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';

import { mergeLoadMorePage, type LoadMorePage } from '../../ui/conversations/Conversations.helpers';
import { conversationListErrorMessage } from '../errorMessage';
import type { DateGroupListItem, FolderListItem } from './conversationListState.types';
import { useArchiveConversation } from './useArchiveConversation.hooks';
import { useConversationRailFolders } from './useConversationRailFolders.hooks';
import { usePinConversation } from './usePinConversation.hooks';
import { useQueryFoldersList } from './useQueryFoldersList.hooks';

const PAGE_SIZE = 10;
const FOLDER_LIST_KEY = ['folder', 'list'] as const;

export interface ConversationRail {
  readonly pinned: readonly Conversation[];
  readonly folders: readonly FolderListItem[];
  readonly dateGroups: readonly DateGroupListItem[];
  readonly isLoading: boolean;
  /** The last failure, in words for the person; `undefined` when there is none. */
  readonly error: string | undefined;
  readonly dismissError: () => void;
  /** Delete and rename are the author's (the web rail's guard, fail-closed on an unknown identity). */
  readonly canModify: (conversation: Conversation) => boolean;
  readonly pin: (conversation: Conversation, shouldPin: boolean) => void;
  readonly rename: (conversation: Conversation, name: string) => void;
  readonly remove: (conversation: Conversation) => Promise<boolean>;
  /** Archives for the caller: the row leaves the lists at once (`useArchiveConversation`). */
  readonly archive: (conversation: Conversation) => void;
  /** Creates a folder with this name. */
  readonly createFolder: (name: string) => Promise<void>;
  /** Moves the conversation into a folder, or out of every folder with `null`. */
  readonly moveToFolder: (conversation: Conversation, folderId: string | null) => Promise<void>;
  readonly loadMoreInGroup: (groupName: string) => void;
  readonly loadMoreInFolder: (folderId: string) => void;
  /** Whether a date group or folder has rows the listing did not deliver yet. */
  readonly hasMore: typeof hasMore;
}

/** The rows a bucket still has on the server. */
function hasMore(bucket: { readonly conversations: readonly unknown[]; readonly total?: number; readonly exhausted?: boolean }): boolean {
  return bucket.exhausted !== true && bucket.conversations.length < (bucket.total ?? 0);
}

function useCurrentUserId(): string | undefined {
  const author = useGetCurrentAuthor();
  const id = (author.data?.data as SocialAuthorProfile | undefined)?.id;
  return id === undefined ? undefined : String(id);
}

/** Every container at once, so a change can never reach some rows and miss others. */
function useContainers() {
  const [folders, setFolders] = useState<readonly FolderListItem[]>([]);
  const [dateGroups, setDateGroups] = useState<readonly DateGroupListItem[]>([]);
  const [pinned, setPinned] = useState<readonly Conversation[]>([]);
  const transform = useCallback((change: (items: readonly Conversation[]) => readonly Conversation[]) => {
    setPinned((prev) => [...change(prev)]);
    setDateGroups((prev) => prev.map((group) => ({ ...group, conversations: [...change(group.conversations)] })));
    setFolders((prev) => prev.map((folder) => ({ ...folder, conversations: [...change(folder.conversations)] })));
  }, []);
  return { folders, setFolders, dateGroups, setDateGroups, pinned, setPinned, transform };
}

type Containers = ReturnType<typeof useContainers>;

/** The next page of one date group or folder, merged the way the web rail merges it. */
function useLoadMore(projectId: string | undefined, containers: Containers, toastError: (message: string) => void) {
  const { dateGroups, folders, pinned, setDateGroups, setFolders } = containers;
  const loading = useRef(new Set<string>());
  const load = useCallback(
    async (key: string, read: () => Promise<LoadMorePage>, apply: (page: LoadMorePage, pinnedIds: ReadonlySet<string>) => void) => {
      if (loading.current.has(key)) return;
      loading.current.add(key);
      try {
        apply(await read(), new Set(pinned.map((conversation) => conversation.id)));
      } catch (caught) {
        toastError(conversationListErrorMessage(caught));
      } finally {
        loading.current.delete(key);
      }
    },
    [pinned, toastError],
  );
  const loadMoreInGroup = useCallback(
    (groupName: string) => {
      const group = dateGroups.find((candidate) => candidate.name === groupName);
      if (projectId === undefined || group === undefined || !hasMore(group)) return;
      const offset = group.offset ?? group.conversations.length;
      void load(`group:${groupName}`, () => folderApi.conversationsByDateGroup({ projectId, dateGroup: groupName, limit: PAGE_SIZE, offset }), (page, pinnedIds) =>
        setDateGroups((prev) => prev.map((g) => (g.name === groupName ? { ...g, ...mergeLoadMorePage(g, page, pinnedIds) } : g))),
      );
    },
    [dateGroups, load, projectId, setDateGroups],
  );
  const loadMoreInFolder = useCallback(
    (folderId: string) => {
      const folder = folders.find((candidate) => candidate.id === folderId);
      if (projectId === undefined || folder === undefined || !hasMore(folder)) return;
      const offset = folder.offset ?? folder.conversations.length;
      void load(`folder:${folderId}`, () => folderApi.conversationsByFolder({ projectId, folderId, limit: PAGE_SIZE, offset }), (page, pinnedIds) =>
        setFolders((prev) => prev.map((f) => (f.id === folderId ? { ...f, ...mergeLoadMorePage(f, page, pinnedIds) } : f))),
      );
    },
    [folders, load, projectId, setFolders],
  );
  return { loadMoreInGroup, loadMoreInFolder };
}

/** Rename and delete, as the web rail does them: the request first, then the rows (no optimistic write). */
function useEdits(projectId: string | undefined, transform: Containers['transform'], toastError: (message: string) => void, invalidate: () => void) {
  const rename = useCallback(
    (conversation: Conversation, name: string) => {
      if (projectId === undefined || name.trim() === '' || name === conversation.name) return;
      void conversationApi
        .edit({ projectId, id: conversation.id, name })
        .then(() => {
          transform((items) => items.map((item) => (item.id === conversation.id ? { ...item, name } : item)));
          invalidate();
        })
        .catch(() => toastError('Failed to rename the conversation'));
    },
    [invalidate, projectId, toastError, transform],
  );
  const remove = useCallback(
    async (conversation: Conversation): Promise<boolean> => {
      if (projectId === undefined) return false;
      try {
        await conversationApi.remove({ projectId, id: conversation.id });
        transform((items) => items.filter((item) => item.id !== conversation.id));
        invalidate();
        return true;
      } catch {
        toastError('Failed to delete the conversation');
        return false;
      }
    },
    [invalidate, projectId, toastError, transform],
  );
  return { rename, remove };
}

export function useConversationRail({ projectId }: { readonly projectId: string | undefined }): ConversationRail {
  const containers = useContainers();
  const [error, setError] = useState<string | undefined>(undefined);
  const toastError = useCallback((message: string) => setError(message), []);
  const dismissError = useCallback(() => setError(undefined), []);
  const currentUserId = useCurrentUserId();
  const queryClient = useQueryClient();
  const invalidate = useCallback(() => void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY }), [queryClient]);

  const listing = useQueryFoldersList({
    projectId,
    toastError,
    setFolders: containers.setFolders,
    setDateGroups: containers.setDateGroups,
    setPinnedConversations: containers.setPinned,
    // The route says which conversation is open; the listing's own pick would only highlight the last one used.
    skipSetConversation: true,
  });

  // The rail has no "active conversation" state of its own (the route is the selection).
  const noActive = useCallback(() => undefined, []);
  const { onPinConversation } = usePinConversation({
    projectId,
    activeConversation: undefined,
    setActiveConversation: noActive,
    setPinnedConversations: containers.setPinned,
    setDateGroups: containers.setDateGroups,
    setFolders: containers.setFolders,
    toastError,
  });
  const pin = useCallback((conversation: Conversation, shouldPin: boolean) => void onPinConversation(conversation, shouldPin), [onPinConversation]);
  const { rename, remove } = useEdits(projectId, containers.transform, toastError, invalidate);
  const { createFolder, moveToFolder } = useConversationRailFolders(projectId, containers, toastError, invalidate);
  const { loadMoreInGroup, loadMoreInFolder } = useLoadMore(projectId, containers, toastError);
  const { transform } = containers;
  const removeRow = useCallback((conversationId: string) => transform((items) => items.filter((item) => item.id !== conversationId)), [transform]);
  const { archive: archiveRequest } = useArchiveConversation({ projectId, removeRow, toastError });
  const archive = useCallback((conversation: Conversation) => void archiveRequest(conversation), [archiveRequest]);
  const canModify = useCallback(
    (conversation: Conversation) => currentUserId !== undefined && conversation.authorId !== undefined && String(conversation.authorId) === currentUserId,
    [currentUserId],
  );

  return {
    pinned: containers.pinned,
    folders: containers.folders,
    dateGroups: containers.dateGroups,
    isLoading: listing.isLoadFolders,
    error,
    dismissError,
    canModify,
    pin,
    rename,
    remove,
    archive,
    createFolder,
    moveToFolder,
    loadMoreInGroup,
    loadMoreInFolder,
    hasMore,
  };
}
