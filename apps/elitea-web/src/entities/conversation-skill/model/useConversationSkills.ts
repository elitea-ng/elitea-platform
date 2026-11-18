/**
 * Reads and changes the skills pinned to a conversation (`listConversationSkills`,
 * `pinConversationSkill`, `unpinConversationSkill`).
 *
 * The server holds the list and the bound; a client only asks. A pin names a
 * project skill by id or a Marketplace listing version by reference, and never
 * carries text: the text is read when each turn starts, under the caller's own
 * visibility (ADR-0050 amendment 1, A1.2).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  listConversationSkills,
  pinConversationSkill,
  unpinConversationSkill,
} from '@/shared/api/generated/chat/chat';
import type { ConversationSkillPin, PinConversationSkillRequest } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

/** How many skills one conversation may hold: the five a turn may invoke, shared with the message's own picks. */
export const CONVERSATION_SKILL_LIMIT = 5;

export const conversationSkillKeys = {
  all: (projectId: string) => ['conversation-skills', projectId] as const,
  list: (projectId: string, conversationId: string) => ['conversation-skills', projectId, conversationId] as const,
};

/** The pins of a conversation, oldest first. Nothing is read without a project and a saved conversation. */
export function useConversationSkills(projectId: string | undefined, conversationId: string | undefined) {
  return useQuery({
    queryKey: conversationSkillKeys.list(projectId ?? '', conversationId ?? ''),
    queryFn: async (): Promise<readonly ConversationSkillPin[]> =>
      unwrapListPage<ConversationSkillPin>(
        await listConversationSkills(projectId ?? '', conversationId ?? ''),
        'conversationSkills',
      ).rows,
    enabled: projectId !== undefined && projectId !== '' && conversationId !== undefined && conversationId !== '',
  });
}

/** Pin and unpin; each refreshes the list. A refusal (404, 409 at five) rejects, so the caller can say why. */
export function useConversationSkillMutations(projectId: string | undefined, conversationId: string | undefined) {
  const queryClient = useQueryClient();
  const refresh = async (): Promise<void> => {
    if (projectId !== undefined && conversationId !== undefined) {
      await queryClient.invalidateQueries({ queryKey: conversationSkillKeys.list(projectId, conversationId) });
    }
  };
  return {
    pin: useMutation({
      mutationFn: (request: PinConversationSkillRequest) =>
        pinConversationSkill(projectId ?? '', conversationId ?? '', request),
      onSuccess: refresh,
    }),
    unpin: useMutation({
      mutationFn: (pinId: number) => unpinConversationSkill(projectId ?? '', conversationId ?? '', pinId),
      onSuccess: refresh,
    }),
  };
}
