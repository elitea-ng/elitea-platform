import { describe, expect, it } from 'vitest';

import {
  AGENT_HOST_TIER,
  NESTING_REASON,
  PIPELINE_HOST_TIER,
  SWARM_REASON,
  getAgentDisabledReason,
  getNestingDisabledReason,
  wouldExceedAgentNestingDepth,
} from './agentNesting.helpers';

describe('agentNesting.helpers (EL-7085)', () => {
  it('computes nesting depth exceedance at boundary (limit vs limit + 1)', () => {
    // Pipeline host consumes 0 tiers: subtree 3 <= max 3 fits; subtree 4 > max 3 exceeds
    expect(wouldExceedAgentNestingDepth(3, 3, PIPELINE_HOST_TIER)).toBe(false);
    expect(wouldExceedAgentNestingDepth(4, 3, PIPELINE_HOST_TIER)).toBe(true);

    // Agent host consumes 1 tier: subtree 2 + 1 = 3 fits; subtree 3 + 1 = 4 exceeds
    expect(wouldExceedAgentNestingDepth(2, 3, AGENT_HOST_TIER)).toBe(false);
    expect(wouldExceedAgentNestingDepth(3, 3, AGENT_HOST_TIER)).toBe(true);
  });

  it('returns false when backend tier metadata is absent', () => {
    expect(wouldExceedAgentNestingDepth(undefined, 3, PIPELINE_HOST_TIER)).toBe(false);
    expect(wouldExceedAgentNestingDepth(2, undefined, PIPELINE_HOST_TIER)).toBe(false);
  });

  it('explains why an over-nested agent is disabled', () => {
    const reason = getNestingDisabledReason(
      { name: 'DeepAgent', agent_subtree_tiers: 4, max_agent_nesting_tiers: 3 },
      PIPELINE_HOST_TIER,
    );
    expect(reason).toBe(NESTING_REASON('DeepAgent', 3));

    expect(
      getNestingDisabledReason(
        { name: 'ShallowAgent', agent_subtree_tiers: 2, max_agent_nesting_tiers: 3 },
        PIPELINE_HOST_TIER,
      ),
    ).toBe('');
  });

  it('prioritizes Swarm Mode reason before nesting depth', () => {
    const reason = getAgentDisabledReason(
      {
        name: 'SwarmAndDeep',
        has_swarm: true,
        agent_subtree_tiers: 5,
        max_agent_nesting_tiers: 3,
      },
      PIPELINE_HOST_TIER,
    );
    expect(reason).toBe(SWARM_REASON('SwarmAndDeep'));
  });

  it('returns empty string when agent can be added', () => {
    expect(
      getAgentDisabledReason(
        { name: 'ReadyAgent', has_swarm: false, agent_subtree_tiers: 1, max_agent_nesting_tiers: 3 },
        PIPELINE_HOST_TIER,
      ),
    ).toBe('');
  });
});
