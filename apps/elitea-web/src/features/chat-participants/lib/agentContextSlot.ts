/**
 * The agent context meter slot (ADR-0050 decision 9b2, issue 1610): which
 * rows get one and which conversation it reads. See
 * `ParticipantsProps.renderAgentContext`.
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { ChatParticipantType } from '../model/constants';
import type { TransformedParticipant } from '../model/types';

/** The page's meter: the conversation the budget may query and the agent. */
export type AgentContextRenderer = (props: { conversationId: string | number; participantId: number }) => ReactNode;

/** The chat participant row id of an agent row, for its meter; undefined for every other row. */
export function agentParticipantId(participant: TransformedParticipant): number | undefined {
  if (participant.entity_name !== ChatParticipantType.Applications) return undefined;
  const id = (participant as unknown as { id?: unknown }).id;
  return typeof id === 'number' && Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

/** The meter for one row, or nothing (not an agent, or no slot). */
export function agentContextOf(
  render: ((props: { participantId: number }) => ReactNode) | undefined,
  participant: TransformedParticipant,
): { contextSlot?: ReactNode } {
  const participantId = render ? agentParticipantId(participant) : undefined;
  return render && participantId !== undefined ? { contextSlot: render({ participantId }) } : {};
}

/** Binds the meter to the conversation the budget may query (none for a new or replayed one). */
export function useAgentContextSlot(
  render: AgentContextRenderer | undefined,
  conversationId: string | number | undefined,
): { renderAgentContext?: (props: { participantId: number }) => ReactNode } {
  return useMemo(
    () => (render && conversationId !== undefined
      ? { renderAgentContext: ({ participantId }: { participantId: number }) => render({ conversationId, participantId }) }
      : {}),
    [render, conversationId],
  );
}
