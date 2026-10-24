/**
 * Names a new chat from its first exchange (client contract 1.24,
 * `generateConversationName`). A chat is called after the start of its
 * first message when it is made (`New Chat` without one); once its first
 * answer is in, the server asks the project's default model for a short
 * title and renames the chat, but only while its name is still that
 * automatic one, so a person's rename is never overwritten. Asked at most
 * once per chat and page load; the rail reads the new name again.
 */
import { useEffect } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { generateConversationName, getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import { t } from '@/shared/i18n';

interface HistoryRow {
  readonly role?: unknown;
  readonly content?: unknown;
  readonly isStreaming?: unknown;
  readonly isLoading?: unknown;
}

interface ConversationLike {
  readonly id?: unknown;
  readonly name?: unknown;
  readonly chat_history?: readonly unknown[];
}

const asked = new Set<string>();

/** The names a client gives a new conversation from its first message: the web's, the desktop's, the default. */
function automaticNames(question: string): string[] {
  return [question.slice(0, 50), question.trim().slice(0, 80), t('widgets.chatBox.defaultConversationName', 'New Chat')];
}

/** The chat's name when it is still an automatic one and its first exchange is complete; else `undefined`. */
export function nameToReplace(conversation: unknown): string | undefined {
  const { name, chat_history: history } = (conversation ?? {}) as ConversationLike;
  if (typeof name !== 'string' || history === undefined) return undefined;
  const rows = history as readonly HistoryRow[];
  const questions = rows.filter((row) => row.role === 'user');
  const answered = rows.some((row) => row.role !== 'user' && typeof row.content === 'string' && row.content.trim() !== '');
  const busy = rows.some((row) => row.isStreaming === true || row.isLoading === true);
  const question = questions[0]?.content;
  if (questions.length !== 1 || !answered || busy || typeof question !== 'string') return undefined;
  return automaticNames(question).includes(name) ? name : undefined;
}

export function useAutoConversationName(projectId: string | undefined, conversation: unknown): void {
  const queryClient = useQueryClient();
  const expected = nameToReplace(conversation);
  const id = (conversation as ConversationLike | undefined)?.id;
  const conversationId = typeof id === 'string' || typeof id === 'number' ? String(id) : undefined;
  useEffect(() => {
    // Conversation ids are per project.
    const key = `${projectId ?? ''}:${conversationId ?? ''}`;
    if (projectId === undefined || conversationId === undefined || expected === undefined || asked.has(key)) return;
    asked.add(key);
    generateConversationName(projectId, conversationId, { expected_name: expected })
      .then(() => {
        void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(projectId).slice(0, 1) });
        void queryClient.invalidateQueries({ queryKey: ['conversation', 'details', projectId] });
      })
      .catch(() => undefined);
  }, [projectId, conversationId, expected, queryClient]);
}
