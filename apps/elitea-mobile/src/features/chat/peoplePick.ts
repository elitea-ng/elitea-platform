import { isPeopleOnlyTurn, type TurnMentions } from '@/api/queries/sendMessage';

/**
 * The agents a message addresses once the "To:" chip's deliberate pick is counted.
 *
 * A message that mentions people and names no agent is for those people alone (contract 1.51) and
 * starts no turn. An agent the person picked on purpose (the chip or "#", kept in the target store;
 * NOT the agent that answered last, which is only the fallback target) has to travel as an address to
 * keep the turn, because Main cannot tell a pick from the sticky `participant_id`. The pick counts only
 * when it is an agent of this conversation: Main refuses any other addressee for the whole message.
 */
export function addressWithChosen(
  addressedTo: readonly number[],
  mentions: TurnMentions | null | undefined,
  chosen: number | null,
  agentIds: readonly number[],
): number[] {
  if (chosen === null || !agentIds.includes(chosen)) return [...addressedTo];
  return isPeopleOnlyTurn({ mentions: mentions ?? null, addressedTo: [...addressedTo] }) ? [chosen] : [...addressedTo];
}

/**
 * Whether the conversation's agents can be addressed by name: with several agents, or with one agent among
 * other people. In a mixed conversation a message that mentions people and names no agent is for them alone,
 * so its one agent has to be addressable too ("@Clarifier @Aksana check this").
 */
export function canAddressAgents(agentCount: number, otherPeople: number): boolean {
  return agentCount > 1 || (agentCount === 1 && otherPeople > 0);
}
