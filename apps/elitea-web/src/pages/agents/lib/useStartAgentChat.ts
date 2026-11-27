import { useCallback, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';
import { defaultRunVersionId } from '@/entities/version';
import { getApplication } from '@/shared/api/generated/applications/applications';
import type { ApplicationDetail, SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { applicationParticipant } from '@/shared/lib/editorTestConversation';

/** `ChatBox` truncates a conversation name to 50 characters; match it so the sidebar entry reads the same everywhere. */
const MAX_CONVERSATION_NAME = 50;

/** What one start needs: the agent, the version it runs and the signed-in user. */
export interface AgentChatInput {
  readonly projectId: string;
  readonly applicationId: string;
  readonly name: string | undefined;
  readonly versionId: string;
  readonly agentType: string;
  readonly userId: string;
}

type CreateConversation = ReturnType<typeof conversationApi.useCreate>['mutateAsync'];
type AddParticipants = ReturnType<typeof useAddParticipantMutation>['mutateAsync'];

/**
 * Creates a private conversation and attaches the user and the agent: the
 * same steps the chat page's "+" menu performs. Answers the new
 * conversation's id. `entity_settings.version_id` is load-bearing (see
 * `ChatWithAgentButton`): a participant without it refuses every turn.
 */
export async function openAgentConversation(
  steps: { readonly createConversation: CreateConversation; readonly addParticipants: AddParticipants },
  input: AgentChatInput,
): Promise<string> {
  const conversation = await steps.createConversation({
    projectId: input.projectId,
    name: (input.name ?? '').slice(0, MAX_CONVERSATION_NAME) || t('pages.agents.chatWithAgent.defaultName', 'New Chat'),
    is_private: true,
  });
  const conversationId = String(conversation.id);
  await steps.addParticipants({
    projectId: input.projectId,
    conversationId,
    participants: [
      { entity_name: 'user', entity_meta: { id: Number(input.userId) } },
      applicationParticipant({
        applicationId: input.applicationId,
        projectId: input.projectId,
        name: input.name,
        versionId: input.versionId,
        agentType: input.agentType,
      }),
    ],
  });
  return conversationId;
}

/** The signed-in user's id from the current-author query, through the one envelope peel (`unwrapBody`). */
export function currentUserIdOf(data: unknown): string | undefined {
  const id = (unwrapBody(data) as SocialAuthorProfile | undefined)?.id;
  return id === undefined ? undefined : String(id);
}

/** The version a chat from the list runs: the agent's default, and its agent type. */
function runVersionOf(detail: ApplicationDetail): { readonly versionId: string; readonly agentType: string } | undefined {
  const versions = detail.versions;
  const versionId = defaultRunVersionId(versions, versions.find((version) => version.is_default === true)?.id);
  if (versionId === undefined) return undefined;
  const agentType = versions.find((version) => Number(version.id) === versionId)?.agent_type ?? 'openai';
  return { versionId: String(versionId), agentType };
}

/**
 * The Agents card's Chat (issue 3133): reads the agent's default version,
 * starts a private conversation with it and opens it. `startingId` is the
 * agent being started, so its card can disable the button; `error` says why
 * the last start failed, in the server's words when it gave some.
 */
export function useStartAgentChat(projectId: string | undefined) {
  const navigate = useNavigate();
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const userId = currentUserIdOf(useGetCurrentAuthor().data);
  const [startingId, setStartingId] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);

  const start = useCallback(
    async (applicationId: string, name: string) => {
      if (projectId === undefined || userId === undefined) return;
      setStartingId(applicationId);
      setError(undefined);
      try {
        const detail = unwrapBody(await getApplication(projectId, Number(applicationId))) as ApplicationDetail;
        const run = runVersionOf(detail);
        if (run === undefined) {
          setError(t('pages.agents.list.chatNoVersion', 'This agent has no saved version to chat with.'));
          return;
        }
        const conversationId = await openAgentConversation({ createConversation, addParticipants }, { projectId, applicationId, name, userId, ...run });
        void navigate({ to: '/chat/$conversationId', params: { conversationId } });
      } catch (cause) {
        setError(serverErrorMessage(cause, t('pages.agents.list.chatFailed', 'The chat could not be started. Try again.')));
      } finally {
        setStartingId(undefined);
      }
    },
    [projectId, userId, createConversation, addParticipants, navigate],
  );

  return { start, startingId, ready: projectId !== undefined && userId !== undefined, error, clearError: () => setError(undefined) };
}
