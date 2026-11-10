import { describe, expect, it } from 'vitest';

import { workforceAgent, workforceProposal } from '../__tests__/fixtures';
import { EMPTY_DEMOTION, abilitiesFor, agentStateLabel, canReinstate, narrowingOf } from './workforce';

describe('workforce rules', () => {
  it('names the most severe state', () => {
    expect(agentStateLabel(workforceAgent())).toBe('Active');
    expect(agentStateLabel(workforceAgent({ under_review: true }))).toBe('Under review');
    expect(agentStateLabel(workforceAgent({ under_review: true, suspension_id: 3 }))).toBe('Suspended');
    expect(agentStateLabel(workforceAgent({ lifecycle_state: 'retired', suspension_id: 3 }))).toBe('Retired');
  });

  it('reinstates only a retired agent inside its window', () => {
    const now = new Date('2026-11-09T00:00:00Z');
    expect(canReinstate(workforceAgent({ lifecycle_state: 'retired', reinstate_until: '2026-12-01T00:00:00Z' }), now)).toBe(true);
    expect(canReinstate(workforceAgent({ lifecycle_state: 'retired', reinstate_until: '2026-11-01T00:00:00Z' }), now)).toBe(false);
    expect(canReinstate(workforceAgent({ reinstate_until: '2026-12-01T00:00:00Z' }), now)).toBe(false);
  });

  it('gives each role its own actions', () => {
    const open = workforceProposal();
    const manager = abilitiesFor(open, workforceAgent(), 4);
    expect(manager).toMatchObject({ decide: true, askOwner: true, comment: false, appeal: false });
    const owner = abilitiesFor(workforceProposal({ status: 'asked_owner' }), workforceAgent({ can_manage: false, is_owner: true }), 5);
    expect(owner).toMatchObject({ decide: true, askOwner: false, comment: true });
    const approved = workforceProposal({ status: 'approved', decided_by_user_id: 4 });
    expect(abilitiesFor(approved, workforceAgent({ can_manage: false, is_owner: true }), 5).appeal).toBe(true);
    expect(abilitiesFor(workforceProposal({ status: 'approved', stage: 'delete' }), workforceAgent({ is_owner: true }), 5).appeal).toBe(false);
    const appealed = workforceProposal({ status: 'approved', appeal_state: 'open', decided_by_user_id: 4 });
    expect(abilitiesFor(appealed, workforceAgent(), 4).decideAppeal).toBe(false);
    expect(abilitiesFor(appealed, workforceAgent(), 9).decideAppeal).toBe(true);
    expect(abilitiesFor(appealed, workforceAgent({ is_owner: true }), 9).decideAppeal).toBe(false);
  });

  it('builds the exact narrowing of a demotion', () => {
    expect(narrowingOf(EMPTY_DEMOTION)).toBeUndefined();
    expect(narrowingOf({ ...EMPTY_DEMOTION, removeToolkitIds: '3, x' })).toBeUndefined();
    expect(narrowingOf({ ...EMPTY_DEMOTION, removeToolkitIds: '3, 12', removeLead: true, removeReviewAgents: true })).toEqual({
      remove_toolkit_ids: [3, 12],
      remove_lead: true,
      remove_capabilities: ['can_review_agents'],
    });
  });
});
