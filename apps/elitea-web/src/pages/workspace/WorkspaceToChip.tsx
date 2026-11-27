/**
 * Who answers a Local work thread's message (ADR-0061 decision 2b, #3056), as the chat shows it: in a thread of two or
 * more agents, by the rule Main routes on (client-core `firstAnswerer`): the first agent it addresses, else the lead,
 * else the composer's agent (the sticky one). The composer footer's "To:" chip (`AgentChip`) shows it, and its menu
 * addresses the message to another agent (`@Name` into the composer); it was a second chip above the input until #3145.
 */
import { FLOOR_POLICY, addressedTo, firstAnswerer, floorOf } from '@elitea/client-core/conversation';

import { requestAddress, useThreadFloor } from './useComposerMentions';
import type { ThreadAgent } from './useEnsureConversation';
import type { ThreadFloor } from './useSendPrompt';

const NO_AGENTS: readonly ThreadAgent[] = [];

/** The composer's agents for a local thread (`conversationId` `''`: none, a new thread or the cloud copy), and addressing one. */
export function useComposerTeam(projectId: number, conversationId: string, local: boolean): { agents: readonly ThreadAgent[]; floor?: ThreadFloor; onAddress: (agent: ThreadAgent) => void } {
  const floor = useThreadFloor(projectId, local ? conversationId : '');
  const onAddress = (agent: ThreadAgent): void => requestAddress(conversationId, agent.name);
  if (floor === undefined) return { agents: NO_AGENTS, onAddress };
  return { agents: floor.agents, floor, onAddress };
}

export interface ToTarget {
  readonly agent: ThreadAgent;
  /** It answers as the lead (the message names no agent). */
  readonly isLead: boolean;
}

/** Who answers `draft` first; `undefined` with fewer than two agents, or when the person has to pick. */
export function toTarget(draft: string, agents: readonly ThreadAgent[], meta: unknown, fallbackParticipantId: number): ToTarget | undefined {
  if (agents.length < 2) return undefined;
  const addressed = addressedTo(draft, agents);
  const floor = floorOf(meta);
  const id = firstAnswerer(addressed, floor, agents, fallbackParticipantId);
  const agent = agents.find((candidate) => candidate.participantId === id);
  if (agent === undefined) return undefined;
  return { agent, isLead: addressed.length === 0 && floor.policy === FLOOR_POLICY.Lead && floor.leadParticipantId === id };
}
