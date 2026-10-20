/**
 * Archive and unarchive a conversation (client contract 1.10,
 * `archiveConversation` / `unarchiveConversation`). The archive is the
 * project's, as the pin is: the row leaves the lists of every member and goes
 * to the project's Archived chats, where any member can unarchive it.
 *
 * Archive is optimistic, like pin and move: the row leaves every list
 * container at once (`removeRow`), then the request runs. On a failure the
 * grouped listing is read again — the server's answer puts the row back where
 * it belongs, which a hand-written rollback could only guess — and the person
 * is told. Both directions refresh the archived list (`archived=only`), and
 * unarchive refreshes the grouped listing, where the row returns.
 */
import { useCallback } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import type { Conversation } from '@/entities/conversation';
import { archiveConversation, getListConversationsQueryKey, unarchiveConversation } from '@/shared/api/generated/chat/chat';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;

export interface UseArchiveConversationParams {
  readonly projectId: string | undefined;
  /** Takes the row out of every container the list renders from. */
  readonly removeRow: (conversationId: string) => void;
  readonly toastError: (message: string) => void;
}

export interface UseArchiveConversationResult {
  readonly archive: (conversation: Pick<Conversation, 'id'>) => Promise<boolean>;
  readonly unarchive: (conversation: Pick<Conversation, 'id'>) => Promise<boolean>;
}

export function useArchiveConversation({ projectId, removeRow, toastError }: UseArchiveConversationParams): UseArchiveConversationResult {
  const queryClient = useQueryClient();

  const refresh = useCallback(
    (lists: 'archived' | 'both') => {
      if (projectId === undefined) return;
      // The conversations list key starts with its URL; the prefix covers every `archived=` page.
      void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(projectId).slice(0, 1) });
      if (lists === 'both') void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
    },
    [projectId, queryClient],
  );

  const archive = useCallback(
    async (conversation: Pick<Conversation, 'id'>): Promise<boolean> => {
      if (projectId === undefined) return false;
      removeRow(conversation.id);
      try {
        await archiveConversation(projectId, conversation.id);
        refresh('archived');
        return true;
      } catch {
        refresh('both');
        toastError('Failed to archive the conversation');
        return false;
      }
    },
    [projectId, refresh, removeRow, toastError],
  );

  const unarchive = useCallback(
    async (conversation: Pick<Conversation, 'id'>): Promise<boolean> => {
      if (projectId === undefined) return false;
      try {
        await unarchiveConversation(projectId, conversation.id);
        return true;
      } catch {
        toastError('Failed to unarchive the conversation');
        return false;
      } finally {
        refresh('both');
      }
    },
    [projectId, refresh, toastError],
  );

  return { archive, unarchive };
}
