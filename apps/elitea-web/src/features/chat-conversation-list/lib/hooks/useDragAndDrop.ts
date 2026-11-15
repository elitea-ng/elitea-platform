import { useMemo, useRef, useState } from 'react';

import type { DragEndEvent, DragStartEvent, UniqueIdentifier, useSensors } from '@dnd-kit/core';

import { hasPlaybackConversation } from '@/entities/conversation';
import type { Conversation } from '@/entities/conversation';
import { isPinnedFolder } from '@/entities/folder';
import { useGroupDnd } from '@/shared/lib/dnd';
import type { CustomDrag, DndPolicy, DropAreaState } from '@/shared/lib/dnd';

import type { FolderListItem } from './conversationListState.types';
import { computeFoldersOrderForDrop, FOLDER_ID_PREFIX, folderIdMatches, resolveFolderReorderTargets } from './useDragAndDrop.positioning';

export { computePositionAtTopOfUnpinned, computePositionBetweenNeighbors } from './useDragAndDrop.positioning';

const UNGROUPED_ID = 'ungrouped-conversations';

/**
 * Whether a dragged conversation may be dropped onto `targetFolder` (`null`
 * = the ungrouped area). Baseline: playback/pinned rows are never draggable; a
 * conversation with a live playback snapshot anywhere is blocked (the
 * `hasPlaybackConversations` guard, duplicated here from
 * `useMoveToFolderConversation.hooks.ts`'s own copy per the no-sideways-features
 * rule); and a no-op move (already in the target location) is skipped.
 */
function isMovableConversation(
  conversation: Conversation,
  conversations: readonly Conversation[],
  foldersForReordering: readonly FolderListItem[],
  targetFolder: FolderListItem | null,
): boolean {
  if (conversation.isPlayback === true || conversation.isPinned === true) return false;

  const hasPlaybacks =
    hasPlaybackConversation(conversations, conversation.id) || foldersForReordering.some((folder) => hasPlaybackConversation(folder.conversations, conversation.id));
  if (hasPlaybacks) return false;

  return targetFolder !== null ? String(conversation.folderId) !== String(targetFolder.id) : conversation.folderId !== undefined;
}

/** What `over.id` was actually dropped on: a real folder, the ungrouped area (`group: null`), or neither. */
function resolveConversationDrop(folders: readonly FolderListItem[], droppedOnId: UniqueIdentifier): { readonly group: FolderListItem | null; readonly label: string } | undefined {
  if (typeof droppedOnId === 'string' && droppedOnId.startsWith(FOLDER_ID_PREFIX)) {
    const folderId = droppedOnId.slice(FOLDER_ID_PREFIX.length);
    const targetFolder = folders.find((f) => folderIdMatches(f, folderId));
    return targetFolder === undefined ? undefined : { group: targetFolder, label: `"${targetFolder.name}" folder` };
  }
  if (droppedOnId === UNGROUPED_ID) return { group: null, label: 'ungrouped area' };
  return undefined;
}

export interface UseDragAndDropParams {
  readonly onMoveToFolderConversation: (conversation: Conversation, targetFolder: FolderListItem | null) => Promise<unknown>;
  readonly onReorderFolders?: (newOrder: readonly FolderListItem[]) => Promise<void>;
  readonly folders: readonly FolderListItem[];
  /** Baseline: `originalFolders || folders` — the folder list to search when resolving a dragged/source folder for a CONVERSATION drag (as opposed to `folders`, used for FOLDER-reordering math). Defaults to `folders` when omitted. */
  readonly originalFolders?: readonly FolderListItem[];
  readonly conversations: readonly Conversation[];
  readonly selectedConversations?: readonly Conversation[];
  readonly toastSuccess?: (message: string) => void;
  readonly toastError: (message: string) => void;
}

export interface UseDragAndDropResult {
  readonly sensors: ReturnType<typeof useSensors>;
  readonly activeId: UniqueIdentifier | null;
  readonly draggedItems: readonly Conversation[];
  readonly draggedFromFolder: FolderListItem | null;
  readonly isDragging: boolean;
  readonly handleDragStart: (event: DragStartEvent) => void;
  readonly handleDragEnd: (event: DragEndEvent) => Promise<void>;
  readonly handleDragOver: () => void;
  readonly getDropAreaState: (dropAreaId: string) => DropAreaState;
}

/** The conversation rules as a policy for `shared/lib/dnd`'s generic hook. */
function conversationPolicy(params: UseDragAndDropParams): DndPolicy<Conversation, FolderListItem> {
  const { onMoveToFolderConversation, folders, originalFolders, conversations, selectedConversations = [] } = params;
  const foldersForReordering = originalFolders ?? folders;
  return {
    resolveDrag: (activeId) => {
      const dragged =
        conversations.find((conv) => conv.id === activeId) ?? foldersForReordering.flatMap((folder) => folder.conversations).find((conv) => conv.id === activeId);
      if (dragged === undefined) return undefined;
      const isSelected = selectedConversations.some((selected) => selected.id === dragged.id);
      const items = isSelected && selectedConversations.length > 1 ? selectedConversations : [dragged];
      const source = dragged.folderId !== undefined ? (foldersForReordering.find((folder) => folder.id === dragged.folderId) ?? null) : null;
      return { items, source };
    },
    resolveDrop: (overId) => resolveConversationDrop(folders, overId),
    canMove: (conversation, target) => isMovableConversation(conversation, conversations, foldersForReordering, target),
    move: onMoveToFolderConversation,
    groupKey: (folder) => String(folder.id),
    messages: {
      moveError: 'Error moving conversations',
      moved: (count, label) => (count === 1 ? `1 conversation moved to ${label} successfully` : `${count} conversations moved to ${label} successfully`),
    },
  };
}

interface FolderDragState {
  readonly dragging: boolean;
  readonly folder: FolderListItem | null;
}

/**
 * The chat rail's drag and drop: conversations into folders (the generic hook
 * with the conversation policy) and folder reordering (a custom drag this file
 * owns). Pure `@dnd-kit` orchestration, no API calls of its own: it only
 * invokes the `onMoveToFolderConversation`/`onReorderFolders` callbacks.
 */
export function useDragAndDrop(params: UseDragAndDropParams): UseDragAndDropResult {
  const { onReorderFolders, folders, toastSuccess, toastError } = params;
  const policy = useMemo(() => conversationPolicy(params), [params]);

  const [folderDrag, setFolderDrag] = useState<FolderDragState>({ dragging: false, folder: null });
  // The custom drag reads the CURRENT drag state and inputs synchronously from a drag event, which can fire before React re-renders.
  const folderDragRef = useRef(folderDrag);
  folderDragRef.current = folderDrag;
  const inputsRef = useRef({ folders, onReorderFolders });
  inputsRef.current = { folders, onReorderFolders };

  const custom = useMemo<CustomDrag>(
    () => ({
      onStart: (activeId) => {
        if (typeof activeId !== 'string' || !activeId.startsWith(FOLDER_ID_PREFIX)) {
          setFolderDrag({ dragging: false, folder: null });
          return false;
        }
        const folderId = activeId.slice(FOLDER_ID_PREFIX.length);
        const dragged = inputsRef.current.folders.find((f) => folderIdMatches(f, folderId));
        if (dragged === undefined) {
          setFolderDrag({ dragging: false, folder: null });
          return false;
        }
        const next = { dragging: true, folder: dragged };
        folderDragRef.current = next;
        setFolderDrag(next);
        return true;
      },
      isActive: () => folderDragRef.current.dragging && folderDragRef.current.folder !== null && inputsRef.current.onReorderFolders !== undefined,
      onEnd: async (event) => {
        const { over, active } = event;
        const currentFolders = inputsRef.current.folders;
        const dropTargetId = over?.id;
        if (over === null || typeof dropTargetId !== 'string' || !dropTargetId.startsWith(FOLDER_ID_PREFIX) || active.id === dropTargetId) return;
        const overData = over.data.current as { readonly folder?: FolderListItem } | undefined;
        const targets = resolveFolderReorderTargets(currentFolders, active.id, dropTargetId, overData?.folder);
        if (targets === undefined) return;
        const toSubmit = computeFoldersOrderForDrop(
          currentFolders,
          targets.draggedFolderId,
          targets.draggedFolderIndex,
          targets.targetFolderIndex,
          targets.targetFolder?.isPinned,
          !isPinnedFolder(targets.draggedFolderData),
        );
        await inputsRef.current.onReorderFolders?.(toSubmit);
      },
      onReset: () => {
        folderDragRef.current = { dragging: false, folder: null };
        setFolderDrag({ dragging: false, folder: null });
      },
      dropAreaState: (dropAreaId) => {
        const { dragging, folder } = folderDragRef.current;
        if (!dragging || folder === null) return undefined;
        if (!dropAreaId.startsWith(FOLDER_ID_PREFIX)) return { isValidDropTarget: false, isActive: false };
        const valid = String(folder.id) !== dropAreaId.slice(FOLDER_ID_PREFIX.length);
        return { isValidDropTarget: valid, isActive: valid };
      },
    }),
    [],
  );

  const dnd = useGroupDnd(policy, { custom, toastSuccess, toastError });
  return {
    sensors: dnd.sensors,
    activeId: dnd.activeId,
    draggedItems: dnd.draggedItems,
    draggedFromFolder: dnd.draggedFrom,
    isDragging: dnd.isDragging,
    handleDragStart: dnd.handleDragStart,
    handleDragEnd: dnd.handleDragEnd,
    handleDragOver: dnd.handleDragOver,
    getDropAreaState: dnd.getDropAreaState,
  };
}
