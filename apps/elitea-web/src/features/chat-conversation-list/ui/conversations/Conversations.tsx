import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { DndContext, closestCenter } from '@dnd-kit/core';
import Box from '@mui/material/Box';
import { useTheme } from '@mui/material/styles';

import { conversationApi, type Conversation } from '@/entities/conversation';
import { folderApi } from '@/entities/folder';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { conversationExportErrorMessage, conversationListErrorMessage } from '../../lib/errorMessage';
import { useDragAndDrop } from '../../lib/hooks/useDragAndDrop';
import { useIsSmallWindow } from '../../lib/hooks/useIsSmallWindow';
import { useHasPermission } from '../../lib/useHasPermission';
import { ConversationsBody } from './Conversations.body';
import { ConversationsHeader } from './Conversations.header';
import { isDraftConversation, mergeLoadMorePage, resolveConversationsDefaults } from './Conversations.helpers';
import { buildMoveToFoldersMenuItems } from './Conversations.menu';
import { useRenderConversationItem, useRenderFoldersSection } from './Conversations.renderers';
import type { ConversationsProps } from './Conversations.types';

export type { ConversationsFolder, ConversationsProps } from './Conversations.types';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/ui/
 * conversations/Conversations.jsx` (unit C2) — the composition root of this
 * whole feature. Split across `.types.ts`/`.helpers.ts`/`.styles.ts`/
 * `.menu.tsx`/`.header.tsx`/`.body.tsx`/`.folders.tsx` purely to keep every
 * file under the §3.5 `max-lines`(400)/`complexity`(12) budgets. Imports
 * `FolderItem`/`GroupedConversations`/`DroppableGroupedArea` directly from
 * their sibling `ui/folders`/`ui/groups` directories rather than through a
 * barrel (no `index.ts` exists in either — matches `features/pipelines/ui/`'s
 * own no-barrel precedent).
 *
 * **MAJOR DISCLOSED STRUCTURAL DIFFERENCE.** The baseline's `Folders.jsx`
 * (a thin "map `folders` -> `FolderItem`, wrap in `SortableContext`" middle
 * tier) has no equivalent file in this port: the concurrently-built
 * `ui/folders` cluster promoted `FolderItem.tsx` to take a `folder`
 * (singular) and left the per-array loop unbuilt — `ui/groups/
 * LoadMoreSentinel.tsx`'s own module doc confirms this explicitly ("that
 * same dedupe already happens one level up, at the caller that owns
 * `loadingGroups`... not yet ported, out of this unit's scope"). That loop
 * now lives in `Conversations.folders.tsx` instead — `hoveredFolderId`/
 * `handleFolderHover` (baseline: `Folders.jsx:40,44-46`) move here with it.
 *
 * Two disclosed-but-real baseline bugs are NOT reproduced (both live in
 * files this unit doesn't own): `onLoadMore`/`isLoadMoreConversations` are
 * accepted (full-prop-list fidelity, see `Conversations.types.ts`) but never
 * threaded anywhere — the `Folders`-shaped dead-prop-recipient they fed no
 * longer exists; `ui/groups/DateGroup.tsx`'s own doc comment already
 * discloses dropping its unused `dropAreaState` computation.
 *
 * `data-tour={CHAT_TOUR_TARGET_IDS.*}` (baseline) is dropped —
 * `features/interactive-tours` does not exist in this worktree, same
 * disclosed gap `widgets/app-shell/ui/AppShell.tsx`'s own doc comment
 * already established for the identical dependency.
 */
export function Conversations(props: ConversationsProps): ReactNode {
  const {
    conversations,
    pinnedConversations,
    dateGroups,
    setDateGroups,
    ungroupedConversationsCount,
    totalConversationsAmount,
    onSelectConversation,
    selectedConversationId,
    onCollapsed,
    onEditConversation,
    onPlaybackConversation,
    onDeleteConversation,
    onPinConversation,
    onCreateConversation,
    onCancelCreateConversation,
    onChangeActiveConversationName,
    onChangeActiveFolderName,
    onCreateFolder,
    onCancelCreateFolder,
    folders,
    setFolders,
    onDeleteFolder,
    onEditFolder,
    onPinFolder,
    onMoveToFolderConversation,
    onMoveToNewFolderConversation,
    moveTargetConversationToNewFolder,
    cancelMovingTargetConversationToNewFolder,
    onClickCreateNewFolder,
    toastSuccess,
    toastError,
    onReorderFolders,
    onSearchQueryChange,
    projectId,
    currentUserId,
    personalProjectId,
    publicProjectId,
    basename,
    onShareLinkCopied,
  } = props;
  const { collapsed, isLoadConversations, enableDragAndDrop, isFolderOperationInProgress, isEditingCanvas, sortBy, sortOrder } = resolveConversationsDefaults(props);

  const theme = useTheme();
  const listRef = useRef<HTMLDivElement | null>(null);
  const { isSmallWindow } = useIsSmallWindow();

  const hasFolderCreatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.create);
  const hasFolderUpdatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.update);

  const [searchQuery, setSearchQuery] = useState('');
  const [isSearchActive, setIsSearchActive] = useState(false);
  const [loadingGroups, setLoadingGroups] = useState<ReadonlySet<string>>(new Set());
  const [loadingFolders, setLoadingFolders] = useState<ReadonlySet<string>>(new Set());
  const [hoveredFolderId, setHoveredFolderId] = useState<string | null>(null);

  const isSearchMode = searchQuery.trim() !== '';

  const handleSearchClear = useCallback(() => {
    setSearchQuery('');
    setIsSearchActive(false);
  }, []);

  // `SimpleSearchBar` already debounces its own `onChange` (default 300ms) —
  // no separate `useDebounceValue` port is needed, unlike the baseline
  // (`Conversations.jsx:104-121`), which paired an undebounced search box
  // with its own external debounce hook.
  useEffect(() => {
    onSearchQueryChange?.(searchQuery.trim() || undefined);
  }, [searchQuery, onSearchQueryChange]);

  useEffect(() => {
    if (isSearchActive && conversations.some(isDraftConversation)) {
      setIsSearchActive(false);
      setSearchQuery('');
    }
  }, [isSearchActive, conversations]);

  const onLoadMoreInGroup = useCallback(
    async (groupName: string): Promise<void> => {
      if (loadingGroups.has(groupName) || projectId === undefined) return;
      const group = dateGroups.find((g) => g.name === groupName);
      if (group === undefined || group.exhausted === true || group.conversations.length >= (group.total ?? 0)) return;

      setLoadingGroups((prev) => new Set(prev).add(groupName));
      try {
        const pinnedIds = new Set(pinnedConversations.map((c) => c.id));
        const page = await folderApi.conversationsByDateGroup({ projectId, dateGroup: groupName, limit: 10, offset: group.offset ?? group.conversations.length, sort_by: sortBy, sort_order: sortOrder });
        setDateGroups((prev) => prev.map((g) => (g.name === groupName ? { ...g, ...mergeLoadMorePage(g, page, pinnedIds) } : g)));
      } catch (caught) {
        toastError(conversationListErrorMessage(caught));
      } finally {
        setLoadingGroups((prev) => {
          const next = new Set(prev);
          next.delete(groupName);
          return next;
        });
      }
    },
    [dateGroups, loadingGroups, pinnedConversations, projectId, setDateGroups, sortBy, sortOrder, toastError],
  );

  const onLoadMoreInFolder = useCallback(
    async (folderId: string): Promise<void> => {
      if (loadingFolders.has(folderId) || projectId === undefined) return;
      const folder = folders.find((f) => f.id === folderId);
      if (folder === undefined || folder.exhausted === true || folder.conversations.length >= (folder.total ?? 0)) return;

      setLoadingFolders((prev) => new Set(prev).add(folderId));
      try {
        const pinnedIds = new Set(pinnedConversations.map((c) => c.id));
        const page = await folderApi.conversationsByFolder({ projectId, folderId, limit: 10, offset: folder.offset ?? folder.conversations.length, sort_by: sortBy, sort_order: sortOrder });
        setFolders((prev) => prev.map((f) => (f.id === folderId ? { ...f, ...mergeLoadMorePage(f, page, pinnedIds) } : f)));
      } catch (caught) {
        toastError(conversationListErrorMessage(caught));
      } finally {
        setLoadingFolders((prev) => {
          const next = new Set(prev);
          next.delete(folderId);
          return next;
        });
      }
    },
    [folders, loadingFolders, pinnedConversations, projectId, setFolders, sortBy, sortOrder, toastError],
  );

  // `exactOptionalPropertyTypes` forbids `onReorderFolders`/`toastSuccess` being
  // present-with-`undefined` on `UseDragAndDropParams` — conditionally spread
  // instead of assigning them directly, same pattern `Conversations.helpers.ts`'s
  // own `mergeLoadMorePage` doc comment cites for the identical class of gotcha.
  const { sensors, handleDragStart, handleDragEnd, handleDragOver, getDropAreaState } = useDragAndDrop({
    onMoveToFolderConversation,
    folders,
    originalFolders: folders,
    conversations,
    selectedConversations: [],
    toastError,
    ...(onReorderFolders !== undefined ? { onReorderFolders } : {}),
    ...(toastSuccess !== undefined ? { toastSuccess } : {}),
  });

  const pinnedFolders = useMemo(() => folders.filter((folder) => folder.isPinned === true), [folders]);
  const unpinnedFolders = useMemo(() => folders.filter((folder) => folder.isPinned !== true), [folders]);

  const handleFolderHover = useCallback((folderId: string, isHovered: boolean) => {
    setHoveredFolderId(isHovered ? folderId : null);
  }, []);

  const onCreateFolderExpanded = useCallback(() => onClickCreateNewFolder(), [onClickCreateNewFolder]);
  const onCreateFolderCollapsed = useCallback(() => {
    onClickCreateNewFolder();
    onCollapsed();
  }, [onClickCreateNewFolder, onCollapsed]);

  const getMoveConversationToFoldersMenuItems = useCallback(
    (conversation: Conversation) =>
      buildMoveToFoldersMenuItems({
        conversation,
        conversations,
        folders,
        hasFolderCreatePermission,
        hasFolderUpdatePermission,
        currentUserId,
        theme,
        onMoveToFolderConversation,
        onMoveToNewFolderConversation,
      }),
    [conversations, folders, hasFolderCreatePermission, hasFolderUpdatePermission, currentUserId, theme, onMoveToFolderConversation, onMoveToNewFolderConversation],
  );

  // Both render-prop factories below live in `Conversations.renderers.tsx`
  // (extracted purely to keep THIS file under the §3.5 `max-lines` budget —
  // see that file's own module doc for why each is a `useLatestRef`-backed
  // hook rather than a plain `useCallback` with every input listed).
  /**
   * ISSUE 851 — the Export row's missing caller.
   *
   * The row menu has rendered "Export" since this feature was ported, and
   * nothing has ever supplied the callback that makes it live: the entry was
   * `disabled: true` with two placeholder options. This is the composition
   * root the exporter belongs at — the row itself has no project id and no
   * toast, and the entity's fetcher deliberately resolves its outcome rather
   * than rejecting, so a refusal becomes a message instead of an unhandled
   * rejection inside a menu click.
   */
  const onExportConversation = useCallback(
    (conversation: Conversation, format: 'md' | 'json'): void => {
      if (projectId === undefined) return;
      void conversationApi
        .downloadExport({ projectId, conversationId: conversation.id, conversationName: conversation.name, format })
        .then((result) => {
          if (!result.ok) toastError(conversationExportErrorMessage(result.error));
        });
    },
    [projectId, toastError],
  );

  const renderConversationItem = useRenderConversationItem({
    onExportConversation,
    selectedConversationId,
    onSelectConversation,
    onEditConversation,
    onPlaybackConversation,
    onDeleteConversation,
    onPinConversation,
    onCreateConversation,
    onCancelCreateConversation,
    onChangeActiveConversationName,
    getMoveConversationToFoldersMenuItems,
    isEditingCanvas,
    enableDragAndDrop,
    projectId,
    currentUserId,
    personalProjectId,
    publicProjectId,
    basename,
    onShareLinkCopied,
  });

  const renderFoldersSection = useRenderFoldersSection({
    hoveredFolderId,
    selectedConversationId,
    ungroupedConversationsCount,
    enableDragAndDrop,
    isSearchMode,
    isFolderOperationInProgress,
    getDropAreaState,
    onFolderHover: handleFolderHover,
    projectId,
    renderConversationItem,
    loadingFolders,
    onLoadMoreInFolder: (folderId) => void onLoadMoreInFolder(folderId),
    callbacks: { onCreateFolder, onCancelCreateFolder, onEditFolder, onPinFolder, onDeleteFolder, onChangeActiveFolderName },
    moveTarget: { moveTargetConversationToNewFolder, cancelMovingTargetConversationToNewFolder },
  });

  const renderPinnedFolders = useCallback(() => renderFoldersSection(pinnedFolders, true), [renderFoldersSection, pinnedFolders]);
  const renderUnpinnedFolders = useCallback(() => renderFoldersSection(unpinnedFolders, false), [renderFoldersSection, unpinnedFolders]);

  const showEmptyState = isSearchActive && isSearchMode && conversations.length === 0 && folders.every((f) => f.conversations.length === 0);

  return (
    <DndContext
      sensors={sensors}
      onDragStart={handleDragStart}
      onDragEnd={(event) => void handleDragEnd(event)}
      onDragOver={handleDragOver}
      collisionDetection={closestCenter}
      autoScroll={false}
    >
      <Box sx={{ height: '100%', position: 'relative', width: collapsed && !isSmallWindow ? '36px' : '100%' }}>
        <ConversationsHeader
          collapsed={collapsed}
          isSmallWindow={isSmallWindow}
          hasFolderCreatePermission={hasFolderCreatePermission}
          onCreateFolderExpanded={onCreateFolderExpanded}
          onCreateFolderCollapsed={onCreateFolderCollapsed}
          onCollapsedToggle={onCollapsed}
          onSearchActivate={setIsSearchActive}
          isSearchActive={isSearchActive}
          searchQuery={searchQuery}
          onSearchChange={setSearchQuery}
          onSearchClear={handleSearchClear}
        />
        <ConversationsBody
          listRef={listRef}
          collapsed={collapsed}
          isSmallWindow={isSmallWindow}
          isLoadConversations={isLoadConversations}
          renderPinnedFolders={renderPinnedFolders}
          renderUnpinnedFolders={renderUnpinnedFolders}
          pinnedConversations={pinnedConversations}
          renderConversationItem={renderConversationItem}
          dateGroups={dateGroups}
          totalConversationsAmount={totalConversationsAmount}
          onLoadMoreInGroup={(groupName) => void onLoadMoreInGroup(groupName)}
          loadingGroups={loadingGroups}
          enableDragAndDrop={enableDragAndDrop}
          getDropAreaState={getDropAreaState}
          selectedConversationId={selectedConversationId}
          isSearchMode={isSearchMode}
          searchQuery={searchQuery}
          isEditingCanvas={isEditingCanvas}
          showEmptyState={showEmptyState}
        />
      </Box>
    </DndContext>
  );
}
