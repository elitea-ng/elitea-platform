import { useCallback, useEffect, useRef, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';

import type { Conversation } from '@/entities/conversation';
import { folderApi } from '@/entities/folder';
import type { DateGroup, Folder, FolderConversationRef } from '@/entities/folder';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { conversationListErrorMessage } from '../errorMessage';
import { sortConversations } from '../helpers/conversationList.helpers';
import { useHasPermission } from '../useHasPermission';
import type { DateGroupListItem, FolderListItem } from './conversationListState.types';

/**
 * `entities/folder`'s `FolderConversationRef` now models `name`/`isPrivate`
 * (widened alongside `entities/folder/api/foldersApi.ts`'s wire normaliser
 * to stop dropping them — was a real, disclosed regression: every row from
 * this hook rendered with a blank title, confirmed against the baseline's
 * `ConversationItem.jsx:57-66`, which destructures `name`/`is_private`
 * directly off this exact list-row shape). Both remain optional on
 * `FolderConversationRef` since not every caller of `entities/folder`
 * necessarily has them (e.g. a client-synthesized ref) — this hook falls
 * back to the previous placeholders only when the wire genuinely omits
 * them, not unconditionally.
 */
function toConversation(ref: FolderConversationRef, extra?: Partial<Conversation>): Conversation {
  return {
    id: ref.id,
    name: ref.name ?? '',
    isPrivate: ref.isPrivate ?? true,
    ...(ref.authorId !== undefined ? { authorId: ref.authorId } : {}),
    ...(ref.updatedAt !== undefined ? { updatedAt: ref.updatedAt } : {}),
    ...(ref.createdAt !== undefined ? { createdAt: ref.createdAt } : {}),
    ...(ref.isPlayback !== undefined ? { isPlayback: ref.isPlayback } : {}),
    ...extra,
  };
}

export interface UseQueryFoldersListParams {
  readonly projectId: string | undefined;
  readonly toastError: (message: string) => void;
  readonly setFolders: Dispatch<SetStateAction<readonly FolderListItem[]>>;
  readonly setDateGroups: Dispatch<SetStateAction<readonly DateGroupListItem[]>>;
  readonly setPinnedConversations: Dispatch<SetStateAction<readonly Conversation[]>>;
  readonly onSelectConversation?: (conversation: Conversation) => void;
  readonly skipSetConversation?: boolean;
  readonly searchQuery?: string;
  /**
   * Baseline: `useSortQueryParamsFromUrl({defaultSortOrder: 'desc',
   * defaultSortBy: 'updated_at'})` reads the CURRENT URL's `sort_by`/
   * `sort_order` query params — a router-level concern `features/` may not
   * reach into directly (R-L1: no `pages`/`app` imports). Explicit params
   * instead (N4 signature deviation), defaulted to the SAME values the
   * baseline's own hook defaults to.
   */
  readonly sortBy?: string;
  readonly sortOrder?: string;
}

export interface UseQueryFoldersListResult {
  readonly isLoadFolders: boolean;
  readonly isLoadMoreFolders: boolean;
  readonly totalFolderCount: number;
  readonly isConversationsLoaded: boolean;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/
 * hooks/useQueryFoldersList.hooks.js` (unit C2) — the grouped initial fetch
 * via `folderApi.useList` (already wire→domain normalised, so no snake_case
 * field translation is needed here, unlike the baseline).
 *
 * **Ref-mirroring idiom substitution, disclosed.** The baseline re-points 4
 * refs (`onSelectConversationRef`/`setDateGroupsRef`/`setFoldersRef`/
 * `setPinnedConversationsRef`) inside 4 separate `useEffect`s — a render
 * -then-commit round trip that leaves the ref one render stale. This port
 * uses `entities/conversation/lib/hooks/useConversationLifecycle.ts`'s own
 * established idiom instead: assign `ref.current = value` directly in the
 * render body (no `useEffect`), which is synchronously current and is this
 * codebase's own precedent for exactly this "caller-supplied
 * setter/callback shouldn't need to be a `useCallback` dependency" need.
 */
export function useQueryFoldersList(params: UseQueryFoldersListParams): UseQueryFoldersListResult {
  const {
    projectId,
    toastError,
    setFolders,
    setDateGroups,
    setPinnedConversations,
    onSelectConversation,
    skipSetConversation,
    searchQuery,
    sortBy = 'updated_at',
    sortOrder = 'desc',
  } = params;

  const onSelectConversationRef = useRef(onSelectConversation);
  onSelectConversationRef.current = onSelectConversation;
  const setDateGroupsRef = useRef(setDateGroups);
  setDateGroupsRef.current = setDateGroups;
  const setFoldersRef = useRef(setFolders);
  setFoldersRef.current = setFolders;
  const setPinnedConversationsRef = useRef(setPinnedConversations);
  setPinnedConversationsRef.current = setPinnedConversations;

  const [isConversationsLoaded, setIsConversationsLoaded] = useState(false);
  const hasGetPermission = useHasPermission(projectId, PERMISSIONS.chat.folders.get);

  const query = folderApi.useList(
    {
      projectId: projectId ?? '',
      params: { sort_by: sortBy, sort_order: sortOrder, ...(searchQuery ? { query: searchQuery } : {}) },
    },
    { enabled: projectId !== undefined && hasGetPermission },
  );

  /**
   * `total` and `offset` are carried through from the answer, not recomputed.
   *
   * This function used to build `{name, conversations, offset:
   * conversations.length}` and nothing else, so every bucket reached the rail
   * with `total` undefined. `DateGroup.tsx` passes `group.total ?? 0` to
   * `LoadMoreSentinel`, whose `hasMore` is `totalAvailableCount >
   * listCurrentSize` — with a total of 0 that is false for every bucket that
   * has ever existed. The sentinel never rendered, `onLoadMoreInGroup` had no
   * caller, and a project longer than one page did not load slowly: it never
   * loaded the rest. The folder half of the same feature always worked
   * because `updateFolders` below spreads the whole `Folder`.
   *
   * `offset` is the server's own "where the next page starts", which is the
   * number of rows it DELIVERED. Falling back to `conversations.length` is
   * only for a listing served whole (no `offset` on the wire); using the
   * post-filter length when the server did send one would ask for a page that
   * starts before the rows already held, every time a pinned row was dropped.
   */
  const updateDateGroups = useCallback((dateGroupsList: readonly DateGroup[], pinnedIds: ReadonlySet<string>): void => {
    const processedGroups: DateGroupListItem[] = dateGroupsList.map((group) => {
      const filtered = pinnedIds.size > 0 ? group.conversations.filter((c) => !pinnedIds.has(c.id)) : group.conversations;
      const conversations = sortConversations(filtered.map((ref) => toConversation(ref)));
      return {
        name: group.name,
        conversations,
        offset: group.offset ?? conversations.length,
        ...(group.total !== undefined ? { total: group.total } : {}),
      };
    });
    setDateGroupsRef.current(processedGroups);
  }, []);

  const updateFolders = useCallback((folderList: readonly Folder[], pinnedIds: ReadonlySet<string>): void => {
    const folderedConversations: FolderListItem[] = folderList.map((folder) => {
      const filtered = pinnedIds.size > 0 ? folder.conversations.filter((c) => !pinnedIds.has(c.id)) : folder.conversations;
      const conversations = sortConversations(filtered.map((ref) => toConversation(ref)));
      // The spread already carries `total`; `offset` is taken from the answer
      // for the same reason `updateDateGroups` above takes it.
      return { ...folder, conversations, offset: folder.offset ?? conversations.length };
    });

    setFoldersRef.current((prevFolders) => {
      const newFolders = prevFolders.filter((folder) => folder.isNew === true);

      const mergedFolders = folderedConversations.map((serverFolder) => {
        const localFolder = prevFolders.find((f) => f.id === serverFolder.id && f.isNew !== true);
        if (localFolder === undefined || localFolder.conversations.length === 0) return serverFolder;

        const serverConvIds = new Set(serverFolder.conversations.map((c) => c.id));
        const localOnly = localFolder.conversations.filter((c) => !serverConvIds.has(c.id));
        if (localOnly.length === 0) return serverFolder;

        return { ...serverFolder, conversations: sortConversations([...serverFolder.conversations, ...localOnly]) };
      });

      return [...newFolders, ...mergedFolders];
    });
  }, []);

  const updatePinnedConversations = useCallback(
    (pinned: { readonly conversations: readonly FolderConversationRef[] } | undefined): void => {
      const conversations = (pinned?.conversations ?? []).map((ref) => toConversation(ref, { isPinned: true }));

      if (searchQuery === undefined || searchQuery === '') {
        setPinnedConversationsRef.current(conversations);
      } else {
        const lowerQuery = searchQuery.toLowerCase();
        setPinnedConversationsRef.current((prevPinnedItems) => {
          const matchedPinnedItems = prevPinnedItems.filter((c) => c.name.toLowerCase().includes(lowerQuery));
          const filtered = conversations.filter((c) => !prevPinnedItems.some((p) => p.id === c.id));
          return [...matchedPinnedItems, ...filtered];
        });
      }
    },
    [searchQuery],
  );

  useEffect(() => {
    if (query.isSuccess && !query.isLoading && query.data !== undefined) {
      const pinnedIds = new Set(query.data.pinned.conversations.map((c) => c.id));
      updateDateGroups(query.data.dateGroups, pinnedIds);
      updateFolders(query.data.folders, pinnedIds);
      updatePinnedConversations(query.data.pinned);
      setIsConversationsLoaded(true);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `query.data` (the whole object) is the correct dependency, matching the baseline's own `[data, isSuccess, isLoadFolders, updateFolders, updateDateGroups, updatePinnedConversations]` — narrowing to `query.data.dateGroups`/etc. would fire on every TanStack Query refetch that returns a referentially-new but value-equal object, same as the baseline avoided by depending on the whole `data`.
  }, [query.data, query.isSuccess, query.isLoading, updateFolders, updateDateGroups, updatePinnedConversations]);

  const selectConversationIfNeeded = useCallback((): void => {
    if (skipSetConversation === true || query.data?.selectedConversationId === undefined) return;

    const pinnedConversations = query.data.pinned.conversations;
    const dateGroupConversations = query.data.dateGroups.flatMap((group) => group.conversations);
    const folderConversations = query.data.folders.flatMap((folder) => folder.conversations);
    const conversationList = [...pinnedConversations, ...dateGroupConversations, ...folderConversations];

    // Baseline uses `==` (loose equality) here — this codebase's ids are always `string` (both `FolderConversationRef.id` and `selectedConversationId`), so `===` is a safe, disclosed simplification with no behaviour change.
    const selectedConversation = conversationList.find((conversation) => conversation.id === query.data?.selectedConversationId) ?? conversationList[0];

    if (selectedConversation !== undefined) onSelectConversationRef.current?.(toConversation(selectedConversation));
  }, [query.data, skipSetConversation]);

  useEffect(() => {
    selectConversationIfNeeded();
  }, [selectConversationIfNeeded]);

  useEffect(() => {
    if (query.isError) {
      toastError(conversationListErrorMessage(query.error));
      setDateGroupsRef.current([]);
      setFoldersRef.current([]);
      setPinnedConversationsRef.current([]);
    }
  }, [query.error, query.isError, toastError]);

  return {
    isLoadFolders: query.isLoading,
    isLoadMoreFolders: query.isFetching,
    totalFolderCount: query.data?.totalFolders ?? 0,
    isConversationsLoaded,
  };
}
