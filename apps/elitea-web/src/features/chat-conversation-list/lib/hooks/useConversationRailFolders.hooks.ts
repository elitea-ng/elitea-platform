/**
 * Folder management for `useConversationRail` (the desktop sidebar's chats):
 * a new folder and "Move to folder…", through the SAME hooks the web rail
 * wires (`useCreateFolder`, `useMoveToFolderConversation`), into the rail's
 * own containers.
 *
 * The rail has no ungrouped list of its own: an ungrouped conversation is a
 * row of a date group. So the move hook's `setConversations` (its ungrouped
 * list) is applied to the date groups: a filter removes the row from every
 * group, and a row that comes back out of a folder goes to the newest group
 * only (the move stamps it `updatedAt` now). The rail's rows do not carry
 * `folderId`, so the move is given the folder that holds the row now. After a
 * move the grouped listing is read again (`invalidate`), so the rows end where
 * the server put them.
 */
import { useCallback, type Dispatch, type SetStateAction } from 'react';

import type { Conversation } from '@/entities/conversation';

import type { DateGroupListItem, FolderListItem } from './conversationListState.types';
import { useCreateFolder } from './useCreateFolder.hooks';
import { useMoveToFolderConversation, type ConversationWithTargetFolder } from './useMoveToFolderConversation.hooks';

export interface RailFolderContainers {
  readonly folders: readonly FolderListItem[];
  readonly setFolders: Dispatch<SetStateAction<readonly FolderListItem[]>>;
  readonly setDateGroups: Dispatch<SetStateAction<readonly DateGroupListItem[]>>;
}

export interface RailFolders {
  /** Creates a folder with this name (a blank name does nothing). */
  readonly createFolder: (name: string) => Promise<void>;
  /** Moves the conversation into the folder, or out of every folder with `null`. */
  readonly moveToFolder: (conversation: Conversation, folderId: string | null) => Promise<void>;
}

/** The move hook's ungrouped-list updates, applied to the date groups (see the module doc). */
function useUngroupedAsDateGroups(setDateGroups: RailFolderContainers['setDateGroups']): Dispatch<SetStateAction<readonly ConversationWithTargetFolder[]>> {
  return useCallback(
    (action) => {
      if (typeof action !== 'function') return;
      setDateGroups((groups) =>
        groups.map((group, index) => {
          const next = action(group.conversations);
          if (index === 0 || next.length <= group.conversations.length) return { ...group, conversations: next };
          const held = new Set(group.conversations.map((conversation) => conversation.id));
          return { ...group, conversations: next.filter((conversation) => held.has(conversation.id)) };
        }),
      );
    },
    [setDateGroups],
  );
}

export function useConversationRailFolders(
  projectId: string | undefined,
  containers: RailFolderContainers,
  toastError: (message: string) => void,
  invalidate: () => void,
): RailFolders {
  const { folders, setFolders, setDateGroups } = containers;
  const noActiveFolder = useCallback(() => undefined, []);
  const setUngrouped = useUngroupedAsDateGroups(setDateGroups);
  const { onCreateFolder } = useCreateFolder({ projectId, setActiveFolder: noActiveFolder, setFolders, toastError });
  const { onMoveToFolderConversation } = useMoveToFolderConversation({ projectId, setFolders, setActiveFolder: noActiveFolder, setConversations: setUngrouped, toastError, folders });

  const createFolder = useCallback(
    async (name: string): Promise<void> => {
      const trimmed = name.trim();
      if (trimmed === '') return;
      await onCreateFolder({ id: `new-folder-${String(Date.now())}`, name: trimmed, conversations: [], isNew: true });
    },
    [onCreateFolder],
  );

  const moveToFolder = useCallback(
    async (conversation: Conversation, folderId: string | null): Promise<void> => {
      const holder = folders.find((folder) => folder.conversations.some((item) => item.id === conversation.id));
      const target = folderId === null ? null : (folders.find((folder) => folder.id === folderId) ?? null);
      if (folderId !== null && target === null) return;
      const { folderId: _stale, ...rest } = conversation;
      const moving: ConversationWithTargetFolder = holder === undefined ? rest : { ...rest, folderId: holder.id };
      const result = await onMoveToFolderConversation(moving, target);
      if (result.success) invalidate();
    },
    [folders, invalidate, onMoveToFolderConversation],
  );

  return { createFolder, moveToFolder };
}
