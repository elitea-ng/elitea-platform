/**
 * The real attach path issue #867 wires up — split out of
 * `ChatWithEditors.hooks.ts` purely to keep that file under the §3.5
 * 400-line budget (same reason `editorParticipantAdapters.ts` is its own
 * file rather than inline there).
 *
 * POSTs a newly-created agent/pipeline/toolkit onto the CURRENT
 * conversation's participant list, via the same `useAddParticipantMutation`
 * (`@/entities/participant`) the "+" menu's existing-entity picker
 * (`widgets/chat-box`'s `useAddEntityParticipant`) already posts through.
 *
 * Returns `undefined` (skips the POST) rather than throwing when there is
 * no conversation yet — a brand-new, unsent chat carries no `conversationId`
 * to attach a participant to, and creating one from here would duplicate
 * `widgets/chat-box`'s own lazy-create-on-first-participant logic one layer
 * away from the state (`activeConversation`) that logic already owns. This
 * is the one remaining disclosed gap: creating an agent/pipeline/toolkit
 * from a conversation THAT ALREADY EXISTS attaches it for real; from the
 * welcome screen's still-draft chat, the entity is created but not attached
 * until the user sends a first message and a real conversation exists.
 */
import { useCallback } from 'react';

import { useAddParticipantMutation, type Participant } from '@/entities/participant';

/** One participant-to-add entry, the wire shape `useAddParticipantMutation` posts — matches `entities/participant`'s own (unexported) `ParticipantAddInput`. */
export interface ParticipantAttachInput {
  readonly entity_name: string;
  readonly entity_meta?: Readonly<Record<string, unknown>>;
  readonly entity_settings?: Readonly<Record<string, unknown>>;
}

export type AttachCreatedParticipant = (input: ParticipantAttachInput) => Promise<readonly Participant[] | undefined>;

export function useAttachCreatedParticipant(projectId: string | undefined, conversationId: string | undefined): AttachCreatedParticipant {
  const { mutateAsync } = useAddParticipantMutation();
  return useCallback(
    async (participant: ParticipantAttachInput) => {
      if (projectId === undefined || conversationId === undefined) return undefined;
      try {
        return await mutateAsync({ projectId, conversationId, participants: [participant] });
      } catch (error) {
        // Same swallow-and-log the baseline's `useAgentCreation.js:76-79`
        // already established for this exact failure — creation itself
        // already succeeded and completed; only the attach step failed.
        console.error('Error adding participant:', error);
        return undefined;
      }
    },
    [mutateAsync, projectId, conversationId],
  );
}

/** `Participant`'s camelCase (`entityName`/`entityMeta.id`) back to the snake_case shape `useAgentCreation`'s `onAdded` callback declares. */
export function toAddedParticipantLike(participant: Participant): { readonly entity_name: string; readonly entity_meta?: { readonly id?: string } } {
  return {
    entity_name: participant.entityName,
    ...(participant.entityMeta?.id !== undefined ? { entity_meta: { id: participant.entityMeta.id } } : {}),
  };
}
