import type { ApplicationListRow } from './toolPicker.types';

/**
 * Ported from EliteaUI `origin/main` (`46d85cda7`) `src/[fsd]/shared/lib/helpers/agentNesting.helpers.js`
 * (EL-7085 / #1559: disabled agents with tooltip explanation when they cannot be added).
 */

export function SWARM_REASON(name: string): string {
  return `The ${name} agent has Swarm Mode enabled and can't be added as a tool. Swarm agents are only supported in direct chat.`;
}

export function NESTING_REASON(name: string, maxTiers: number): string {
  return (
    `The default version of ${name} nests agents too deeply: agent nesting is limited to ${maxTiers} tiers ` +
    '(orchestrator → sub-orchestrator → leaf). Make a shallower version its default to add it.'
  );
}

/**
 * Tiers the entity being edited already uses. An agent is tier 1; a pipeline is a composition primitive
 * that consumes none (the backend's `agent_tier_contribution`), so its agents start at tier 1.
 */
export const AGENT_HOST_TIER = 1;
export const PIPELINE_HOST_TIER = 0;

/**
 * Whether adding an agent whose subtree is `candidateSubtreeTiers` deep would pass the tier limit.
 * Without both numbers from the backend it answers false: the backend check on add stays authoritative.
 */
export function wouldExceedAgentNestingDepth(
  candidateSubtreeTiers: number | undefined,
  maxTiers: number | undefined,
  hostTier: number = AGENT_HOST_TIER,
): boolean {
  return (
    typeof candidateSubtreeTiers === 'number' &&
    typeof maxTiers === 'number' &&
    hostTier + candidateSubtreeTiers > maxTiers
  );
}

/**
 * Why a project list row cannot be added as a tool because of its nesting depth.
 * Rows carry `agent_subtree_tiers` and `max_agent_nesting_tiers` when requested.
 */
export function getNestingDisabledReason(
  row: Pick<ApplicationListRow, 'name' | 'agent_subtree_tiers' | 'max_agent_nesting_tiers'>,
  hostTier?: number,
): string {
  if (
    wouldExceedAgentNestingDepth(row.agent_subtree_tiers, row.max_agent_nesting_tiers, hostTier) &&
    typeof row.max_agent_nesting_tiers === 'number'
  ) {
    return NESTING_REASON(row.name, row.max_agent_nesting_tiers);
  }
  return '';
}

/**
 * Why a project list row cannot be added as a tool by an agent or pipeline:
 * Swarm Mode first, then nesting depth.
 */
export function getAgentDisabledReason(
  row: Pick<ApplicationListRow, 'name' | 'has_swarm' | 'agent_subtree_tiers' | 'max_agent_nesting_tiers'>,
  hostTier?: number,
): string {
  return row.has_swarm ? SWARM_REASON(row.name) : getNestingDisabledReason(row, hostTier);
}
