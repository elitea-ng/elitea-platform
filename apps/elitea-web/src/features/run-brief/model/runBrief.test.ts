import { describe, expect, it } from 'vitest';

import { goalCheckOfChainEnd, readGoalCheck, readRunBrief } from './runBrief';

const brief = {
  goal: 'Fix the failing login test',
  done_when: [
    { id: 'c1', text: 'The login test passes' },
    { id: 'c2', text: 'No other test fails' },
  ],
  constraints: ['Do not change the public API'],
  assumptions: ['The test runner is vitest'],
  ambiguity: 'low',
};

describe('readRunBrief', () => {
  it('reads a ready brief and the states without one', () => {
    expect(readRunBrief({ state: 'ready', brief })).toEqual({ state: 'ready', brief });
    expect(readRunBrief({ state: 'pending' })).toEqual({ state: 'pending' });
    expect(readRunBrief({ state: 'failed', brief })).toEqual({ state: 'failed' });
    expect(readRunBrief({ state: 'off' })).toEqual({ state: 'off' });
  });

  it('reads a ready state with no valid brief as failed, and an unknown shape as absent', () => {
    expect(readRunBrief({ state: 'ready', brief: { goal: '  ' } })).toEqual({ state: 'failed' });
    expect(readRunBrief({ state: 'done' })).toBeUndefined();
    expect(readRunBrief('ready')).toBeUndefined();
    expect(readRunBrief(undefined)).toBeUndefined();
  });

  it('bounds the fields again: five items, the lengths of the contract, and only valid conditions', () => {
    const many = Array.from({ length: 7 }, (_, index) => `item ${index}`);
    const read = readRunBrief({
      state: 'ready',
      brief: {
        goal: 'g'.repeat(400),
        done_when: [{ id: 'c1', text: 'x'.repeat(400) }, { id: 'c2' }, 'c3', { id: 'c4', text: 'ok' }],
        constraints: many,
        assumptions: [1, 'kept'],
        ambiguity: 'extreme',
      },
    });
    const bounded = read?.brief;
    if (bounded === undefined) throw new Error('no brief');
    expect(bounded.goal).toHaveLength(300);
    expect(bounded.done_when.map((condition) => condition.id)).toEqual(['c1', 'c4']);
    expect(bounded.done_when.map((condition) => condition.text.length)).toEqual([300, 2]);
    expect(bounded.constraints).toEqual(many.slice(0, 5));
    expect(bounded.assumptions).toEqual(['kept']);
    expect(bounded.ambiguity).toBe('low');
  });
});

describe('readGoalCheck', () => {
  it('reads each outcome with its counts and reason', () => {
    const met = { outcome: 'met', conditions: [{ id: 'c1', text: 'a', verdict: 'met', reason: 'shown' }], met: 1, total: 1 };
    expect(readGoalCheck(met)).toEqual(met);
    expect(readGoalCheck({ outcome: 'not_met', conditions: [], met: 1, total: 3, reason: 'Test 2 still fails' })).toEqual({ outcome: 'not_met', conditions: [], met: 1, total: 3, reason: 'Test 2 still fails' });
    expect(readGoalCheck({ outcome: 'not_checked', met: 0, total: 0 })).toEqual({ outcome: 'not_checked', conditions: [], met: 0, total: 0 });
  });

  it('counts from the conditions when the totals are absent, and drops a shape that is not a check', () => {
    const counted = readGoalCheck({
      outcome: 'blocked',
      conditions: [
        { id: 'c1', text: 'a', verdict: 'met', reason: '' },
        { id: 'c2', text: 'b', verdict: 'impossible', reason: 'No access' },
        { id: 'c3', text: 'c', verdict: 'maybe' },
      ],
      reason: 'No access',
    });
    expect(counted).toMatchObject({ outcome: 'blocked', met: 1, total: 2, reason: 'No access' });
    expect(readGoalCheck({ outcome: 'done' })).toBeUndefined();
    expect(readGoalCheck(null)).toBeUndefined();
  });

  it('never lets met pass total', () => {
    expect(readGoalCheck({ outcome: 'met', met: 5, total: 3 })).toMatchObject({ met: 3, total: 3 });
  });
});

describe('goalCheckOfChainEnd', () => {
  it('reads the short goal of chain_ended, and nothing when it has none', () => {
    expect(goalCheckOfChainEnd({ outcome: 'met', met: 3, total: 3 })).toEqual({ outcome: 'met', conditions: [], met: 3, total: 3 });
    expect(goalCheckOfChainEnd(undefined)).toBeUndefined();
  });
});
