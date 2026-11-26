/**
 * The team of a Local work thread (ADR-0061 decision 2a, #3055): its conversation as the chat's participants panel
 * reads it, its agents, the lead, and the writes the Participants tab makes. Every write is the chat's own: the lead
 * through `metaWithLead` (`useConversationLead`), where an agent runs through the participant's `entity_settings`
 * (the whole object, `executor: cloud` set or dropped, as `executorFixes` writes it), and Remove through the
 * participant delete. Each write also drops the kept thread floor (`threadFloorQuery`), so the next send routes on it.
 */
import { useCallback, useMemo } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';

type ConversationWire = NonNullable<ReturnType<typeof conversationApi.useDetails>['data']>;
import { useDeleteParticipantMutation, useUpdateParticipantSettingsMutation } from '@/entities/participant';
import { useConversationLead } from '@/pages/chat/ChatAgentLead';

import { threadAgentsOf, type ThreadAgent } from './useEnsureConversation';

export interface ThreadTeam {
  /** The conversation (`undefined` before its first turn, or while it loads). */
  readonly conversation: ConversationWire | undefined;
  readonly agents: readonly ThreadAgent[];
  /** The lead's participant id; 0 with no lead. */
  readonly leadParticipantId: number;
  readonly setLead: (participantId: number) => Promise<void>;
  /** Runs `participantId` here (`false`) or in the cloud (`true`). */
  readonly setCloud: (participantId: number, cloud: boolean) => Promise<void>;
  /** Removes `participantId` from the thread; the lead's removal clears the lead first. */
  readonly remove: (participantId: number) => Promise<void>;
  readonly saving: boolean;
}

export interface UseThreadTeamInput {
  readonly projectId: number;
  /** The thread; `''` before its first turn. */
  readonly conversationId: string;
}

/** The participant's `entity_settings` to write so it runs in the cloud (`cloud`) or here. */
export function executorSettings(participant: unknown, cloud: boolean): Record<string, unknown> {
  const settings = (participant as { readonly entity_settings?: Record<string, unknown> } | null)?.entity_settings ?? {};
  const { executor: _executor, ...rest } = settings;
  return cloud ? { ...rest, executor: 'cloud' } : rest;
}

export function useThreadTeam({ projectId, conversationId }: UseThreadTeamInput): ThreadTeam {
  const queryClient = useQueryClient();
  const project = String(projectId);
  const details = conversationApi.useDetails({ projectId: project, id: conversationId }, { enabled: conversationId !== '' });
  const conversation = conversationId === '' ? undefined : details.data;
  const agents = useMemo(() => threadAgentsOf(conversation?.participants as Parameters<typeof threadAgentsOf>[0]), [conversation]);
  const lead = useConversationLead({ projectId: project, conversationId: conversationId === '' ? undefined : conversationId, meta: conversation?.meta });
  const settingsWrite = useUpdateParticipantSettingsMutation();
  const deleteWrite = useDeleteParticipantMutation();

  const refloor = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ['workspace', 'threadFloor', projectId, conversationId] }),
    [queryClient, projectId, conversationId],
  );
  const { setLead: writeLead, leadParticipantId } = lead;
  const setLead = useCallback(async (participantId: number) => {
    await writeLead(participantId);
    await refloor();
  }, [writeLead, refloor]);

  const { mutateAsync: updateSettings } = settingsWrite;
  const setCloud = useCallback(async (participantId: number, cloud: boolean) => {
    const participant = conversation?.participants?.find((row) => Number(row.id) === participantId);
    if (participant === undefined) return;
    await updateSettings({ projectId: project, conversationId, participantId: String(participantId), settings: executorSettings(participant, cloud) });
    await refloor();
  }, [conversation, updateSettings, project, conversationId, refloor]);

  const { mutateAsync: deleteParticipant } = deleteWrite;
  const remove = useCallback(async (participantId: number) => {
    // A lead that left the thread would route every unaddressed message to no one: clear it first (the chat's rule).
    if (participantId === leadParticipantId) await writeLead(0);
    await deleteParticipant({ projectId: project, conversationId, id: String(participantId) });
    await refloor();
  }, [leadParticipantId, writeLead, deleteParticipant, project, conversationId, refloor]);

  return {
    conversation,
    agents,
    leadParticipantId,
    setLead,
    setCloud,
    remove,
    saving: lead.isSaving || settingsWrite.isPending || deleteWrite.isPending,
  };
}
