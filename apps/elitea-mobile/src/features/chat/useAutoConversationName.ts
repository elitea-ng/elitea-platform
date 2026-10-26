import { useQueryClient } from '@tanstack/react-query';
import { useEffect } from 'react';

import { generateConversationName } from '@/api/queries/conversationName';
import type { ConversationDetail } from '@/api/queries/conversation';
import { queryKeys } from '@/api/queries/keys';
import type { ChatMessage } from '@/features/chat/messageModel';
import { participantName } from '@/features/chat/messageModel';
import { log } from '@/lib/log';

/** The names the clients give a new conversation: the web's and the desktop's cut of the first message, the defaults. */
const DEFAULT_NAMES = ['New Chat', 'New conversation'];

/**
 * The conversation's name when it is still an automatic one (the agent's or the model's name, a
 * cut of the first message, a default) and its first exchange is complete; else null.
 */
export function nameToReplace(
  detail: Pick<ConversationDetail, 'name' | 'participants'> | undefined,
  messages: readonly Pick<ChatMessage, 'role' | 'text' | 'isStreaming'>[],
): string | null {
  const name = detail?.name;
  if (typeof name !== 'string') return null;
  const questions = messages.filter((m) => m.role === 'user');
  const answered = messages.some((m) => m.role === 'assistant' && m.text.trim() !== '');
  const question = questions[0]?.text;
  if (questions.length !== 1 || !answered || messages.some((m) => m.isStreaming) || question === undefined) return null;
  const participants = Array.isArray(detail?.participants) ? detail.participants : [];
  const automatic = [
    question.slice(0, 50),
    question.trim().slice(0, 80),
    ...DEFAULT_NAMES,
    ...participants.filter((p) => p?.entity_name !== 'user').map((p) => participantName(p)?.slice(0, 200) ?? ''),
  ];
  return automatic.includes(name) && name !== '' ? name : null;
}

const asked = new Set<string>();

interface AutoNameInput {
  projectId: number | null;
  conversationId: number | null;
  detail: Pick<ConversationDetail, 'name' | 'participants'> | undefined;
  messages: readonly Pick<ChatMessage, 'role' | 'text' | 'isStreaming'>[];
  /** The workspace speaks client contract 1.24, the thread is not the offline copy, and the device is online. */
  enabled: boolean;
}

/**
 * Names a new chat from its first exchange once its first answer is in (client contract 1.24),
 * as the web and the desktop do. Asked at most once per chat while the app runs; a failure keeps
 * the name. The thread and the Chats list read the new name again.
 */
export function useAutoConversationName({ projectId, conversationId, detail, messages, enabled }: AutoNameInput): void {
  const queryClient = useQueryClient();
  const expected = enabled ? nameToReplace(detail, messages) : null;
  useEffect(() => {
    if (projectId === null || conversationId === null || expected === null) return;
    const key = `${projectId}:${conversationId}`;
    if (asked.has(key)) return;
    asked.add(key);
    generateConversationName(projectId, conversationId, expected)
      .then(({ renamed }) => {
        if (!renamed) return;
        void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
        void queryClient.invalidateQueries({ queryKey: ['p', projectId, 'conversation'] });
      })
      .catch((error: unknown) => log.warn('chat: the conversation could not be named', { error }));
  }, [projectId, conversationId, expected, queryClient]);
}
