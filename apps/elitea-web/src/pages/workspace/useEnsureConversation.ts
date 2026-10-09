/**
 * Creates the server conversation on the first send, exactly the way the
 * agents page's "Chat with agent" does: the conversation, then the USER and
 * APPLICATION participants (nothing server-side adds the user on the REST
 * path, and the agent resolver refuses a conversation without that mapping).
 */
import { useCallback } from 'react';

import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';

const MAX_NAME = 80;

export interface EnsureConversationInput {
  projectId: number;
  /** An existing conversation to reuse; '' creates one. */
  conversationId: string;
  prompt: string;
  applicationId: number;
  applicationName: string;
  versionId: number;
  agentType: string;
}

export function useEnsureConversation(): (input: EnsureConversationInput) => Promise<string> {
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const author = useGetCurrentAuthor();
  const userId = (unwrapBody(author.data) as SocialAuthorProfile | undefined)?.id;

  return useCallback(
    async (input) => {
      if (input.conversationId !== '') return input.conversationId;
      if (userId === undefined) throw new Error('The signed-in user is not loaded yet.');
      const conversation = await createConversation({
        projectId: input.projectId,
        name: input.prompt.trim().slice(0, MAX_NAME) || input.applicationName,
        is_private: true,
      });
      const id = String(conversation.id);
      await addParticipants({
        projectId: input.projectId,
        conversationId: id,
        participants: [
          { entity_name: 'user', entity_meta: { id: Number(userId) } },
          {
            entity_name: 'application',
            entity_meta: { id: input.applicationId, name: input.applicationName, project_id: input.projectId },
            entity_settings: { version_id: input.versionId, agent_type: input.agentType, variables: [], icon_meta: {} },
          },
        ],
      });
      return id;
    },
    [addParticipants, createConversation, userId],
  );
}
