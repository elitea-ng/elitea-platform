/**
 * The conversation rail's actions beyond pin, rename, move, archive and
 * delete (`useConversationRail`): Duplicate, Export, the visibility switch
 * (Make public / Restrict access) and the folder's own Rename, Pin and Delete.
 * Each goes through the call the chat list has always made for it, and each
 * change marks the cached listing stale (`invalidate`), so the rows end where
 * the server put them.
 */
import { useCallback } from 'react';

import type { Conversation } from '@/entities/conversation';
import { conversationApi } from '@/entities/conversation';

import { conversationExportErrorMessage } from '../errorMessage';
import type { FolderListItem } from './conversationListState.types';
import { useDeleteFolder } from './useDeleteFolder.hooks';
import { useDuplicateConversation } from './useDuplicateConversation.hooks';
import { useEditFolder } from './useEditFolder';

type ConversationExportFormat = 'md' | 'json';

export interface RailExtras {
  /** Copies the conversation (participants, settings, visibility) into a new one; resolves to its id. */
  readonly duplicate: (conversation: Conversation) => Promise<string | undefined>;
  /** Downloads the transcript in the format named. */
  readonly exportAs: (conversation: Conversation, format: ConversationExportFormat) => void;
  /** Make public (`false`) or Restrict access (`true`). */
  readonly setPrivate: (conversation: Conversation, isPrivate: boolean) => void;
  readonly renameFolder: (folderId: string, name: string) => void;
  readonly pinFolder: (folderId: string, isPinned: boolean) => void;
  readonly deleteFolder: (folderId: string) => void;
}

interface ExtrasContainers {
  readonly folders: readonly FolderListItem[];
  readonly setFolders: Parameters<typeof useDeleteFolder>[0]['setFolders'];
  readonly transform: (change: (items: readonly Conversation[]) => readonly Conversation[]) => void;
}

function useFolderActions(projectId: string | undefined, containers: ExtrasContainers, toastError: (message: string) => void) {
  const { folders, setFolders } = containers;
  const noActiveFolder = useCallback(() => undefined, []);
  const { onEditFolder, onPinFolder } = useEditFolder({ projectId, activeFolder: undefined, setActiveFolder: noActiveFolder, setFolders, toastError });
  const { onDeleteFolder } = useDeleteFolder({ projectId, setFolders, toastError });
  const find = useCallback((folderId: string) => folders.find((folder) => folder.id === folderId), [folders]);
  const renameFolder = useCallback(
    (folderId: string, name: string) => {
      const folder = find(folderId);
      const trimmed = name.trim();
      if (folder !== undefined && trimmed !== '' && trimmed !== folder.name) void onEditFolder({ ...folder, name: trimmed });
    },
    [find, onEditFolder],
  );
  const pinFolder = useCallback(
    (folderId: string, isPinned: boolean) => {
      const folder = find(folderId);
      if (folder !== undefined) void onPinFolder(folder, isPinned);
    },
    [find, onPinFolder],
  );
  const deleteFolder = useCallback(
    (folderId: string) => {
      const folder = find(folderId);
      if (folder !== undefined) void onDeleteFolder(folder);
    },
    [find, onDeleteFolder],
  );
  return { renameFolder, pinFolder, deleteFolder };
}

export function useConversationRailExtras(projectId: string | undefined, containers: ExtrasContainers, toastError: (message: string) => void, invalidate: () => void): RailExtras {
  const { transform } = containers;
  const duplicate = useDuplicateConversation(projectId, toastError);
  const exportAs = useCallback(
    (conversation: Conversation, format: ConversationExportFormat) => {
      if (projectId === undefined) return;
      void conversationApi.downloadExport({ projectId, conversationId: conversation.id, conversationName: conversation.name, format }).then((result) => {
        if (!result.ok) toastError(conversationExportErrorMessage(result.error));
      });
    },
    [projectId, toastError],
  );
  const setPrivate = useCallback(
    (conversation: Conversation, isPrivate: boolean) => {
      if (projectId === undefined) return;
      void conversationApi
        .edit({ projectId, id: conversation.id, name: conversation.name, is_private: isPrivate })
        .then(() => {
          transform((items) => items.map((item) => (item.id === conversation.id ? { ...item, isPrivate } : item)));
          invalidate();
        })
        .catch(() => toastError('Failed to edit the conversation'));
    },
    [invalidate, projectId, toastError, transform],
  );
  const folderActions = useFolderActions(projectId, containers, toastError);
  return { duplicate, exportAs, setPrivate, ...folderActions };
}
