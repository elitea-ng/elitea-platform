/**
 * Creates the server conversation on the first send, exactly the way the
 * agents page's "Chat with agent" does: the conversation, then the USER and
 * APPLICATION participants (nothing server-side adds the user on the REST
 * path, and the agent resolver refuses a conversation without that mapping).
 *
 * The conversation is marked as Local work (`source: local_work`), with the
 * folder's NAME (never its path) in `meta`: web Chats keeps it under its
 * "Local work" filter and opens it read-only, because only this computer can
 * continue it.
 *
 * The thread's other agents (ADR-0050 decision 7f, `companions`) are added
 * with it, and on a later send to an existing conversation too (the add is
 * get-or-create): a cloud agent's mapping says `executor: cloud`. The answer
 * names each agent's participant id, which a message's `addressed_to` uses.
 */
import { useCallback } from 'react';

import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import { LOCAL_WORK_SOURCE, localWorkMeta } from '@/shared/lib/localWork';

const MAX_NAME = 80;

/** The agent (or pipeline) a conversation is created for. */
export interface ConversationAgent {
  projectId: number;
  applicationId: number;
  applicationName: string;
  versionId: number;
  agentType: string;
}

/** Another agent of the thread (ADR-0050 decision 7f). */
export interface CompanionAgent {
  applicationId: number;
  applicationName: string;
  versionId: number;
  agentType: string;
  cloud: boolean;
}

/** An agent participant of the thread, as the add answered it. */
export interface ThreadAgent {
  participantId: number;
  applicationId: number;
  name: string;
  versionId: number;
  cloud: boolean;
}

/** A conversation participant as its detail lists it. */
interface DetailParticipant {
  readonly id?: string | number;
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: string | number; readonly name?: unknown; readonly [key: string]: unknown };
  readonly entity_settings?: { readonly [key: string]: unknown };
  readonly meta?: { readonly [key: string]: unknown };
}

/** The agent participants of a conversation's detail (a thread reopened later: its agents are not in the composer's state). */
export function threadAgentsOf(participants: readonly DetailParticipant[] | undefined): ThreadAgent[] {
  return (participants ?? []).flatMap((participant): ThreadAgent[] => {
    const participantId = Number(participant.id);
    const applicationId = Number(participant.entity_meta?.id);
    const versionId = Number(participant.entity_settings?.['version_id']);
    if (participant.entity_name !== 'application' || ![participantId, applicationId, versionId].every((n) => Number.isSafeInteger(n) && n > 0)) return [];
    const named = [participant.meta?.['name'], participant.entity_meta?.name].find((name): name is string => typeof name === 'string' && name.trim() !== '');
    return [{ participantId, applicationId, name: named?.trim() ?? '', versionId, cloud: participant.entity_settings?.['executor'] === 'cloud' }];
  });
}

export interface EnsuredConversation {
  id: string;
  /** The thread's agents, the composer's first; empty when nothing was added (an existing thread of one agent). */
  agents: ThreadAgent[];
}

export interface EnsureConversationInput extends ConversationAgent {
  /** An existing conversation to reuse; '' creates one. */
  conversationId: string;
  /**
   * `conversationId` was created by an earlier attempt that may have failed
   * before its participants were added: add them now. Safe to repeat — the
   * server's add is get-or-create per entity, all-or-nothing per batch.
   */
  completeSetup?: boolean;
  /**
   * Called with the new conversation's id as soon as it exists, BEFORE the
   * participants are added, so a caller can retry in it (with
   * `completeSetup`) instead of creating another one when the add fails.
   */
  onCreated?: (conversationId: string) => void;
  prompt: string;
  /** The workspace folder's display name, recorded on the conversation. */
  folderName: string;
  /** The thread's other agents: added on every send (get-or-create), so a companion added later joins too. */
  companions?: readonly CompanionAgent[];
}

/** A companion's APPLICATION participant: a cloud agent's mapping says `executor: cloud` (Main reads it when it routes). */
export function companionParticipant(projectId: number, companion: CompanionAgent) {
  return {
    entity_name: 'application',
    entity_meta: { id: companion.applicationId, name: companion.applicationName, project_id: projectId },
    entity_settings: {
      version_id: companion.versionId,
      agent_type: companion.agentType,
      variables: [],
      icon_meta: {},
      ...(companion.cloud ? { executor: 'cloud' } : {}),
    },
  };
}

/** The USER and APPLICATION participants a new conversation needs (the "Chat with agent" shape; a pipeline is an application with `agent_type: pipeline`). */
export function agentParticipants(userId: string | number, agent: ConversationAgent) {
  return [
    { entity_name: 'user', entity_meta: { id: Number(userId) } },
    {
      entity_name: 'application',
      entity_meta: { id: agent.applicationId, name: agent.applicationName, project_id: agent.projectId },
      entity_settings: { version_id: agent.versionId, agent_type: agent.agentType, variables: [], icon_meta: {} },
    },
  ];
}

/** The signed-in user's author id (`undefined` until it is read). */
export function useSignedInUserId(): string | number | undefined {
  const author = useGetCurrentAuthor();
  return (unwrapBody(author.data) as SocialAuthorProfile | undefined)?.id;
}

export function useEnsureConversation(): (input: EnsureConversationInput) => Promise<EnsuredConversation> {
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const userId = useSignedInUserId();

  return useCallback(
    async (input) => {
      const companions = input.companions ?? [];
      if (input.conversationId !== '' && input.completeSetup !== true && companions.length === 0) return { id: input.conversationId, agents: [] };
      if (userId === undefined) throw new Error('The signed-in user is not loaded yet.');
      let id = input.conversationId;
      if (id === '') {
        const conversation = await createConversation({
          projectId: input.projectId,
          name: input.prompt.trim().slice(0, MAX_NAME) || input.applicationName,
          is_private: true,
          source: LOCAL_WORK_SOURCE,
          meta: localWorkMeta(input.folderName),
        });
        id = String(conversation.id);
        input.onCreated?.(id);
      }
      const added = await addParticipants({
        projectId: input.projectId,
        conversationId: id,
        participants: [...agentParticipants(userId, input), ...companions.map((companion) => companionParticipant(input.projectId, companion))],
      });
      // The answer lists this request's participants in its order: the user, the composer's agent, the companions.
      const wanted = [{ applicationId: input.applicationId, applicationName: input.applicationName, versionId: input.versionId, cloud: false }, ...companions];
      const agents = wanted.flatMap((agent, index): ThreadAgent[] => {
        const participantId = Number(added[index + 1]?.id);
        return Number.isSafeInteger(participantId) && participantId > 0
          ? [{ participantId, applicationId: agent.applicationId, name: agent.applicationName, versionId: agent.versionId, cloud: agent.cloud }]
          : [];
      });
      return { id, agents };
    },
    [addParticipants, createConversation, userId],
  );
}
