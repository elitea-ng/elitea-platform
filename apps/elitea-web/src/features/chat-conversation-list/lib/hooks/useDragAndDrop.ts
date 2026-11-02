import { useCallback, useState } from 'react';

import { KeyboardSensor, PointerSensor, useSensor, useSensors } from '@dnd-kit/core';
import type { DragEndEvent, DragStartEvent, UniqueIdentifier } from '@dnd-kit/core';
import { sortableKeyboardCoordinates } from '@dnd-kit/sortable';

import { hasPlaybackConversation } from '@/entities/conversation';
import type { Conversation } from '@/entities/conversation';
import { isPinnedFolder } from '@/entities/folder';

import type { FolderListItem } from './conversationListState.types';
import { useLatestRef } from './useLatestRef';
import { computeFoldersOrderForDrop, FOLDER_ID_PREFIX, folderIdMatches, resolveFolderReorderTargets } from './useDragAndDrop.positioning';

export { computePositionAtTopOfUnpinned, computePositionBetweenNeighbors } from './useDragAndDrop.positioning';

/**
 * Whether a dragged conversation may be dropped onto `targetFolder` (`null`
 * = the ungrouped area) — extracted from `handleDragEnd`'s two near-
 * identical loop bodies purely to stay under the §3.5 complexity budget.
 * Baseline: playback/pinned rows are never draggable; a conversation with a
 * live playback snapshot anywhere is blocked (the `hasPlaybackConversations`
 * guard, duplicated here from `useMoveToFolderConversation.hooks.ts`'s own
 * copy per the no-sideways-features rule — both read-only checks on
 * externally-owned data, not worth a shared module for 4 lines); and a
 * no-op move (already in the target location) is skipped.
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

interface DropTarget {
  readonly targetFolder: FolderListItem | null;
  readonly targetLocation: string;
}

/** Resolves what `over.id` was actually dropped on — a real folder (`targetFolder` set), the ungrouped area (`targetFolder: null`), or neither (`undefined`, e.g. dropped outside any recognised drop zone) — extracted from `handleDragEnd` purely to stay under the §3.5 complexity budget. */
function resolveDropTarget(folders: readonly FolderListItem[], droppedOnId: UniqueIdentifier): DropTarget | undefined {
  if (typeof droppedOnId === 'string' && droppedOnId.startsWith(FOLDER_ID_PREFIX)) {
    const folderId = droppedOnId.slice(FOLDER_ID_PREFIX.length);
    const targetFolder = folders.find((f) => folderIdMatches(f, folderId));
    return targetFolder === undefined ? undefined : { targetFolder, targetLocation: `"${targetFolder.name}" folder` };
  }
  if (droppedOnId === 'ungrouped-conversations') {
    return { targetFolder: null, targetLocation: 'ungrouped area' };
  }
  return undefined;
}

/** `successCount === 1 ? "1 conversation moved..." : "N conversations moved..."` — extracted purely to stay under the §3.5 complexity budget. */
function buildMoveSuccessMessage(successCount: number, targetLocation: string): string {
  return successCount === 1 ? `1 conversation moved to ${targetLocation} successfully` : `${successCount} conversations moved to ${targetLocation} successfully`;
}

/** Both below: extracted multi-condition booleans, purely to keep `handleDragEnd` under the §3.5 complexity budget. */
function shouldReorderFolders(wasDraggingFolder: boolean, currentDraggedFolder: FolderListItem | null, onReorderFolders: unknown): boolean {
  return wasDraggingFolder && currentDraggedFolder !== null && onReorderFolders !== undefined;
}

function shouldAnnounceMoveSuccess(successCount: number, hasToastSuccess: boolean, itemCount: number): boolean {
  return successCount > 0 && hasToastSuccess && itemCount > 1;
}

/** Sequentially attempts to move every draggable item in `currentDraggedItems` onto `targetFolder`, toasting per-item failures and returning the count that succeeded — extracted from `handleDragEnd` purely to stay under the §3.5 complexity budget. */
async function moveDraggedConversationsToTarget(
  currentDraggedItems: readonly Conversation[],
  conversations: readonly Conversation[],
  foldersForReordering: readonly FolderListItem[],
  targetFolder: FolderListItem | null,
  onMoveToFolderConversation: (conversation: Conversation, targetFolder: FolderListItem | null) => Promise<unknown>,
  toastError: (message: string) => void,
): Promise<number> {
  let successCount = 0;
  for (const conversation of currentDraggedItems) {
    if (!isMovableConversation(conversation, conversations, foldersForReordering, targetFolder)) continue;
    try {
      // eslint-disable-next-line no-await-in-loop -- sequential by design, matching the baseline: each move must resolve (and be individually catchable) before the next starts, there is no batched "move many" endpoint.
      await onMoveToFolderConversation(conversation, targetFolder);
      successCount++;
    } catch {
      toastError('Error moving conversations');
    }
  }
  return successCount;
}

interface DropAreaState {
  readonly isValidDropTarget: boolean;
  readonly isActive: boolean;
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

/**
 * Ported from `apps/elitea-ui/src/hooks/chat/useDragAndDrop.js` (unit C2) —
 * pure `@dnd-kit` orchestration, no API calls of its own (it only invokes
 * the `onMoveToFolderConversation`/`onReorderFolders` callbacks it is given
 * as params, matching the brief).
 */
export function useDragAndDrop(params: UseDragAndDropParams): UseDragAndDropResult {
  const { onMoveToFolderConversation, onReorderFolders, folders, originalFolders, conversations, selectedConversations = [], toastSuccess, toastError } = params;
  const foldersForReordering = originalFolders ?? folders;

  const [activeId, setActiveId] = useState<UniqueIdentifier | null>(null);
  const [draggedItems, setDraggedItems] = useState<readonly Conversation[]>([]);
  const [draggedFromFolder, setDraggedFromFolder] = useState<FolderListItem | null>(null);
  const [draggedFolder, setDraggedFolder] = useState<FolderListItem | null>(null);
  const [isDraggingFolder, setIsDraggingFolder] = useState(false);

  const handleFolderReordering = useCallback(
    async (over: NonNullable<DragEndEvent['over']>, active: DragEndEvent['active']): Promise<void> => {
      const dropTargetId = over.id;
      if (typeof dropTargetId !== 'string' || !dropTargetId.startsWith(FOLDER_ID_PREFIX) || active.id === dropTargetId) return;

      const overData = over.data.current as { readonly folder?: FolderListItem } | undefined;
      const targets = resolveFolderReorderTargets(folders, active.id, dropTargetId, overData?.folder);
      if (targets === undefined) return;

      const foldersToSubmit = computeFoldersOrderForDrop(
        folders,
        targets.draggedFolderId,
        targets.draggedFolderIndex,
        targets.targetFolderIndex,
        targets.targetFolder?.isPinned,
        !isPinnedFolder(targets.draggedFolderData),
      );
      await onReorderFolders?.(foldersToSubmit);
    },
    [folders, onReorderFolders],
  );

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const handleDragStart = useCallback(
    (event: DragStartEvent): void => {
      const { active } = event;
      setActiveId(active.id);

      if (typeof active.id === 'string' && active.id.startsWith(FOLDER_ID_PREFIX)) {
        const folderId = active.id.slice(FOLDER_ID_PREFIX.length);
        const draggedFolderObj = folders.find((f) => folderIdMatches(f, folderId));

        if (draggedFolderObj !== undefined) {
          setIsDraggingFolder(true);
          setDraggedFolder(draggedFolderObj);
          setDraggedItems([]);
          setDraggedFromFolder(null);
          return;
        }
      }

      setIsDraggingFolder(false);
      setDraggedFolder(null);

      const draggedConversation =
        conversations.find((conv) => conv.id === active.id) ?? foldersForReordering.flatMap((folder) => folder.conversations).find((conv) => conv.id === active.id);

      if (draggedConversation === undefined) return;

      const isSelected = selectedConversations.some((selected) => selected.id === draggedConversation.id);
      const conversationsToDrag = isSelected && selectedConversations.length > 1 ? selectedConversations : [draggedConversation];

      const sourceFolder =
        draggedConversation.folderId !== undefined ? (foldersForReordering.find((folder) => folder.id === draggedConversation.folderId) ?? null) : null;

      setDraggedItems(conversationsToDrag);
      setDraggedFromFolder(sourceFolder);
    },
    [conversations, folders, selectedConversations, foldersForReordering],
  );

  const resetDragState = useCallback((): void => {
    setActiveId(null);
    setDraggedItems([]);
    setDraggedFromFolder(null);
    setDraggedFolder(null);
    setIsDraggingFolder(false);
  }, []);

  // Bundles every value `handleDragEnd` reads-but-doesn't-itself-own into one
  // ref via `useLatestRef` — purely to bring the `useCallback` dependency
  // array under the §3.5 `hook-deps` budget (8) without changing behaviour.
  // Safe per `useLatestRef`'s own doc comment: `handleDragEnd` only fires on
  // a LATER drag event, by which point a fresh render (and thus a fresh ref
  // assignment) has always already happened.
  const dragEndInputsRef = useLatestRef({
    activeId,
    draggedItems,
    draggedFolder,
    isDraggingFolder,
    folders,
    conversations,
    foldersForReordering,
    onMoveToFolderConversation,
    onReorderFolders,
    toastSuccess,
    toastError,
  });

  const handleDragEnd = useCallback(
    async (event: DragEndEvent): Promise<void> => {
      const { over, active } = event;
      const {
        activeId: currentActiveId,
        draggedItems: currentItems,
        draggedFolder: currentFolder,
        isDraggingFolder: currentIsDraggingFolder,
        folders: currentFolders,
        conversations: currentConversations,
        foldersForReordering: currentFoldersForReordering,
        onMoveToFolderConversation: currentOnMoveToFolderConversation,
        onReorderFolders: currentOnReorderFolders,
        toastSuccess: currentToastSuccess,
        toastError: currentToastError,
      } = dragEndInputsRef.current;

      if (currentActiveId !== null && active.id !== currentActiveId) {
        resetDragState();
        return;
      }

      const currentDraggedItems = [...currentItems];
      const wasDraggingFolder = currentIsDraggingFolder;
      const currentDraggedFolder = currentFolder;
      resetDragState();

      if (over === null || over === undefined) return;

      if (shouldReorderFolders(wasDraggingFolder, currentDraggedFolder, currentOnReorderFolders)) {
        await handleFolderReordering(over, active);
        return;
      }

      if (currentDraggedItems.length === 0) return;

      const dropTarget = resolveDropTarget(currentFolders, over.id);
      if (dropTarget === undefined) return;

      try {
        const successCount = await moveDraggedConversationsToTarget(
          currentDraggedItems,
          currentConversations,
          currentFoldersForReordering,
          dropTarget.targetFolder,
          currentOnMoveToFolderConversation,
          currentToastError,
        );

        if (shouldAnnounceMoveSuccess(successCount, currentToastSuccess !== undefined, currentDraggedItems.length)) {
          currentToastSuccess?.(buildMoveSuccessMessage(successCount, dropTarget.targetLocation));
        }
      } catch {
        currentToastError('Error moving conversations');
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- every `dragEndInputsRef.current.*` read above is intentionally NOT listed: `useLatestRef`'s own doc comment is the contract this callback relies on (`.current` is always the value from whichever render most recently ran, read synchronously — never a stale closure), and listing them would defeat the whole point of bundling them into a ref (recreating `handleDragEnd` on every one of their changes is exactly what the §3.5 `hook-deps` budget flagged as "the hook does too much").
    [handleFolderReordering, resetDragState],
  );

  const handleDragOver = useCallback((): void => {}, []);

  const isDragging = activeId !== null;

  const getDropAreaState = useCallback(
    (dropAreaId: string): DropAreaState => {
      if (!isDragging) return { isValidDropTarget: false, isActive: false };

      if (isDraggingFolder && draggedFolder !== null) {
        if (dropAreaId.startsWith(FOLDER_ID_PREFIX)) {
          const folderId = dropAreaId.slice(FOLDER_ID_PREFIX.length);
          const isValidDropTarget = String(draggedFolder.id) !== folderId;
          return { isValidDropTarget, isActive: isValidDropTarget };
        }
        return { isValidDropTarget: false, isActive: false };
      }

      if (draggedItems.length === 0) return { isValidDropTarget: false, isActive: false };

      const isDroppingFromFolder = draggedFromFolder !== null;
      const isDroppingFromUngrouped = draggedFromFolder === null;

      if (dropAreaId === 'ungrouped-conversations') {
        return { isValidDropTarget: isDroppingFromFolder, isActive: isDroppingFromFolder };
      }

      if (dropAreaId.startsWith(FOLDER_ID_PREFIX)) {
        const folderId = dropAreaId.slice(FOLDER_ID_PREFIX.length);
        const isValidDropTarget = isDroppingFromUngrouped || (isDroppingFromFolder && String(draggedFromFolder?.id) !== folderId);
        return { isValidDropTarget, isActive: isValidDropTarget };
      }

      return { isValidDropTarget: false, isActive: false };
    },
    [isDragging, draggedItems, draggedFromFolder, isDraggingFolder, draggedFolder],
  );

  return {
    sensors,
    activeId,
    draggedItems,
    draggedFromFolder,
    isDragging,
    handleDragStart,
    handleDragEnd,
    handleDragOver,
    getDropAreaState,
  };
}
