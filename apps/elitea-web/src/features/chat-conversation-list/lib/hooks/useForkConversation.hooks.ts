/**
 * Fork a conversation into a new one (client contract 1.11,
 * `forkConversation`): the whole history ("Fork" on a list row), or the
 * history up to and including one message ("Fork from here" on a message).
 *
 * The server copies the history in one transaction and answers the new list
 * row. The hook refreshes the conversation lists (the conversations list
 * prefix and the grouped folders listing, where the new row shows) and hands
 * the row back, so the caller opens it. A failure is told to the person and
 * answers `undefined`: a 409 means the fork point is not settled yet (a reply
 * is still streaming or waits for an approval).
 */
import { useCallback } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { forkConversation, getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import type { ConversationSummary } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;

/** The new conversation, as the caller needs it to open it. */
interface ForkedConversation {
  /** The numeric id, as a string: the id the `/chat/$conversationId` route takes. */
  readonly id: string;
  readonly uuid: string | undefined;
  readonly name: string;
  readonly source: string | undefined;
}

interface ForkConversationOptions {
  /** The fork point: a message group's id or UUID. Absent forks the whole conversation. */
  readonly messageId?: string | undefined;
  readonly name?: string | undefined;
}

export interface UseForkConversationParams {
  readonly projectId: string | undefined;
  readonly toastError: (message: string) => void;
}

export type ForkConversation = (conversationId: string, options?: ForkConversationOptions) => Promise<ForkedConversation | undefined>;

function failureMessage(error: unknown): string {
  const status = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure.status : undefined;
  if (status === 409) return t('features.chatConversationList.fork.unsettled', 'Wait for the reply to finish, then fork again.');
  if (status === 404) return t('features.chatConversationList.fork.notFound', 'This conversation or message is no longer available.');
  return t('features.chatConversationList.fork.failed', 'Failed to fork the conversation');
}

export function useForkConversation({ projectId, toastError }: UseForkConversationParams): ForkConversation {
  const queryClient = useQueryClient();
  return useCallback<ForkConversation>(
    async (conversationId, options = {}) => {
      if (projectId === undefined) return undefined;
      try {
        const body = {
          ...(options.messageId !== undefined ? { message_id: options.messageId } : {}),
          ...(options.name !== undefined ? { name: options.name } : {}),
        };
        const response = await forkConversation(projectId, conversationId, body);
        const row = response.data as ConversationSummary;
        // The conversations list key starts with its URL; the prefix covers every page and filter.
        void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(projectId).slice(0, 1) });
        void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
        return { id: String(row.id), uuid: row.uuid, name: row.name, source: row.source };
      } catch (error) {
        toastError(failureMessage(error));
        return undefined;
      }
    },
    [projectId, queryClient, toastError],
  );
}
