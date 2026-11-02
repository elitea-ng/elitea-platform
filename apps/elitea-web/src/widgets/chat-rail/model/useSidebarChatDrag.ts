/**
 * Drag a conversation onto a folder in the sidebar's Chats list, or out of a
 * folder onto the recent chats, through the web list's drag model
 * (`useDragAndDrop`: the same drop ids, the same "pinned and playback rows
 * stay", the same no-op rule) and the rail's ONE move path
 * (`ConversationRail.moveToFolder`, the web list's `useMoveToFolderConversation`).
 *
 * The rail's rows do not carry `folderId`; the drag model reads it to know
 * where a row comes from, so the folders' rows are stamped with their folder
 * here. Only a pointer starts a drag: the keyboard way is "Move to folder…".
 */
import { useCallback, useMemo } from 'react';

import { PointerSensor, useSensor, useSensors } from '@dnd-kit/core';

import type { Conversation } from '@/entities/conversation';
import { useDragAndDrop, type FolderListItem } from '@/features/chat-conversation-list';

import type { ChatRail } from './chatRail';
import { useDesktopLayout } from './desktopLayout.store';

/** The web list's drop ids (`useDragAndDrop.positioning.ts` `FOLDER_ID_PREFIX`, `resolveDropTarget`). */
export const folderDropId = (folderId: string): string => `folder-${folderId}`;
export const UNGROUPED_DROP_ID = 'ungrouped-conversations';

function notice(message: string): void {
  useDesktopLayout.getState().setNotice(message);
}

export function useSidebarChatDrag(rail: Pick<ChatRail, 'folders' | 'pinned' | 'dateGroups' | 'moveToFolder'>) {
  const { folders: railFolders, pinned, dateGroups, moveToFolder } = rail;
  const folders = useMemo<FolderListItem[]>(
    () => railFolders.map((folder) => ({ ...folder, conversations: folder.conversations.map((conversation) => ({ ...conversation, folderId: folder.id })) })),
    [railFolders],
  );
  const conversations = useMemo<Conversation[]>(() => [...pinned, ...dateGroups.flatMap((group) => group.conversations)], [pinned, dateGroups]);
  const onMoveToFolderConversation = useCallback(
    (conversation: Conversation, target: FolderListItem | null) => moveToFolder(conversation, target === null ? null : target.id),
    [moveToFolder],
  );
  const drag = useDragAndDrop({ onMoveToFolderConversation, folders, originalFolders: folders, conversations, toastError: notice });
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 6 } }));
  return { ...drag, sensors };
}
